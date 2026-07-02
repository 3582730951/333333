// protocol_v2 registration: runs the maintained curl_cffi + local-Sentinel-PoW registrar
// (services/codex_register/protocol_register.py) as a subprocess and imports its result.
//
// This is the flow that actually clears OpenAI's anti-abuse end-to-end (verified to OTP
// validation + /about-you): curl_cffi Chrome impersonation, a pure-Python FNV-1a Sentinel
// PoW solver (no captcha API), email-OTP via a real inbox (Hotmail plus-addressing / a
// catch-all the OTP reader serves — OpenAI does not deliver to public disposable domains),
// all egressing through a cliproxy residential proxy. The email API calls go DIRECT (not
// through the residential proxy) — proxying them is unreliable and unnecessary.
//
// The script prints a "__CODEX_ACCOUNT__ {json}" line per successful account; we parse it
// and UpsertAccount, so a successful registration auto-lands in the pool.
package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/registration/provider/proxy"
	"codex-account-pool/internal/storage"
)

type codexAccountLine struct {
	Type         string `json:"type"`
	Email        string `json:"email"`
	AccountID    string `json:"account_id"`
	UserID       string `json:"user_id"`
	PlanType     string `json:"plan_type"`
	Name         string `json:"name"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	SessionToken string `json:"session_token,omitempty"`
}

// proxyURLFromEgress returns the egress profile's proxy URL (rotated for cliproxy), or "".
//
// Two cliproxy IP-acquisition modes (egress.ProxyAuthMode):
//   - "credential" (default, empty): the endpoint is a cliproxy gateway URL with region/sid
//     embedded in the username. RotateSID swaps the sid for a fresh exit IP per call. This
//     is the legacy mode and what sid-rotation relies on.
//   - "api_whitelist": call api.cliproxy.io/white/api to extract an ip:port (whitelisted to
//     the VPS public IP), return it as http://ip:port. The IP is pinned to egress.Region so
//     the exit country matches the SMS country. Cached per-region for the IP's sticky window.
func (p *Pipeline) proxyURLFromEgress(ctx context.Context, egressID string) string {
	egress, err := p.store.GetEgressProfile(ctx, egressID)
	if err != nil {
		return ""
	}
	// API whitelist mode: extract a region-locked ip:port via the cliproxy API.
	if strings.EqualFold(strings.TrimSpace(egress.ProxyAuthMode), "api_whitelist") {
		ip, err := p.cliproxyAPIIP(ctx, egress)
		if err != nil || ip == "" {
			return ""
		}
		return "http://" + ip
	}
	// Credential mode (default): rotate the cliproxy sid in the stored endpoint.
	ep := strings.TrimSpace(egress.Endpoint)
	if ep == "" {
		return ""
	}
	if proxy.IsCliproxy(ep) {
		ep = proxy.RotateSID(ep) // fresh residential IP per registration
	}
	// curl_cffi wants an http(s)/socks5 proxy URL; the egress endpoint already is one.
	if _, e := url.Parse(ep); e != nil {
		return ""
	}
	return ep
}

// cliproxyAPIIP extracts one region-locked ip:port from the cliproxy white-api for the
// egress's region, using the egress's stored API key. Cached per-region.
func (p *Pipeline) cliproxyAPIIP(ctx context.Context, egress storage.EgressProfile) (string, error) {
	base := "https://api.cliproxy.io"
	if p.cfg != nil {
		if v := strings.TrimSpace(p.cfg.CliproxyAPIBase); v != "" {
			base = v
		}
	}
	dynamic := egressDynamicConfig(egress)
	if v := dynamicString(dynamic, "api_base"); v != "" {
		base = v
	}
	apiKey := strings.TrimSpace(egress.ProxyAPIKey)
	if apiKey == "" && p.cfg != nil {
		// Fall back to a global key from the secrets/config if the per-egress one is empty.
		apiKey = strings.TrimSpace(p.cfg.CliproxyAPIKey)
	}
	region := strings.TrimSpace(egress.Region)
	if region == "" {
		region = "Rand"
	}
	ext := &proxy.CliproxyAPIExtractor{BaseURL: base, APIKey: apiKey, HC: p.httpClient}
	return ext.CachedIP(ctx, region, dynamicInt(dynamic, "api_num", 1), dynamicInt(dynamic, "api_time", 10))
}

func egressDynamicConfig(egress storage.EgressProfile) map[string]interface{} {
	text := strings.TrimSpace(egress.DynamicConfigJSON)
	if text == "" {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil
	}
	return m
}

func dynamicString(m map[string]interface{}, key string) string {
	if len(m) == 0 {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func dynamicInt(m map[string]interface{}, key string, fallback int) int {
	if len(m) == 0 {
		return fallback
	}
	switch v := m[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func (p *Pipeline) protocolV2RegisterOne(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	python := firstEnv("python3", "CODEX_REG_PYTHON")
	script := firstEnv("services/codex_register/protocol_register.py", "CODEX_REG_PROTOCOL_SCRIPT")
	proxyURL := p.proxyURLFromEgress(ctx, req.EgressID)
	emailProvider := firstEnv("hotmail_otp", "CODEX_REG_EMAIL_PROVIDER")

	cctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, "-u", script)
	cmd.Env = append(os.Environ(),
		"REG_AUTO=1",
		"REG_COUNT=1",
		"REG_WORKERS=1",
		"REG_RETRIES=2",
		"REG_PROXY="+proxyURL,
		"EMAIL_PROVIDER="+emailProvider,
	)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("protocol_v2: registrar failed: %w", err)
	}

	// Parse the "__CODEX_ACCOUNT__ {json}" marker line.
	var acct *codexAccountLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, "__CODEX_ACCOUNT__ ")
		if i < 0 {
			continue
		}
		var a codexAccountLine
		if json.Unmarshal([]byte(line[i+len("__CODEX_ACCOUNT__ "):]), &a) == nil && strings.TrimSpace(a.AccessToken) != "" {
			acct = &a // keep the last successful account
		}
	}
	if acct == nil {
		return nil, fmt.Errorf("protocol_v2: no account produced (OpenAI signup not completed — see registrar logs)")
	}

	account := &storage.Account{
		ID:                generateAccountID(),
		Label:             "protocol-" + acct.Email,
		GroupName:         req.GroupName,
		UpstreamAccountID: acct.AccountID,
		Email:             acct.Email,
		Provider:          "codex",
		Status:            "active",
		CreatedAt:         time.Now().Unix(),
		UpdatedAt:         time.Now().Unix(),
	}
	token := &storage.AccountToken{
		AccountID:    account.ID,
		AccessToken:  acct.AccessToken,
		RefreshToken: acct.RefreshToken,
		IDTokenRaw:   acct.IDToken,
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
	}
	if err := p.store.UpsertAccount(ctx, *account, *token); err != nil {
		return nil, fmt.Errorf("upsertAccount: %w", err)
	}
	return account, nil
}
