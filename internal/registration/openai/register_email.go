package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"codex-account-pool/internal/registration/sentinel"
)

// RegisterEmail runs the email-based ChatGPT signup flow. It reuses the phone flow's
// session-establishment and account-creation steps (visit/csrf/jumpToAuth/registerUser/
// createAccount/oauthCallback/getAccessToken) and swaps the verification step to email
// OTP: GET /api/accounts/email-otp/send → poll the mailbox via otpGetter → POST
// /api/accounts/email-otp/validate {"code":...}. Endpoints mirror the reference
// (aBaiAutoplus platforms/chatgpt/constants.py OPENAI_API_ENDPOINTS).
func (c *RegisterClient) RegisterEmail(ctx context.Context, email, password, name, birthdate string, otpGetter func() (string, error)) (*RegisterResult, error) {
	if err := c.visit(ctx); err != nil {
		return nil, fmt.Errorf("visit: %w", err)
	}
	csrf, err := c.getCSRF(ctx)
	if err != nil {
		return nil, fmt.Errorf("csrf: %w", err)
	}
	redirectURL, err := c.signinEmail(ctx, email, csrf)
	if err != nil {
		return nil, fmt.Errorf("signin: %w", err)
	}
	if err := c.jumpToAuth(ctx, redirectURL); err != nil {
		return nil, fmt.Errorf("jumpToAuth: %w", err)
	}
	if _, err := c.registerUser(ctx, email, password); err != nil {
		return nil, fmt.Errorf("registerUser: %w", err)
	}
	if err := c.sendEmailOTP(ctx); err != nil {
		return nil, fmt.Errorf("sendEmailOTP: %w", err)
	}
	otp, err := otpGetter()
	if err != nil {
		return nil, fmt.Errorf("otpGetter: %w", err)
	}
	if err := c.validateEmailOTP(ctx, otp); err != nil {
		return nil, fmt.Errorf("validateEmailOTP: %w", err)
	}
	callbackURL, err := c.createAccount(ctx, name, birthdate)
	if err != nil {
		return nil, fmt.Errorf("createAccount: %w", err)
	}
	sessionToken, err := c.oauthCallback(ctx, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("oauthCallback: %w", err)
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("getAccessToken: %w", err)
	}
	return &RegisterResult{SessionToken: sessionToken, AccessToken: accessToken, Email: email}, nil
}

// signinEmail initiates the OAuth signup with an email login hint (vs the phone hint).
func (c *RegisterClient) signinEmail(ctx context.Context, email, csrf string) (string, error) {
	params := url.Values{}
	params.Set("prompt", "login")
	params.Set("screen_hint", "signup")
	params.Set("login_hint", email)
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
	var data struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	return data.URL, nil
}

// sendEmailOTP triggers OpenAI to email the verification code.
func (c *RegisterClient) sendEmailOTP(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", AUTH+"/api/accounts/email-otp/send", nil)
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/email-verification")
	req.Header.Set("oai-device-id", c.deviceID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, registerResponseBodyLimit))
	return nil
}

// validateEmailOTP submits the 6-digit code pulled from the mailbox.
func (c *RegisterClient) validateEmailOTP(ctx context.Context, code string) error {
	st, _ := c.sentinel.Get(ctx, sentinel.FlowAuthorizeContinue)
	bodyBytes, _ := json.Marshal(map[string]string{"code": code})
	req, _ := http.NewRequestWithContext(ctx, "POST", AUTH+"/api/accounts/email-otp/validate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", AUTH+"/email-verification")
	req.Header.Set("oai-device-id", c.deviceID)
	if st != nil && st.MainToken != "" {
		req.Header.Set("OpenAI-Sentinel-Token", st.MainToken)
		if st.SOToken != "" {
			req.Header.Set("OpenAI-Sentinel-SO-Token", st.SOToken)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("email-otp validate failed: %d %s", resp.StatusCode, string(snippet))
	}
	return nil
}
