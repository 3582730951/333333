// Package openai implements ChatGPT registration protocol
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"codex-account-pool/internal/registration/sentinel"
)

// dbg logs a registration step's upstream HTTP status and a short body snippet, so an
// operator can see exactly where OpenAI rejected an automated signup (the flow otherwise
// only decodes JSON and would silently swallow a 4xx). Gated behind REG_DEBUG=1.
func dbg(step string, status int, body []byte) {
	if os.Getenv("REG_DEBUG") == "" {
		return
	}
	snippet := string(body)
	if len(snippet) > 700 {
		snippet = snippet[:700]
	}
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	log.Printf("[REG-DEBUG] %s status=%d body=%s", step, status, snippet)
}

const (
	CHATGPT = "https://chatgpt.com"
	AUTH    = "https://auth.openai.com"
	UA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

	registerHTTPTimeout       = 90 * time.Second
	registerResponseBodyLimit = 1 << 20
)

// RegisterClient handles phone number registration
type RegisterClient struct {
	httpClient *http.Client
	sentinel   *sentinel.Client
	deviceID   string
}

// NewRegisterClient creates a registration client
func NewRegisterClient(httpClient *http.Client, deviceID string) *RegisterClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: registerHTTPTimeout}
	}
	return &RegisterClient{
		httpClient: httpClient,
		sentinel:   sentinel.NewClient(httpClient, UA, deviceID, deviceID),
		deviceID:   deviceID,
	}
}

// RegisterResult holds registration outcome
type RegisterResult struct {
	SessionToken string
	AccessToken  string
	Email        string
	Phone        string
	UserID       string
}

// Register executes the 9-step phone registration flow
func (c *RegisterClient) Register(ctx context.Context, phone, password, name, birthdate string, otpGetter func() (string, error)) (*RegisterResult, error) {
	// Step 1: Visit chatgpt.com
	if err := c.visit(ctx); err != nil {
		return nil, fmt.Errorf("visit: %w", err)
	}

	// Step 2: Get CSRF
	csrf, err := c.getCSRF(ctx)
	if err != nil {
		return nil, fmt.Errorf("csrf: %w", err)
	}

	// Step 3: Signin
	redirectURL, err := c.signin(ctx, phone, csrf)
	if err != nil {
		return nil, fmt.Errorf("signin: %w", err)
	}

	// Step 4: Jump to auth
	if err := c.jumpToAuth(ctx, redirectURL); err != nil {
		return nil, fmt.Errorf("jumpToAuth: %w", err)
	}

	// Step 5: Register user
	continueURL, err := c.registerUser(ctx, phone, password)
	if err != nil {
		return nil, fmt.Errorf("registerUser: %w", err)
	}

	// Step 6: Send OTP
	if err := c.sendOTP(ctx, continueURL); err != nil {
		return nil, fmt.Errorf("sendOTP: %w", err)
	}

	// Get OTP from external source
	otp, err := otpGetter()
	if err != nil {
		return nil, fmt.Errorf("otpGetter: %w", err)
	}

	// Step 7: Validate OTP
	aboutYouURL, err := c.validateOTP(ctx, otp)
	if err != nil {
		return nil, fmt.Errorf("validateOTP: %w", err)
	}

	// Step 7.5: Visit about-you
	if err := c.visitAboutYou(ctx, aboutYouURL); err != nil {
		return nil, fmt.Errorf("visitAboutYou: %w", err)
	}

	// Step 8: Create account
	callbackURL, err := c.createAccount(ctx, name, birthdate)
	if err != nil {
		return nil, fmt.Errorf("createAccount: %w", err)
	}

	// Step 9: OAuth callback
	sessionToken, err := c.oauthCallback(ctx, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("oauthCallback: %w", err)
	}

	// Step 10: Get access token
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("getAccessToken: %w", err)
	}

	// At least one credential must have been obtained, else the account is unusable.
	// (When egressing via the sidecar the session-token cookie lives in the sidecar's
	// server-side jar, so sessionToken may be empty here while accessToken is set.)
	if strings.TrimSpace(accessToken) == "" && strings.TrimSpace(sessionToken) == "" {
		return nil, fmt.Errorf("no credential obtained (access token and session token both empty — upstream likely challenged the flow)")
	}

	return &RegisterResult{
		SessionToken: sessionToken,
		AccessToken:  accessToken,
		Phone:        phone,
	}, nil
}

func (c *RegisterClient) visit(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", CHATGPT+"/auth/login", nil)
	req.Header.Set("User-Agent", UA)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	return nil
}

