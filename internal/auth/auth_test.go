package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func codexJWTForTest(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	headerRaw, err := json.Marshal(map[string]interface{}{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claimsRaw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(headerRaw) + "." + encode(claimsRaw) + "." + encode([]byte("sig"))
}

func TestParseChatGPTWebSessionBuildsUsableExternalCredentials(t *testing.T) {
	expiresAt := time.Now().Add(20 * time.Minute).Unix()
	accessToken := codexJWTForTest(t, map[string]interface{}{
		"email": "web@example.internal",
		"exp":   expiresAt,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id":            "user-web",
			"chatgpt_account_id":         "workspace-web",
			"chatgpt_plan_type":          "pro",
			"chatgpt_account_is_fedramp": true,
		},
	})
	raw, _ := json.Marshal(map[string]interface{}{
		"session": map[string]interface{}{
			"user":         map[string]interface{}{"id": "user-web", "name": "Web User", "email": "web@example.internal"},
			"account":      map[string]interface{}{"id": "workspace-web", "planType": "pro"},
			"expires":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"accessToken":  accessToken,
			"authProvider": "auth0",
		},
	})
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse Web session: %v", err)
	}
	if parsed.Provider != "codex" || parsed.CredentialMode != CredentialModeChatGPTAuthTokens || parsed.RefreshToken != "" {
		t.Fatalf("external credential classification = %+v", parsed)
	}
	if parsed.AccessToken != accessToken || parsed.UpstreamAccountID != "workspace-web" || parsed.ChatGPTUserID != "user-web" {
		t.Fatalf("Web identity/token mismatch: %+v", parsed)
	}
	if parsed.Email != "web@example.internal" || parsed.Name != "Web User" || parsed.PlanType != "pro" || !parsed.IsFedramp {
		t.Fatalf("Web metadata mismatch: %+v", parsed)
	}
	if parsed.ExpiresAt != expiresAt {
		t.Fatalf("expires_at = %d, want access-token exp %d", parsed.ExpiresAt, expiresAt)
	}
	if !parsed.SyntheticIDToken || parsed.IDTokenRaw == accessToken || !codexParsableJWT(parsed.IDTokenRaw) {
		t.Fatalf("synthetic id_token is not Codex-compatible: synthetic=%v token=%q", parsed.SyntheticIDToken, parsed.IDTokenRaw)
	}
	claims := decodeIDClaims(parsed.IDTokenRaw)
	if claims.ChatGPTAccountID != "workspace-web" || claims.ChatGPTUserID != "user-web" || claims.PlanType != "pro" {
		t.Fatalf("synthetic id_token claims = %+v", claims)
	}
}

func TestParseChatGPTWebSessionExtractsSessionTokenAliases(t *testing.T) {
	accessToken := codexJWTForTest(t, map[string]interface{}{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id":    "user-session-token",
			"chatgpt_account_id": "workspace-session-token",
		},
	})
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"accessToken": accessToken,
			"account":     map[string]interface{}{"id": "workspace-session-token"},
			"user":        map[string]interface{}{"id": "user-session-token"},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]interface{})
		want   string
	}{
		{
			name: "camel case",
			mutate: func(root map[string]interface{}) {
				root["sessionToken"] = "camel-session-token"
			},
			want: "camel-session-token",
		},
		{
			name: "snake case",
			mutate: func(root map[string]interface{}) {
				root["session_token"] = "snake-session-token"
			},
			want: "snake-session-token",
		},
		{
			name: "explicit cookie wins",
			mutate: func(root map[string]interface{}) {
				root["sessionToken"] = "fallback-session-token"
				root["session_cookie"] = "explicit-session-cookie"
			},
			want: "explicit-session-cookie",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := base()
			tc.mutate(payload)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseAuthJSON(raw)
			if err != nil {
				t.Fatalf("parse Web session: %v", err)
			}
			if parsed.SessionCookie != tc.want {
				t.Fatalf("session cookie = %q, want %q", parsed.SessionCookie, tc.want)
			}
		})
	}
}

func TestParseCPAExternalCredentialsReplacesInvalidPlaceholder(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	lastRefresh := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	accessToken := codexJWTForTest(t, map[string]interface{}{
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id": "user-cpa",
		},
	})
	raw, _ := json.Marshal(map[string]interface{}{
		"type": "codex", "access_token": accessToken, "id_token": "placeholder",
		"account_id": "workspace-cpa", "email": "cpa@example.internal", "plan_type": "plus",
		"expired":      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"last_refresh": lastRefresh.Format(time.RFC3339),
	})
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse CPA JSON: %v", err)
	}
	if parsed.Provider != "codex" || parsed.CredentialMode != CredentialModeChatGPTAuthTokens {
		t.Fatalf("CPA credential classification = %+v", parsed)
	}
	if !parsed.SyntheticIDToken || parsed.IDTokenRaw == "placeholder" || !codexParsableJWT(parsed.IDTokenRaw) {
		t.Fatalf("CPA placeholder was not repaired: %+v", parsed)
	}
	if parsed.ExpiresAt != expiresAt {
		t.Fatalf("expires_at = %d, want authoritative JWT exp %d", parsed.ExpiresAt, expiresAt)
	}
	if parsed.LastRefresh != lastRefresh.Unix() {
		t.Fatalf("last_refresh = %d, want %d", parsed.LastRefresh, lastRefresh.Unix())
	}
}

