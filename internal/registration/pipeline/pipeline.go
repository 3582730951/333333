// Package pipeline orchestrates registration flow
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registration/httpclient"
	"codex-account-pool/internal/registration/openai"
	"codex-account-pool/internal/registration/provider"
	"codex-account-pool/internal/registration/provider/proxy"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

// Pipeline coordinates providers and protocol engines
type Pipeline struct {
	store                      *storage.Store
	providerMgr                *provider.Manager
	upstream                   *upstream.Client
	httpClient                 *http.Client // shared HTTP client for geo-validation / cliproxy API calls
	cfg                        *config.Config
	remoteVerificationRequired bool
	// LogEvent is set by the registration Handler to receive structured step
	// logs from the pipeline (proxy assign, sid rotate, fingerprint, browser
	// launch, sms, mailbox, token read, subprocess output). When nil the
	// pipeline logs nothing extra (the legacy one-line path).
	LogEvent func(ctx context.Context, taskID, level, message string, detail interface{})
}

// NewPipeline creates a registration pipeline. up is the shared upstream client used to
// build an egress-aware HTTP client for the OpenAI protocol flow (may be nil → direct).
// cfg carries global cliproxy API settings (api base, fallback key, validate-region toggle).
func NewPipeline(store *storage.Store, providerMgr *provider.Manager, up *upstream.Client, cfg *config.Config) *Pipeline {
	return &Pipeline{
		store:                      store,
		providerMgr:                providerMgr,
		upstream:                   up,
		httpClient:                 &http.Client{Timeout: 15 * time.Second},
		cfg:                        cfg,
		remoteVerificationRequired: cfg != nil && cfg.RegistrationEnabled,
	}
}

// egressClient returns an HTTP client whose transport routes through the exact
// registration egress. It fails closed: a missing profile, transport builder, or
// sidecar never degrades to the VPS's direct network path.
func (p *Pipeline) egressClient(ctx context.Context, egressID string) (*http.Client, error) {
	if p == nil || p.upstream == nil || p.store == nil {
		return nil, fmt.Errorf("registration egress transport unavailable")
	}
	egress, err := p.store.GetEgressProfile(ctx, egressID)
	if err != nil || strings.TrimSpace(egress.ID) == "" {
		return nil, fmt.Errorf("registration egress profile unavailable")
	}
	endpoint := egress.Endpoint
	// Rotating-residential (cliproxy) egress: swap in a fresh session id so each
	// registration leaves from a different exit IP than the previous one.
	if proxy.IsCliproxy(endpoint) {
		endpoint = proxy.RotateSID(endpoint)
	}
	// Prefer the curl_cffi sidecar so the CF-walled OpenAI signup/OAuth calls present
	// a real browser JA3, chaining through the residential proxy above.
	if sc := p.upstream.SidecarEndpoint(); sc != "" && httpclient.SidecarHealthy(ctx, sc) {
		return httpclient.NewSidecarClient(sc, endpoint, "reg_"+randToken()), nil
	}
	egress.Endpoint = endpoint
	hc, err := p.upstream.EgressHTTPClient(egress)
	if err != nil || hc == nil {
		return nil, fmt.Errorf("registration egress transport unavailable")
	}
	return hc, nil
}

