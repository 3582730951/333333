package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ParsedAuth struct {
	AccountID         string
	UpstreamAccountID string
	AccessToken       string
	RefreshToken      string
	OpenAIAPIKey      string
	IDTokenRaw        string
	Email             string
	ChatGPTUserID     string
	PlanType          string
	IsFedramp         bool
}

func ParseAuthJSON(raw []byte) (ParsedAuth, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return ParsedAuth{}, err
	}

	var out ParsedAuth
	out.OpenAIAPIKey = stringField(root, "OPENAI_API_KEY")
	if tokens, ok := root["tokens"].(map[string]interface{}); ok {
		out.AccessToken = stringField(tokens, "access_token")
		out.RefreshToken = stringField(tokens, "refresh_token")
		out.UpstreamAccountID = stringField(tokens, "account_id")
		out.IDTokenRaw = extractIDTokenRaw(tokens["id_token"])
		if out.IDTokenRaw != "" {
			claims := decodeIDClaims(out.IDTokenRaw)
			if out.Email == "" {
				out.Email = claims.Email
			}
			if out.ChatGPTUserID == "" {
				out.ChatGPTUserID = claims.ChatGPTUserID
			}
			if out.UpstreamAccountID == "" {
				out.UpstreamAccountID = claims.ChatGPTAccountID
			}
			out.PlanType = claims.PlanType
			out.IsFedramp = claims.IsFedramp
		}
	}
	if out.AccessToken == "" {
		out.AccessToken = out.OpenAIAPIKey
	}
	if out.AccessToken == "" && out.OpenAIAPIKey == "" {
		return ParsedAuth{}, errors.New("auth.json has neither tokens.access_token nor OPENAI_API_KEY")
	}
	out.AccountID = stableAccountID(out.UpstreamAccountID, out.Email, out.AccessToken, out.OpenAIAPIKey)
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
	out.AccountID = stableAccountID(out.UpstreamAccountID, out.Email, out.AccessToken)
	return out, nil
}

// ParseOAuthClaude builds a ParsedAuth from the Anthropic OAuth token exchange
// (Claude Pro/Max web-login). The access token is an opaque sk-ant-oat string
// (not a JWT), so the provider is later inferred from that prefix and only the
// account email returned by the exchange is carried through.
func ParseOAuthClaude(accessToken, refreshToken, email string) (ParsedAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return ParsedAuth{}, errors.New("oauth token exchange returned no access_token")
	}
	out := ParsedAuth{
		AccessToken:  accessToken,
		RefreshToken: strings.TrimSpace(refreshToken),
		Email:        strings.TrimSpace(email),
	}
	out.AccountID = stableAccountID(out.Email, out.AccessToken)
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
	out := ParsedAuth{AccessToken: accessToken, UpstreamAccountID: strings.TrimSpace(accountID)}
	claims := decodeIDClaims(accessToken)
	out.Email = claims.Email
	out.ChatGPTUserID = claims.ChatGPTUserID
	if out.UpstreamAccountID == "" {
		out.UpstreamAccountID = claims.ChatGPTAccountID
	}
	out.PlanType = claims.PlanType
	out.IsFedramp = claims.IsFedramp
	out.AccountID = stableAccountID(out.UpstreamAccountID, out.Email, out.AccessToken)
	return out, nil
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
	ChatGPTUserID    string
	ChatGPTAccountID string
	PlanType         string
	IsFedramp        bool
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
	claims := idClaims{Email: stringField(raw, "email")}
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
