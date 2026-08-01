package mailbox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

const (
	emailPoolMicrosoftTokenURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	emailPoolMicrosoftScope    = "https://outlook.office.com/.default"
	emailPoolIMAPAddress       = "outlook.office365.com:993"
	emailPoolMessageBodyLimit  = 256 * 1024
)

var (
	emailPoolOTPPattern   = regexp.MustCompile(`(?:^|[^0-9#])([0-9]{6})(?:[^0-9]|$)`)
	emailPoolHTMLPattern  = regexp.MustCompile(`<[^>]+>`)
	emailPoolAddressRegex = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
)

type emailPoolLease struct {
	account       storage.EmailAccount
	releaseStatus string
	releaseError  string
}

// EmailPoolProvider adapts historical Outlook/Hotmail rows to the current
// MailboxProvider contract. Secrets stay inside storage/provider memory and are
// never returned by an admin list endpoint or passed to registrar subprocesses.
type EmailPoolProvider struct {
	store       *storage.Store
	httpClient  *http.Client
	tokenURL    string
	imapAddress string

	mu     sync.Mutex
	leases map[string]*emailPoolLease
}

func NewEmailPoolProvider(store *storage.Store, httpClient *http.Client) *EmailPoolProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &EmailPoolProvider{
		store: store, httpClient: httpClient, tokenURL: emailPoolMicrosoftTokenURL,
		imapAddress: emailPoolIMAPAddress, leases: map[string]*emailPoolLease{},
	}
}

func (p *EmailPoolProvider) Name() string { return "email_pool" }
func (p *EmailPoolProvider) Type() string { return "mailbox" }

func (p *EmailPoolProvider) CreateEmail(ctx context.Context) (string, string, string, error) {
	if p == nil || p.store == nil {
		return "", "", "", errors.New("email pool is unavailable")
	}
	// Skip malformed legacy rows deterministically so one stale import cannot
	// block the valid rows that follow it.
	for attempt := 0; attempt < 8; attempt++ {
		account, err := p.store.ReserveEmailAccount(ctx, "")
		if err != nil {
			return "", "", "", err
		}
		if strings.Count(strings.TrimSpace(account.Email), "@") != 1 ||
			strings.TrimSpace(account.ClientID) == "" || strings.TrimSpace(account.RefreshToken) == "" {
			_ = p.store.ReleaseEmailAccount(context.WithoutCancel(ctx), account.ID, "error", "mailbox OAuth credentials are incomplete")
			continue
		}
		lease := &emailPoolLease{account: account, releaseStatus: "idle"}
		p.mu.Lock()
		p.leases[account.ID] = lease
		p.mu.Unlock()
		return strings.TrimSpace(account.Email), account.Password, account.ID, nil
	}
	return "", "", "", errors.New("email pool contains no usable mailbox row")
}

func (p *EmailPoolProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	lease := p.lease(mailboxID)
	if lease == nil {
		return "", errors.New("email pool lease was not found")
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	accessToken, err := p.exchangeMicrosoftToken(waitCtx, lease.account)
	if err != nil {
		p.markLease(mailboxID, "error", "mailbox OAuth token exchange failed")
		return "", err
	}
	code, err := p.waitOutlookOTP(waitCtx, lease.account.Email, accessToken)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			p.markLease(mailboxID, "error", "mailbox IMAP authentication failed")
		}
		return "", err
	}
	p.markLease(mailboxID, "idle", "")
	return code, nil
}

func (p *EmailPoolProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	if p == nil || p.store == nil || strings.TrimSpace(mailboxID) == "" {
		return nil
	}
	p.mu.Lock()
	lease := p.leases[mailboxID]
	delete(p.leases, mailboxID)
	p.mu.Unlock()
	if lease == nil {
		// Idempotent cleanup: only release a row still owned by a stale lease.
		account, found, err := p.store.GetEmailAccount(ctx, mailboxID)
		if err != nil || !found || account.Status != "in_use" {
			return err
		}
		return p.store.ReleaseEmailAccount(ctx, mailboxID, "idle", "")
	}
	status := strings.TrimSpace(lease.releaseStatus)
	if status == "" {
		status = "idle"
	}
	return p.store.ReleaseEmailAccount(ctx, mailboxID, status, lease.releaseError)
}

func (p *EmailPoolProvider) lease(id string) *emailPoolLease {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leases[strings.TrimSpace(id)]
}

func (p *EmailPoolProvider) markLease(id, status, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lease := p.leases[strings.TrimSpace(id)]; lease != nil {
		lease.releaseStatus = status
		lease.releaseError = message
	}
}

