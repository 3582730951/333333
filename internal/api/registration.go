// Package api provides HTTP API for registration features
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/registration/provider"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

// Handler provides registration HTTP endpoints
type Handler struct {
	store      *storage.Store
	up         *upstream.Client
	httpClient *http.Client
	pipeline   *pipeline.Pipeline
	cfg        *config.Config
	pipelineMu sync.RWMutex

	defaultMethod string // registration engine when a trigger names none (boot default)
	defaultGroup  string // account group when a trigger names none (boot default)
	concurrency   int    // max parallel registrations per batch (boot default)
	enabled       bool

	mu            sync.Mutex
	jobCancels    map[string]context.CancelFunc // running job id → cancel
	jobWG         sync.WaitGroup
	runtimeCtx    context.Context
	runtimeCancel context.CancelFunc
	runtimeActive bool
}

const (
	registrationBatchMaxCount      = 100
	registrationPersistenceTimeout = 5 * time.Second
)

var errInvalidRegisterRequest = errors.New("invalid registration request")
var errPaymentFeatureRemoved = errors.New("payment_feature_removed")
var errRegistrationDisabled = errors.New("registration is disabled until readiness and canary checks pass")
var errRegistrationNotReady = errors.New("registration method readiness checks failed")

// NewHandler creates a registration handler. It builds the live provider Manager from the
// provider_settings table and wires the pipeline with an egress-aware upstream client, so
// the operator's saved SMS/mailbox/captcha providers actually run. Call ReloadProviders
// whenever provider settings change.
func NewHandler(store *storage.Store, up *upstream.Client, defaultMethod string, concurrency int, cfg *config.Config) *Handler {
	hc := &http.Client{Timeout: 60 * time.Second}
	if strings.TrimSpace(defaultMethod) == "" {
		defaultMethod = "protocol_v2"
	}
	defaultGroup := config.DefaultGroupName
	if cfg != nil {
		defaultGroup = firstNonEmpty(cfg.RegistrationDefaultGroup, cfg.DefaultGroup, config.DefaultGroupName)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	enabled := cfg != nil && cfg.RegistrationEnabled
	h := &Handler{store: store, up: up, httpClient: hc, pipeline: nil, cfg: cfg, defaultMethod: defaultMethod, defaultGroup: defaultGroup, concurrency: concurrency, enabled: enabled, jobCancels: map[string]context.CancelFunc{}}
	mgr, err := provider.BuildManagerWithError(context.Background(), store, hc)
	if err != nil {
		log.Printf("[REGISTRATION] provider manager bootstrap failed: %v", err)
	}
	h.pipeline = pipeline.NewPipeline(store, mgr, up, cfg)
	// Wire the structured log bridge so the pipeline's step logs flow into
	// registration_task_events and appear in the admin "注册事件记录" view.
	h.pipeline.LogEvent = h.logEventDetail
	return h
}

// StartRuntime binds future registration jobs to the active worker lifetime.
// Standby workers never call this method, so they cannot launch registrar
// subprocesses or acquire provider resources.
func (h *Handler) StartRuntime(ctx context.Context) {
	if h == nil || ctx == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runtimeActive && h.runtimeCtx != nil && h.runtimeCtx.Err() == nil {
		return
	}
	if h.store != nil {
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, registrationPersistenceTimeout)
		recovered, err := h.store.RecoverInterruptedRegistrationWorkflows(recoveryCtx)
		recoveryCancel()
		if err != nil {
			h.runtimeActive = false
			log.Printf("[REGISTRATION] interrupted workflow recovery failed; runtime remains disabled: %v", err)
			return
		}
		if recovered.JobsFinalized > 0 {
			log.Printf(
				"[REGISTRATION] isolated interrupted workflows: jobs=%d records=%d items=%d",
				recovered.JobsFinalized,
				recovered.RecordsFailed,
				recovered.ItemsQuarantined,
			)
		}
	}
	if h.runtimeCancel != nil {
		h.runtimeCancel()
	}
	if ctx.Err() != nil {
		h.runtimeActive = false
		return
	}
	h.runtimeCtx, h.runtimeCancel = context.WithCancel(ctx)
	h.runtimeActive = true
	runtimeCtx := h.runtimeCtx
	h.jobWG.Add(1)
	go h.runSMSPriceScanner(runtimeCtx)
}

func (h *Handler) runSMSPriceScanner(ctx context.Context) {
	defer h.jobWG.Done()
	defer supervisor.Recover("registration-sms-price-scanner")
	refresh := func() {
		refreshCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		active := h.currentPipeline()
		if active == nil {
			return
		}
		count, err := active.RefreshSMSPrices(refreshCtx)
		if err != nil {
			log.Printf("[REGISTRATION] SMS country price refresh completed with warnings: rows=%d err=%v", count, err)
			return
		}
		if count > 0 {
			log.Printf("[REGISTRATION] SMS country price refresh completed: rows=%d", count)
		}
	}
	refresh()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// StopRuntime prevents new jobs, cancels every child process through its job
// context, and waits only until the caller's shutdown deadline.
func (h *Handler) StopRuntime(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.runtimeActive = false
	if h.runtimeCancel != nil {
		h.runtimeCancel()
	}
	for _, cancel := range h.jobCancels {
		cancel()
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer supervisor.Recover("registration-runtime-wait")
		h.jobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func registrationPersistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, registrationPersistenceTimeout)
}

// resolveConcurrency returns the max parallel registrations for a batch: the admin-set
// "registration_concurrency" setting wins, else the boot default. Each parallel job is
// fully isolated by the orchestrator (unique egress IP via SID rotation, fingerprint
// seed, throwaway profile), so raising this is safe ON A ROTATING EGRESS; a fixed-IP
// egress should stay at 1 to honor the per-browser IP-uniqueness requirement.
//
// Memory/health guard: when the recent registration failure rate exceeds the configured
// threshold ("reg_failure_threshold", default 0.6) OR the system has been auto-degraded
// ("reg_degraded"), concurrency is forced to 1 so a struggling VPS is not asked to run
// multiple browser instances at once — the most common cause of OOM on low-RAM hosts.
func (h *Handler) resolveConcurrency(ctx context.Context) int {
	// Auto-degrade: if the operator (or the failure-rate watcher) has flipped
	// reg_degraded, force single-flight registration until it is cleared.
	if degraded, _ := h.flagEnabledStr(ctx, "reg_degraded", "false"); degraded {
		return 1
	}
	if h.recentFailureRate(ctx) >= h.failureThreshold(ctx) {
		return 1
	}
	// VPS 低配内存感知并发上限：根据系统总内存限制最多并发多少个浏览器实例。
	// ≤1.5G → max 1（绝不多开），≤4.5G → max 2，>4.5G → 不限制。
	memCap := memoryBasedConcurrencyCap()
	if v, ok := h.setting(ctx, "registration_concurrency"); ok {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil && n >= 1 {
			if n > memCap {
				return memCap
			}
			return n
		} else {
			logInvalidRegistrationSetting("registration_concurrency", trimmed, "integer >= 1")
		}
	}
	if v, ok := h.setting(ctx, "email_registration_concurrency"); ok {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil && n >= 1 {
			if n > memCap {
				return memCap
			}
			return n
		}
	}
	if legacy, ok := h.legacyEmailRegistrationSettings(ctx); ok && legacy.Concurrency > 0 {
		if legacy.Concurrency > memCap {
			return memCap
		}
		return legacy.Concurrency
	}
	if h.concurrency < 1 {
		return 1
	}
	if h.concurrency > memCap {
		return memCap
	}
	return h.concurrency
}

// memoryBasedConcurrencyCap returns the max browser instances this host can safely run in
// parallel based on total RAM. Reads /proc/meminfo on Linux; non-Linux returns a generous 4.
func memoryBasedConcurrencyCap() int {
	if runtime.GOOS != "linux" {
		return 4
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 4
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		mb := kb / 1024
		switch {
		case mb <= 1536:
			return 1 // 1H1G — single browser only
		case mb <= 4608:
			return 2 // 2H2G / 2H4G
		default:
			return 4 // 4H4G+
		}
	}
	return 4
}

// failureThreshold reads the "reg_failure_threshold" setting (default 0.6).
func (h *Handler) failureThreshold(ctx context.Context) float64 {
	if v, ok := h.setting(ctx, "reg_failure_threshold"); ok {
		trimmed := strings.TrimSpace(v)
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil && f > 0 && f <= 1 {
			return f
		} else {
			logInvalidRegistrationSetting("reg_failure_threshold", trimmed, "float in (0,1]")
		}
	}
	return 0.6
}

// recentFailureRate computes the failure rate over the most recent registration jobs
// (default window: last 10 jobs within the last 10 minutes). Returns 0 when there is
// not enough history to judge, so a cold start never auto-degrades.
func (h *Handler) recentFailureRate(ctx context.Context) float64 {
	var total, failed int
	rows, err := h.store.DB().QueryContext(ctx,
		`SELECT total, failed FROM registration_jobs
		 WHERE total > 0 AND status IN ('completed','completed_with_review','failed','cancelled')
		 ORDER BY created_at DESC LIMIT 10`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	for rows.Next() {
		var t, f int
		if err := rows.Scan(&t, &f); err != nil {
			continue
		}
		total += t
		failed += f
	}
	if total < 5 { // not enough history to judge
		return 0
	}
	return float64(failed) / float64(total)
}

// flagEnabledStr reads a boolean setting stored as a string ("true"/"1"/"false"/"0").
// Defaults to def when unset. Mirrors the api.Server.flagEnabled shape but on the
// registration Handler, which only has the store.
func (h *Handler) flagEnabledStr(ctx context.Context, key, def string) (bool, error) {
	v, ok := h.setting(ctx, key)
	if !ok {
		switch strings.ToLower(strings.TrimSpace(def)) {
		case "true", "1", "on", "yes":
			return true, nil
		}
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true, nil
	case "false", "0", "off", "no":
		return false, nil
	}
	logInvalidRegistrationSetting(key, strings.TrimSpace(v), "boolean")
	return false, nil
}

// registrationEnabled resolves the hot setting first and the boot value second.
// This lets an upgraded deployment turn registration on or off from the settings
// center without restarting, while malformed stored values retain the known boot
// state instead of silently changing behavior.
func (h *Handler) registrationEnabled(ctx context.Context) bool {
	if value, ok := h.setting(ctx, "registration_enabled"); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "on", "yes":
			return true
		case "false", "0", "off", "no":
			return false
		default:
			logInvalidRegistrationSetting("registration_enabled", strings.TrimSpace(value), "boolean")
		}
	}
	if value, ok := h.setting(ctx, "email_registration_enabled"); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "on", "yes":
			return true
		case "false", "0", "off", "no":
			return false
		}
	}
	return h != nil && h.enabled
}

