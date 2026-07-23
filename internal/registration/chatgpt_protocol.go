package registration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	authOrigin    = "https://auth.openai.com"
	chatgptOrigin = "https://chatgpt.com"
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthRedirectURI = "http://localhost:1455/auth/callback"
)

// Standard browser headers matching Chrome 145 (keygen profile).
var baseNavHeaders = map[string]string{
	"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
	"accept-language":           "en-US,en;q=0.9",
	"sec-ch-ua":                 `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`,
	"sec-ch-ua-mobile":          "?0",
	"sec-ch-ua-platform":        `"Windows"`,
	"sec-fetch-dest":            "document",
	"sec-fetch-mode":            "navigate",
	"sec-fetch-site":            "same-origin",
	"sec-fetch-user":            "?1",
	"upgrade-insecure-requests": "1",
}

var baseAPIHeaders = map[string]string{
	"accept":             "application/json",
	"accept-language":    "en-US,en;q=0.9",
	"content-type":       "application/json",
	"origin":             authOrigin,
	"sec-ch-ua":          `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`,
	"sec-ch-ua-mobile":   "?0",
	"sec-ch-ua-platform": `"Windows"`,
	"sec-fetch-dest":     "empty",
	"sec-fetch-mode":     "cors",
	"sec-fetch-site":     "same-origin",
}

func mergeHeaders(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[strings.ToLower(k)] = v
	}
	for k, v := range extra {
		out[strings.ToLower(k)] = v
	}
	return out
}

func pickUA() string {
	uas := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36",
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(uas))))
	return uas[n.Int64()]
}

// ── Signup Session ──────────────────────────────────────────────────

type SignupSession struct {
	Sidecar       *SidecarHTTPClient
	UserAgent     string
	Email         string
	Password      string
	DeviceID      string
	ProxyURL      string
	CodeVerifier  string
	CodeChallenge string
	AuthState     string
	Name          string
	Birthdate     string
}

// RegisterResult holds the outcome.
type RegisterResult struct {
	Email        string                 `json:"email"`
	AccountID    string                 `json:"account_id"`
	PlanType     string                 `json:"plan_type"`
	AccessToken  string                 `json:"access_token"`
	SessionToken string                 `json:"session_token"`
	ExpiresAt    string                 `json:"expires_at"`
	Profile      map[string]interface{} `json:"profile"`
	Cookies      map[string]string      `json:"cookies,omitempty"`
}

// ── PKCE ─────────────────────────────────────────────────────────────

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:43]
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ── Full Keygen Registration Flow ────────────────────────────────────

