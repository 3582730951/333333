package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseOfficialAuthJSONShape(t *testing.T) {
	claims := map[string]interface{}{
		"email": "user@example.internal",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id":            "user-1",
			"chatgpt_account_id":         "workspace-1",
			"chatgpt_plan_type":          "pro",
			"chatgpt_account_is_fedramp": true,
		},
	}
	claimsRaw, _ := json.Marshal(claims)
	jwt := "header." + base64.RawURLEncoding.EncodeToString(claimsRaw) + ".sig"
	raw := []byte(`{"OPENAI_API_KEY":"api-key","tokens":{"id_token":"` + jwt + `","access_token":"access","refresh_token":"refresh"}}`)
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.UpstreamAccountID != "workspace-1" || parsed.Email != "user@example.internal" || !parsed.IsFedramp {
		t.Fatalf("unexpected parsed auth: %+v", parsed)
	}
	if BearerToken(parsed) != "access" {
		t.Fatalf("bearer = %q", BearerToken(parsed))
	}
}

func TestParseClaudeCredentialsJSONShape(t *testing.T) {
	raw := []byte(`{
		"claudeAiOauth": {
			"accessToken": "sk-ant-oat-old",
			"refreshToken": "refresh-claude",
			"expiresAt": 1760000123,
			"scopes": ["user:profile", "user:inference", "user:sessions:claude_code"],
			"subscriptionType": "max",
			"rateLimitTier": "tier_4",
			"account": {"email_address": "claude@example.com"}
		}
	}`)
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Provider != "claude" {
		t.Fatalf("provider = %q, want claude", parsed.Provider)
	}
	if parsed.AccessToken != "sk-ant-oat-old" || parsed.RefreshToken != "refresh-claude" {
		t.Fatalf("tokens = %q/%q", parsed.AccessToken, parsed.RefreshToken)
	}
	if parsed.ExpiresAt != 1760000123 {
		t.Fatalf("expires_at = %d", parsed.ExpiresAt)
	}
	if parsed.Email != "claude@example.com" {
		t.Fatalf("email = %q", parsed.Email)
	}
	if parsed.PlanType != "max" {
		t.Fatalf("plan_type = %q, want max from subscriptionType", parsed.PlanType)
	}
	if parsed.OAuthRateLimitTier != "tier_4" {
		t.Fatalf("rate tier = %q", parsed.OAuthRateLimitTier)
	}
	if got := parsed.Scopes; len(got) != 3 || got[2] != "user:sessions:claude_code" {
		t.Fatalf("scopes = %#v", got)
	}
}
