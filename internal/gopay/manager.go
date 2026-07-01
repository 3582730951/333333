// Package gopay integrates the bundled GoPay Plus auto-subscribe tool
// (gopay/plus, Python) into the pool server as a managed, default-OFF feature.
//
// The proven Stripe→Midtrans→GoPay payment chain is NOT re-ported to Go (its
// request signing is intricate and battle-tested in Python). Instead this Manager
// owns the lifecycle: it renders the Python project's config.json from operator
// settings (payment credentials, OTP mode, and the proxy resolved from a pool
// egress), optionally launches the two Python services as managed subprocesses,
// and exposes thin HTTP clients for the orchestrator's /subscribe and /otp
// endpoints. The pool server feeds each account's STORED session token into the
// flow, so an operator only has to click "订阅 Plus" on a pooled account.
package gopay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	settingEnabled   = "gopay_enabled"
	settingSettings  = "gopay_settings"
	settingAuthToken = "gopay_auth_token"

	gopayRestartDelay = 2 * time.Second
)

// Settings are the operator-supplied payment/runtime values. They are persisted
// (as JSON) in the settings table so they survive restarts and are editable from
// the admin UI. Secrets never leave the server except into the generated
// config.json the bundled Python reads.
type Settings struct {
	PhoneNumber          string `json:"phone_number"`
	Pin                  string `json:"pin"`
	CountryCode          string `json:"country_code"`
	StripePublishableKey string `json:"stripe_publishable_key"`
	MidtransClientID     string `json:"midtrans_client_id"`
	OTPMode              string `json:"otp_mode"`    // manual | sms_api | whatsapp
	OTPChannel           string `json:"otp_channel"` // whatsapp | sms
	OTPTimeout           int    `json:"otp_timeout"`
	SMSAPIKey            string `json:"sms_api_key"`
	SMSBaseURL           string `json:"sms_base_url"`
	WhatsAppGRPCAddr     string `json:"whatsapp_grpc_addr"`
	// ProxyEgressID selects a pool egress profile to route the payment flow
	// through (must be a JP/TW exit for ChatGPT region checks). ProxyURL is an
	// explicit override used when no egress is selected.
	ProxyEgressID string `json:"proxy_egress_id"`
	ProxyURL      string `json:"proxy_url"`
}

func (s Settings) withDefaults() Settings {
	if s.CountryCode == "" {
		s.CountryCode = "62"
	}
	if s.OTPMode == "" {
		s.OTPMode = "manual"
	}
	if s.OTPChannel == "" {
		s.OTPChannel = "whatsapp"
	}
	if s.OTPTimeout <= 0 {
		s.OTPTimeout = 90
	}
	if s.WhatsAppGRPCAddr == "" {
		s.WhatsAppGRPCAddr = "127.0.0.1:50056"
	}
	return s
}

// Redacted returns a copy safe to expose in admin GETs (secrets masked).
func (s Settings) Redacted() Settings {
	if s.Pin != "" {
		s.Pin = "******"
	}
	if s.SMSAPIKey != "" {
		s.SMSAPIKey = "********"
	}
	return s
}

// Manager owns the bundled GoPay feature lifecycle.
type Manager struct {
	cfg   config.Config
	store *storage.Store

	mu      sync.Mutex
	procs   []*exec.Cmd
	running bool
	logs    []string
	httpc   *http.Client
}

func NewManager(cfg config.Config, store *storage.Store) *Manager {
	return &Manager{
		cfg:   cfg,
		store: store,
		httpc: &http.Client{Timeout: 150 * time.Second},
	}
}

// Enabled reports the effective enable flag (settings override, else boot config).
func (m *Manager) Enabled(ctx context.Context) bool {
	if v, ok, _ := m.store.GetSetting(ctx, settingEnabled); ok {
		return v == "1"
	}
	return m.cfg.GopayEnabled
}