func (h *Handler) resolveRegistrationTimeout(ctx context.Context) time.Duration {
	seconds := 0
	if value, ok := h.setting(ctx, "registration_timeout"); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			seconds = parsed
		} else {
			logInvalidRegistrationSetting("registration_timeout", strings.TrimSpace(value), "integer >= 1")
		}
	}
	if seconds == 0 {
		if value, ok := h.setting(ctx, "email_registration_timeout_seconds"); ok {
			seconds, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if seconds == 0 && h != nil && h.cfg != nil {
		seconds = h.cfg.RegistrationTimeout
	}
	if seconds <= 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (h *Handler) setting(ctx context.Context, key string) (string, bool) {
	if h == nil || h.store == nil {
		return "", false
	}
	v, ok, err := h.store.GetSetting(ctx, key)
	if err != nil {
		log.Printf("[REGISTRATION-CONFIG-ERROR] read setting %q failed: %v", key, err)
		return "", false
	}
	return v, ok
}

func logInvalidRegistrationSetting(key, value, kind string) {
	log.Printf("[REGISTRATION-CONFIG-WARN] setting %q has invalid %s value %q; using configured default", key, kind, value)
}

// resolveMethod returns the registration engine to use: an explicit request method
// wins; otherwise the admin-set "default_register_method" setting; otherwise the boot
// default (config DefaultRegisterMethod, "node"). One setting flips the engine for both
// the admin trigger and auto-refill.
func (h *Handler) resolveMethod(ctx context.Context, reqMethod string) string {
	if m := strings.TrimSpace(reqMethod); m != "" {
		return normalizeRegistrationMethodAlias(m)
	}
	if v, ok := h.setting(ctx, "default_register_method"); ok {
		if v = strings.TrimSpace(v); v != "" {
			return normalizeRegistrationMethodAlias(v)
		}
	}
	return normalizeRegistrationMethodAlias(h.defaultMethod)
}

func lockedIdentityModeForMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "node", "browser":
		return "phone"
	case "protocol_v2", "browser_v3":
		return "email"
	default:
		return ""
	}
}

func supportedRegistrationMethod(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "protocol", "protocol_v2", "node", "browser", "browser_v3":
		return true
	default:
		return false
	}
}

func registrationMethodUsesSMSCountry(method, identityMode string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "node", "browser", "browser_v3":
		return true
	case "protocol", "":
		return strings.EqualFold(strings.TrimSpace(identityMode), "phone")
	default:
		return false
	}
}

func registrationMethodRequiresSMSProvider(method, identityMode string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "node", "browser":
		return true
	case "protocol", "":
		return strings.EqualFold(strings.TrimSpace(identityMode), "phone")
	default:
		return false
	}
}

func registrationMethodRequiresMailboxProvider(method, identityMode string) bool {
	return strings.EqualFold(strings.TrimSpace(method), "protocol") && strings.EqualFold(strings.TrimSpace(identityMode), "email")
}

func registrationMethodRequiresEmailOTPProvider(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "protocol_v2", "browser_v3":
		return true
	default:
		return false
	}
}

