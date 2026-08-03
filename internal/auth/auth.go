package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/agentidentity"
)

const CredentialModeChatGPTAuthTokens = "chatgpt_auth_tokens"

type ParsedAuth struct {
	AccountID            string
	UpstreamAccountID    string
	AccessToken          string
	RefreshToken         string
	OpenAIAPIKey         string
	IDTokenRaw           string
	Email                string
	Name                 string
	ChatGPTUserID        string
	PlanType             string
	Provider             string
	ExpiresAt            int64
	LastRefresh          int64
	Scopes               []string
	OAuthRateLimitTier   string
	IsFedramp            bool
	CredentialMode       string
	AgentRuntimeID       string
	AgentPrivateKey      string
	AgentTaskID          string
	AntigravityProjectID string
	AntigravityBaseURL   string
	AntigravityUserAgent string
	SessionCookie        string
	SyntheticIDToken     bool
}

func ParseOAuthAntigravity(accessToken, refreshToken, email, projectID string, expiresAt int64, scopes []string) (ParsedAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	email = strings.TrimSpace(email)
	projectID = strings.TrimSpace(projectID)
	if accessToken == "" {
		return ParsedAuth{}, errors.New("antigravity oauth token exchange returned no access_token")
	}
	if email == "" {
		return ParsedAuth{}, errors.New("antigravity oauth userinfo returned no email")
	}
	if projectID == "" {
		return ParsedAuth{}, errors.New("antigravity project discovery returned no project_id")
	}
	return ParsedAuth{
		AccountID:            stableAccountID("antigravity:" + strings.ToLower(email)),
		Provider:             "antigravity",
		AccessToken:          accessToken,
		RefreshToken:         strings.TrimSpace(refreshToken),
		Email:                email,
		ExpiresAt:            expiresAt,
		Scopes:               append([]string(nil), scopes...),
		AntigravityProjectID: projectID,
		AntigravityBaseURL:   "",
		AntigravityUserAgent: "antigravity/hub/2.2.1 darwin/arm64",
	}, nil
}

