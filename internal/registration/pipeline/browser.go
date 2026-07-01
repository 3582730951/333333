// Browser-based registration: runs the Playwright signup harness in
// services/codex_register/browser_register.py as a subprocess and imports its result
// into the pool. This is the real-browser path for OpenAI's JS-gated signup (the HTTP
// protocol flow hits an invalid_state because the authorize step needs browser JS).
//
// The harness needs three things, resolved here from the same places the protocol flow
// uses, so the operator configures providers once:
//   - cliproxy residential proxy  ← the request's egress profile endpoint
//   - hero-sms api_key            ← provider_settings (sms/herosms)
//   - a Chrome binary + venv python ← env (CODEX_REG_CHROME / CODEX_REG_PYTHON /
//     CODEX_REG_SCRIPT), with sensible repo-relative defaults.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

type browserResult struct {
	Success     bool   `json:"success"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	UserID      string `json:"user_id"`
	PlanType    string `json:"plan_type"`
	Name        string `json:"name"`
	Error       string `json:"error"`
}

// firstEnv returns the first non-empty env var value, else def.
func firstEnv(def string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return def
}

// cliproxySpecFromEndpoint turns a proxy URL "http://user:pass@host:port" into the
// harness's "host:port:user:pass" spec. Returns "" if endpoint isn't a userinfo proxy URL.
func cliproxySpecFromEndpoint(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || u.User == nil {
		return ""
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	return fmt.Sprintf("%s:%s:%s", u.Host, user, pass)
}

// herosmsKey reads the hero-sms api_key from provider_settings.
func (p *Pipeline) herosmsKey(ctx context.Context) string {
	var cfgJSON string
	err := p.store.DB().QueryRowContext(ctx,
		`SELECT config_json FROM provider_settings
		 WHERE provider_type='sms' AND provider_key='herosms' AND enabled=1 LIMIT 1`).Scan(&cfgJSON)
	if err != nil {
		return ""
	}
	cfg := map[string]interface{}{}
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)
	s, _ := cfg["api_key"].(string)
	return strings.TrimSpace(s)
}

func (p *Pipeline) browserRegisterOne(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	egress, _ := p.store.GetEgressProfile(ctx, req.EgressID)
	spec := cliproxySpecFromEndpoint(egress.Endpoint)
	if spec == "" {
		return nil, fmt.Errorf("browser register: egress %q has no usable proxy endpoint (need a cliproxy http_proxy egress)", req.EgressID)
	}
	herokey := p.herosmsKey(ctx)
	if herokey == "" {
		return nil, fmt.Errorf("browser register: no enabled hero-sms provider configured")
	}

	python := firstEnv("python3", "CODEX_REG_PYTHON")
	script := firstEnv("services/codex_register/browser_register.py", "CODEX_REG_SCRIPT")
	chrome := firstEnv("", "CODEX_REG_CHROME")
	headless := firstEnv("1", "CODEX_REG_HEADLESS")

	// The signup flow (CF clear + email/SMS OTP waits) can take a couple of minutes.
	cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, python, script)
	cmd.Env = append(os.Environ(),
		"CLIPROXY_SPEC="+spec,
		"HEROSMS_KEY="+herokey,
		"CHROME_PATH="+chrome,
		"REG_HEADLESS="+headless,
	)
	out, err := cmd.Output() // stdout = the JSON result line; stderr = step logs
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("browser register: harness failed: %w", err)
	}
	// The harness prints exactly one JSON line on stdout.
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	var res browserResult
	if e := json.Unmarshal([]byte(line), &res); e != nil {
		return nil, fmt.Errorf("browser register: unparseable harness output: %v", e)
	}
	if !res.Success || strings.TrimSpace(res.AccessToken) == "" {
		return nil, fmt.Errorf("browser register failed: %s", firstNonEmpty(res.Error, "no access token"))
	}

	account := &storage.Account{
		ID:                generateAccountID(),
		Label:             "browser-" + firstNonEmpty(res.Email, res.Phone),
		GroupName:         req.GroupName,
		UpstreamAccountID: res.AccountID,
		ChatGPTUserID:     res.UserID,
		Email:             res.Email,
		PlanType:          firstNonEmpty(res.PlanType, "free"),
		Provider:          "codex",
		Status:            "active",
		CreatedAt:         time.Now().Unix(),
		UpdatedAt:         time.Now().Unix(),
	}
	token := &storage.AccountToken{
		AccountID:   account.ID,
		AccessToken: res.AccessToken,
		CreatedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
	}
	if err := p.store.UpsertAccount(ctx, *account, *token); err != nil {
		return nil, fmt.Errorf("upsertAccount: %w", err)
	}
	return account, nil
}