func TestParseExternalChatGPTCredentialsRejectsGuaranteedRuntimeFailures(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	expired := time.Now().Add(-time.Minute).Unix()
	validWithoutWorkspace := codexJWTForTest(t, map[string]interface{}{"exp": future})
	expiredWithWorkspace := codexJWTForTest(t, map[string]interface{}{
		"exp":                         expired,
		"https://api.openai.com/auth": map[string]interface{}{"chatgpt_account_id": "workspace"},
	})
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"malformed bearer", `{"type":"codex","access_token":"not-a-jwt","account_id":"workspace"}`, "Codex-parseable JWT"},
		{"missing workspace", `{"type":"codex","access_token":"` + validWithoutWorkspace + `"}`, "chatgpt_account_id"},
		{"expired bearer", `{"type":"codex","access_token":"` + expiredWithWorkspace + `"}`, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAuthJSON([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), validWithoutWorkspace) || strings.Contains(err.Error(), expiredWithWorkspace) {
				t.Fatalf("error leaked bearer token: %v", err)
			}
		})
	}
}

func TestParseCodexAppServerExternalTokenShape(t *testing.T) {
	accessToken := codexJWTForTest(t, map[string]interface{}{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]interface{}{"chatgpt_user_id": "user-app-server"},
	})
	raw, _ := json.Marshal(map[string]interface{}{
		"type": "chatgptAuthTokens", "accessToken": accessToken,
		"chatgptAccountId": "workspace-app-server", "chatgptPlanType": "business",
	})
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse app-server token shape: %v", err)
	}
	if parsed.UpstreamAccountID != "workspace-app-server" || parsed.ChatGPTUserID != "user-app-server" ||
		parsed.CredentialMode != CredentialModeChatGPTAuthTokens {
		t.Fatalf("app-server token shape = %+v", parsed)
	}
}

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

