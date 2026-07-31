package mailbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MailTMProvider implements the public mail.tm-compatible accounts/token/messages
// contract. mail.tm and mail.gw can therefore share one concurrency-safe adapter.
type MailTMProvider struct {
	name       string
	apiURL     string
	domain     string
	httpClient *http.Client
}

type mailTMLease struct {
	Email     string `json:"email"`
	Token     string `json:"token"`
	AccountID string `json:"account_id,omitempty"`
}

func NewMailTMProvider(name, apiURL, domain string, httpClient *http.Client) *MailTMProvider {
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &MailTMProvider{
		name: strings.TrimSpace(name), apiURL: apiURL,
		domain: strings.TrimSpace(strings.TrimPrefix(domain, "@")),
		httpClient: newGuardedMailboxHTTPClient(httpClient, apiURL),
	}
}

func (p *MailTMProvider) Name() string {
	if p.name == "" {
		return "mailtm"
	}
	return p.name
}

func (p *MailTMProvider) Type() string { return "mailbox" }

func (p *MailTMProvider) MailboxDomains() []string {
	if p.domain == "" {
		return nil
	}
	return []string{p.domain}
}

func (p *MailTMProvider) MailboxUsesCustomDomain() bool { return false }

func (p *MailTMProvider) requestJSON(
	ctx context.Context,
	method, path, bearer string,
	body interface{},
	out interface{},
) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.apiURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, mailboxHTTPResponseBodyLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > mailboxHTTPResponseBodyLimit {
		return errors.New("mail.tm response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mail.tm endpoint returned HTTP %d", resp.StatusCode)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mail.tm response contract: %w", err)
	}
	return nil
}

func collectionItems(payload map[string]interface{}) []interface{} {
	for _, key := range []string{"hydra:member", "member", "data", "results", "messages"} {
		if items, ok := payload[key].([]interface{}); ok {
			return items
		}
	}
	return nil
}

func (p *MailTMProvider) resolveDomain(ctx context.Context) (string, error) {
	if p.domain != "" {
		return p.domain, nil
	}
	payload := map[string]interface{}{}
	if err := p.requestJSON(ctx, http.MethodGet, "/domains", "", nil, &payload); err != nil {
		return "", err
	}
	for _, raw := range collectionItems(payload) {
		item, _ := raw.(map[string]interface{})
		domain := strings.TrimSpace(fmt.Sprint(item["domain"]))
		if domain == "" || domain == "<nil>" {
			continue
		}
		if active, exists := item["isActive"].(bool); exists && !active {
			continue
		}
		return domain, nil
	}
	return "", errors.New("mail.tm returned no active domain")
}

func (p *MailTMProvider) CreateEmail(ctx context.Context) (string, string, string, error) {
	if p.apiURL == "" {
		return "", "", "", errors.New("mail.tm api_url is required")
	}
	domain, err := p.resolveDomain(ctx)
	if err != nil {
		return "", "", "", err
	}
	email := randomLocalPart(12) + "@" + domain
	password := randomLocalPart(18) + "A9!"
	account := map[string]interface{}{}
	if err := p.requestJSON(ctx, http.MethodPost, "/accounts", "", map[string]string{
		"address": email, "password": password,
	}, &account); err != nil {
		return "", "", "", err
	}
	tokenPayload := map[string]interface{}{}
	if err := p.requestJSON(ctx, http.MethodPost, "/token", "", map[string]string{
		"address": email, "password": password,
	}, &tokenPayload); err != nil {
		return "", "", "", err
	}
	token := strings.TrimSpace(fmt.Sprint(tokenPayload["token"]))
	if token == "" || token == "<nil>" {
		return "", "", "", errors.New("mail.tm token response is empty")
	}
	leaseRaw, _ := json.Marshal(mailTMLease{
		Email: email, Token: token, AccountID: strings.TrimSpace(fmt.Sprint(account["id"])),
	})
	return email, password, base64.RawURLEncoding.EncodeToString(leaseRaw), nil
}

func decodeMailTMLease(mailboxID string) (mailTMLease, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(mailboxID))
	if err != nil {
		return mailTMLease{}, errors.New("mail.tm mailbox reference is invalid")
	}
	var lease mailTMLease
	if json.Unmarshal(raw, &lease) != nil || lease.Token == "" {
		return mailTMLease{}, errors.New("mail.tm mailbox reference is invalid")
	}
	return lease, nil
}

var mailTMCodePattern = regexp.MustCompile(`(?i)(?:verification\s+code|code\s+is|验证码|代码为)?\D{0,24}(\d{6})`)

func mailTMText(item map[string]interface{}) string {
	parts := make([]string, 0, 8)
	for _, key := range []string{"subject", "intro", "text", "html", "body", "content", "preview"} {
		switch value := item[key].(type) {
		case string:
			parts = append(parts, value)
		case []interface{}:
			for _, part := range value {
				parts = append(parts, fmt.Sprint(part))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func (p *MailTMProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	lease, err := decodeMailTMLease(mailboxID)
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("mail.tm timeout waiting for email: %w", waitCtx.Err())
		case <-timer.C:
			payload := map[string]interface{}{}
			if err := p.requestJSON(waitCtx, http.MethodGet, "/messages", lease.Token, nil, &payload); err != nil {
				timer.Reset(3 * time.Second)
				continue
			}
			for _, raw := range collectionItems(payload) {
				item, _ := raw.(map[string]interface{})
				id := strings.TrimSpace(fmt.Sprint(item["id"]))
				if id == "" || id == "<nil>" || seen[id] {
					continue
				}
				seen[id] = true
				content := mailTMText(item)
				if detail := strings.TrimPrefix(strings.TrimSpace(fmt.Sprint(item["@id"])), "/messages/"); content == "" || len(content) < 24 {
					if detail == "" || detail == "<nil>" {
						detail = id
					}
					detailPayload := map[string]interface{}{}
					if p.requestJSON(waitCtx, http.MethodGet, "/messages/"+url.PathEscape(detail), lease.Token, nil, &detailPayload) == nil {
						content += "\n" + mailTMText(detailPayload)
					}
				}
				if match := mailTMCodePattern.FindStringSubmatch(content); len(match) > 1 {
					return match[1], nil
				}
			}
			timer.Reset(3 * time.Second)
		}
	}
}

func (p *MailTMProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	lease, err := decodeMailTMLease(mailboxID)
	if err != nil || lease.AccountID == "" || lease.AccountID == "<nil>" {
		return nil
	}
	return p.requestJSON(ctx, http.MethodDelete, "/accounts/"+url.PathEscape(lease.AccountID), lease.Token, nil, nil)
}