func (c *RegisterClient) getCSRF(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", CHATGPT+"/api/auth/csrf", nil)
	req.Header.Set("User-Agent", UA)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		CSRFToken string `json:"csrfToken"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	return data.CSRFToken, nil
}

func (c *RegisterClient) signin(ctx context.Context, phone, csrf string) (string, error) {
	encoded := strings.ReplaceAll(phone, "+", "%2B")
	params := url.Values{}
	params.Set("prompt", "login")
	params.Set("screen_hint", "login_or_signup")
	params.Set("login_hint", encoded)
	params.Set("ext-oai-did", c.deviceID)
	params.Set("auth_session_logging_id", c.deviceID)

	body := url.Values{}
	body.Set("callbackUrl", "/")
	body.Set("csrfToken", csrf)
	body.Set("json", "true")

	req, _ := http.NewRequestWithContext(ctx, "POST",
		CHATGPT+"/api/auth/signin/openai?"+params.Encode(),
		strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", CHATGPT+"/auth/login")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := readRegisterResponseBody(resp.Body)
	dbg("signin", resp.StatusCode, raw)
	var data struct {
		URL string `json:"url"`
	}
	json.Unmarshal(raw, &data)
	return data.URL, nil
}

func (c *RegisterClient) jumpToAuth(ctx context.Context, redirectURL string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", redirectURL, nil)
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", CHATGPT)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	return nil
}

func (c *RegisterClient) registerUser(ctx context.Context, phone, password string) (string, error) {
	st, _ := c.sentinel.Get(ctx, sentinel.FlowUsernamePasswordCreate)
	reqBody := map[string]string{"username": phone, "password": password}
	bodyBytes, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", AUTH+"/api/accounts/user/register", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/create-account/password")
	req.Header.Set("oai-device-id", c.deviceID)
	if st != nil && st.MainToken != "" {
		req.Header.Set("OpenAI-Sentinel-Token", st.MainToken)
		if st.SOToken != "" {
			req.Header.Set("OpenAI-Sentinel-SO-Token", st.SOToken)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := readRegisterResponseBody(resp.Body)
	dbg("registerUser", resp.StatusCode, raw)
	var data struct {
		ContinueURL string `json:"continue_url"`
	}
	json.Unmarshal(raw, &data)
	return data.ContinueURL, nil
}

func (c *RegisterClient) sendOTP(ctx context.Context, continueURL string) error {
	fullURL := continueURL
	if !strings.HasPrefix(continueURL, "http") {
		fullURL = AUTH + continueURL
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/create-account/password")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	return nil
}

func (c *RegisterClient) validateOTP(ctx context.Context, code string) (string, error) {
	st, _ := c.sentinel.Get(ctx, sentinel.FlowAuthorizeContinue)
	reqBody := map[string]string{"code": code}
	bodyBytes, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", AUTH+"/api/accounts/phone-otp/validate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/contact-verification")
	req.Header.Set("oai-device-id", c.deviceID)
	if st != nil && st.MainToken != "" {
		req.Header.Set("OpenAI-Sentinel-Token", st.MainToken)
		if st.SOToken != "" {
			req.Header.Set("OpenAI-Sentinel-SO-Token", st.SOToken)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := readRegisterResponseBody(resp.Body)
	dbg("validateOTP", resp.StatusCode, raw)
	var data struct {
		ContinueURL string `json:"continue_url"`
	}
	json.Unmarshal(raw, &data)
	return data.ContinueURL, nil
}

func (c *RegisterClient) visitAboutYou(ctx context.Context, continueURL string) error {
	fullURL := continueURL
	if !strings.HasPrefix(continueURL, "http") {
		fullURL = AUTH + continueURL
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/contact-verification")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	return nil
}

func (c *RegisterClient) createAccount(ctx context.Context, name, birthdate string) (string, error) {
	st, _ := c.sentinel.Get(ctx, sentinel.FlowOAuthCreateAccount)
	reqBody := map[string]string{"name": name, "birthdate": birthdate}
	bodyBytes, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", AUTH+"/api/accounts/create_account", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/about-you")
	req.Header.Set("oai-device-id", c.deviceID)
	if st != nil && st.MainToken != "" {
		req.Header.Set("OpenAI-Sentinel-Token", st.MainToken)
		if st.SOToken != "" {
			req.Header.Set("OpenAI-Sentinel-SO-Token", st.SOToken)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := readRegisterResponseBody(resp.Body)
	dbg("createAccount", resp.StatusCode, raw)
	var data struct {
		ContinueURL string `json:"continue_url"`
	}
	json.Unmarshal(raw, &data)
	return data.ContinueURL, nil
}

func (c *RegisterClient) oauthCallback(ctx context.Context, callbackURL string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", callbackURL, nil)
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__Secure-next-auth.session-token" {
			return cookie.Value, nil
		}
	}
	// Not an error: when egressing through the curl_cffi sidecar the session-token cookie
	// is retained in the sidecar's server-side jar (not surfaced as a Set-Cookie header to
	// this client), and getAccessToken will mint the bearer from it on the next call.
	return "", nil
}

func readRegisterResponseBody(body io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(body, registerResponseBodyLimit))
	return raw
}

func (c *RegisterClient) getAccessToken(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", CHATGPT+"/api/auth/session", nil)
	req.Header.Set("User-Agent", UA)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		AccessToken string `json:"accessToken"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	return data.AccessToken, nil
}
