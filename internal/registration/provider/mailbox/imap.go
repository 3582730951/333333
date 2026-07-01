package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"codex-account-pool/internal/supervisor"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

const imapTextPartBodyLimit = 256 * 1024

// IMAPProvider connects to generic IMAP servers (Gmail, Outlook, self-hosted) to receive
// verification codes. It does NOT create email accounts (the user must provide an existing
// email + credentials). Use for: Gmail App Passwords, Outlook, custom domains.
type IMAPProvider struct {
	host     string
	port     string
	email    string
	password string
	useTLS   bool
}

// NewIMAPProvider builds an IMAP mailbox provider. Credentials must be pre-configured
// (this provider does not create accounts, only reads mail).
//
// Config keys:
//
//	host: IMAP server (e.g. "imap.gmail.com")
//	port: IMAP port (default 993 for TLS, 143 for non-TLS)
//	email: full email address
//	password: account password (or App Password for Gmail)
//	use_tls: true (default) for IMAPS, false for plain IMAP
func NewIMAPProvider(config map[string]interface{}) *IMAPProvider {
	host := getString(config, "host", "")
	port := getString(config, "port", "993")
	email := getString(config, "email", "")
	password := getString(config, "password", "")
	useTLS := getBool(config, "use_tls", true)
	return &IMAPProvider{
		host:     host,
		port:     port,
		email:    email,
		password: password,
		useTLS:   useTLS,
	}
}

func (p *IMAPProvider) Name() string { return "imap" }
func (p *IMAPProvider) Type() string { return "mailbox" }

// CreateEmail returns the pre-configured email (IMAP providers don't create accounts).
// The mailboxID is empty (we poll INBOX directly).
func (p *IMAPProvider) CreateEmail(ctx context.Context) (string, string, string, error) {
	if p.email == "" || p.password == "" || p.host == "" {
		return "", "", "", fmt.Errorf("imap: email, password, and host are required")
	}
	// No actual creation; just return the configured email
	return p.email, p.password, "", nil
}

// WaitOTP polls INBOX for a verification code (6-digit regex). timeout is in seconds.
func (p *IMAPProvider) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	start := time.Now()
	codeRe := regexp.MustCompile(`(?:\D|^)(\d{6})(?:\D|$)`)
	seenUIDs := make(map[uint32]bool)

	for time.Since(start) < timeout {
		c, err := p.connect()
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		// Select INBOX
		_, err = c.Select("INBOX", false)
		if err != nil {
			c.Logout()
			time.Sleep(3 * time.Second)
			continue
		}
		// Search for UNSEEN messages
		criteria := imap.NewSearchCriteria()
		criteria.WithoutFlags = []string{imap.SeenFlag}
		uids, err := c.Search(criteria)
		if err != nil {
			c.Logout()
			time.Sleep(3 * time.Second)
			continue
		}
		if len(uids) == 0 {
			c.Logout()
			time.Sleep(3 * time.Second)
			continue
		}
		// Fetch new messages
		seqset := new(imap.SeqSet)
		for _, uid := range uids {
			if !seenUIDs[uid] {
				seqset.AddNum(uid)
				seenUIDs[uid] = true
			}
		}
		if seqset.Empty() {
			c.Logout()
			time.Sleep(3 * time.Second)
			continue
		}
		messages := make(chan *imap.Message, 10)
		done := make(chan error, 1)
		go func() {
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("imap-fetch", v)
					closeIMAPMessages(messages)
					done <- fmt.Errorf("imap fetch panic: %v", v)
				}
			}()
			done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchBody + "[]"}, messages)
		}()
		for msg := range messages {
			body := p.extractBody(msg)
			if matches := codeRe.FindStringSubmatch(body); len(matches) > 1 {
				c.Logout()
				return matches[1], nil
			}
		}
		if err := <-done; err != nil {
			c.Logout()
			return "", err
		}
		c.Logout()
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("timeout waiting for IMAP code (%v)", timeout)
}

func closeIMAPMessages(messages chan *imap.Message) {
	defer func() {
		if v := recover(); v != nil {
			supervisor.LogPanic("imap-close-messages", v)
		}
	}()
	close(messages)
}

func (p *IMAPProvider) DeleteEmail(ctx context.Context, mailboxID string) error {
	// IMAP providers don't delete the account (it's pre-configured)
	return nil
}

// ── internal helpers ──

func (p *IMAPProvider) connect() (*client.Client, error) {
	addr := fmt.Sprintf("%s:%s", p.host, p.port)
	var c *client.Client
	var err error
	if p.useTLS {
		c, err = client.DialTLS(addr, &tls.Config{InsecureSkipVerify: true})
	} else {
		c, err = client.Dial(addr)
	}
	if err != nil {
		return nil, err
	}
	if err := c.Login(p.email, p.password); err != nil {
		c.Logout()
		return nil, err
	}
	return c, nil
}

func (p *IMAPProvider) extractBody(msg *imap.Message) string {
	if msg == nil {
		return ""
	}
	for _, literal := range msg.Body {
		if literal == nil {
			continue
		}
		mr, err := mail.CreateReader(literal)
		if err != nil {
			continue
		}
		var parts []string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if strings.HasPrefix(part.Header.Get("Content-Type"), "text/") {
				b, _ := io.ReadAll(io.LimitReader(part.Body, imapTextPartBodyLimit))
				parts = append(parts, string(b))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return ""
}

func getString(m map[string]interface{}, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getBool(m map[string]interface{}, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}
