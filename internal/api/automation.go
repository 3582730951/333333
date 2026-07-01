package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/registration/provider"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// Automation policy management + a conservative background scheduler.
//
// Policies are persisted as a single JSON blob in the settings table (no migration), so
// they survive restarts (the old in-memory map did not). The scheduler only ACTS on
// operator-enabled policies and is heavily guarded (interval-gated, pool-threshold-gated,
// hard-capped per tick) so it can never run away.

const (
	PolicyTypeScheduled = "scheduled" // scheduled batch registration (persisted; execution TODO: needs cron)
	PolicyTypeRefill    = "refill"    // auto-refill when the active pool drops below a threshold
	PolicyTypeHealth    = "health"    // health-check automation (handled by the lifecycle subsystem)
	PolicyTypePlus      = "plus"      // Plus upgrade automation (persisted; execution is Stage 7)

	automationPoliciesKey = "automation_policies"
	automationRefillCap   = 10 // max registrations a single refill tick may start
)

// Policy represents an automation policy.
type Policy struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config"`
	Created int64                  `json:"created_at"`
	Updated int64                  `json:"updated_at"`
}

func (s *Server) loadPolicies(ctx context.Context) map[string]*Policy {
	out, _ := s.loadPoliciesWithError(ctx)
	return out
}

func (s *Server) loadPoliciesWithError(ctx context.Context) (map[string]*Policy, error) {
	out := map[string]*Policy{}
	v, ok, err := s.store.GetSetting(ctx, automationPoliciesKey)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return out, err
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return out, fmt.Errorf("%s has invalid JSON: %w", automationPoliciesKey, err)
	}
	if out == nil {
		out = map[string]*Policy{}
	}
	return out, nil
}

func (s *Server) savePolicies(ctx context.Context, m map[string]*Policy) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, automationPoliciesKey, string(b))
}

