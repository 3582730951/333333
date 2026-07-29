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
	"errors"
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
	cfg, err := p.providerConfig(ctx, "sms", "herosms")
	if err != nil {
		return ""
	}
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
	cmd.Env = append(registrarBaseEnv(),
		"CLIPROXY_SPEC="+spec,
		"HEROSMS_KEY="+herokey,
		"CHROME_PATH="+chrome,
		"REG_HEADLESS="+headless,
	)
	out, err := p.runRegistrarCommand(cctx, cmd)
	if err != nil && len(out) == 0 {
		return nil, errors.New("browser register: harness process failed")
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
		return nil, errors.New("browser register did not produce verified credentials")
	}

	return p.persistVerifiedRegistration(ctx, req, registrationCredential{
		LabelPrefix:       "browser-",
		Email:             res.Email,
		UpstreamAccountID: res.AccountID,
		ChatGPTUserID:     res.UserID,
		AccessToken:       res.AccessToken,
	})
}