func (h *Handler) normalizeRegisterRequest(ctx context.Context, req *pipeline.RegisterRequest) error {
	if req.UpgradeToPlus {
		return errPaymentFeatureRemoved
	}
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
	if req.Platform == "" {
		req.Platform = "chatgpt"
	}
	if req.Platform != "chatgpt" {
		return invalidRegisterRequest("unsupported platform %q", req.Platform)
	}

	req.Method = normalizeRegistrationMethodAlias(h.resolveMethod(ctx, req.Method))
	if req.Method == "" {
		req.Method = "protocol_v2"
	}
	if !supportedRegistrationMethod(req.Method) {
		return invalidRegisterRequest("unsupported method %q", req.Method)
	}

	if req.Count < 1 {
		return invalidRegisterRequest("count must be >= 1")
	}
	if req.Count > registrationBatchMaxCount {
		return invalidRegisterRequest("count must be <= %d", registrationBatchMaxCount)
	}

	req.GroupName = strings.TrimSpace(req.GroupName)
	if req.GroupName == "" {
		if value, ok := h.setting(ctx, "registration_default_group"); ok {
			req.GroupName = strings.TrimSpace(value)
		}
	}
	if req.GroupName == "" {
		if value, ok := h.setting(ctx, "reg_default_group"); ok {
			req.GroupName = strings.TrimSpace(value)
		}
	}
	if req.GroupName == "" {
		if legacy, ok := h.legacyEmailRegistrationSettings(ctx); ok {
			req.GroupName = strings.TrimSpace(legacy.GroupName)
		}
	}
	if req.GroupName == "" && h.cfg != nil {
		req.GroupName = strings.TrimSpace(h.cfg.RegistrationDefaultGroup)
	}
	if req.GroupName == "" {
		req.GroupName = strings.TrimSpace(h.defaultGroup)
	}
	if req.GroupName == "" {
		req.GroupName = config.DefaultGroupName
	}
	if _, err := h.store.GetGroup(ctx, req.GroupName); err != nil {
		return invalidRegisterRequest("group %q not found", req.GroupName)
	}

	req.EgressID = strings.TrimSpace(req.EgressID)
	if req.EgressID != "" {
		if _, err := h.store.GetEgressProfile(ctx, req.EgressID); err != nil {
			return invalidRegisterRequest("egress %q not found", req.EgressID)
		}
		req.RegistrationEgressPoolID = ""
	} else {
		req.RegistrationEgressPoolID = strings.TrimSpace(req.RegistrationEgressPoolID)
		if req.RegistrationEgressPoolID == "" {
			if poolID, ok := h.setting(ctx, "registration_egress_pool_id"); ok {
				req.RegistrationEgressPoolID = strings.TrimSpace(poolID)
			}
		}
		if req.RegistrationEgressPoolID == "" {
			if poolID, ok := h.setting(ctx, "reg_default_egress"); ok {
				req.RegistrationEgressPoolID = strings.TrimSpace(poolID)
			}
		}
		if req.RegistrationEgressPoolID == "" {
			if poolID, ok := h.setting(ctx, "email_registration_egress_pool_id"); ok {
				req.RegistrationEgressPoolID = strings.TrimSpace(poolID)
			}
		}
		if req.RegistrationEgressPoolID == "" {
			if legacy, ok := h.legacyEmailRegistrationSettings(ctx); ok {
				req.RegistrationEgressPoolID = strings.TrimSpace(legacy.EgressPoolID)
			}
		}
		if req.RegistrationEgressPoolID == "" && h.cfg != nil {
			req.RegistrationEgressPoolID = strings.TrimSpace(h.cfg.RegistrationEgressPoolID)
		}
		if req.RegistrationEgressPoolID == "" {
			return invalidRegisterRequest("registration_egress_pool_id is required; configure the default registration egress pool or pass a registration pool")
		}
		if _, err := h.getRegistrationEgressPool(ctx, req.RegistrationEgressPoolID); err != nil {
			return invalidRegisterRequest("registration egress pool %q is not a registration pool: %v", req.RegistrationEgressPoolID, err)
		}
		if _, err := h.store.SelectEgressFromPool(ctx, req.RegistrationEgressPoolID); err != nil {
			return invalidRegisterRequest("registration egress pool %q is not usable: %v", req.RegistrationEgressPoolID, err)
		}
	}
	req.RuntimeEgressPoolID = ""

	req.IdentityMode = strings.ToLower(strings.TrimSpace(req.IdentityMode))
	if req.IdentityMode == "mail" || req.IdentityMode == "mailbox" {
		req.IdentityMode = "email"
	}
	switch req.IdentityMode {
	case "", "phone", "sms", "email":
	default:
		return invalidRegisterRequest("unsupported identity_mode %q", req.IdentityMode)
	}
	if req.IdentityMode == "sms" {
		req.IdentityMode = "phone"
	}
	if lock := lockedIdentityModeForMethod(req.Method); lock != "" {
		if req.IdentityMode != "" && req.IdentityMode != lock {
			return invalidRegisterRequest("%s requires identity_mode=%s", req.Method, lock)
		}
		req.IdentityMode = lock
	}
	if req.IdentityMode == "" {
		req.IdentityMode = "phone"
	}
	req.Country = strings.ToUpper(strings.TrimSpace(req.Country))
	if h.store != nil {
		strategy := "auto"
		if v, ok, err := h.store.GetSetting(ctx, "sms_platform_strategy"); err != nil {
			return fmt.Errorf("read sms_platform_strategy: %w", err)
		} else if ok {
			strategy = strings.ToLower(strings.TrimSpace(v))
		}
		switch strategy {
		case "", "auto":
		case "manual":
			if registrationMethodUsesSMSCountry(req.Method, req.IdentityMode) && req.Country == "" {
				if v, ok, err := h.store.GetSetting(ctx, "sms_manual_country"); err != nil {
					return fmt.Errorf("read sms_manual_country: %w", err)
				} else if ok {
					country, err := normalizePhoneCountryISO(v, true)
					if err != nil {
						return invalidRegisterRequest("sms_manual_country: %v", err)
					}
					req.Country = country
				}
			}
			if registrationMethodUsesSMSCountry(req.Method, req.IdentityMode) && req.Country == "" {
				return invalidRegisterRequest("sms manual country is required when sms_platform_strategy=manual")
			}
		default:
			return invalidRegisterRequest("unsupported sms_platform_strategy %q", strategy)
		}
	}
	req.SMSProvider = strings.ToLower(strings.TrimSpace(req.SMSProvider))
	if req.SMSProvider == "" {
		if value, ok := h.setting(ctx, "default_sms_provider"); ok {
			req.SMSProvider = strings.ToLower(strings.TrimSpace(value))
		} else if value, ok := h.setting(ctx, "reg_default_sms"); ok {
			req.SMSProvider = strings.ToLower(strings.TrimSpace(value))
		} else if h.cfg != nil {
			req.SMSProvider = strings.ToLower(strings.TrimSpace(h.cfg.DefaultSMSProvider))
		}
	}
	req.MailboxProvider = normalizeMailboxProviderAlias(req.MailboxProvider)
	if req.MailboxProvider == "" && req.IdentityMode == "email" {
		req.MailboxProvider = h.resolveDefaultMailboxProvider(ctx)
	}
	var mailboxDomainErr error
	req.MailboxDomain, mailboxDomainErr = storage.NormalizeMailboxDomain(req.MailboxDomain)
	if mailboxDomainErr != nil {
		return invalidRegisterRequest("mailbox_domain: %v", mailboxDomainErr)
	}
	req.CaptchaSolver = strings.TrimSpace(req.CaptchaSolver)
	if req.CaptchaSolver == "" {
		if value, ok := h.setting(ctx, "default_captcha_provider"); ok {
			req.CaptchaSolver = strings.TrimSpace(value)
		} else if value, ok := h.setting(ctx, "reg_default_captcha"); ok {
			req.CaptchaSolver = strings.TrimSpace(value)
		} else if h.cfg != nil {
			req.CaptchaSolver = strings.TrimSpace(h.cfg.DefaultCaptchaProvider)
		}
	}
	return nil
}

func (h *Handler) resolveDefaultMailboxProvider(ctx context.Context) string {
	if value, ok := h.setting(ctx, "default_mailbox_provider"); ok && strings.TrimSpace(value) != "" {
		return normalizeMailboxProviderAlias(value)
	}
	if value, ok := h.setting(ctx, "reg_default_mailbox"); ok && strings.TrimSpace(value) != "" {
		return normalizeMailboxProviderAlias(value)
	}
	configured := ""
	if h != nil && h.cfg != nil {
		configured = normalizeMailboxProviderAlias(h.cfg.DefaultMailboxProvider)
	}
	manager, err := provider.BuildManagerWithError(ctx, h.store, h.httpClient)
	if err != nil || manager == nil {
		return configured
	}
	fallback := ""
	for _, candidate := range manager.Mailbox {
		name := normalizeMailboxProviderAlias(candidate.Name())
		if name == configured && configured != "" {
			return configured
		}
		if name == "email_pool" {
			fallback = name
		} else if fallback == "" {
			fallback = name
		}
	}
	return firstNonEmpty(fallback, configured)
}

func invalidRegisterRequest(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", errInvalidRegisterRequest, fmt.Sprintf(format, args...))
}

func registrationErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, storage.ErrRegistrationIdentityExists):
		return "duplicate_remote_identity"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "provider"), strings.Contains(text, "mailbox"), strings.Contains(text, "sms"):
		return "provider_unavailable"
	case strings.Contains(text, "egress"), strings.Contains(text, "proxy"):
		return "egress_unavailable"
	case strings.Contains(text, "credential"), strings.Contains(text, "access token"), strings.Contains(text, "identity mismatch"):
		return "credential_invalid"
	case strings.Contains(text, "liveness"):
		return "remote_liveness_failed"
	case strings.Contains(text, "no account produced"), strings.Contains(text, "signup"):
		return "registration_incomplete"
	default:
		return "internal_failure"
	}
}

// ReloadProviders rebuilds the provider Manager from the current provider_settings and
// re-wires the pipeline, so saving providers in the UI takes effect without a restart.
func (h *Handler) ReloadProviders(ctx context.Context) error {
	mgr, err := provider.BuildManagerWithError(ctx, h.store, h.httpClient)
	if err != nil {
		return err
	}
	// cfg is nil here — ReloadProviders is called after save, the pipeline already has
	// its original cfg from NewHandler. We keep the same pipeline's httpClient/cfg.
	next := pipeline.NewPipeline(h.store, mgr, h.up, h.cfg)
	next.LogEvent = h.logEventDetail
	h.pipelineMu.Lock()
	h.pipeline = next
	h.pipelineMu.Unlock()
	return nil
}

func (h *Handler) currentPipeline() *pipeline.Pipeline {
	h.pipelineMu.RLock()
	defer h.pipelineMu.RUnlock()
	return h.pipeline
}

func (h *Handler) refreshSMSPrices(ctx context.Context) (int, error) {
	active := h.currentPipeline()
	if active == nil {
		return 0, errors.New("registration pipeline is not initialized")
	}
	return active.RefreshSMSPrices(ctx)
}

func (h *Handler) smsMarketSnapshot(ctx context.Context) ([]provider.SMSMarketCandidate, float64, float64, []string) {
	active := h.currentPipeline()
	if active == nil {
		return []provider.SMSMarketCandidate{}, 0, 0, []string{"BR", "CO", "PL"}
	}
	return active.SMSMarketSnapshot(ctx)
}

// HandleRegisterBatch starts a batch registration job
func (h *Handler) HandleRegisterBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	req, err := decodeRegistrationRequest(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	jobID, err := h.StartJob(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errPaymentFeatureRemoved) {
			status = http.StatusGone
		} else if errors.Is(err, errRegistrationDisabled) {
			status = http.StatusForbidden
		} else if errors.Is(err, errRegistrationCanaryRequired) {
			status = http.StatusConflict
		} else if errors.Is(err, errRegistrationNotReady) {
			status = http.StatusServiceUnavailable
		} else if errors.Is(err, errInvalidRegisterRequest) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}

	w.Header().Set("Location", "/admin/register/jobs/"+jobID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"job_id": jobID,
		"status": "queued",
	})
}

