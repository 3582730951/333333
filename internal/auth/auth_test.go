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
