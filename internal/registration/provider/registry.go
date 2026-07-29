package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/registration/provider/captcha"
	"codex-account-pool/internal/registration/provider/mailbox"
	"codex-account-pool/internal/registration/provider/sms"
	"codex-account-pool/internal/storage"
)

func BuildManager(ctx context.Context, store *storage.Store, httpClient *http.Client) *Manager {
	m, _ := BuildManagerWithError(ctx, store, httpClient)
	if m == nil {
		return &Manager{}
	}
	return m
}

func BuildManagerWithError(ctx context.Context, store *storage.Store, httpClient *http.Client) (*Manager, error) {
	m := &Manager{}
	if store == nil {
		return m, nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	// Wire historical success-rate queries from registration_records.
	m.Stats = NewSMSStats(store.DB())
	rows, err := store.DB().QueryContext(ctx, `
SELECT provider_type,provider_key,config_json,auth_json
FROM provider_settings WHERE enabled=1 ORDER BY priority DESC`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var ptype, pkey, cfgJSON, authJSON string
		if err := rows.Scan(&ptype, &pkey, &cfgJSON, &authJSON); err != nil {
			return m, err
		}
		cfg := map[string]interface{}{}
		if strings.TrimSpace(cfgJSON) != "" {
			if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
				return m, fmt.Errorf("provider_settings %s/%s has invalid config_json: %w", strings.TrimSpace(ptype), strings.TrimSpace(pkey), err)
			}
		}
		secrets, err := store.OpenProviderAuthJSON(ptype, pkey, authJSON)
		if err != nil {
			return m, fmt.Errorf("provider_settings %s/%s credentials unavailable: %w", strings.TrimSpace(ptype), strings.TrimSpace(pkey), err)
		}
		for field, value := range secrets {
			cfg[field] = value
		}
		switch strings.ToLower(strings.TrimSpace(ptype)) {
		case "sms":
			if p := buildSMS(normKey(pkey), cfg, httpClient); p != nil {
				m.SMS = append(m.SMS, p)
			}
		case "mailbox":
			if p := buildMailbox(normKey(pkey), cfg, httpClient); p != nil {
				m.Mailbox = append(m.Mailbox, p)
			}
		case "captcha":
			if p := buildCaptcha(normKey(pkey), asString(cfg["api_key"]), httpClient); p != nil {
				m.Captcha = append(m.Captcha, p)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	return m, nil
}

func normKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func buildSMS(key string, cfg map[string]interface{}, hc *http.Client) SMSProvider {
	apiKey := asString(cfg["api_key"])
	if apiKey == "" {
		return nil
	}
	switch key {
	case "smsbower":
		return sms.NewSMSBowerProvider(apiKey, hc)
	case "herosms":
		return sms.NewHeroSMSProvider(apiKey, hc)
	case "smsactivate":
		return sms.NewSMSActivateProvider(apiKey, hc)
	case "smspool":
		return sms.NewSMSPoolProvider(apiKey, asString(cfg["service"]), asString(cfg["max_price"]), hc)
	default:
		return nil
	}
}

func buildMailbox(key string, cfg map[string]interface{}, hc *http.Client) MailboxProvider {
	switch key {
	case "tempmail", "tempmaillol":
		return mailbox.NewTempMailLolProvider(hc)
	case "cloudflare", "moemail", "freemail", "cftempemail", "cfworker":
		apiURL := asString(cfg["api_url"])
		if apiURL == "" {
			return nil
		}
		adminToken := asString(cfg["admin_token"])
		if adminToken == "" {
			adminToken = asString(cfg["api_key"])
		}
		return mailbox.NewCloudflareTempEmailProvider(apiURL, adminToken, asString(cfg["domain"]), hc)
	case "outlookemail", "duckmail":
		pipeline := getGenericHTTPPipeline(key)
		if pipeline == nil {
			return nil
		}
		adapter, err := mailbox.NewGenericHTTPAdapter(pipeline, cfg, "", key)
		if err != nil {
			return nil
		}
		return adapter
	case "imap":
		if strings.TrimSpace(asString(cfg["host"])) == "" ||
			strings.TrimSpace(asString(cfg["email"])) == "" ||
			strings.TrimSpace(asString(cfg["password"])) == "" {
			return nil
		}
		return mailbox.NewIMAPProvider(cfg)
	default:
		return nil
	}
}

func getGenericHTTPPipeline(key string) map[string]interface{} {
	seeds := map[string]map[string]interface{}{
		"outlookemail": {"email_mode": "generate"},
		"duckmail":     {"email_mode": "generate"},
	}
	return seeds[key]
}

func buildCaptcha(key, apiKey string, hc *http.Client) CaptchaSolver {
	if apiKey == "" {
		return nil
	}
	switch key {
	case "yescaptcha":
		return &captchaAdapter{inner: captcha.NewYesCaptchaProvider(apiKey, hc)}
	case "2captcha", "twocaptcha":
		return &captchaAdapter{inner: captcha.NewTwoCaptchaProvider(apiKey, hc)}
	default:
		return nil
	}
}

type captchaAdapter struct {
	inner interface {
		Solve(ctx context.Context, req captcha.CaptchaRequest) (string, error)
		Name() string
		Type() string
	}
}

func (a *captchaAdapter) Solve(ctx context.Context, req CaptchaRequest) (string, error) {
	innerReq := captcha.CaptchaRequest{
		Type:     req.Type,
		SiteKey:  req.SiteKey,
		PageURL:  req.PageURL,
		ProxyURL: req.ProxyURL,
	}
	return a.inner.Solve(ctx, innerReq)
}

func (a *captchaAdapter) Name() string {
	return a.inner.Name()
}

func (a *captchaAdapter) Type() string {
	return a.inner.Type()
}
