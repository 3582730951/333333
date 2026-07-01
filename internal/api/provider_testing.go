package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Provider test connection handler

func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}

	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}

	var req struct {
		Type   string                 `json:"type"`   // sms, mailbox, captcha
		Key    string                 `json:"key"`    // provider key (smsbower, yescaptcha, etc)
		Config map[string]interface{} `json:"config"` // API key and other config
	}

	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Test the provider connection
	start := time.Now()
	success, message := testProviderConnection(r.Context(), req.Type, req.Key, req.Config)
	latency := time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":    success,
		"message":    message,
		"latency_ms": latency,
	})
}

func testProviderConnection(ctx context.Context, providerType, key string, config map[string]interface{}) (bool, string) {
	get := func(k string) string { s, _ := config[k].(string); return strings.TrimSpace(s) }
	switch providerType {
	case "sms":
		return testSMSProvider(ctx, key, get("api_key"))
	case "mailbox":
		return testMailboxProvider(ctx, key, get("api_key"), get("api_url"))
	case "captcha":
		return testCaptchaProvider(ctx, key, get("api_key"))
	default:
		return false, "unknown provider type"
	}
}

func testSMSProvider(ctx context.Context, key, apiKey string) (bool, string) {
	if apiKey == "" {
		return false, "API key is required"
	}
	switch key {
	case "smsbower", "herosms", "smsactivate", "smspool":
		if len(apiKey) < 10 {
			return false, "API key looks invalid"
		}
		// TODO: live balance check; for now validate key format.
		return true, "API key format OK"
	default:
		return false, "unknown SMS provider"
	}
}

func testMailboxProvider(ctx context.Context, key, apiKey, apiURL string) (bool, string) {
	switch key {
	case "tempmail":
		// TempMail.lol doesn't require an API key for basic usage.
		return true, "Provider available"
	case "cloudflare", "moemail", "freemail":
		if apiURL == "" {
			return false, "API URL is required"
		}
		return true, "API URL configured"
	default:
		return false, "unknown mailbox provider"
	}
}

func testCaptchaProvider(ctx context.Context, key, apiKey string) (bool, string) {
	if apiKey == "" {
		return false, "API key is required"
	}
	switch key {
	case "yescaptcha", "2captcha":
		if len(apiKey) < 10 {
			return false, "key looks invalid"
		}
		// TODO: live balance check; for now validate key format.
		return true, "key format OK"
	default:
		return false, "unknown captcha provider"
	}
}