func ParseAuthJSON(raw []byte) (ParsedAuth, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return ParsedAuth{}, err
	}
	root, webSession := normalizeChatGPTWebSession(root)

	if claude, ok := objectField(root, "claudeAiOauth", "claude_ai_oauth"); ok {
		return parseClaudeCredentialsJSON(claude)
	}
	if agent, ok := agentIdentityObject(root); ok {
		return parseAgentIdentity(agent)
	}

	var out ParsedAuth
	out.OpenAIAPIKey = stringField(root, "OPENAI_API_KEY")
	out.AccessToken = stringFieldAny(root, "access_token", "accessToken")
	out.RefreshToken = stringFieldAny(root, "refresh_token", "refreshToken")
	out.UpstreamAccountID = stringFieldAny(root, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId", "chatgptAccountID")
	out.ChatGPTUserID = stringFieldAny(root, "chatgpt_user_id", "chatgptUserId", "chatgptUserID", "user_id", "userId")
	out.Email = stringFieldAny(root, "email", "email_address", "emailAddress")
	out.Name = stringFieldAny(root, "name", "full_name", "display_name", "displayName")
	out.PlanType = stringFieldAny(root, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType")
	out.ExpiresAt = epochSecondsField(root, "expired", "expires_at", "expiresAt", "expires", "expiry")
	out.LastRefresh = epochSecondsField(root, "last_refresh", "lastRefresh")
	out.IDTokenRaw = extractIDTokenRaw(firstPresent(root["id_token"], root["idToken"]))
	out.Provider = importedProvider(root)
	out.SessionCookie = stringFieldAny(root, "cookie_header", "cookieHeader", "session_cookie", "sessionCookie", "cookie")
	if webSession && strings.TrimSpace(out.SessionCookie) == "" {
		// The raw /api/auth/session response names the encrypted browser
		// session value sessionToken. Treat it as a cookie candidate only when
		// the same document is already recognized as a ChatGPT Web session;
		// explicit cookie fields above retain precedence.
		out.SessionCookie = stringFieldAny(root, "sessionToken", "session_token")
	}
	fillChatGPTWebSessionMetadata(&out, root)
	if tokens, ok := root["tokens"].(map[string]interface{}); ok {
		if out.AccessToken == "" {
			out.AccessToken = stringField(tokens, "access_token")
		}
		if out.RefreshToken == "" {
			out.RefreshToken = stringField(tokens, "refresh_token")
		}
		if out.UpstreamAccountID == "" {
			out.UpstreamAccountID = stringFieldAny(tokens, "account_id", "chatgpt_account_id")
		}
		if out.IDTokenRaw == "" {
			out.IDTokenRaw = extractIDTokenRaw(tokens["id_token"])
		}
		if out.ExpiresAt == 0 {
			out.ExpiresAt = epochSecondsField(tokens, "expired", "expires_at", "expiresAt", "expires", "expiry")
		}
		if out.LastRefresh == 0 {
			out.LastRefresh = epochSecondsField(tokens, "last_refresh", "lastRefresh")
		}
	}
	if out.IDTokenRaw != "" {
		claims := decodeIDClaims(out.IDTokenRaw)
		fillMissingIDClaims(&out, claims)
	}
	accessClaims := decodeIDClaims(out.AccessToken)
	fillMissingIDClaims(&out, accessClaims)
	if out.AccessToken == "" {
		out.AccessToken = out.OpenAIAPIKey
	}
	if out.AccessToken == "" && out.OpenAIAPIKey == "" {
		return ParsedAuth{}, errors.New("auth.json has neither tokens.access_token, access_token nor OPENAI_API_KEY")
	}
	authMode := strings.TrimSpace(stringFieldAny(root, "auth_mode", "authMode"))
	externalChatGPTTokens := webSession || isExternalChatGPTAuthMode(authMode) ||
		(strings.EqualFold(out.Provider, "codex") && out.RefreshToken == "" && out.OpenAIAPIKey == "")
	if externalChatGPTTokens {
		out.CredentialMode = CredentialModeChatGPTAuthTokens
		out.Provider = "codex"
	}
	if externalChatGPTTokens && accessClaims.ExpiresAt > 0 {
		// /api/auth/session "expires" is the browser session lifetime, and CPA's
		// copied expiry can be stale. The bearer JWT exp controls request validity.
		out.ExpiresAt = accessClaims.ExpiresAt
	} else if out.ExpiresAt == 0 {
		out.ExpiresAt = accessClaims.ExpiresAt
	}
	if externalChatGPTTokens {
		if !codexParsableJWT(out.AccessToken) {
			return ParsedAuth{}, errors.New("external ChatGPT access_token is not a Codex-parseable JWT")
		}
		if out.UpstreamAccountID == "" {
			return ParsedAuth{}, errors.New("external ChatGPT credentials have no chatgpt_account_id in either JSON or access_token")
		}
		if out.ExpiresAt > 0 && out.ExpiresAt <= time.Now().Unix() {
			return ParsedAuth{}, errors.New("external ChatGPT access_token is expired; import a fresh session")
		}
	}
	if shouldSynthesizeCodexIDToken(root, out, webSession) {
		idToken, err := synthesizeCodexIDToken(out)
		if err != nil {
			return ParsedAuth{}, err
		}
		out.IDTokenRaw = idToken
		out.SyntheticIDToken = true
	}
	out.AccountID = stableAccountID(codexAccountIdentity(out.ChatGPTUserID, out.UpstreamAccountID), out.AccessToken, out.OpenAIAPIKey)
	return out, nil
}

func agentIdentityObject(root map[string]interface{}) (map[string]interface{}, bool) {
	if nested, ok := objectField(root, "agent_identity", "agentIdentity"); ok {
		return nested, true
	}
	mode := stringFieldAny(root, "auth_mode", "authMode")
	return root, strings.EqualFold(strings.TrimSpace(mode), agentidentity.ExportAuthMode) ||
		strings.EqualFold(strings.TrimSpace(mode), agentidentity.CredentialMode)
}

func parseAgentIdentity(m map[string]interface{}) (ParsedAuth, error) {
	out := ParsedAuth{
		Provider:          "codex",
		CredentialMode:    agentidentity.CredentialMode,
		AgentRuntimeID:    stringFieldAny(m, "agent_runtime_id", "agentRuntimeId"),
		AgentPrivateKey:   stringFieldAny(m, "agent_private_key", "agentPrivateKey"),
		AgentTaskID:       stringFieldAny(m, "task_id", "taskId"),
		UpstreamAccountID: stringFieldAny(m, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"),
		ChatGPTUserID:     stringFieldAny(m, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"),
		Email:             stringFieldAny(m, "email", "email_address", "emailAddress"),
		Name:              stringFieldAny(m, "name", "display_name", "displayName"),
		PlanType:          stringFieldAny(m, "plan_type", "planType", "chatgpt_plan_type"),
		IsFedramp:         boolFieldAny(m, "chatgpt_account_is_fedramp", "chatgptAccountIsFedramp", "is_fedramp"),
	}
	if out.UpstreamAccountID == "" || out.ChatGPTUserID == "" {
		return ParsedAuth{}, errors.New("agent identity is missing account_id or chatgpt_user_id")
	}
	if err := agentidentity.Validate(agentidentity.Credentials{
		RuntimeID: out.AgentRuntimeID, PrivateKey: out.AgentPrivateKey, TaskID: out.AgentTaskID,
	}, false); err != nil {
		return ParsedAuth{}, err
	}
	// A sub2api account_id can be a shared ChatGPT workspace/account identifier,
	// so it cannot safely identify an Agent Identity credential by itself. The
	// runtime and task IDs rotate, but account + user stays stable and keeps two
	// users in the same workspace from being collapsed into one pool account.
	out.AccountID = stableAccountID("agent_identity:" + codexAccountIdentity(out.ChatGPTUserID, out.UpstreamAccountID))
	return out, nil
}

func parseClaudeCredentialsJSON(m map[string]interface{}) (ParsedAuth, error) {
	out := ParsedAuth{
		Provider:           "claude",
		AccessToken:        stringFieldAny(m, "accessToken", "access_token"),
		RefreshToken:       stringFieldAny(m, "refreshToken", "refresh_token"),
		ExpiresAt:          epochSecondsField(m, "expiresAt", "expires_at"),
		Scopes:             stringSliceField(m, "scopes", "scope"),
		PlanType:           stringFieldAny(m, "subscriptionType", "subscription_type"),
		OAuthRateLimitTier: stringFieldAny(m, "rateLimitTier", "rate_limit_tier"),
	}
	if out.PlanType == "" {
		out.PlanType = out.OAuthRateLimitTier
	}
	if acct, ok := objectField(m, "account", "user", "profile"); ok {
		out.Email = stringFieldAny(acct, "email_address", "emailAddress", "email")
	}
	if out.Email == "" {
		out.Email = stringFieldAny(m, "email", "email_address", "emailAddress")
	}
	if out.AccessToken == "" {
		return ParsedAuth{}, errors.New("claude credentials have no access token")
	}
	out.AccountID = stableAccountID(out.AccessToken)
	return out, nil
}

func BearerToken(p ParsedAuth) string {
	if p.AccessToken != "" {
		return p.AccessToken
	}
	return p.OpenAIAPIKey
}

// ParseOAuthCodex builds a ParsedAuth from the tokens returned by the OpenAI
// OAuth token exchange (the web-login / paste-back import flow). The id_token is
// the same JWT the official Codex auth.json carries, so account id, email, plan
// and FedRAMP status are recovered exactly as in ParseAuthJSON.
func ParseOAuthCodex(accessToken, refreshToken, idTokenRaw string) (ParsedAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ParsedAuth{}, errors.New("oauth token exchange returned no access_token")
	}
	out := ParsedAuth{
		AccessToken:  accessToken,
		RefreshToken: strings.TrimSpace(refreshToken),
		IDTokenRaw:   strings.TrimSpace(idTokenRaw),
	}
	if out.IDTokenRaw != "" {
		claims := decodeIDClaims(out.IDTokenRaw)
		out.Email = claims.Email
		out.ChatGPTUserID = claims.ChatGPTUserID
		out.UpstreamAccountID = claims.ChatGPTAccountID
		out.PlanType = claims.PlanType
		out.IsFedramp = claims.IsFedramp
	}
	out.AccountID = stableAccountID(codexAccountIdentity(out.ChatGPTUserID, out.UpstreamAccountID), out.AccessToken)
	return out, nil
}

// ParseOAuthClaude builds a ParsedAuth from the Anthropic OAuth token exchange
// (Claude Pro/Max web-login). The access token is an opaque sk-ant-oat string
// (not a JWT), so the provider is later inferred from that prefix and only the
// account email returned by the exchange is carried through.
func ParseOAuthClaude(accessToken, refreshToken, email string) (ParsedAuth, error) {
	return ParseOAuthClaudeMetadata(accessToken, refreshToken, email, "", "", 0, nil)
}

func ParseOAuthClaudeMetadata(accessToken, refreshToken, email, subscriptionType, rateLimitTier string, expiresAt int64, scopes []string) (ParsedAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ParsedAuth{}, errors.New("oauth token exchange returned no access_token")
	}
	out := ParsedAuth{
		Provider:           "claude",
		AccessToken:        accessToken,
		RefreshToken:       strings.TrimSpace(refreshToken),
		Email:              strings.TrimSpace(email),
		PlanType:           strings.TrimSpace(subscriptionType),
		OAuthRateLimitTier: strings.TrimSpace(rateLimitTier),
		ExpiresAt:          expiresAt,
		Scopes:             append([]string(nil), scopes...),
	}
	if out.PlanType == "" {
		out.PlanType = out.OAuthRateLimitTier
	}
	out.AccountID = stableAccountID(out.AccessToken)
	return out, nil
}

// ParseAccessToken builds a ParsedAuth from a bare ChatGPT access token (the
// "AT" import flow), optionally with an explicit ChatGPT-Account-ID. The access
// token is a JWT whose payload carries the OpenAI auth claims, so account id,
// email, plan and FedRAMP status are recovered the same way as from id_token.
func ParseAccessToken(accessToken, accountID string) (ParsedAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ParsedAuth{}, errors.New("access_token required")
	}
	if !codexParsableJWT(accessToken) {
		return ParsedAuth{}, errors.New("access_token is not a Codex-parseable JWT")
	}
	out := ParsedAuth{
		AccessToken: accessToken, UpstreamAccountID: strings.TrimSpace(accountID),
		Provider: "codex", CredentialMode: CredentialModeChatGPTAuthTokens,
	}
	claims := decodeIDClaims(accessToken)
	fillMissingIDClaims(&out, claims)
	out.ExpiresAt = claims.ExpiresAt
	if out.UpstreamAccountID == "" {
		return ParsedAuth{}, errors.New("ChatGPT account_id required when it is absent from access_token claims")
	}
	if out.ExpiresAt > 0 && out.ExpiresAt <= time.Now().Unix() {
		return ParsedAuth{}, errors.New("access_token is expired")
	}
	idToken, err := synthesizeCodexIDToken(out)
	if err != nil {
		return ParsedAuth{}, err
	}
	out.IDTokenRaw = idToken
	out.SyntheticIDToken = true
	out.AccountID = stableAccountID(codexAccountIdentity(out.ChatGPTUserID, out.UpstreamAccountID), out.AccessToken)
	return out, nil
}