// GetSettings returns the persisted operator settings with defaults applied.
func (m *Manager) GetSettings(ctx context.Context) Settings {
	var s Settings
	if raw, ok, _ := m.store.GetSetting(ctx, settingSettings); ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &s)
	}
	return s.withDefaults()
}

// SaveSettings persists operator settings (merging over the stored ones so a
// masked secret left unchanged in the UI does not wipe the real value).
func (m *Manager) SaveSettings(ctx context.Context, in Settings) (Settings, error) {
	cur := m.GetSettings(ctx)
	if in.PhoneNumber != "" {
		cur.PhoneNumber = in.PhoneNumber
	}
	if in.Pin != "" && in.Pin != "******" {
		cur.Pin = in.Pin
	}
	if in.CountryCode != "" {
		cur.CountryCode = in.CountryCode
	}
	if in.StripePublishableKey != "" {
		cur.StripePublishableKey = in.StripePublishableKey
	}
	if in.MidtransClientID != "" {
		cur.MidtransClientID = in.MidtransClientID
	}
	if in.OTPMode != "" {
		cur.OTPMode = in.OTPMode
	}
	if in.OTPChannel != "" {
		cur.OTPChannel = in.OTPChannel
	}
	if in.OTPTimeout > 0 {
		cur.OTPTimeout = in.OTPTimeout
	}
	if in.SMSAPIKey != "" && in.SMSAPIKey != "********" {
		cur.SMSAPIKey = in.SMSAPIKey
	}
	if in.SMSBaseURL != "" {
		cur.SMSBaseURL = in.SMSBaseURL
	}
	if in.WhatsAppGRPCAddr != "" {
		cur.WhatsAppGRPCAddr = in.WhatsAppGRPCAddr
	}
	// Proxy selection is allowed to be cleared, so assign directly.
	cur.ProxyEgressID = in.ProxyEgressID
	cur.ProxyURL = in.ProxyURL
	blob, err := json.Marshal(cur)
	if err != nil {
		return Settings{}, err
	}
	if err := m.store.SetSetting(ctx, settingSettings, string(blob)); err != nil {
		return Settings{}, err
	}
	return cur, nil
}

// SetEnabled flips the runtime flag and starts/stops the managed services.
func (m *Manager) SetEnabled(ctx context.Context, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	if err := m.store.SetSetting(ctx, settingEnabled, val); err != nil {
		return err
	}
	if on {
		return m.Start(ctx)
	}
	m.Stop()
	return nil
}

func (m *Manager) authToken(ctx context.Context) string {
	if v, ok, _ := m.store.GetSetting(ctx, settingAuthToken); ok && v != "" {
		return v
	}
	if m.cfg.GopayAuthToken != "" {
		return m.cfg.GopayAuthToken
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "gopay-local-token"
	}
	tok := hex.EncodeToString(buf)
	_ = m.store.SetSetting(ctx, settingAuthToken, tok)
	return tok
}

// resolveProxy returns the proxy URL the payment flow should use and the egress's
// region (for UI warnings). Egress selection takes precedence over an explicit URL.
func (m *Manager) resolveProxy(ctx context.Context, s Settings) (proxyURL, region string) {
	if s.ProxyEgressID != "" {
		if eg, err := m.store.GetEgressProfile(ctx, s.ProxyEgressID); err == nil {
			return eg.Endpoint, eg.Region
		}
	}
	return strings.TrimSpace(s.ProxyURL), ""
}

