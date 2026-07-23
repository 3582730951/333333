package registration

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

const (
	msTokenURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	msIMAPScope = "https://outlook.office.com/.default"
	imapHost    = "outlook.office365.com"
	imapPort    = "993"
	otpTimeout  = 180 * time.Second
	otpPollInterval = 5 * time.Second
)

// imapTokenCache caches Microsoft OAuth2 access tokens per email.
var (
	imapTokenCache   = map[string]*imapTokenEntry{}
	imapTokenCacheMu sync.Mutex
)

type imapTokenEntry struct {
	AccessToken string
	ExpiresAt   time.Time
}

// otpCodeRE matches a standalone 6-digit code.
var otpCodeRE = regexp.MustCompile(`(?:^|[^\d#])(\d{6})(?:\D|$)`)

// GetIMAPAccessToken exchanges a Microsoft refresh_token for an IMAP access token
// via the Microsoft OAuth2 token endpoint.
func GetIMAPAccessToken(ctx context.Context, proxyURL, clientID, refreshToken string) (string, error) {
	imapTokenCacheMu.Lock()
	if entry, ok := imapTokenCache[refreshToken[:min(20, len(refreshToken))]]; ok && time.Now().Before(entry.ExpiresAt) {
		imapTokenCacheMu.Unlock()
		return entry.AccessToken, nil
	}
	imapTokenCacheMu.Unlock()

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", msIMAPScope)

	var httpClient *http.Client
	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return "", fmt.Errorf("invalid proxy URL: %w", err)
		}
		httpClient = &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
			Timeout:   30 * time.Second,
		}
	} else {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, msTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Microsoft token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Microsoft token endpoint returned HTTP %d: %s", resp.StatusCode, string(body[:min(500, len(body))]))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse Microsoft token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("Microsoft token response missing access_token")
	}

	// Cache the token
	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if tokenResp.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(1 * time.Hour)
	}
	imapTokenCacheMu.Lock()
	imapTokenCache[refreshToken[:min(20, len(refreshToken))]] = &imapTokenEntry{
		AccessToken: tokenResp.AccessToken,
		ExpiresAt:   expiresAt,
	}
	imapTokenCacheMu.Unlock()

	return tokenResp.AccessToken, nil
}

// IMAPOTPResult holds the result of waiting for an OTP code via IMAP.
type IMAPOTPResult struct {
	Code    string
	Message string // subject + body snippet for debugging
}

// WaitForOTP connects to Outlook IMAP via XOAUTH2 and polls for a 6-digit OTP code
// addressed to targetEmail. Returns the code when found, or an error on timeout.
// beforeIDs should contain message IDs already seen before the OTP was requested.
func WaitForOTP(ctx context.Context, accessToken, targetEmail string, beforeIDs map[string]bool, timeout time.Duration) (*IMAPOTPResult, error) {
	if timeout <= 0 {
		timeout = otpTimeout
	}

	deadline := time.Now().Add(timeout)

	// Connect to IMAP
	tlsConfig := &tls.Config{ServerName: imapHost}
	c, err := client.DialTLS(imapHost+":"+imapPort, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("IMAP dial failed: %w", err)
	}
	defer c.Logout()

	// XOAUTH2 authentication
	saslClient := &xoauth2Client{username: targetEmail, token: accessToken}
	if err := c.Authenticate(saslClient); err != nil {
		return nil, fmt.Errorf("IMAP XOAUTH2 auth failed: %w", err)
	}

	// Select INBOX
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return nil, fmt.Errorf("IMAP select INBOX failed: %w", err)
	}
	_ = mbox

	if beforeIDs == nil {
		beforeIDs = map[string]bool{}
	}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Search for recent messages
		criteria := imap.NewSearchCriteria()
		criteria.WithoutFlags = []string{imap.SeenFlag}
		ids, err := c.Search(criteria)
		if err != nil {
			time.Sleep(otpPollInterval)
			continue
		}

		// Check messages in reverse order (newest first)
		for i := len(ids) - 1; i >= 0 && i >= len(ids)-50; i-- {
			msgID := fmt.Sprintf("%d", ids[i])
			if beforeIDs[msgID] {
				continue
			}
			beforeIDs[msgID] = true

			code, subject, bodySnippet, found := fetchAndExtractOTP(c, ids[i], targetEmail)
			if found {
				return &IMAPOTPResult{
					Code:    code,
					Message: fmt.Sprintf("Subject: %s | Body: %s", subject, bodySnippet),
				}, nil
			}
		}

		time.Sleep(otpPollInterval)
	}

	return nil, fmt.Errorf("OTP timeout after %v for %s", timeout, targetEmail)
}

func fetchAndExtractOTP(c *client.Client, seqNum uint32, targetEmail string) (code, subject, bodySnippet string, found bool) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(seqNum)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}

	messages := make(chan *imap.Message, 1)
	if err := c.Fetch(seqSet, items, messages); err != nil {
		return "", "", "", false
	}

	msg := <-messages
	if msg == nil {
		return "", "", "", false
	}

	// Check recipient matches
	if msg.Envelope != nil {
		subject = msg.Envelope.Subject
		matched := false
		for _, to := range msg.Envelope.To {
			if strings.Contains(strings.ToLower(to.Address()), strings.ToLower(targetEmail)) {
				matched = true
				break
			}
		}
		if !matched {
			return "", "", "", false
		}
	}

	// Extract body text
	bodySnippet = extractMessageBody(msg)

	// Search for 6-digit code
	combined := subject + " " + bodySnippet
	// Remove HTML tags
	combined = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(combined, " ")
	// Remove email addresses
	combined = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`).ReplaceAllString(combined, " ")
	// Extract first 1000 chars for scanning
	if len(combined) > 1000 {
		combined = combined[:1000]
	}

	m := otpCodeRE.FindStringSubmatch(combined)
	if m != nil {
		return m[1], subject, bodySnippet[:min(200, len(bodySnippet))], true
	}

	return "", "", "", false
}

func extractMessageBody(msg *imap.Message) string {
	if msg == nil {
		return ""
	}
	for _, literal := range msg.Body {
		bodyBytes, err := io.ReadAll(literal)
		if err != nil {
			continue
		}
		bodyStr := string(bodyBytes)
		// Try to parse as MIME message
		mr, err := mail.CreateReader(strings.NewReader(bodyStr))
		if err != nil {
			// Plain text body
			return bodyStr
		}
		var parts []string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			switch p.Header.(type) {
			case *mail.InlineHeader:
				b, _ := io.ReadAll(p.Body)
				parts = append(parts, string(b))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// xoauth2Client implements the sasl.Client interface for XOAUTH2.
type xoauth2Client struct {
	username string
	token    string
}

func (c *xoauth2Client) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)), nil
}

func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return nil, nil
}
