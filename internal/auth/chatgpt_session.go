package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

func normalizeChatGPTWebSession(root map[string]interface{}) (map[string]interface{}, bool) {
	candidate := root
	if session, ok := objectField(root, "session"); ok {
		candidate = make(map[string]interface{}, len(root)+len(session))
		for key, value := range root {
			candidate[key] = value
		}
		for key, value := range session {
			candidate[key] = value
		}
	}
	accessToken := strings.TrimSpace(stringField(candidate, "accessToken"))
	if accessToken == "" {
		return root, false
	}
	return candidate, true
}

func importedProvider(root map[string]interface{}) string {
	provider := strings.ToLower(strings.TrimSpace(stringField(root, "provider")))
	if provider != "" {
		return provider
	}
	switch strings.ToLower(strings.TrimSpace(stringField(root, "type"))) {
	case "codex", "openai", "chatgptauthtokens", "chatgpt_auth_tokens":
		return "codex"
	case "claude", "anthropic":
		return "claude"
	default:
		return ""
	}
}

func fillChatGPTWebSessionMetadata(out *ParsedAuth, root map[string]interface{}) {
	if user, ok := objectField(root, "user"); ok {
		if out.Email == "" {
			out.Email = stringFieldAny(user, "email", "email_address", "emailAddress")
		}
		if out.Name == "" {
			out.Name = stringFieldAny(user, "name", "full_name", "display_name", "displayName")
		}
		if out.ChatGPTUserID == "" {
			out.ChatGPTUserID = stringFieldAny(user, "id", "user_id", "userId", "chatgpt_user_id", "chatgptUserId", "chatgptUserID")
		}
	}
	if out.ChatGPTUserID == "" {
		out.ChatGPTUserID = stringFieldAny(root, "chatgpt_user_id", "chatgptUserId", "chatgptUserID", "user_id", "userId")
	}
	if out.UpstreamAccountID == "" {
		out.UpstreamAccountID = stringFieldAny(root, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId", "chatgptAccountID")
	}
	if account, ok := objectField(root, "account", "workspace"); ok {
		if out.UpstreamAccountID == "" {
			out.UpstreamAccountID = stringFieldAny(account, "id", "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId", "chatgptAccountID")
		}
		if out.PlanType == "" {
			out.PlanType = stringFieldAny(account, "plan_type", "planType", "chatgpt_plan_type")
		}
	}
}

func fillMissingIDClaims(out *ParsedAuth, claims idClaims) {
	if out.Email == "" {
		out.Email = claims.Email
	}
	if out.Name == "" {
		out.Name = claims.Name
	}
	if out.ChatGPTUserID == "" {
		out.ChatGPTUserID = claims.ChatGPTUserID
	}
	if out.UpstreamAccountID == "" {
		out.UpstreamAccountID = claims.ChatGPTAccountID
	}
	if out.PlanType == "" {
		out.PlanType = claims.PlanType
	}
	out.IsFedramp = out.IsFedramp || claims.IsFedramp
}

func shouldSynthesizeCodexIDToken(root map[string]interface{}, parsed ParsedAuth, webSession bool) bool {
	if parsed.AccessToken == "" || parsed.OpenAIAPIKey != "" {
		return false
	}
	authMode := strings.TrimSpace(stringFieldAny(root, "auth_mode", "authMode"))
	external := webSession || isExternalChatGPTAuthMode(authMode) ||
		(strings.EqualFold(parsed.Provider, "codex") && parsed.RefreshToken == "")
	if !external && parsed.IDTokenRaw == "" {
		return false
	}
	return !codexParsableJWT(parsed.IDTokenRaw)
}

func isExternalChatGPTAuthMode(mode string) bool {
	mode = strings.TrimSpace(mode)
	return strings.EqualFold(mode, "chatgptAuthTokens") || strings.EqualFold(mode, CredentialModeChatGPTAuthTokens)
}

func codexParsableJWT(token string) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]interface{}
	return json.Unmarshal(payload, &claims) == nil
}

func synthesizeCodexIDToken(parsed ParsedAuth) (string, error) {
	header := map[string]interface{}{"alg": "none", "typ": "JWT"}
	authClaims := map[string]interface{}{"chatgpt_plan_type": firstNonEmpty(parsed.PlanType, "unknown")}
	if parsed.ChatGPTUserID != "" {
		authClaims["chatgpt_user_id"] = parsed.ChatGPTUserID
		authClaims["user_id"] = parsed.ChatGPTUserID
	}
	if parsed.UpstreamAccountID != "" {
		authClaims["chatgpt_account_id"] = parsed.UpstreamAccountID
	}
	if parsed.IsFedramp {
		authClaims["chatgpt_account_is_fedramp"] = true
	}
	claims := map[string]interface{}{"https://api.openai.com/auth": authClaims}
	if parsed.Email != "" {
		claims["email"] = parsed.Email
		claims["https://api.openai.com/profile"] = map[string]interface{}{"email": parsed.Email}
	}
	if parsed.Name != "" {
		claims["name"] = parsed.Name
	}
	if parsed.ChatGPTUserID != "" {
		claims["sub"] = parsed.ChatGPTUserID
	}
	if parsed.ExpiresAt > 0 {
		claims["exp"] = parsed.ExpiresAt
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(headerJSON) + "." + encode(claimsJSON) + "." + encode([]byte("external-chatgpt-session")), nil
}