// writeConfig renders gopay/plus/config.json from the operator settings + the
// resolved proxy + a stable auth token.
func (m *Manager) writeConfig(ctx context.Context, s Settings) error {
	proxyURL, _ := m.resolveProxy(ctx, s)
	cfg := map[string]interface{}{
		"gopay": map[string]interface{}{
			"country_code":             s.CountryCode,
			"phone_number":             s.PhoneNumber,
			"pin":                      s.Pin,
			"midtrans_client_id":       s.MidtransClientID,
			"browser_locale":           "zh-CN",
			"pin_locale":               "id",
			"otp_channel":              s.OTPChannel,
			"sms_switch_countdown_sec": 30,
			"sms_switch_endpoint":      "",
			"sms_switch_body_extra":    map[string]interface{}{},
		},
		"stripe": map[string]interface{}{"publishable_key": s.StripePublishableKey},
		"proxy":  proxyURL,
		"orchestrator": map[string]interface{}{
			"port":        8800,
			"otp_timeout": s.OTPTimeout,
			"auth_token":  m.authToken(ctx),
		},
		"otp": map[string]interface{}{
			"mode": s.OTPMode,
			"sms_api": map[string]interface{}{
				"provider":          "herosms",
				"api_key":           s.SMSAPIKey,
				"base_url":          s.SMSBaseURL,
				"country":           "id",
				"service":           "gopay",
				"poll_interval_sec": 3,
				"poll_timeout_sec":  s.OTPTimeout,
			},
			"whatsapp": map[string]interface{}{"grpc_addr": s.WhatsAppGRPCAddr},
		},
	}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := m.cfg.GopayDir
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("gopay dir %q not found (bundle the project or set gopay_dir): %w", dir, err)
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), blob, 0o600)
}

// Start renders the config and (when auto-start is on) launches the two Python
// services as managed subprocesses, then waits briefly for orchestrator health.
func (m *Manager) Start(ctx context.Context) error {
	s := m.GetSettings(ctx)
	if err := m.writeConfig(ctx, s); err != nil {
		return err
	}
	if !m.cfg.GopayAutoStart {
		return nil // operator runs the services themselves
	}
	dir := m.cfg.GopayDir
	py := m.cfg.GopayPython
	// payment_server.py imports sibling modules in plus_gopay_links/, so it must
	// run from that directory with --config pointing back at ../config.json.
	payment := exec.Command(py, "payment_server.py", "--config", "../config.json", "--listen", ":50051")
	payment.Dir = filepath.Join(dir, "plus_gopay_links")
	payment.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	// orchestrator.py resolves its own paths from __file__, so cwd is just the dir.
	orch := exec.Command(py, "orchestrator.py")
	orch.Dir = dir
	orch.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.procs = []*exec.Cmd{payment, orch}
	m.running = true
	m.mu.Unlock()

	started := make([]*exec.Cmd, 0, 2)
	for _, c := range []*exec.Cmd{payment, orch} {
		if err := m.spawn(c); err != nil {
			m.mu.Lock()
			m.procs = nil
			m.running = false
			m.mu.Unlock()
			for _, startedCmd := range started {
				if startedCmd.Process != nil {
					_ = startedCmd.Process.Kill()
				}
			}
			m.Stop()
			return fmt.Errorf("launch %s: %w", filepath.Base(c.Path), err)
		}
		started = append(started, c)
	}

	// Give the orchestrator a moment to bind, then health-check (non-fatal).
	for i := 0; i < 10; i++ {
		time.Sleep(400 * time.Millisecond)
		if ok, _ := m.Health(ctx); ok {
			break
		}
	}
	return nil
}

// spawn starts a subprocess and pumps its combined output into the log ring.
func (m *Manager) spawn(c *exec.Cmd) error {
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = c.Stdout
	if err := c.Start(); err != nil {
		return err
	}
	name := filepath.Base(c.Path)
	if c.Dir != "" {
		name = filepath.Base(c.Dir)
	}
	go func() {
		defer supervisor.Recover("gopay-log-pump")
		m.pump(name, stdout)
	}()
	go func() {
		defer supervisor.Recover("gopay-process-watch")
		m.handleProcessExit(name, c, c.Wait())
	}()
	return nil
}

func (m *Manager) pump(name string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		m.appendLog("[" + name + "] " + sc.Text())
	}
}

func (m *Manager) appendLog(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendLogLocked(line)
}

func (m *Manager) appendLogLocked(line string) {
	m.logs = append(m.logs, time.Now().UTC().Format("15:04:05")+" "+line)
	if len(m.logs) > 300 {
		m.logs = m.logs[len(m.logs)-300:]
	}
}