// RegisterRequest defines a registration task
type RegisterRequest struct {
	Platform                 string `json:"platform"` // "chatgpt"
	Method                   string `json:"method"`   // "protocol" | "browser"
	Count                    int    `json:"count"`
	GroupName                string `json:"group_name"`
	EgressID                 string `json:"egress_id"`
	RegistrationEgressPoolID string `json:"registration_egress_pool_id"`
	RuntimeEgressPoolID      string `json:"runtime_egress_pool_id"`
	// UpgradeToPlus is accepted for one compatibility release only so callers
	// receive an explicit 410; no registration implementation consumes it.
	UpgradeToPlus        bool    `json:"upgrade_to_plus,omitempty"`
	IdentityMode         string  `json:"identity_mode"` // "phone" (default) | "email"
	Country              string  `json:"country"`       // SMS country code/id (default "ID")
	SMSProvider          string  `json:"sms_provider"`
	SMSCountry           string  `json:"sms_country,omitempty"` // ISO-2 of the country chosen (for stats recording)
	SMSCost              float64 `json:"sms_cost,omitempty"`    // cost of the SMS number (for stats recording)
	MailboxProvider      string  `json:"mailbox_provider"`
	MailboxDomain        string  `json:"mailbox_domain,omitempty"`
	CaptchaSolver        string  `json:"captcha_solver"`
	Canary               bool    `json:"canary,omitempty"`
	JobID                string  `json:"-"`
	RecordID             string  `json:"-"`
	WorkflowItemID       string  `json:"-"`
	ReadinessFingerprint string  `json:"-"`
	// Team lifecycle handoff fields are server-generated and never accepted from
	// the public registration JSON. A successful replacement registration uses
	// them to enqueue the next durable invite/quota/rotation cycle.
	TeamLifecycleSourceWorkflowID   string `json:"-"`
	TeamLifecycleWorkspaceID        string `json:"-"`
	TeamLifecycleParentAccountID    string `json:"-"`
	TeamLifecycleReplacementMethod  string `json:"-"`
	TeamLifecycleRotateThresholdBPS int    `json:"-"`
	TeamLifecycleMaxAttempts        int    `json:"-"`
}

// acquireSMS resolves the SMS provider + phone number + order id for one registration.
//
// Strategy (read from the "sms_platform_strategy" setting, default "auto"):
//   - "auto": Manager.GetBestSMS picks the best (platform, country) from live platform
//     statistics — each platform's same-day success ranking (getTopCountriesByService) plus
//     price + inventory, weighted by the operator's preferred-country list (BR>CO>PL). It
//     tries the top-N candidates in order, falling back on NO_NUMBERS/timeout.
//   - "manual": the request's explicit req.Country is used verbatim against the providers in
//     priority order (the legacy GetSMS path).
//
// Returns the chosen provider, phone, and order id. The caller must settle the lease
// exactly once: complete it after an OTP was consumed, otherwise cancel it.
func (p *Pipeline) acquireSMS(ctx context.Context, req RegisterRequest) (provider.SMSProvider, string, string, error) {
	if p == nil || p.providerMgr == nil || len(p.providerMgr.SMS) == 0 {
		return nil, "", "", provider.ErrNoProviderAvailable
	}
	requestedProvider := strings.ToLower(strings.TrimSpace(req.SMSProvider))
	if requestedProvider != "" && requestedProvider != "auto" {
		country := strings.TrimSpace(req.Country)
		if country == "" {
			country = firstPreferredCountry(ctx, p.store)
		}
		return p.providerMgr.GetSMSFromProvider(ctx, requestedProvider, country)
	}
	strategy := "auto"
	if p.store != nil {
		if v, ok, _ := p.store.GetSetting(ctx, "sms_platform_strategy"); ok {
			if v = strings.TrimSpace(v); v != "" {
				strategy = strings.ToLower(v)
			}
		}
	}
	if strategy == "manual" {
		// Explicit country wins; fall back to the preferred-default list's first entry.
		country := strings.TrimSpace(req.Country)
		if country == "" {
			country = firstPreferredCountry(ctx, p.store)
		}
		return p.providerMgr.GetSMS(ctx, country)
	}
	// auto: live-stats smart selection across all funded platforms.
	pref := preferredCountries(ctx, p.store)
	topN := smsStatsTopN(ctx, p.store)
	prov, phone, orderID, err := p.providerMgr.GetBestSMS(ctx, pref, topN)
	if err == nil {
		return prov, phone, orderID, nil
	}
	// Fallback: if smart selection found nothing (e.g. no platform exposes PriceProvider,
	// or all stats calls failed), degrade to the legacy explicit-country path so a
	// misconfigured-but-funded single platform still works.
	country := strings.TrimSpace(req.Country)
	if country == "" {
		country = firstPreferredCountry(ctx, p.store)
	}
	return p.providerMgr.GetSMS(ctx, country)
}

