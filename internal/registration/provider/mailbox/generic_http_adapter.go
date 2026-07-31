package mailbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// GenericHTTPAdapter wraps GenericHTTP into the MailboxProvider interface (which expects
// CreateEmail → (email, password, mailboxID, error) but GenericHTTP only generates email
// addresses, not passwords). Password is always empty for HTTP mailbox APIs.
type GenericHTTPAdapter struct {
	pipeline map[string]interface{}
	settings map[string]interface{}
	proxyURL string
	name     string
	mu       sync.Mutex
	sessions map[string]genericMailboxSession
}

type genericMailboxSession struct {
	inner     *GenericHTTP
	email     string
	accountID string
}

// NewGenericHTTPAdapter builds the adapter. name is the provider key (outlook_email,
// duckmail, tempmail_lol) used for Name().
func NewGenericHTTPAdapter(pipeline, settings map[string]interface{}, proxyURL, name string) (*GenericHTTPAdapter, error) {
	// Validate the pipeline once, but create an isolated GenericHTTP instance for
	// every mailbox lease. GenericHTTP owns mutable auth/extraction variables and
	// sharing one instance allowed concurrent jobs to poll each other's inboxes.
	if _, err := NewGenericHTTP(pipeline, settings, proxyURL); err != nil {
		return nil, err
	}
	return &GenericHTTPAdapter{
		pipeline: copyMap(pipeline),
		settings: copyMap(settings),
		proxyURL: proxyURL,
		name:     strings.TrimSpace(name),
		sessions: make(map[string]genericMailboxSession),
	}, nil
}

func newGenericMailboxLeaseID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err == nil {
		return "gmb_" + hex.EncodeToString(raw)
	}
	return fmt.Sprintf("gmb_%d", time.Now().UnixNano())
}

func (a *GenericHTTPAdapter) newSession() (*GenericHTTP, error) {
	g, err := NewGenericHTTP(a.pipeline, a.settings, a.proxyURL)
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (a *GenericHTTPAdapter) Name() string { return a.name }
func (a *GenericHTTPAdapter) Type() string { return "mailbox" }

func (a *GenericHTTPAdapter) CreateEmail(ctx context.Context) (string, string, string, error) {
	g, err := a.newSession()
	if err != nil {
		return "", "", "", err
	}
	email, accountID, err := g.GetEmail(ctx)
	if err != nil {
		return "", "", "", err
	}
	leaseID := newGenericMailboxLeaseID()
	a.mu.Lock()
	a.sessions[leaseID] = genericMailboxSession{inner: g, email: email, accountID: accountID}
	a.mu.Unlock()
	// password is always empty for HTTP mailbox APIs (they don't generate account passwords)
	return email, "", leaseID, nil
}

func (a *GenericHTTPAdapter) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	a.mu.Lock()
	session, ok := a.sessions[mailboxID]
	a.mu.Unlock()
	if !ok || session.inner == nil {
		return "", fmt.Errorf("generic mailbox lease not found")
	}
	return session.inner.WaitForCode(ctx, session.email, session.accountID, nil, int(timeout.Seconds()))
}

func (a *GenericHTTPAdapter) DeleteEmail(ctx context.Context, mailboxID string) error {
	// Most HTTP mailbox APIs don't support explicit delete (they auto-expire). This is a no-op.
	a.mu.Lock()
	delete(a.sessions, mailboxID)
	a.mu.Unlock()
	return nil
}

func (a *GenericHTTPAdapter) MailboxDomains() []string {
	domain := strings.TrimSpace(fmt.Sprint(a.settings["domain"]))
	if domain == "" {
		if email := strings.TrimSpace(fmt.Sprint(a.settings["email"])); email != "" {
			if index := strings.LastIndexByte(email, '@'); index >= 0 {
				domain = email[index+1:]
			}
		}
	}
	if domain == "" || domain == "<nil>" {
		return nil
	}
	return []string{domain}
}

func (a *GenericHTTPAdapter) MailboxUsesCustomDomain() bool {
	value, _ := a.settings["custom_domain"].(bool)
	return value
}