func (p *EmailPoolProvider) exchangeMicrosoftToken(ctx context.Context, account storage.EmailAccount) (string, error) {
	form := url.Values{
		"client_id": {account.ClientID}, "grant_type": {"refresh_token"},
		"refresh_token": {account.RefreshToken}, "scope": {emailPoolMicrosoftScope},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("Microsoft mailbox token request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil {
		return "", err
	}
	if len(body) > 64*1024 {
		return "", errors.New("Microsoft mailbox token response is too large")
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Microsoft mailbox token request returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("Microsoft mailbox token response is incompatible")
	}
	return strings.TrimSpace(payload.AccessToken), nil
}

func (p *EmailPoolProvider) waitOutlookOTP(ctx context.Context, address, accessToken string) (string, error) {
	seen := map[uint32]bool{}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		code, authFailure, err := p.pollOutlookOTP(ctx, address, accessToken, seen)
		if code != "" {
			return code, nil
		}
		if authFailure {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *EmailPoolProvider) pollOutlookOTP(ctx context.Context, address, accessToken string, seen map[uint32]bool) (string, bool, error) {
	c, err := p.connectOutlook(ctx, address, accessToken)
	if err != nil {
		return "", true, fmt.Errorf("Outlook IMAP authentication failed: %w", err)
	}
	defer func() { _ = c.Logout() }()
	if _, err := c.Select("INBOX", false); err != nil {
		return "", false, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	criteria.Since = time.Now().Add(-30 * time.Minute)
	ids, err := c.Search(criteria)
	if err != nil {
		return "", false, err
	}
	for index := len(ids) - 1; index >= 0 && index >= len(ids)-50; index-- {
		id := ids[index]
		if seen[id] {
			continue
		}
		seen[id] = true
		code, err := fetchEmailPoolOTP(c, id, address)
		if err != nil {
			continue
		}
		if code != "" {
			return code, false, nil
		}
	}
	return "", false, nil
}

func (p *EmailPoolProvider) connectOutlook(ctx context.Context, address, accessToken string) (*client.Client, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	c, err := client.DialWithDialerTLS(dialer, p.imapAddress, &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: strings.Split(p.imapAddress, ":")[0],
	})
	if err != nil {
		return nil, err
	}
	c.Timeout = 15 * time.Second
	if err := c.Authenticate(&emailPoolXOAUTH2{username: address, token: accessToken}); err != nil {
		_ = c.Logout()
		return nil, err
	}
	return c, nil
}

type emailPoolXOAUTH2 struct {
	username string
	token    string
}

func (c *emailPoolXOAUTH2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)), nil
}

func (c *emailPoolXOAUTH2) Next([]byte) ([]byte, error) { return nil, nil }

func fetchEmailPoolOTP(c *client.Client, sequence uint32, targetAddress string) (string, error) {
	set := new(imap.SeqSet)
	set.AddNum(sequence)
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 1)
	if err := c.Fetch(set, []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}, messages); err != nil {
		return "", err
	}
	var message *imap.Message
	for candidate := range messages {
		message = candidate
	}
	if message == nil {
		return "", nil
	}
	if message.Envelope != nil && len(message.Envelope.To) > 0 {
		matched := false
		for _, recipient := range message.Envelope.To {
			if strings.EqualFold(strings.TrimSpace(recipient.Address()), strings.TrimSpace(targetAddress)) {
				matched = true
				break
			}
		}
		if !matched {
			return "", nil
		}
	}
	text := ""
	if message.Envelope != nil {
		text = message.Envelope.Subject + " "
	}
	text += emailPoolMessageText(message)
	text = emailPoolHTMLPattern.ReplaceAllString(text, " ")
	text = emailPoolAddressRegex.ReplaceAllString(text, " ")
	if len(text) > 64*1024 {
		text = text[:64*1024]
	}
	match := emailPoolOTPPattern.FindStringSubmatch(text)
	if len(match) > 1 {
		return match[1], nil
	}
	return "", nil
}

func emailPoolMessageText(message *imap.Message) string {
	if message == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	for _, literal := range message.Body {
		if literal == nil {
			continue
		}
		reader, err := mail.CreateReader(io.LimitReader(literal, emailPoolMessageBodyLimit))
		if err != nil {
			continue
		}
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			if _, ok := part.Header.(*mail.InlineHeader); !ok {
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(part.Body, emailPoolMessageBodyLimit))
			parts = append(parts, string(body))
		}
	}
	return strings.Join(parts, " ")
}

// EmailPoolFingerprint returns a secret-free value suitable for canary
// invalidation whenever usable mailbox inventory changes.
func EmailPoolFingerprint(ctx context.Context, store *storage.Store) (string, int, error) {
	if store == nil {
		return "", 0, nil
	}
	var count int
	var updated int64
	err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*),COALESCE(MAX(updated_at),0) FROM email_pool
WHERE status='idle' AND TRIM(email)<>'' AND TRIM(client_id)<>'' AND TRIM(refresh_token)<>''`).Scan(&count, &updated)
	if err != nil || count == 0 {
		return "", count, err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", count, updated)))
	return hex.EncodeToString(sum[:]), count, nil
}