func (s *Server) handleAutomationPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.automationListPolicies(w, r)
	case http.MethodPost:
		s.automationSavePolicy(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) automationListPolicies(w http.ResponseWriter, r *http.Request) {
	m, err := s.loadPoliciesWithError(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	list := make([]*Policy, 0, len(m))
	for _, p := range m {
		list = append(list, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"policies": list})
}

func (s *Server) automationSavePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type    string                 `json:"type"`
		Enabled bool                   `json:"enabled"`
		Config  map[string]interface{} `json:"config"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !validAutomationPolicyType(req.Type) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid policy type: %s", req.Type))
		return
	}
	m, err := s.loadPoliciesWithError(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().Unix()
	p := m[req.Type]
	if p == nil {
		p = &Policy{ID: req.Type, Type: req.Type, Created: now}
	}
	p.Enabled = req.Enabled
	p.Config = req.Config
	p.Updated = now
	m[req.Type] = p
	if err := s.savePolicies(r.Context(), m); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "policy": p})
}

func validAutomationPolicyType(policyType string) bool {
	switch policyType {
	case PolicyTypeScheduled, PolicyTypeRefill, PolicyTypeHealth, PolicyTypePlus:
		return true
	default:
		return false
	}
}

func (s *Server) handleAutomationStats(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	stats, err := s.automationStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleRegisterReadiness(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.registrationReadiness(r.Context()))
}

// registrationReadiness reports whether "deploy → auto-fill the pool" is actually
// configured to run: the refill policy state, the providers that are wired (mailbox /
// sms / captcha from provider_settings), the current pool deficit, and human-readable
// blockers explaining why it would NOT auto-fill. Pure read; constructs the provider
// manager (no network) to count actually-usable providers, not just enabled rows.
func (s *Server) registrationReadiness(ctx context.Context) map[string]interface{} {
	policies, policyErr := s.loadPoliciesWithError(ctx)
	rp := policies[PolicyTypeRefill]
	refillEnabled := rp != nil && rp.Enabled
	cfg := map[string]interface{}{}
	if rp != nil {
		cfg = rp.Config
	}
	target := intFromConfig(cfg, "target", 20)
	threshold := intFromConfig(cfg, "threshold", 10)
	method := strings.ToLower(firstNonEmpty(
		strFromConfig(cfg, "register_method", ""),
		s.settingString(ctx, "default_register_method", firstNonEmpty(s.cfg.DefaultRegisterMethod, "node")),
		"node",
	))
	identityMode, identityConfigured := optionalStrFromConfig(cfg, "identity_mode")
	identityMode = strings.ToLower(strings.TrimSpace(identityMode))
	if identityMode == "sms" {
		identityMode = "phone"
	}
	identityErr := ""
	switch identityMode {
	case "", "phone", "email":
	default:
		identityErr = fmt.Sprintf("identity_mode %q 不支持", identityMode)
	}
	if lock := lockedIdentityModeForMethod(method); lock != "" {
		if identityConfigured && identityMode != "" && identityMode != lock {
			identityErr = fmt.Sprintf("%s requires identity_mode=%s", method, lock)
		}
		identityMode = lock
	}
	if identityMode == "" {
		identityMode = "phone"
	}
	group := firstNonEmpty(strFromConfig(cfg, "group", ""), s.cfg.DefaultGroup, "default")
	egressID := strFromConfig(cfg, "egress", "egress_direct")
	platform := strFromConfig(cfg, "platform", "chatgpt")

	mgr, providerErr := provider.BuildManagerWithError(ctx, s.store, nil)
	mailboxN, smsN, captchaN := len(mgr.Mailbox), len(mgr.SMS), len(mgr.Captcha)
	emailOTPN := s.enabledProviderKeyCount(ctx, "email", "hotmail_otp")

	accounts, accountErr := s.store.ListAccounts(ctx)
	active := 0
	for _, a := range accounts {
		if a.Status == "active" {
			active++
		}
	}
	deficit := target - active
	if deficit < 0 {
		deficit = 0
	}

	blockers := []string{}
	if s.regHandler == nil {
		blockers = append(blockers, "注册子系统未初始化")
	}
	if policyErr != nil {
		blockers = append(blockers, "automation_policies 读取失败: "+policyErr.Error())
	}
	if providerErr != nil {
		blockers = append(blockers, "provider_settings 配置读取失败: "+providerErr.Error())
	}
	if accountErr != nil {
		blockers = append(blockers, "账号池读取失败: "+accountErr.Error())
	}
	if !refillEnabled {
		blockers = append(blockers, "auto-refill 策略未启用（在「自动注册」页开启 refill 策略并设置 target/threshold）")
	}
	if !supportedRegistrationMethod(method) {
		blockers = append(blockers, fmt.Sprintf("注册引擎 %q 不支持", method))
	}
	if identityErr != "" {
		blockers = append(blockers, identityErr)
	}
	if registrationMethodRequiresMailboxProvider(method, identityMode) && mailboxN == 0 {
		blockers = append(blockers, "未配置任何 mailbox provider（邮箱验证需要）")
	}
	if registrationMethodRequiresEmailOTPProvider(method) && emailOTPN == 0 {
		blockers = append(blockers, "未配置 hotmail_otp provider（protocol_v2/browser_v3 需要）")
	}
	if registrationMethodRequiresSMSProvider(method, identityMode) && smsN == 0 {
		blockers = append(blockers, "identity_mode=sms 但未配置任何 SMS provider")
	}
	if group != "" {
		if _, err := s.store.GetGroup(ctx, group); err != nil {
			blockers = append(blockers, fmt.Sprintf("分组 %q 不存在", group))
		}
	}
	if egressID != "" && egressID != "egress_direct" {
		if _, err := s.store.GetEgressProfile(ctx, egressID); err != nil {
			blockers = append(blockers, fmt.Sprintf("出口 %q 不存在", egressID))
		}
	}
	notes := []string{}
	if refillEnabled && deficit == 0 {
		notes = append(notes, "活跃池已达目标，当前 tick 无需补号")
	}
	if captchaN == 0 {
		notes = append(notes, "未配置 captcha provider；若 ChatGPT 触发验证码，注册可能失败")
	}
	providerError := ""
	if providerErr != nil {
		providerError = providerErr.Error()
	}
	policyError := ""
	if policyErr != nil {
		policyError = policyErr.Error()
	}
	poolError := ""
	if accountErr != nil {
		poolError = accountErr.Error()
	}

	return map[string]interface{}{
		"refill": map[string]interface{}{
			"enabled": refillEnabled, "target": target, "threshold": threshold,
			"register_method": method, "identity_mode": identityMode, "group": group, "egress": egressID, "platform": platform,
		},
		"providers":      map[string]int{"mailbox": mailboxN, "email_otp": emailOTPN, "sms": smsN, "captcha": captchaN},
		"pool":           map[string]int{"active": active, "target": target, "deficit": deficit},
		"ready":          policyErr == nil && providerErr == nil && s.regHandler != nil && refillEnabled && len(blockers) == 0,
		"policy_error":   policyError,
		"provider_error": providerError,
		"pool_error":     poolError,
		"blockers":       blockers,
		"notes":          notes,
	}
}

func (s *Server) enabledProviderKeyCount(ctx context.Context, providerType, providerKey string) int {
	var count int
	if err := s.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_settings WHERE provider_type=? AND provider_key=? AND enabled=1`, providerType, providerKey).Scan(&count); err != nil {
		return 0
	}
	return count
}

