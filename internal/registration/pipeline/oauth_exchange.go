package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	registrationOAuthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	registrationOAuthRedirectURI = "http://localhost:1455/auth/callback"
	registrationOAuthTokenURL    = "https://auth.openai.com/oauth/token"
)

type registrationOAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

// exchangeRegistrationOAuthCode keeps token exchange in the trusted Go process so the
// browser worker never receives long-lived credentials. The request uses the same
// concrete registration egress as the browser and accepts only the pinned Codex OAuth
// redirect/client tuple.
func (p *Pipeline) exchangeRegistrationOAuthCode(
	ctx context.Context,
	req RegisterRequest,
	code, verifier, redirectURI string,
) (registrationOAuthTokens, error) {
	var tokens registrationOAuthTokens
	if strings.TrimSpace(code) == "" || len(strings.TrimSpace(verifier)) < 43 ||
		strings.TrimSpace(redirectURI) != registrationOAuthRedirectURI {
		return tokens, errors.New("node registrar returned an invalid OAuth result")
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {registrationOAuthClientID},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {registrationOAuthRedirectURI},
		"code_verifier": {strings.TrimSpace(verifier)},
	}
	httpReq, err := http.NewRequestWithContext(
		exchangeCtx,
		http.MethodPost,
		registrationOAuthTokenURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return tokens, errors.New("registration OAuth exchange request failed")
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	client, err := p.egressClient(exchangeCtx, req.EgressID)
	if err != nil {
		return tokens, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return tokens, errors.New("registration OAuth exchange transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return tokens, errors.New("registration OAuth exchange was rejected")
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 256<<10))
	if err := decoder.Decode(&tokens); err != nil {
		return tokens, errors.New("registration OAuth exchange returned an invalid response")
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return registrationOAuthTokens{}, errors.New("registration OAuth exchange returned no access token")
	}
	return tokens, nil
}
