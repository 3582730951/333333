// Package mailbox — Cloudflare temp-email provider.
package mailbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
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
	providerKey string
	apiURL      string
	adminToken  string
	domain      string
	httpClient  *http.Client
}

// NewCloudflareTempEmailProvider builds the provider. adminToken is optional (set when
// the worker restricts address creation to admins); domain is the worker's mail domain.
func NewCloudflareTempEmailProvider(apiURL, adminToken, domain string, httpClient *http.Client) *CloudflareTempEmailProvider {
	return NewNamedCloudflareTempEmailProvider("cloudflare", apiURL, adminToken, domain, httpClient)
}

func NewNamedCloudflareTempEmailProvider(providerKey, apiURL, adminToken, domain string, httpClient *http.Client) *CloudflareTempEmailProvider {
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &CloudflareTempEmailProvider{
		providerKey: strings.TrimSpace(providerKey),
		apiURL:      apiURL,
		adminToken:  strings.TrimSpace(adminToken),
		domain:      strings.TrimSpace(strings.TrimPrefix(domain, "@")),
		httpClient:  newGuardedMailboxHTTPClient(httpClient, apiURL),
	}
}

func (p *CloudflareTempEmailProvider) Name() string {
	if strings.TrimSpace(p.providerKey) == "" {
		return "cloudflare"
	}
	return p.providerKey
}
func (p *CloudflareTempEmailProvider) Type() string { return "mailbox" }

func (p *CloudflareTempEmailProvider) MailboxDomains() []string {
	if p.domain != "" {
		return []string{p.domain}
	}
	return nil
}

func (p *CloudflareTempEmailProvider) MailboxUsesCustomDomain() bool { return true }

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
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, mailboxHTTPResponseBodyLimit+1))
	if readErr != nil {
		return "", "", "", fmt.Errorf("cloudflare: read new_address: %w", readErr)
	}
	if len(raw) > mailboxHTTPResponseBodyLimit {
		return "", "", "", fmt.Errorf("cloudflare: new_address response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("cloudflare: new_address returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		JWT     string `json:"jwt"`
		Address string `json:"address"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
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
	if p.domain != "" {
		index := strings.LastIndexByte(email, '@')
		if index < 0 || !strings.EqualFold(strings.TrimSpace(email[index+1:]), p.domain) {
			return "", "", "", fmt.Errorf("cloudflare: returned address is outside configured domain")
		}
	}
	return email, "", data.JWT, nil
}

var sixDigit = regexp.MustCompile(`\b(\d{6})\b`)

// WaitOTP polls the mailbox (Bearer = the jwt mailboxID) for a 6-digit code.
func (p *CloudflareTempEmailProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(mailboxID) == "" {
		return "", fmt.Errorf("cloudflare: mailbox token is required")
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	seen := map[string]bool{}
	for {
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("cloudflare: timeout waiting for email: %w", waitCtx.Err())
		case <-timer.C:
			req, _ := http.NewRequestWithContext(waitCtx, http.MethodGet, p.apiURL+"/api/mails?limit=20&offset=0", nil)
			req.Header.Set("Authorization", "Bearer "+mailboxID)
			resp, err := p.httpClient.Do(req)
			if err != nil {
				timer.Reset(3 * time.Second)
				continue
			}
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, mailboxHTTPResponseBodyLimit+1))
			status := resp.StatusCode
			resp.Body.Close()
			if readErr != nil || len(raw) > mailboxHTTPResponseBodyLimit || status < 200 || status >= 300 {
				timer.Reset(3 * time.Second)
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
			if json.Unmarshal(raw, &data) != nil {
				timer.Reset(3 * time.Second)
				continue
			}
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
			timer.Reset(3 * time.Second)
		}
	}
}

// DeleteEmail is a no-op (temp addresses expire on their own).
func (p *CloudflareTempEmailProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	return nil
}