func (m *Manager) handleProcessExit(name string, c *exec.Cmd, err error) {
	m.mu.Lock()
	tracked := false
	for _, proc := range m.procs {
		if proc == c {
			tracked = true
			break
		}
	}
	if !tracked {
		m.mu.Unlock()
		return
	}
	procs := append([]*exec.Cmd(nil), m.procs...)
	m.procs = nil
	m.running = false
	if err != nil {
		m.appendLogLocked(fmt.Sprintf("[%s] exited: %v", name, err))
	} else {
		m.appendLogLocked(fmt.Sprintf("[%s] exited", name))
	}
	m.mu.Unlock()

	for _, proc := range procs {
		if proc != c && proc.Process != nil {
			_ = proc.Process.Kill()
		}
	}
	if m.cfg.GopayAutoStart {
		go func() {
			defer supervisor.Recover("gopay-restart")
			time.Sleep(gopayRestartDelay)
			if !m.Enabled(context.Background()) {
				return
			}
			m.appendLog("[supervisor] restarting GoPay services after subprocess exit")
			if err := m.Start(context.Background()); err != nil {
				m.appendLog("[supervisor] restart failed: " + err.Error())
			}
		}()
	}
}

// Stop terminates any managed subprocesses.
func (m *Manager) Stop() {
	m.mu.Lock()
	procs := m.procs
	m.procs = nil
	m.running = false
	m.mu.Unlock()
	for _, c := range procs {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}

func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Health probes the orchestrator's /health and returns liveness + its OTP mode.
func (m *Manager) Health(ctx context.Context) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.GopayOrchestratorURL+"/health", nil)
	if err != nil {
		return false, ""
	}
	client := m.httpc
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, ""
	}
	var body struct {
		OK      bool   `json:"ok"`
		OTPMode string `json:"otp_mode"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.OK, body.OTPMode
}

// Subscribe runs the full auto-subscribe flow for one account's session token.
func (m *Manager) Subscribe(ctx context.Context, sessionToken, phone, pin string) (map[string]interface{}, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, errors.New("empty session token")
	}
	payload := map[string]string{"session_token": sessionToken}
	if phone != "" {
		payload["phone_number"] = phone
	}
	if pin != "" {
		payload["pin"] = pin
	}
	return m.post(ctx, "/subscribe", payload)
}

// PushOTP forwards a manual OTP to the orchestrator (manual mode).
func (m *Manager) PushOTP(ctx context.Context, otp, phone string) (map[string]interface{}, error) {
	return m.post(ctx, "/otp", map[string]string{"otp": otp, "phone": phone})
}

func (m *Manager) post(ctx context.Context, path string, payload interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.GopayOrchestratorURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.authToken(ctx))
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("orchestrator unreachable (%s): %w", m.cfg.GopayOrchestratorURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]interface{}{"ok": false, "error": "bad orchestrator response", "raw": string(raw)}, nil
	}
	return out, nil
}

// Status is the admin-facing snapshot.
func (m *Manager) Status(ctx context.Context) map[string]interface{} {
	enabled := m.Enabled(ctx)
	s := m.GetSettings(ctx)
	proxyURL, region := m.resolveProxy(ctx, s)
	health, otpMode := false, ""
	if enabled {
		health, otpMode = m.Health(ctx)
	}
	m.mu.Lock()
	logs := append([]string(nil), m.logs...)
	running := m.running
	m.mu.Unlock()
	return map[string]interface{}{
		"enabled":         enabled,
		"running":         running,
		"auto_start":      m.cfg.GopayAutoStart,
		"healthy":         health,
		"otp_mode":        otpMode,
		"orchestrator":    m.cfg.GopayOrchestratorURL,
		"gopay_dir":       m.cfg.GopayDir,
		"proxy_url":       proxyURL,
		"proxy_region":    region,
		"proxy_egress_id": s.ProxyEgressID,
		"settings":        s.Redacted(),
		"logs":            logs,
	}
}