func (p *Pipeline) settleSMSLease(ctx context.Context, req RegisterRequest, smsProvider provider.SMSProvider, orderID string, consumed bool) {
	if smsProvider == nil || strings.TrimSpace(orderID) == "" {
		return
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	action := "cancel"
	var err error
	if consumed {
		action = "complete"
		if completer, ok := smsProvider.(provider.SMSSettlementProvider); ok {
			err = completer.CompleteNumber(settleCtx, orderID)
		}
	} else {
		err = smsProvider.CancelNumber(settleCtx, orderID)
	}
	if err != nil && p.LogEvent != nil {
		logCtx, logCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer logCancel()
		p.LogEvent(logCtx, req.JobID, "warn",
			"SMS resource settlement failed",
			map[string]interface{}{"provider": smsProvider.Name(), "action": action})
	}
}

// preferredCountries reads the "sms_preferred_countries" setting (comma-separated ISO-2,
// default "BR,CO,PL") and returns it as a clean slice in priority order.
func preferredCountries(ctx context.Context, store *storage.Store) []string {
	def := []string{"BR", "CO", "PL"}
	if store == nil {
		return def
	}
	v, ok, _ := store.GetSetting(ctx, "sms_preferred_countries")
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if c := strings.ToUpper(strings.TrimSpace(p)); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// firstPreferredCountry returns the first entry of the preferred list (the highest-priority
// country), used as the manual-mode fallback when the request names no country.
func firstPreferredCountry(ctx context.Context, store *storage.Store) string {
	return preferredCountries(ctx, store)[0]
}

// smsStatsTopN reads the "sms_stats_top_n" setting (default 3).
func smsStatsTopN(ctx context.Context, store *storage.Store) int {
	if store == nil {
		return 3
	}
	v, ok, _ := store.GetSetting(ctx, "sms_stats_top_n")
	if !ok {
		return 3
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 3
	}
	return n
}

// RegisterOne registers a single account
func (p *Pipeline) RegisterOne(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	p.updateWorkflow(ctx, req, storage.RegistrationItemRegistering, "")
	// "browser" runs the Playwright signup harness (services/codex_register) as a
	// subprocess — the real-browser path for OpenAI's JS-gated signup.
	if strings.EqualFold(strings.TrimSpace(req.Method), "browser") {
		return p.browserRegisterOne(ctx, req)
	}
	// "protocol_v2" runs the maintained curl_cffi + local-Sentinel-PoW registrar.
	if strings.EqualFold(strings.TrimSpace(req.Method), "protocol_v2") {
		return p.protocolV2RegisterOne(ctx, req)
	}
	// "node" runs the puppeteer-real-browser registrar (other_new_gpt_register) as an
	// orchestrated per-job subprocess: pool_server owns the per-job egress IP, browser
	// fingerprint seed, isolated throwaway profile (cookie purge), and teardown. This is
	// the transplanted registration engine.
	if strings.EqualFold(strings.TrimSpace(req.Method), "node") {
		return p.nodeRegisterOne(ctx, req)
	}
	// "browser_v3" runs the GuJumpgate Playwright harness — the flow that PROVABLY
	// registers ChatGPT accounts (headless Chrome + --enable-automation removal +
	// stealth + timestamp-filtered Hotmail OTP). Proxy from the admin egress profile.
	if strings.EqualFold(strings.TrimSpace(req.Method), "browser_v3") {
		return p.browserV3RegisterOne(ctx, req)
	}
	if req.Method != "" && req.Method != "protocol" {
		return nil, fmt.Errorf("unsupported method %q (use \"protocol\", \"protocol_v2\", \"node\", \"browser\", or \"browser_v3\")", req.Method)
	}
	// Email-based signup uses a mailbox provider for the OTP; phone uses SMS.
	if strings.EqualFold(strings.TrimSpace(req.IdentityMode), "email") {
		return p.registerViaEmail(ctx, req)
	}
	if p.providerMgr == nil {
		return nil, fmt.Errorf("no providers configured: add an SMS/mailbox provider on the Provider page first")
	}

	// Resolve the SMS provider + country. Under the "auto" strategy (default), the Manager
	// queries each platform's live stats (balance + success-ranked price/inventory) and the
	// operator's preferred-country priority (BR>CO>PL) to pick the best (platform, country);
	// under "manual" the request's explicit country is used verbatim. Either way the chosen
	// provider lease is settled exactly once after the flow returns.
	smsProvider, phone, orderID, err := p.acquireSMS(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("acquireSMS: %w", err)
	}
	codeConsumed := false
	defer func() {
		p.settleSMSLease(ctx, req, smsProvider, orderID, codeConsumed)
	}()

	// Generate device ID
	deviceID := generateDeviceID()

	// Egress-aware HTTP client: the OpenAI signup/OAuth flow egresses through the task's
	// chosen proxy/WARP exit (off the shared VPS IP) instead of the old stub client.
	egressHTTP, err := p.egressClient(ctx, req.EgressID)
	if err != nil {
		return nil, err
	}

	// Create OpenAI register client
	client := openai.NewRegisterClient(egressHTTP, deviceID)

	// Generate credentials
	password := generatePassword()
	name := generateName()
	birthdate := generateBirthdate()

	// OTP getter
	otpGetter := func() (string, error) {
		code, err := smsProvider.WaitCode(ctx, orderID, 90*time.Second)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(code) != "" {
			codeConsumed = true
		}
		return code, nil
	}

	// Execute registration
	result, err := client.Register(ctx, phone, password, name, birthdate, otpGetter)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	return p.persistVerifiedRegistration(ctx, req, registrationCredential{
		LabelPrefix:   "protocol-",
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		SessionToken:  result.SessionToken,
		LoginPassword: password,
	})
}

// registerViaEmail registers one account using a mailbox provider for the email OTP
// (the email identity mode). Mirrors RegisterOne's account-persistence tail.
func (p *Pipeline) registerViaEmail(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	if p.providerMgr == nil {
		return nil, fmt.Errorf("no providers configured: add a mailbox provider on the Provider page first")
	}
	mbox, email, _, mailboxID, err := p.providerMgr.GetMailboxWithConstraints(ctx, req.MailboxProvider, req.MailboxDomain)
	if err != nil {
		return nil, fmt.Errorf("getMailbox: %w", err)
	}
	defer mbox.DeleteEmail(ctx, mailboxID)

	deviceID := generateDeviceID()
	egressHTTP, err := p.egressClient(ctx, req.EgressID)
	if err != nil {
		return nil, err
	}
	client := openai.NewRegisterClient(egressHTTP, deviceID)

	password := generatePassword()
	name := generateName()
	birthdate := generateBirthdate()

	otpGetter := func() (string, error) {
		return mbox.WaitOTP(ctx, mailboxID, 180*time.Second)
	}

	result, err := client.RegisterEmail(ctx, email, password, name, birthdate, otpGetter)
	if err != nil {
		return nil, fmt.Errorf("registerEmail: %w", err)
	}

	return p.persistVerifiedRegistration(ctx, req, registrationCredential{
		LabelPrefix:   "protocol-",
		Email:         email,
		AccessToken:   result.AccessToken,
		SessionToken:  result.SessionToken,
		LoginPassword: password,
	})
}

// firstNonEmpty returns the first non-empty (after trimming) string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func generateDeviceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generatePassword() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateName() string {
	names := []string{"Alex", "Jordan", "Taylor", "Morgan", "Casey", "Riley", "Jamie", "Avery"}
	return names[time.Now().UnixNano()%int64(len(names))]
}

func generateBirthdate() string {
	year := 1980 + time.Now().UnixNano()%30
	return fmt.Sprintf("%d-01-01", year)
}

func generateAccountID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "acc_" + hex.EncodeToString(b)
}

// randToken returns a short random hex token (for per-task cookie-jar keys).
func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