// StartJob inserts a registration job row and launches its async batch processing under a
// cancelable context. Returns the job id. Shared by the HTTP handler and the automation
// scheduler (auto-refill).
func (h *Handler) StartJob(ctx context.Context, req pipeline.RegisterRequest) (string, error) {
	if req.Canary {
		return "", invalidRegisterRequest("canary jobs must use /admin/register/canary")
	}
	return h.startRegistrationJob(ctx, req, false)
}

// EnqueueLifecycleReplacement reuses the exact registration readiness, canary,
// egress-pool, provider, concurrency, and persistence gates used by manual and
// refill jobs. The team workflow never grows a second registration stack.
func (h *Handler) EnqueueLifecycleReplacement(ctx context.Context, workflow storage.TeamLifecycleWorkflow) (string, error) {
	if h == nil {
		return "", errRegistrationNotReady
	}
	method := h.defaultMethod
	if candidate := strings.TrimSpace(workflow.ReplacementMethod); candidate != "" {
		method = candidate
	}
	identityMode := ""
	if strings.TrimSpace(workflow.MailboxProviderKey) != "" ||
		strings.TrimSpace(workflow.RequiredEmailDomain) != "" {
		identityMode = "email"
	}
	return h.StartJob(ctx, pipeline.RegisterRequest{
		Platform:                        "chatgpt",
		Method:                          method,
		Count:                           1,
		GroupName:                       h.defaultGroup,
		IdentityMode:                    identityMode,
		MailboxProvider:                 workflow.MailboxProviderKey,
		MailboxDomain:                   workflow.RequiredEmailDomain,
		TeamLifecycleSourceWorkflowID:   workflow.ID,
		TeamLifecycleWorkspaceID:        workflow.WorkspaceID,
		TeamLifecycleParentAccountID:    workflow.ParentAccountID,
		TeamLifecycleReplacementMethod:  method,
		TeamLifecycleRotateThresholdBPS: workflow.RotateThresholdBPS,
		TeamLifecycleMaxAttempts:        workflow.MaxAttempts,
	})
}

func (h *Handler) StartCanary(ctx context.Context, req pipeline.RegisterRequest) (string, error) {
	if req.Count != 0 && req.Count != 1 {
		return "", invalidRegisterRequest("canary count must be exactly 1")
	}
	req.Count = 1
	return h.startRegistrationJob(ctx, req, true)
}