// registrationReadinessLine is a one-line startup summary of the readiness state, logged
// once when the automation scheduler starts so the operator immediately sees whether the
// pool will auto-fill (and if not, why).
func (s *Server) registrationReadinessLine(ctx context.Context) string {
	rd := s.registrationReadiness(ctx)
	ready, _ := rd["ready"].(bool)
	prov, _ := rd["providers"].(map[string]int)
	pool, _ := rd["pool"].(map[string]int)
	blockers, _ := rd["blockers"].([]string)
	status := "READY (auto-refill armed)"
	if !ready {
		status = "NOT auto-filling: " + strings.Join(blockers, "; ")
	}
	return fmt.Sprintf("auto-registration %s | providers mailbox=%d sms=%d captcha=%d | pool active=%d target=%d deficit=%d",
		status, prov["mailbox"], prov["sms"], prov["captcha"], pool["active"], pool["target"], pool["deficit"])
}

// ── scheduler ──
// StartAutomation launches the background automation scheduler. It is a no-op-friendly
// loop: it does nothing unless the operator has enabled a policy. Started from main.
func (s *Server) StartAutomation(ctx context.Context) {
	supervisor.Go(ctx, "registration-automation", func(ctx context.Context) {
		// One-shot readiness summary at startup so the operator immediately sees whether
		// the pool will auto-fill given the current config (and why not, if blocked).
		log.Printf("[REGISTRATION] %s", s.registrationReadinessLine(ctx))
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		last := map[string]int64{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runAutomationTick(ctx, last)
			}
		}
	})
}

func (s *Server) runAutomationTick(ctx context.Context, last map[string]int64) {
	m, err := s.loadPoliciesWithError(ctx)
	if err != nil {
		log.Printf("automation: policy load failed: %v", err)
		return
	}
	now := time.Now().Unix()
	if rp := m[PolicyTypeRefill]; rp != nil && rp.Enabled {
		interval := int64(intFromConfig(rp.Config, "interval", 3600))
		if interval < 300 {
			interval = 300 // floor: never refill more often than every 5 min
		}
		if now-last[PolicyTypeRefill] >= interval {
			last[PolicyTypeRefill] = now
			s.autoRefill(ctx, rp)
		}
	}
	if pp := m[PolicyTypePlus]; pp != nil && pp.Enabled {
		interval := int64(intFromConfig(pp.Config, "interval", 86400))
		if interval < 3600 {
			interval = 3600 // floor: never upgrade more often than hourly
		}
		if now-last[PolicyTypePlus] >= interval {
			last[PolicyTypePlus] = now
			s.autoUpgradePlus(ctx, pp)
		}
	}
}

