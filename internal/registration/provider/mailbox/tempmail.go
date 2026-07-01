// Package mailbox implements mailbox provider adapters
package mailbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// TempMailLolProvider implements tempmail.lol API
type TempMailLolProvider struct {
	httpClient *http.Client
}

// NewTempMailLolProvider creates a tempmail.lol provider
func NewTempMailLolProvider(httpClient *http.Client) *TempMailLolProvider {
	return &TempMailLolProvider{httpClient: httpClient}
}

func (p *TempMailLolProvider) Name() string { return "tempmail_lol" }
func (p *TempMailLolProvider) Type() string { return "mailbox" }

// CreateEmail generates a random temp email
func (p *TempMailLolProvider) CreateEmail(ctx context.Context) (string, string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.tempmail.lol/generate", nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	var data struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", "", err
	}
	return data.Address, "", data.Token, nil
}

// WaitOTP polls for verification code
func (p *TempMailLolProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	otpRegex := regexp.MustCompile(`\b\d{6}\b`)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for email")
			}

			params := url.Values{}
			params.Set("token", mailboxID)
			req, _ := http.NewRequestWithContext(ctx, "GET",
				"https://api.tempmail.lol/auth/"+mailboxID, nil)
			resp, err := p.httpClient.Do(req)
			if err != nil {
				continue
			}

			var data struct {
				Email []struct {
					Subject string `json:"subject"`
					Body    string `json:"body"`
				} `json:"email"`
			}
			json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()

			for _, email := range data.Email {
				if matches := otpRegex.FindString(email.Body); matches != "" {
					return matches, nil
				}
			}
		}
	}
}

// DeleteEmail is a no-op for tempmail.lol
func (p *TempMailLolProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	return nil
}