func (h *Handler) startRegistrationJob(ctx context.Context, req pipeline.RegisterRequest, canary bool) (string, error) {
	if req.UpgradeToPlus {
		return "", errPaymentFeatureRemoved
	}
	if err := h.normalizeRegisterRequest(ctx, &req); err != nil {
		return "", err
	}
	if !h.registrationEnabled(ctx) {
		return "", errRegistrationDisabled
	}
	req.Canary = canary
	readiness, err := h.registrationMethodReadiness(ctx, req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errRegistrationNotReady, err)
	}
	if !readiness.Ready {
		return "", errRegistrationNotReady
	}
	if !canary && !readiness.CanaryReady {
		return "", errRegistrationCanaryRequired
	}
	req.ReadinessFingerprint = readiness.Fingerprint
	jobID := fmt.Sprintf("job_%d", time.Now().UnixNano())
	configJSON, _ := json.Marshal(req)

	h.mu.Lock()
	if !h.runtimeActive || h.runtimeCtx == nil || h.runtimeCtx.Err() != nil {
		h.mu.Unlock()
		return "", errRegistrationNotReady
	}
	jctx, cancel := context.WithCancel(h.runtimeCtx)
	h.jobCancels[jobID] = cancel
	h.jobWG.Add(1)
	h.mu.Unlock()

	if _, err := h.store.DB().ExecContext(ctx,
		`INSERT INTO registration_jobs (id, platform, method, total, status, config_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
		jobID, req.Platform, req.Method, req.Count, string(configJSON), time.Now().Unix(), time.Now().Unix()); err != nil {
		cancel()
		h.mu.Lock()
		delete(h.jobCancels, jobID)
		h.mu.Unlock()
		h.jobWG.Done()
		return "", err
	}
	go h.runProcessBatch(jctx, jobID, req)
	return jobID, nil
}

func (h *Handler) runProcessBatch(ctx context.Context, jobID string, req pipeline.RegisterRequest) {
	defer h.jobWG.Done()
	defer func() {
		if v := recover(); v != nil {
			supervisor.LogPanic("registration-job", v)
			h.failRegistrationJob(jobID, fmt.Sprintf("registration job panic: %v", v))
		}
	}()
	h.processBatch(ctx, jobID, req)
}

func (h *Handler) failRegistrationJob(jobID, _ string) {
	bg, cancel := registrationPersistenceContext(nil)
	defer cancel()
	now := time.Now().Unix()
	_, _ = h.store.DB().ExecContext(bg,
		`UPDATE registration_jobs SET status='failed', error=?, completed_at=?, updated_at=? WHERE id=?`,
		"registration_internal_failure", now, now, jobID)
	h.logEvent(bg, jobID, "error", "Registration job failed (internal_failure)")
}

func (h *Handler) processBatch(ctx context.Context, jobID string, req pipeline.RegisterRequest) {
	// Release the cancel registration when the job ends (also cancels the context to
	// free any in-flight provider polls).
	defer func() {
		h.mu.Lock()
		if c := h.jobCancels[jobID]; c != nil {
			c()
		}
		delete(h.jobCancels, jobID)
		h.mu.Unlock()
	}()

	h.store.DB().ExecContext(ctx,
		`UPDATE registration_jobs SET status='running', started_at=?, updated_at=? WHERE id=?`,
		time.Now().Unix(), time.Now().Unix(), jobID)

	succeeded := 0
	failed := 0
	cancelled := false
	canaryAccountID := ""
	canaryErrorClass := ""

	// Bounded-concurrency worker pool. Each parallel registration is isolated by the
	// orchestrator (unique egress IP, fingerprint, throwaway profile). DB writes and the
	// shared counters run under `mu`, so concurrency never trips sqlite — only the slow
	// part (RegisterOne: the browser automation) runs in parallel. concurrency=1 (default)
	// preserves the original sequential behavior exactly.
	concurrency := h.resolveConcurrency(ctx)
	var mu sync.Mutex
	cancelled = runBounded(ctx, req.Count, concurrency, func(i int) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, h.resolveRegistrationTimeout(ctx))
		defer attemptCancel()
		recordID := fmt.Sprintf("rec_%d_%d", time.Now().UnixNano(), i)
		workflowItemID := fmt.Sprintf("rwi_%d_%d", time.Now().UnixNano(), i)
		start := time.Now()
		recordCreated := false
		settled := false
		defer func() {
			if v := recover(); v != nil {
				supervisor.LogPanic("registration-batch-worker", v)
				if settled {
					return
				}
				bg, cancel := registrationPersistenceContext(ctx)
				defer cancel()
				message := fmt.Sprintf("Registration %d failed (internal_failure)", i+1)
				duration := int(time.Since(start).Seconds())
				mu.Lock()
				defer mu.Unlock()
				failed++
				if !recordCreated {
					_, _ = h.store.DB().ExecContext(bg,
						`INSERT INTO registration_records (id, job_id, status, error, duration_seconds, created_at)
						 VALUES (?, ?, 'failed', ?, ?, ?)`,
						recordID, jobID, message, duration, time.Now().Unix())
				} else {
					_, _ = h.store.DB().ExecContext(bg,
						`UPDATE registration_records SET status='failed', error=?, duration_seconds=? WHERE id=?`,
						message, duration, recordID)
				}
				h.logEvent(bg, jobID, "error", message)
				_ = h.store.UpdateRegistrationWorkflowItem(bg, workflowItemID, storage.RegistrationItemFailed, "internal_failure")
				_, _ = h.store.DB().ExecContext(bg,
					`UPDATE registration_jobs SET succeeded=?, failed=?, updated_at=? WHERE id=?`,
					succeeded, failed, time.Now().Unix(), jobID)
			}
		}()
		mu.Lock()
		h.store.DB().ExecContext(ctx,
			`INSERT INTO registration_records (id, job_id, status, created_at)
				 VALUES (?, ?, 'pending', ?)`,
			recordID, jobID, time.Now().Unix())
		_ = h.store.CreateRegistrationWorkflowItem(ctx, workflowItemID, jobID, req.Method, req.Platform)
		recordCreated = true
		mu.Unlock()

		// The slow part runs OUTSIDE the lock → genuine parallelism across browsers.
		// Each worker resolves one concrete registration-pool member just before launch,
		// so concurrent jobs can spread across the dynamic residential pool.
		workerReq := req
		var err error
		if strings.TrimSpace(workerReq.EgressID) == "" {
			egress, selErr := h.store.SelectEgressFromPool(attemptCtx, workerReq.RegistrationEgressPoolID)
			if selErr != nil {
				err = fmt.Errorf("select registration egress: %w", selErr)
			} else {
				workerReq.EgressID = egress.ID
			}
		}
		workerReq.JobID = jobID
		workerReq.RecordID = recordID
		workerReq.WorkflowItemID = workflowItemID
		if err == nil {
			err = h.store.UpdateRegistrationWorkflowItem(attemptCtx, workflowItemID, storage.RegistrationItemResourcesLeased, "")
		}
		var account *storage.Account
		if err == nil {
			activePipeline := h.currentPipeline()
			if activePipeline == nil {
				err = errors.New("registration pipeline is not initialized")
			} else {
				// Snapshot one immutable pipeline for the complete attempt. A provider
				// reload affects the next attempt and cannot swap dependencies midway
				// through a browser/OAuth transaction.
				account, err = activePipeline.RegisterOne(attemptCtx, workerReq)
			}
		}
		if err == nil && account != nil {
			err = h.bindRegisteredAccountToRuntimePool(attemptCtx, workerReq, account.ID)
		}
		if err == nil && account != nil {
			err = h.enqueueRegisteredTeamLifecycle(attemptCtx, workerReq, account.ID)
		}
		duration := int(time.Since(start).Seconds())
		bg, cancel := registrationPersistenceContext(ctx)
		defer cancel()

		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed++
			errorClass := registrationErrorClass(err)
			if req.Canary {
				canaryErrorClass = errorClass
			}
			message := fmt.Sprintf("Registration %d failed (%s)", i+1, errorClass)
			h.store.DB().ExecContext(bg,
				`UPDATE registration_records SET status='failed', error=?, duration_seconds=? WHERE id=?`,
				errorClass, duration, recordID)
			_ = h.store.UpdateRegistrationWorkflowItem(bg, workflowItemID, storage.RegistrationItemFailed, errorClass)
			h.logEvent(bg, jobID, "error", message)
		} else {
			succeeded++
			if req.Canary {
				canaryAccountID = account.ID
			}
			h.store.DB().ExecContext(bg,
				`UPDATE registration_records SET status='success', account_id=?, duration_seconds=? WHERE id=?`,
				account.ID, duration, recordID)
			h.logEvent(bg, jobID, "info", fmt.Sprintf("Registration %d succeeded", i+1))
		}
		h.store.DB().ExecContext(bg,
			`UPDATE registration_jobs SET succeeded=?, failed=?, updated_at=? WHERE id=?`,
			succeeded, failed, time.Now().Unix(), jobID)
		settled = true
	})

	bg, cancel := registrationPersistenceContext(ctx)
	defer cancel()
	status := "completed"
	if cancelled {
		status = "cancelled"
	} else if failed > 0 && succeeded > 0 {
		status = "completed_with_review"
	} else if failed > 0 {
		status = "failed"
	}
	h.store.DB().ExecContext(bg,
		`UPDATE registration_jobs SET status=?, completed_at=?, updated_at=? WHERE id=?`,
		status, time.Now().Unix(), time.Now().Unix(), jobID)
	if req.Canary {
		canaryStatus := "failed"
		if status == "completed" && succeeded == 1 && failed == 0 {
			canaryStatus = "passed"
		}
		if cancelled {
			canaryErrorClass = "cancelled"
		}
		_ = h.store.RecordRegistrationCanary(
			bg, req.Method, canaryStatus, req.ReadinessFingerprint,
			jobID, canaryAccountID, canaryErrorClass,
		)
	}

	h.logEvent(bg, jobID, "info", fmt.Sprintf("Batch %s: %d succeeded, %d failed", status, succeeded, failed))

	// Post-batch health watch: if recent failures push the rolling failure rate past the
	// configured threshold, auto-degrade registration concurrency to 1 so a struggling
	// low-RAM VPS is not asked to run parallel browsers — and surface it in the log so
	// the operator sees why concurrency dropped. The operator can clear reg_degraded
	// from the SettingsV2 "日志与降级" tab once the underlying issue is fixed.
	if !req.Canary && succeeded+failed > 0 {
		rate := float64(failed) / float64(succeeded+failed)
		threshold := h.failureThreshold(bg)
		if rate >= threshold {
			_ = h.store.SetSetting(bg, "reg_degraded", "true")
			h.logEvent(bg, jobID, "warn", fmt.Sprintf(
				"Auto-degraded: batch failure rate %.0f%% >= threshold %.0f%%; concurrency forced to 1. Clear 'reg_degraded' in Settings to re-enable.",
				rate*100, threshold*100))
		}
	}
}

func (h *Handler) bindRegisteredAccountToRuntimePool(ctx context.Context, req pipeline.RegisterRequest, accountID string) error {
	// Legacy compatibility hook: registered accounts keep the direct binding created
	// by storage.UpsertAccount. Operators change runtime egress per account.
	_ = ctx
	_ = req
	_ = accountID
	return nil
}

func (h *Handler) enqueueRegisteredTeamLifecycle(
	ctx context.Context,
	req pipeline.RegisterRequest,
	accountID string,
) error {
	workspaceID := strings.TrimSpace(req.TeamLifecycleWorkspaceID)
	if workspaceID == "" {
		return nil
	}
	source := strings.TrimSpace(req.TeamLifecycleSourceWorkflowID)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"team-replacement", source, workspaceID, strings.TrimSpace(accountID),
	}, "\x00")))
	idempotencyKey := "team-replacement:" + hex.EncodeToString(sum[:16])
	_, _, err := h.store.CreateTeamLifecycleWorkflow(ctx, storage.CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:      idempotencyKey,
		WorkspaceID:         workspaceID,
		ParentAccountID:     req.TeamLifecycleParentAccountID,
		ChildAccountID:      accountID,
		ReplacementMethod:   req.TeamLifecycleReplacementMethod,
		MailboxProviderKey:  req.MailboxProvider,
		RequiredEmailDomain: req.MailboxDomain,
		RotateThresholdBPS:  req.TeamLifecycleRotateThresholdBPS,
		MaxAttempts:         req.TeamLifecycleMaxAttempts,
		ShadowMode:          false,
	})
	if err != nil {
		return fmt.Errorf("enqueue next team lifecycle: %w", err)
	}
	return nil
}

// runBounded runs n indexed tasks with at most `limit` running concurrently, calling
// work(i) for each. It returns cancelled=true if ctx was cancelled before all tasks were
// launched (already-running tasks still finish). limit<1 is treated as 1. Pure (no I/O),
// so the concurrency bound is unit-testable without the registration pipeline.
func runBounded(ctx context.Context, n, limit int, work func(i int)) (cancelled bool) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			cancelled = true
			wg.Wait()
			return cancelled
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer supervisor.Recover("registration-batch-worker")
			defer func() { <-sem }()
			work(i)
		}(i)
	}
	wg.Wait()
	return cancelled
}

// HandleJobList returns recent registration jobs for the task list. Optional ?status=
// filters by status (other than "all").
func (h *Handler) HandleJobList(w http.ResponseWriter, r *http.Request) {
	query := `SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error, created_at
		FROM registration_jobs`
	args := []interface{}{}
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" && st != "all" {
		query += ` WHERE status=?`
		args = append(args, st)
	}
	query += ` ORDER BY created_at DESC LIMIT 200`

	rows, err := h.store.DB().QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type job struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at"`
		CompletedAt int64  `json:"completed_at"`
		Error       string `json:"error"`
		CreatedAt   int64  `json:"created_at"`
	}
	jobs := []job{}
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.Platform, &j.Method, &j.Total, &j.Succeeded, &j.Failed,
			&j.Status, &j.StartedAt, &j.CompletedAt, &j.Error, &j.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": jobs})
}