// autoRefill tops the active pool back up to `target` when it drops below `threshold`,
// capped at automationRefillCap registrations per tick. Operator-opt-in (only fires when
// the refill policy is enabled) and a no-op when registration isn't wired.
func (s *Server) autoRefill(ctx context.Context, p *Policy) {
	if s.regHandler == nil {
		return
	}
	threshold := intFromConfig(p.Config, "threshold", 10)
	target := intFromConfig(p.Config, "target", 20)
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return
	}
	active := 0
	for _, a := range accounts {
		if a.Status == "active" {
			active++
		}
	}
	if active >= threshold {
		return
	}
	need := target - active
	if need <= 0 {
		return
	}
	if need > automationRefillCap {
		need = automationRefillCap
	}
	req := pipeline.RegisterRequest{
		Platform:     strFromConfig(p.Config, "platform", "chatgpt"),
		Method:       strFromConfig(p.Config, "register_method", ""), // "" → StartJob applies the configured default ("node")
		IdentityMode: strFromConfig(p.Config, "identity_mode", ""),
		Count:        need,
		GroupName:    strFromConfig(p.Config, "group", ""),
		EgressID:     strFromConfig(p.Config, "egress", "egress_direct"),
	}
	jobID, err := s.regHandler.StartJob(ctx, req)
	if err != nil {
		log.Printf("automation: auto-refill failed to start: %v", err)
		return
	}
	log.Printf("automation: auto-refill started job %s for %d accounts (active=%d threshold=%d)", jobID, need, active, threshold)
}

// autoUpgradePlus upgrades free accounts to Plus when enabled. Operator-opt-in (only fires
// when the plus policy is enabled) and payment-manager-gated (no-op when no payment provider
// is wired). Respects a daily_limit cap to avoid billing surprises.
func (s *Server) autoUpgradePlus(ctx context.Context, p *Policy) {
	if s.paymentMgr == nil {
		return
	}
	providerName := strFromConfig(p.Config, "payment_provider", "gopay")
	provider, err := s.paymentMgr.Get(providerName)
	if err != nil {
		log.Printf("automation: auto-upgrade-plus: %v", err)
		return
	}
	dailyLimit := intFromConfig(p.Config, "daily_limit", 5)
	if dailyLimit <= 0 {
		return
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return
	}
	candidates := []*storage.Account{}
	for i := range accounts {
		a := &accounts[i]
		if a.Status == "active" && (a.PlanType == "" || a.PlanType == "free") {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return
	}
	if len(candidates) > dailyLimit {
		candidates = candidates[:dailyLimit]
	}
	egressID := strFromConfig(p.Config, "egress", "egress_direct")
	proxyURL := ""
	if egress, err := s.store.GetEgressProfile(ctx, egressID); err == nil {
		// Best-effort proxy URL extraction for the payment flow (GoPay has its own egress
		// setting so this is informational; PayPal might use it).
		proxyURL = egress.Endpoint
	}
	succeeded := 0
	for _, acc := range candidates {
		if err := provider.Subscribe(ctx, acc, proxyURL); err != nil {
			log.Printf("automation: upgrade %s to Plus failed: %v", acc.ID, err)
			continue
		}
		// The payment provider already updated account.PlanType inside Subscribe.
		log.Printf("automation: upgraded %s to Plus via %s", acc.ID, providerName)
		succeeded++
	}
	log.Printf("automation: auto-upgrade-plus: %d/%d succeeded (provider=%s)", succeeded, len(candidates), providerName)
}

func intFromConfig(cfg map[string]interface{}, key string, def int) int {
	if cfg == nil {
		return def
	}
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func strFromConfig(cfg map[string]interface{}, key, def string) string {
	if cfg != nil {
		if s, ok := cfg[key].(string); ok && s != "" {
			return s
		}
	}
	return def
}

func optionalStrFromConfig(cfg map[string]interface{}, key string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	s, ok := cfg[key].(string)
	if !ok {
		return "", false
	}
	return s, true
}