func TestParseTopLevelSessionAuthJSONShape(t *testing.T) {
	raw := []byte(`{
		"access_token": "access-top",
		"refresh_token": "refresh-top",
		"id_token": "",
		"account_id": "acct-top",
		"chatgpt_account_id": "acct-chatgpt",
		"chatgpt_user_id": "user-top",
		"email": "top@example.internal",
		"name": "Top User",
		"plan_type": "plus",
		"expired": 1760000000,
		"last_refresh": 1750000000
	}`)
	parsed, err := ParseAuthJSON(raw)
	if err != nil {
		t.Fatalf("parse top-level session auth json: %v", err)
	}
	if parsed.AccessToken != "access-top" || parsed.RefreshToken != "refresh-top" {
		t.Fatalf("tokens = %q/%q", parsed.AccessToken, parsed.RefreshToken)
	}
	if parsed.UpstreamAccountID != "acct-top" {
		t.Fatalf("account_id priority = %q, want acct-top", parsed.UpstreamAccountID)
	}
	if parsed.ChatGPTUserID != "user-top" || parsed.Email != "top@example.internal" || parsed.Name != "Top User" || parsed.PlanType != "plus" {
		t.Fatalf("metadata not parsed: %+v", parsed)
	}
	if parsed.ExpiresAt != 1760000000 || parsed.LastRefresh != 1750000000 {
		t.Fatalf("times = expires %d last_refresh %d", parsed.ExpiresAt, parsed.LastRefresh)
	}

	// Dedupe identity priority includes chatgpt_user_id before token fingerprint when
	// account/email are absent, so token rotation for the same ChatGPT user is stable.
	a, err := ParseAuthJSON([]byte(`{"access_token":"access-a","chatgpt_user_id":"user-stable"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuthJSON([]byte(`{"access_token":"access-b","chatgpt_user_id":"user-stable"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID != b.AccountID {
		t.Fatalf("same chatgpt_user_id produced different AccountID: %q vs %q", a.AccountID, b.AccountID)
	}
}

func TestParseAuthJSONPrefersChatGPTUserIDOverSharedWorkspaceAccountID(t *testing.T) {
	makeIDToken := func(userID, accountID, email string) string {
		claims := map[string]interface{}{
			"https://api.openai.com/auth": map[string]interface{}{
				"chatgpt_account_id": accountID,
				"chatgpt_user_id":    userID,
			},
			"https://api.openai.com/profile": map[string]interface{}{
				"email": email,
			},
		}
		raw, _ := json.Marshal(claims)
		return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
	}

	sharedWorkspace := "workspace-shared"
	a, err := ParseAuthJSON([]byte(`{"account_id":"` + sharedWorkspace + `","id_token":"` + makeIDToken("user-a", sharedWorkspace, "alias+a@example.internal") + `","access_token":"access-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuthJSON([]byte(`{"account_id":"` + sharedWorkspace + `","id_token":"` + makeIDToken("user-b", sharedWorkspace, "alias+b@example.internal") + `","access_token":"access-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.UpstreamAccountID != sharedWorkspace || b.UpstreamAccountID != sharedWorkspace {
		t.Fatalf("workspace metadata not preserved: %+v / %+v", a, b)
	}
	if a.ChatGPTUserID != "user-a" || b.ChatGPTUserID != "user-b" {
		t.Fatalf("chatgpt user ids not parsed: %+v / %+v", a, b)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("different chatgpt_user_id values under one workspace collapsed into %q", a.AccountID)
	}
}

func TestParseAuthJSONSeparatesWorkspacesForSameChatGPTUserID(t *testing.T) {
	makeIDToken := func(userID, accountID string) string {
		claims := map[string]interface{}{
			"https://api.openai.com/auth": map[string]interface{}{
				"chatgpt_account_id": accountID,
				"chatgpt_user_id":    userID,
			},
		}
		raw, _ := json.Marshal(claims)
		return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
	}

	a, err := ParseAuthJSON([]byte(`{"account_id":"workspace-a","id_token":"` + makeIDToken("user-same", "workspace-a") + `","access_token":"access-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuthJSON([]byte(`{"account_id":"workspace-b","id_token":"` + makeIDToken("user-same", "workspace-b") + `","access_token":"access-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.ChatGPTUserID != "user-same" || b.ChatGPTUserID != "user-same" {
		t.Fatalf("chatgpt user ids not parsed: %+v / %+v", a, b)
	}
	if a.UpstreamAccountID != "workspace-a" || b.UpstreamAccountID != "workspace-b" {
		t.Fatalf("workspace ids not parsed: %+v / %+v", a, b)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("same chatgpt_user_id in different workspaces collapsed into %q", a.AccountID)
	}

	rotated, err := ParseAuthJSON([]byte(`{"account_id":"workspace-a","id_token":"` + makeIDToken("user-same", "workspace-a") + `","access_token":"access-rotated"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccountID != a.AccountID {
		t.Fatalf("same chatgpt_user_id and workspace should survive token rotation: %q vs %q", rotated.AccountID, a.AccountID)
	}
}

func TestParseAuthJSONDoesNotDedupeByEmailWhenAccountIDsAreMissing(t *testing.T) {
	a, err := ParseAuthJSON([]byte(`{"access_token":"access-alias-a","email":"alias@example.internal"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuthJSON([]byte(`{"access_token":"access-alias-b","email":"alias@example.internal"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("same alias email should not collapse distinct token-only accounts: %q", a.AccountID)
	}
}

func TestParseClaudeCredentialsDoesNotDedupeByEmailWhenTokenDiffers(t *testing.T) {
	a, err := ParseAuthJSON([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-a","email":"alias@example.internal"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuthJSON([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-b","email":"alias@example.internal"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("same alias email should not collapse distinct Claude token accounts: %q", a.AccountID)
	}
}

func TestParseCodexOAuthAndAccessTokenUseChatGPTUserIDDedupe(t *testing.T) {
	claims := map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id":    "user-stable",
			"chatgpt_account_id": "workspace-stable",
		},
	}
	claimsRaw, _ := json.Marshal(claims)
	idToken := "header." + base64.RawURLEncoding.EncodeToString(claimsRaw) + ".sig"
	oauthA, err := ParseOAuthCodex("access-a", "refresh-a", idToken)
	if err != nil {
		t.Fatal(err)
	}
	oauthB, err := ParseOAuthCodex("access-b", "refresh-b", idToken)
	if err != nil {
		t.Fatal(err)
	}
	if oauthA.AccountID != oauthB.AccountID {
		t.Fatalf("oauth token rotation changed account id: %q vs %q", oauthA.AccountID, oauthB.AccountID)
	}

	accessTokenA := "header." + base64.RawURLEncoding.EncodeToString(claimsRaw) + ".sig-a"
	accessTokenB := "header." + base64.RawURLEncoding.EncodeToString(claimsRaw) + ".sig-b"
	atA, err := ParseAccessToken(accessTokenA, "")
	if err != nil {
		t.Fatal(err)
	}
	atB, err := ParseAccessToken(accessTokenB, "")
	if err != nil {
		t.Fatal(err)
	}
	if atA.AccountID != atB.AccountID || atA.AccountID != oauthA.AccountID {
		t.Fatalf("access-token and oauth dedupe diverged: at=%q/%q oauth=%q", atA.AccountID, atB.AccountID, oauthA.AccountID)
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