// HandleJobCancel cancels a running job: it cancels the in-flight context (if the job is
// still running in this process) and marks a pending/running job 'cancelled' in the DB.
func (h *Handler) HandleJobCancel(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if strings.TrimSpace(jobID) == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing job id"))
		return
	}
	h.mu.Lock()
	cancel := h.jobCancels[jobID]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	res, err := h.store.DB().ExecContext(r.Context(),
		`UPDATE registration_jobs SET status='cancelled', completed_at=?, updated_at=? WHERE id=? AND status IN ('queued','pending','running')`,
		time.Now().Unix(), time.Now().Unix(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := res.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed == 0 {
		status, err := h.registrationJobStatus(r.Context(), jobID)
		if err != nil {
			h.writeRegistrationJobStatusError(w, jobID, err)
			return
		}
		writeError(w, http.StatusConflict, fmt.Errorf("registration job %q is %s and cannot be cancelled", jobID, status))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": jobID, "cancelled": true})
}

// HandleStats returns registration success-dashboard aggregates over the jobs/records
// tables: totals, a per-day success/fail series (last 14 days), and per-provider-error
// counts. Powers the 注册成功率仪表盘.
func (h *Handler) HandleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	db := h.store.DB()
	ctx := r.Context()

	var jobs, jobSucc, jobFail int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(succeeded),0), COALESCE(SUM(failed),0) FROM registration_jobs`).
		Scan(&jobs, &jobSucc, &jobFail); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var recTotal, recSucc, recFail int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN status='failed'  THEN 1 ELSE 0 END),0)
		 FROM registration_records`).
		Scan(&recTotal, &recSucc, &recFail); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	byDay := []map[string]interface{}{}
	rows, err := db.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') d,
		        SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status='failed'  THEN 1 ELSE 0 END)
		 FROM registration_records GROUP BY d ORDER BY d DESC LIMIT 14`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var s, f int
		if err := rows.Scan(&d, &s, &f); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		byDay = append(byDay, map[string]interface{}{"date": d, "succeeded": s, "failed": f})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Top failure reasons (truncated) for error aggregation.
	errs := []map[string]interface{}{}
	rows, err = db.QueryContext(ctx,
		`SELECT substr(error,1,80) e, COUNT(*) c FROM registration_records
		 WHERE status='failed' AND error<>'' GROUP BY e ORDER BY c DESC LIMIT 10`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		var c int
		if err := rows.Scan(&e, &c); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		errs = append(errs, map[string]interface{}{"error": e, "count": c})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	rate := 0.0
	if recTotal > 0 {
		rate = float64(recSucc) / float64(recTotal)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"totals": map[string]interface{}{"jobs": jobs, "records": recTotal, "succeeded": recSucc, "failed": recFail, "success_rate": rate},
		"by_day": byDay,
		"errors": errs,
	})
}

func (h *Handler) logEvent(ctx context.Context, taskID, level, message string) {
	h.logEventDetail(ctx, taskID, level, message, nil)
}

// logEventDetail writes a structured registration event with an optional detail
// object (JSON-encoded into detail_json). Used by the node registrar's step
// logging so each phase of a registration (proxy assign / sid rotate /
// fingerprint / browser launch / sms / mailbox / token read / subprocess output)
// is captured for the admin "注册事件记录" view and AI-analysis export.
func (h *Handler) logEventDetail(ctx context.Context, taskID, level, message string, detail interface{}) {
	// Respect the verbose-logging toggle: when an operator has turned OFF
	// reg_verbose_logging (e.g. on a low-RAM VPS where the DB was growing too
	// fast), only keep error/warn-level events so failures are still diagnosable
	// but the high-volume info steps are dropped.
	verbose := true
	if v, ok := h.setting(ctx, "reg_verbose_logging"); ok {
		trimmed := strings.TrimSpace(v)
		switch strings.ToLower(trimmed) {
		case "false", "0", "off", "no":
			verbose = false
		case "true", "1", "on", "yes", "":
			verbose = true
		default:
			logInvalidRegistrationSetting("reg_verbose_logging", trimmed, "boolean")
		}
	}
	if !verbose && level != "error" && level != "warn" {
		return
	}
	detailJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	h.store.DB().ExecContext(ctx,
		`INSERT INTO registration_task_events (task_id, level, message, detail_json, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		taskID, level, message, detailJSON, time.Now().Unix())
}

// HandleJobStatus returns job status
func (h *Handler) HandleJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("id")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing job id"))
		return
	}

	row := h.store.DB().QueryRowContext(r.Context(),
		`SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error
		 FROM registration_jobs WHERE id=?`, jobID)

	var job struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at"`
		CompletedAt int64  `json:"completed_at"`
		Error       string `json:"error"`
	}

	err := row.Scan(&job.ID, &job.Platform, &job.Method, &job.Total, &job.Succeeded,
		&job.Failed, &job.Status, &job.StartedAt, &job.CompletedAt, &job.Error)
	if err != nil {
		h.writeRegistrationJobStatusError(w, jobID, err)
		return
	}

	writeJSON(w, http.StatusOK, job)
}

// HandleJobEvents streams events via SSE
func (h *Handler) HandleJobEvents(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("id")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing job id"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}
	if _, err := h.registrationJobStatus(r.Context(), jobID); err != nil {
		h.writeRegistrationJobStatusError(w, jobID, err)
		return
	}

	rows, err := h.store.DB().QueryContext(r.Context(),
		`SELECT level, message, created_at FROM registration_task_events WHERE task_id=? ORDER BY id`,
		jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for rows.Next() {
		var level, message string
		var createdAt int64
		if err := rows.Scan(&level, &message, &createdAt); err != nil {
			writeRegistrationSSEError(w, err)
			flusher.Flush()
			return
		}
		data := map[string]interface{}{
			"level":   level,
			"message": message,
			"time":    createdAt,
		}
		dataJSON, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", dataJSON)
	}
	if err := rows.Err(); err != nil {
		writeRegistrationSSEError(w, err)
		flusher.Flush()
		return
	}
	flusher.Flush()

	// Keep connection alive (in production, use pub/sub)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (h *Handler) registrationJobStatus(ctx context.Context, jobID string) (string, error) {
	var status string
	err := h.store.DB().QueryRowContext(ctx, `SELECT status FROM registration_jobs WHERE id=?`, jobID).Scan(&status)
	return status, err
}

func (h *Handler) writeRegistrationJobStatusError(w http.ResponseWriter, jobID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("registration job %q not found", jobID))
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func writeRegistrationSSEError(w io.Writer, err error) {
	requestID := newRequestID()
	log.Printf("[REGISTRATION-SSE] internal stream failure request_id=%s class=%T", requestID, err)
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"type":       "server_error",
			"code":       "service_unavailable",
			"message":    "The relay service is temporarily unavailable. Please retry.",
			"request_id": requestID,
		},
	})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
}

// HandleProviderSettings manages provider configurations
func (h *Handler) HandleProviderSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listProviders(w, r)
	case http.MethodPost:
		h.createProvider(w, r)
	case http.MethodPut:
		h.updateProvider(w, r)
	case http.MethodDelete:
		h.deleteProvider(w, r)
	default:
		methodNotAllowed(w)
	}
}

// HandleProviderOptions returns the registered SMS / mailbox / captcha providers
// as {label, value} option lists, so the registration / automation / lifecycle
// forms can render Select dropdowns instead of free-text inputs. Reuses the live
// provider Manager built from provider_settings.
func (h *Handler) HandleProviderOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx := r.Context()
	mgr, err := provider.BuildManagerWithError(ctx, h.store, h.httpClient)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	smsOpts := make([]map[string]string, 0, len(mgr.SMS))
	for _, p := range mgr.SMS {
		name := p.Name()
		smsOpts = append(smsOpts, map[string]string{"label": name, "value": name})
	}
	mailOpts := make([]map[string]string, 0, len(mgr.Mailbox))
	for _, p := range mgr.Mailbox {
		name := p.Name()
		mailOpts = append(mailOpts, map[string]string{"label": name, "value": name})
	}
	capOpts := make([]map[string]string, 0, len(mgr.Captcha))
	for _, p := range mgr.Captcha {
		name := p.Name()
		capOpts = append(capOpts, map[string]string{"label": name, "value": name})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sms":     smsOpts,
		"mailbox": mailOpts,
		"captcha": capOpts,
	})
}

// regDefaultKeys maps the UI's default fields to their settings-table keys.
var regDefaultKeys = map[string]string{
	"sms":     "reg_default_sms",
	"mailbox": "reg_default_mailbox",
	"captcha": "reg_default_captcha",
	"group":   "reg_default_group",
	"egress":  "reg_default_egress",
}

var regCanonicalDefaultKeys = map[string]string{
	"sms":     "default_sms_provider",
	"mailbox": "default_mailbox_provider",
	"captcha": "default_captcha_provider",
	"group":   "registration_default_group",
	"egress":  "registration_egress_pool_id",
}