// InitiateSignup performs the keygen registration flow: authorize → continue → register → OTP.
// After this, the OTP email has been sent. Use WaitForOTP + CompleteRegistration to finish.
func InitiateSignup(ctx context.Context, sidecar *SidecarHTTPClient, email, password, proxyURL string) (*SignupSession, error) {
	ua := pickUA()
	deviceID := randomUUID()
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)

	session := &SignupSession{
		Sidecar:       sidecar,
		UserAgent:     ua,
		Email:         email,
		Password:      password,
		DeviceID:      deviceID,
		ProxyURL:      proxyURL,
		CodeVerifier:  codeVerifier,
		CodeChallenge: codeChallenge,
	}

	// Inject oai-did cookie via the sidecar (handled by cookie_jar_key)
	time.Sleep(time.Duration(200+randomInt(0, 300)) * time.Millisecond)

	// Step 0a: GET /oauth/authorize (screen_hint=signup) with PKCE
	state := randomHex(32)
	params := fmt.Sprintf(
		"response_type=code&client_id=%s&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s&screen_hint=signup&prompt=login",
		urlEncode(oauthClientID), urlEncode(oauthRedirectURI), urlEncode("openid profile email offline_access"),
		urlEncode(codeChallenge), urlEncode(state),
	)
	authorizeURL := authOrigin + "/oauth/authorize?" + params

	navHeaders := mergeHeaders(baseNavHeaders, map[string]string{
		"user-agent": ua,
	})
	resp, err := sidecar.Get(ctx, authorizeURL, navHeaders, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("authorize GET: %w", err)
	}
	resp.Body.Close()

	// Check we got login_session cookie
	// (sidecar handles cookies automatically via cookie_jar_key)

	// Step 0b: Build sentinel token
	sentinel, err := BuildSentinelToken(ctx, sidecar, deviceID, "authorize_continue", proxyURL)
	if err != nil {
		return nil, fmt.Errorf("sentinel token: %w", err)
	}

	// Step 0c: POST /api/accounts/authorize/continue
	time.Sleep(time.Duration(500+randomInt(0, 500)) * time.Millisecond)
	continueBody, _ := json.Marshal(map[string]interface{}{
		"username":    map[string]string{"kind": "email", "value": email},
		"screen_hint": "signup",
	})
	apiHeaders := mergeHeaders(baseAPIHeaders, map[string]string{
		"user-agent":            ua,
		"referer":               authOrigin + "/create-account",
		"oai-device-id":         deviceID,
		"openai-sentinel-token": sentinel,
	})
	resp, err = sidecar.Post(ctx, authOrigin+"/api/accounts/authorize/continue", apiHeaders, continueBody, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("authorize continue: %w", err)
	}
	respBody, _ := ReadBody(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("authorize continue HTTP %d: %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	session.AuthState = state

	return session, nil
}

// RegisterAndSendOTP does the password page visit, register, and send OTP.
// Returns after the OTP email has been triggered.
func RegisterAndSendOTP(ctx context.Context, session *SignupSession) error {
	ua := session.UserAgent
	proxyURL := session.ProxyURL

	// Step 1: GET /create-account/password
	time.Sleep(time.Duration(500+randomInt(0, 500)) * time.Millisecond)
	navHeaders := mergeHeaders(baseNavHeaders, map[string]string{
		"user-agent": ua,
		"referer":    authOrigin + "/create-account",
	})
	resp, _ := session.Sidecar.Get(ctx, authOrigin+"/create-account/password?state="+session.AuthState, navHeaders, proxyURL)
	if resp != nil {
		resp.Body.Close()
	}

	// Step 2: Build sentinel for register step
	time.Sleep(time.Duration(500+randomInt(0, 500)) * time.Millisecond)
	sentinel, err := BuildSentinelToken(ctx, session.Sidecar, session.DeviceID, "authorize_continue", proxyURL)
	if err != nil {
		return fmt.Errorf("sentinel for register: %w", err)
	}

	// Step 3: POST /api/accounts/user/register
	regBody, _ := json.Marshal(map[string]string{
		"username": session.Email,
		"password": session.Password,
	})
	apiHeaders := mergeHeaders(baseAPIHeaders, map[string]string{
		"user-agent":            ua,
		"referer":               authOrigin + "/create-account/password",
		"oai-device-id":         session.DeviceID,
		"openai-sentinel-token": sentinel,
	})
	resp, err = session.Sidecar.Post(ctx, authOrigin+"/api/accounts/user/register", apiHeaders, regBody, proxyURL)
	if err != nil {
		return fmt.Errorf("user register: %w", err)
	}
	respBody, _ := ReadBody(resp)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("user register HTTP %d: %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	// Step 4: POST /api/accounts/email-otp/send (trigger OTP email)
	time.Sleep(time.Duration(500+randomInt(0, 500)) * time.Millisecond)
	apiHeaders = mergeHeaders(baseAPIHeaders, map[string]string{
		"user-agent":    ua,
		"oai-device-id": session.DeviceID,
	})
	resp, err = session.Sidecar.Post(ctx, authOrigin+"/api/accounts/email-otp/send", apiHeaders, []byte("{}"), proxyURL)
	if err != nil {
		return fmt.Errorf("send OTP: %w", err)
	}
	respBody, _ = ReadBody(resp)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("send OTP HTTP %d: %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	return nil
}

// CompleteRegistration finishes the flow after OTP is received.
func CompleteRegistration(ctx context.Context, session *SignupSession, otpCode string) (*RegisterResult, error) {
	ua := session.UserAgent
	proxyURL := session.ProxyURL

	// Step 5: POST /api/accounts/email-otp/validate
	valBody, _ := json.Marshal(map[string]string{"code": otpCode})
	apiHeaders := mergeHeaders(baseAPIHeaders, map[string]string{
		"user-agent":    ua,
		"oai-device-id": session.DeviceID,
	})
	resp, err := session.Sidecar.Post(ctx, authOrigin+"/api/accounts/email-otp/validate", apiHeaders, valBody, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("validate OTP: %w", err)
	}
	respBody, _ := ReadBody(resp)
	if resp.StatusCode >= 400 {
		// Retry once on wrong code
		return nil, fmt.Errorf("validate OTP HTTP %d: %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	// Step 6: POST /api/accounts/create_account
	name := session.Name
	birthdate := session.Birthdate
	if name == "" {
		first, last := randomNameInner()
		name = strings.Title(first + " " + last)
	}
	if birthdate == "" {
		birthdate = "1995-06-15"
	}
	createBody, _ := json.Marshal(map[string]string{
		"name":      name,
		"birthdate": birthdate,
	})
	resp, err = session.Sidecar.Post(ctx, authOrigin+"/api/accounts/create_account", apiHeaders, createBody, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	respBody, _ = ReadBody(resp)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("create account HTTP %d: %s", resp.StatusCode, string(respBody[:min(500, len(respBody))]))
	}

	// Follow callback if any
	var created struct {
		ContinueURL string `json:"continue_url"`
	}
	json.Unmarshal(respBody, &created)
	if created.ContinueURL != "" {
		cbURL := created.ContinueURL
		if !strings.HasPrefix(cbURL, "http") {
			cbURL = authOrigin + cbURL
		}
		navHeaders := mergeHeaders(baseNavHeaders, map[string]string{"user-agent": ua})
		resp, _ = session.Sidecar.Get(ctx, cbURL, navHeaders, proxyURL)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Step 7: GET /api/auth/session to get tokens
	sessionHeaders := map[string]string{
		"user-agent": ua,
		"accept":     "application/json",
	}
	resp, err = session.Sidecar.Get(ctx, chatgptOrigin+"/api/auth/session", sessionHeaders, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	respBody, _ = ReadBody(resp)

	var sessionData struct {
		AccessToken  string                 `json:"accessToken"`
		SessionToken string                 `json:"sessionToken"`
		Expires      string                 `json:"expires"`
		User         map[string]interface{} `json:"user"`
		Account      map[string]interface{} `json:"account"`
	}
	if err := json.Unmarshal(respBody, &sessionData); err != nil {
		return nil, fmt.Errorf("parse session: %w (body:%s)", err, string(respBody[:min(200, len(respBody))]))
	}
	if sessionData.AccessToken == "" {
		return nil, fmt.Errorf("session missing accessToken: %s", string(respBody[:min(300, len(respBody))]))
	}

	claims := decodeJWTPayload(sessionData.AccessToken)
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]interface{})
	profile, _ := claims["https://api.openai.com/profile"].(map[string]interface{})

	accountID := ""
	planType := "free"
	email := session.Email
	if authClaims != nil {
		if id, ok := authClaims["chatgpt_account_id"].(string); ok {
			accountID = id
		}
		if pt, ok := authClaims["chatgpt_plan_type"].(string); ok {
			planType = pt
		}
	}
	if profile != nil {
		if e, ok := profile["email"].(string); ok {
			email = e
		}
	}

	return &RegisterResult{
		Email:        email,
		AccountID:    accountID,
		PlanType:     planType,
		AccessToken:  sessionData.AccessToken,
		SessionToken: sessionData.SessionToken,
		ExpiresAt:    sessionData.Expires,
		Profile:      sessionData.User,
	}, nil
}

func decodeJWTPayload(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	// Pad and decode
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var claims map[string]interface{}
	json.Unmarshal(raw, &claims)
	return claims
}

func urlEncode(s string) string {
	result := ""
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			result += string(c)
		} else {
			result += fmt.Sprintf("%%%02X", c)
		}
	}
	return result
}

func randomHex(n int) string {
	b := make([]byte, n/2+1)
	rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}

func randomNameInner() (string, string) {
	first := []string{"James", "Mary", "Robert", "Patricia", "John", "Jennifer"}
	last := []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia"}
	fn, _ := rand.Int(rand.Reader, big.NewInt(int64(len(first))))
	ln, _ := rand.Int(rand.Reader, big.NewInt(int64(len(last))))
	return first[fn.Int64()], last[ln.Int64()]
}
