// Package mailbox — Cloudflare temp-email provider.
package mailbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// CloudflareTempEmailProvider talks to a self-hosted cloudflare_temp_email worker
// (dreamhunter2333/cloudflare_temp_email, which MoeMail/Freemail are based on):
//
//	create: POST {api}/admin/new_address  (x-admin-auth: <admin>)  -> {jwt}
//	        POST {api}/api/new_address     (when no admin token)    -> {jwt}
//	list:   GET  {api}/api/mails?limit=20  (Authorization: Bearer <jwt>) -> {results:[{id,raw,...}]}
//
// The per-mailbox jwt is returned as the mailboxID (stateless) so a single shared
// instance can serve concurrent registrations.
type CloudflareTempEmailProvider struct {
	apiURL     string
	adminToken string
	domain     string
	httpClient *http.Client
}

// NewCloudflareTempEmailProvider builds the provider. adminToken is optional (set when
// the worker restricts address creation to admins); domain is the worker's mail domain.
func NewCloudflareTempEmailProvider(apiURL, adminToken, domain string, httpClient *http.Client) *CloudflareTempEmailProvider {
	return &CloudflareTempEmailProvider{
		apiURL:     strings.TrimRight(strings.TrimSpace(apiURL), "/"),
		adminToken: strings.TrimSpace(adminToken),
		domain:     strings.TrimSpace(strings.TrimPrefix(domain, "@")),
		httpClient: httpClient,
	}
}

func (p *CloudflareTempEmailProvider) Name() string { return "cloudflare" }
func (p *CloudflareTempEmailProvider) Type() string { return "mailbox" }

func randomLocalPart(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[idx.Int64()]
	}
	return string(b)
}

// CreateEmail provisions a new address and returns (email, "", jwt).
func (p *CloudflareTempEmailProvider) CreateEmail(ctx context.Context) (string, string, string, error) {
	if p.apiURL == "" {
		return "", "", "", fmt.Errorf("cloudflare: api_url is required")
	}
	name := randomLocalPart(12)
	payload, _ := json.Marshal(map[string]interface{}{
		"enablePrefix": true,
		"name":         name,
		"domain":       p.domain,
	})
	path := "/api/new_address"
	if p.adminToken != "" {
		path = "/admin/new_address"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+path, bytes.NewReader(payload))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.adminToken != "" {
		req.Header.Set("x-admin-auth", p.adminToken)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	var data struct {
		JWT     string `json:"jwt"`
		Address string `json:"address"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", "", fmt.Errorf("cloudflare: decode new_address: %w", err)
	}
	if data.JWT == "" {
		return "", "", "", fmt.Errorf("cloudflare: new_address returned no jwt (status %d)", resp.StatusCode)
	}
	email := data.Address
	if email == "" {
		local := data.Name
		if local == "" {
			local = name
		}
		email = local + "@" + p.domain
	}
	return email, "", data.JWT, nil
}

var sixDigit = regexp.MustCompile(`\b(\d{6})\b`)

// WaitOTP polls the mailbox (Bearer = the jwt mailboxID) for a 6-digit code.
func (p *CloudflareTempEmailProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	seen := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for email")
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL+"/api/mails?limit=20&offset=0", nil)
			req.Header.Set("Authorization", "Bearer "+mailboxID)
			resp, err := p.httpClient.Do(req)
			if err != nil {
				continue
			}
			var data struct {
				Results []struct {
					ID      interface{} `json:"id"`
					Subject string      `json:"subject"`
					Raw     string      `json:"raw"`
					Text    string      `json:"text"`
					Message string      `json:"message"`
				} `json:"results"`
			}
			json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			for _, m := range data.Results {
				id := fmt.Sprintf("%v", m.ID)
				if seen[id] {
					continue
				}
				seen[id] = true
				body := m.Subject + "\n" + m.Raw + "\n" + m.Text + "\n" + m.Message
				if code := sixDigit.FindStringSubmatch(body); len(code) > 1 {
					return code[1], nil
				}
			}
		}
	}
}

// DeleteEmail is a no-op (temp addresses expire on their own).
func (p *CloudflareTempEmailProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	return nil
}