func cfgString(v interface{}) string { s, _ := v.(string); return strings.TrimSpace(s) }

func (h *Handler) getDefaults(ctx context.Context) map[string]string {
	out, _ := h.getDefaultsWithError(ctx)
	return out
}

func (h *Handler) getDefaultsWithError(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for field, legacyKey := range regDefaultKeys {
		for _, key := range []string{regCanonicalDefaultKeys[field], legacyKey} {
			v, ok, err := h.store.GetSetting(ctx, key)
			if err != nil {
				return out, fmt.Errorf("read registration default %q: %w", field, err)
			}
			if ok && v != "" {
				out[field] = v
				break
			}
		}
	}
	return out, nil
}

func (h *Handler) saveDefaults(ctx context.Context, d map[string]interface{}) error {
	if err := h.saveDefaultsWithExecutor(ctx, h.store.DB(), d); err != nil {
		return err
	}
	// saveDefaultsWithExecutor writes the settings table directly (bypassing SetSettings),
	// so invalidate the settings snapshot to make the change visible immediately.
	h.store.InvalidateSettingsCache()
	return nil
}

func (h *Handler) saveDefaultsWithExecutor(ctx context.Context, exec sqlExecutor, d map[string]interface{}) error {
	for field, key := range regDefaultKeys {
		if v, ok := d[field]; ok {
			for _, storageKey := range []string{regCanonicalDefaultKeys[field], key} {
				if _, err := exec.ExecContext(ctx,
					`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
					 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
					storageKey, cfgString(v), storage.Now()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// listProviders returns the configured providers AND the global defaults, in the
// {providers, defaults} shape the Provider/registration pages expect (the old bare-array
// response left the UI's data.providers / data.defaults undefined → nothing rendered).
func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.DB().QueryContext(r.Context(),
		`SELECT id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json
		 FROM provider_settings ORDER BY provider_type, priority DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	providers := []map[string]interface{}{}
	for rows.Next() {
		var id, providerType, providerKey, displayName, configJSON, authJSON string
		var enabled, priority int
		if err := rows.Scan(&id, &providerType, &providerKey, &displayName, &enabled, &priority, &configJSON, &authJSON); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		var config map[string]interface{}
		if strings.TrimSpace(configJSON) != "" {
			if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("provider %s/%s has invalid config_json: %w", providerType, providerKey, err))
				return
			}
		}
		if config == nil {
			config = map[string]interface{}{}
		}
		credentials := storage.ProviderAuthMetadata(authJSON)
		for field, value := range config {
			if !storage.IsProviderSecretField(field) {
				continue
			}
			if strings.TrimSpace(cfgString(value)) != "" {
				if _, exists := credentials[field]; !exists {
					credentials[field] = map[string]interface{}{
						"configured":  true,
						"masked":      "••••",
						"key_version": "legacy",
						"key_id":      "",
					}
				}
			}
			delete(config, field)
		}
		for field := range credentials {
			config[field] = ""
			config[field+"_configured"] = true
		}

		providers = append(providers, map[string]interface{}{
			"id":           id,
			"type":         providerType,
			"key":          providerKey,
			"display_name": displayName,
			"enabled":      enabled == 1,
			"priority":     priority,
			"config":       config,
			"credentials":  credentials,
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	defaults, err := h.getDefaultsWithError(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": providers,
		"defaults":  defaults,
	})
}

type providerInput struct {
	Type        string                 `json:"type"`
	Key         string                 `json:"key"`
	DisplayName string                 `json:"display_name"`
	Enabled     bool                   `json:"enabled"`
	Priority    int                    `json:"priority"`
	Config      map[string]interface{} `json:"config"`
}

type providerBulkInput struct {
	Providers     []providerInput        `json:"providers"`
	Defaults      map[string]interface{} `json:"defaults"`
	Registrar     map[string]interface{} `json:"registrar"`
	RegistrarMode string                 `json:"registrar_mode"`
}

type registrarBatchPatch struct {
	Provided bool
	Values   map[string]interface{}
	Mode     string
}

type providerBatchSaveResult struct {
	RegistrarSaved bool
	SettingsSaved  []settingsCenterDiff
}

type providerWriteError struct {
	ProviderType string
	ProviderKey  string
	cause        error
}

func (e *providerWriteError) Error() string { return "provider settings write failed" }
func (e *providerWriteError) Unwrap() error { return e.cause }

const providerReloadWarning = "Settings were saved, but the registration runtime could not reload them; the previous active configuration remains in use."

// createProvider accepts EITHER a bulk save
// {providers:[...], defaults:{...}, registrar:{...}, registrar_mode:"merge|replace"}
// or a single provider {type,key,...}. The registrar fields are optional, preserving the
// old request contract. When present, provider rows, defaults, and node registrar config
// are committed together before the live Manager is reloaded.
func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_, hasProviders := envelope["providers"]
	_, hasDefaults := envelope["defaults"]
	registrarRaw, hasRegistrar := envelope["registrar"]
	_, hasRegistrarMode := envelope["registrar_mode"]
	if hasProviders || hasDefaults || hasRegistrar || hasRegistrarMode {
		var bulk providerBulkInput
		if err := json.Unmarshal(raw, &bulk); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		registrarPatch := registrarBatchPatch{
			Provided: hasRegistrar,
			Values:   bulk.Registrar,
			Mode:     strings.ToLower(strings.TrimSpace(bulk.RegistrarMode)),
		}
		if hasRegistrar {
			if strings.TrimSpace(string(registrarRaw)) == "null" {
				writeError(w, http.StatusBadRequest, errors.New("registrar must be an object"))
				return
			}
			if registrarPatch.Values == nil {
				registrarPatch.Values = map[string]interface{}{}
			}
		}
		if !hasRegistrar && registrarPatch.Mode != "" {
			writeError(w, http.StatusBadRequest, errors.New("registrar_mode requires registrar"))
			return
		}
		if hasRegistrar {
			if _, nestedDefaults := registrarPatch.Values["defaults"]; nestedDefaults {
				writeError(w, http.StatusBadRequest, errors.New("registrar defaults must use the top-level defaults field"))
				return
			}
			if _, err := applyRegistrarConfigPatch(map[string]interface{}{}, registrarPatch.Values, registrarPatch.Mode); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		providers := make([]providerInput, 0, len(bulk.Providers))
		for i, p := range bulk.Providers {
			normalized, err := normalizeProviderInput(p)
			if err != nil {
				writeProviderScopedError(w, http.StatusBadRequest, "invalid_provider", "Provider type/key is invalid.", normalized.Type, normalized.Key, i)
				return
			}
			providers = append(providers, normalized)
		}
		defaults := bulk.Defaults
		if len(defaults) > 0 {
			normalized, err := normalizeRegDefaults(defaults)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			defaults = normalized
		}
		result, err := h.saveProviderBatch(r.Context(), providers, defaults, registrarPatch)
		if err != nil {
			var providerErr *providerWriteError
			if errors.As(err, &providerErr) {
				writeProviderScopedError(w, http.StatusServiceUnavailable, "provider_save_failed", "Provider settings could not be saved.", providerErr.ProviderType, providerErr.ProviderKey, -1)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		reloadOK, warning := h.reloadProvidersAfterCommittedSave(r.Context())
		response := map[string]interface{}{
			"saved":           len(providers),
			"registrar_saved": result.RegistrarSaved,
			"settings_saved":  result.SettingsSaved,
			"reload_ok":       reloadOK,
		}
		if warning != "" {
			response["warning"] = warning
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	var p providerInput
	if err := json.Unmarshal(raw, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	normalized, err := normalizeProviderInput(p)
	if err != nil {
		writeProviderScopedError(w, http.StatusBadRequest, "invalid_provider", "Provider type/key is invalid.", normalized.Type, normalized.Key, -1)
		return
	}
	id, err := h.upsertProvider(r.Context(), normalized)
	if err != nil {
		var providerErr *providerWriteError
		if errors.As(err, &providerErr) {
			writeProviderScopedError(w, http.StatusServiceUnavailable, "provider_save_failed", "Provider settings could not be saved.", providerErr.ProviderType, providerErr.ProviderKey, -1)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reloadOK, warning := h.reloadProvidersAfterCommittedSave(r.Context())
	response := map[string]interface{}{"id": id, "reload_ok": reloadOK}
	if warning != "" {
		response["warning"] = warning
	}
	writeJSON(w, http.StatusOK, response)
}

type sqlReadWriter interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func writeProviderScopedError(w http.ResponseWriter, status int, code, message, providerType, providerKey string, index int) {
	errorType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		status = http.StatusServiceUnavailable
		errorType = "server_error"
	}
	providerType = strings.TrimSpace(providerType)
	providerKey = strings.TrimSpace(providerKey)
	if len(providerType) > 128 {
		providerType = providerType[:128]
	}
	if len(providerKey) > 128 {
		providerKey = providerKey[:128]
	}
	errorBody := map[string]interface{}{
		"message":       message,
		"type":          errorType,
		"code":          code,
		"request_id":    publicRequestID(w),
		"provider_type": providerType,
		"provider_key":  providerKey,
	}
	if index >= 0 {
		errorBody["provider_index"] = index
	}
	resetPublicErrorHeaders(w)
	writeJSON(w, status, map[string]interface{}{"error": errorBody})
}

func (h *Handler) reloadProvidersAfterCommittedSave(ctx context.Context) (bool, string) {
	if err := h.ReloadProviders(ctx); err != nil {
		// Do not include the underlying error in the response: provider loader errors can
		// describe credential storage internals. The committed DB state remains valid and
		// the old pipeline is left active because ReloadProviders swaps only on success.
		log.Printf("[REGISTRATION] committed provider settings reload failed: class=%T", err)
		return false, providerReloadWarning
	}
	return true, ""
}

func normalizeProviderInput(p providerInput) (providerInput, error) {
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Key = strings.TrimSpace(p.Key)
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if p.Type == "" || p.Key == "" {
		return p, fmt.Errorf("type and key are required")
	}
	switch p.Type {
	case "sms", "mailbox", "captcha", "email":
	default:
		return p, fmt.Errorf("unsupported provider type %q", p.Type)
	}
	if p.Config == nil {
		p.Config = map[string]interface{}{}
	}
	if p.Type == "mailbox" {
		switch providerKey := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(p.Key, "_", ""), "-", "")); providerKey {
		case "cloudflare", "moemail", "freemail", "cftempemail", "cfworker":
			if strings.TrimSpace(cfgString(p.Config["adapter"])) == "" {
				p.Config["adapter"] = cloudflareMailboxAdapter
			}
		}
	}
	return p, nil
}

func (h *Handler) saveProviderBatch(ctx context.Context, providers []providerInput, defaults map[string]interface{}, registrar registrarBatchPatch) (providerBatchSaveResult, error) {
	result := providerBatchSaveResult{
		RegistrarSaved: registrar.Provided,
		SettingsSaved:  []settingsCenterDiff{},
	}
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var registrarJSON string
	if registrar.Provided {
		old, err := loadNodeRegistrarConfigWithExecutor(ctx, tx)
		if err != nil {
			return result, err
		}
		next, err := applyRegistrarConfigPatch(old, registrar.Values, registrar.Mode)
		if err != nil {
			return result, err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return result, err
		}
		registrarJSON = string(raw)
		result.SettingsSaved = registrarDiffs(old, next)
	}
	for _, p := range providers {
		if _, err := h.upsertProviderWithExecutor(ctx, tx, p); err != nil {
			return result, &providerWriteError{ProviderType: p.Type, ProviderKey: p.Key, cause: err}
		}
	}
	if len(defaults) > 0 {
		if err := h.saveDefaultsWithExecutor(ctx, tx, defaults); err != nil {
			return result, err
		}
	}
	if registrar.Provided {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key, value, updated_at) VALUES('node_registrar_config', ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			registrarJSON, storage.Now()); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	committed = true
	if len(defaults) > 0 || registrar.Provided {
		// The transaction wrote the settings table directly; refresh the snapshot.
		h.store.InvalidateSettingsCache()
	}
	return result, nil
}

func loadNodeRegistrarConfigWithExecutor(ctx context.Context, exec sqlReadWriter) (map[string]interface{}, error) {
	var raw string
	err := exec.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='node_registrar_config'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]interface{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}, nil
	}
	cfg := map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, errors.New("stored node registrar configuration is invalid")
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	return cfg, nil
}

// upsertProvider inserts or updates a provider row keyed by (provider_type, provider_key)
// via an explicit select-then-write (so it does not depend on a UNIQUE constraint being
// present). A blank incoming api_key preserves the previously stored one, so re-saving the
// whole page (where a password field may not echo back) never wipes an existing key.
func (h *Handler) upsertProvider(ctx context.Context, p providerInput) (string, error) {
	normalized, err := normalizeProviderInput(p)
	if err != nil {
		return "", err
	}
	id, err := h.upsertProviderWithExecutor(ctx, h.store.DB(), normalized)
	if err != nil {
		return "", &providerWriteError{ProviderType: normalized.Type, ProviderKey: normalized.Key, cause: err}
	}
	return id, nil
}

func (h *Handler) upsertProviderWithExecutor(ctx context.Context, exec sqlReadWriter, p providerInput) (string, error) {
	cfg := make(map[string]interface{}, len(p.Config))
	for k, v := range p.Config {
		cfg[k] = v
	}
	var existingID, existingCfg, existingAuth string
	err := exec.QueryRowContext(ctx,
		`SELECT id, config_json, auth_json FROM provider_settings WHERE provider_type=? AND provider_key=?`,
		p.Type, p.Key).Scan(&existingID, &existingCfg, &existingAuth)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	incomingPublicConfig, incomingSecrets, err := storage.SplitProviderConfig(cfg)
	if err != nil {
		return "", err
	}
	publicConfig := map[string]interface{}{}
	existingSecrets := map[string]string{}
	if existingID != "" {
		existingSecrets, err = h.store.OpenProviderAuthJSON(p.Type, p.Key, existingAuth)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(existingCfg) != "" {
			old := map[string]interface{}{}
			if err := json.Unmarshal([]byte(existingCfg), &old); err != nil {
				return "", err
			}
			oldPublicConfig, legacySecrets, err := storage.SplitProviderConfig(old)
			if err != nil {
				return "", err
			}
			for field, value := range oldPublicConfig {
				publicConfig[field] = value
			}
			for field, value := range legacySecrets {
				if strings.TrimSpace(value) != "" {
					existingSecrets[field] = value
				}
			}
		}
	}
	// Preserve public fields unknown to this client (for example fields introduced by a
	// newer server or plugin), while letting every explicitly supplied field overwrite
	// the stored value, including an explicit blank.
	for field, value := range incomingPublicConfig {
		publicConfig[field] = value
	}
	for field, value := range incomingSecrets {
		if strings.TrimSpace(value) != "" {
			existingSecrets[field] = value
		}
	}
	configJSON, err := json.Marshal(publicConfig)
	if err != nil {
		return "", err
	}
	authJSON, err := h.store.SealProviderAuthJSON(p.Type, p.Key, existingSecrets)
	if err != nil {
		return "", err
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	display := p.DisplayName
	if display == "" {
		display = p.Key
	}
	now := time.Now().Unix()
	if existingID != "" {
		if _, err := exec.ExecContext(ctx,
			`UPDATE provider_settings SET display_name=?, enabled=?, priority=?, config_json=?, auth_json=?, updated_at=? WHERE id=?`,
			display, enabled, p.Priority, string(configJSON), authJSON, now, existingID); err != nil {
			return "", err
		}
		return existingID, nil
	}
	id := fmt.Sprintf("prov_%d", time.Now().UnixNano())
	if _, err := exec.ExecContext(ctx,
		`INSERT INTO provider_settings (id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.Type, p.Key, display, enabled, p.Priority, string(configJSON), authJSON, now, now); err != nil {
		return "", err
	}
	return id, nil
}

// updateProvider upserts a single provider (PUT). Kept for API completeness; the UI uses
// the bulk POST path.
func (h *Handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	var p providerInput
	if err := decodeJSONRequestBody(r.Body, &p, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	normalized, err := normalizeProviderInput(p)
	if err != nil {
		writeProviderScopedError(w, http.StatusBadRequest, "invalid_provider", "Provider type/key is invalid.", normalized.Type, normalized.Key, -1)
		return
	}
	id, err := h.upsertProvider(r.Context(), normalized)
	if err != nil {
		var providerErr *providerWriteError
		if errors.As(err, &providerErr) {
			writeProviderScopedError(w, http.StatusServiceUnavailable, "provider_save_failed", "Provider settings could not be saved.", providerErr.ProviderType, providerErr.ProviderKey, -1)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reloadOK, warning := h.reloadProvidersAfterCommittedSave(r.Context())
	response := map[string]interface{}{"id": id, "reload_ok": reloadOK}
	if warning != "" {
		response["warning"] = warning
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing id"))
		return
	}

	_, err := h.store.DB().ExecContext(r.Context(), `DELETE FROM provider_settings WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := h.ReloadProviders(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