func codexAccountIdentity(chatGPTUserID, upstreamAccountID string) string {
	userID := strings.TrimSpace(chatGPTUserID)
	accountID := strings.TrimSpace(upstreamAccountID)
	if userID != "" && accountID != "" {
		encoded, _ := json.Marshal([]string{"codex", userID, accountID})
		return string(encoded)
	}
	if userID != "" {
		return userID
	}
	return accountID
}

func stableAccountID(parts ...string) string {
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			sum := sha256.Sum256([]byte(part))
			return "acc_" + hex.EncodeToString(sum[:])[:16]
		}
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "acc_unknown"
	}
	return "acc_" + hex.EncodeToString(b[:])
}

func stringField(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func stringFieldAny(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(stringField(m, key)); v != "" {
			return v
		}
	}
	return ""
}

func boolFieldAny(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		switch value := m[key].(type) {
		case bool:
			return value
		case string:
			return strings.EqualFold(strings.TrimSpace(value), "true")
		}
	}
	return false
}

func firstPresent(vs ...interface{}) interface{} {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func objectField(m map[string]interface{}, keys ...string) (map[string]interface{}, bool) {
	for _, key := range keys {
		if v, ok := m[key].(map[string]interface{}); ok {
			return v, true
		}
	}
	return nil, false
}

func epochSecondsField(m map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		var n int64
		switch t := v.(type) {
		case float64:
			n = int64(t)
		case json.Number:
			parsed, _ := t.Int64()
			n = parsed
		case string:
			value := strings.TrimSpace(t)
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				n = parsed
			} else if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				n = parsed.Unix()
			}
		}
		if n > 1_000_000_000_000 {
			n /= 1000
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

func stringSliceField(m map[string]interface{}, keys ...string) []string {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case []interface{}:
			out := make([]string, 0, len(t))
			for _, item := range t {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			if len(out) > 0 {
				return out
			}
		case []string:
			out := make([]string, 0, len(t))
			for _, s := range t {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			fields := strings.FieldsFunc(t, func(r rune) bool { return r == ' ' || r == ',' })
			out := make([]string, 0, len(fields))
			for _, s := range fields {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func extractIDTokenRaw(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		if raw := stringField(t, "raw_jwt"); raw != "" {
			return raw
		}
		if raw := stringField(t, "id_token"); raw != "" {
			return raw
		}
	}
	return ""
}

type idClaims struct {
	Email            string
	Name             string
	ChatGPTUserID    string
	ChatGPTAccountID string
	PlanType         string
	IsFedramp        bool
	ExpiresAt        int64
}

func decodeIDClaims(jwt string) idClaims {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return idClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return idClaims{}
		}
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return idClaims{}
	}
	claims := idClaims{
		Email:     stringField(raw, "email"),
		Name:      stringFieldAny(raw, "name", "display_name", "displayName"),
		ExpiresAt: epochSecondsField(raw, "exp"),
	}
	if profile, ok := raw["https://api.openai.com/profile"].(map[string]interface{}); ok && claims.Email == "" {
		claims.Email = stringField(profile, "email")
	}
	if authMap, ok := raw["https://api.openai.com/auth"].(map[string]interface{}); ok {
		claims.ChatGPTUserID = firstNonEmpty(
			stringField(authMap, "chatgpt_user_id"),
			stringField(authMap, "user_id"),
		)
		claims.ChatGPTAccountID = stringField(authMap, "chatgpt_account_id")
		claims.PlanType = planTypeString(authMap["chatgpt_plan_type"])
		if v, ok := authMap["chatgpt_account_is_fedramp"].(bool); ok {
			claims.IsFedramp = v
		}
	}
	return claims
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func planTypeString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		return firstNonEmpty(stringField(t, "Known"), stringField(t, "Unknown"), stringField(t, "known"), stringField(t, "unknown"))
	default:
		return ""
	}
}
