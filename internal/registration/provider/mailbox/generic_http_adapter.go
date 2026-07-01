package mailbox

import (
	"context"
	"time"
)

// GenericHTTPAdapter wraps GenericHTTP into the MailboxProvider interface (which expects
// CreateEmail → (email, password, mailboxID, error) but GenericHTTP only generates email
// addresses, not passwords). Password is always empty for HTTP mailbox APIs.
type GenericHTTPAdapter struct {
	inner *GenericHTTP
	name  string
}

// NewGenericHTTPAdapter builds the adapter. name is the provider key (outlook_email,
// duckmail, tempmail_lol) used for Name().
func NewGenericHTTPAdapter(pipeline, settings map[string]interface{}, proxyURL, name string) (*GenericHTTPAdapter, error) {
	g, err := NewGenericHTTP(pipeline, settings, proxyURL)
	if err != nil {
		return nil, err
	}
	return &GenericHTTPAdapter{inner: g, name: name}, nil
}

func (a *GenericHTTPAdapter) Name() string { return a.name }
func (a *GenericHTTPAdapter) Type() string { return "mailbox" }

func (a *GenericHTTPAdapter) CreateEmail(ctx context.Context) (string, string, string, error) {
	email, accountID, err := a.inner.GetEmail(ctx)
	if err != nil {
		return "", "", "", err
	}
	// password is always empty for HTTP mailbox APIs (they don't generate account passwords)
	return email, "", accountID, nil
}

func (a *GenericHTTPAdapter) WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (string, error) {
	// GenericHTTP expects email but the interface passes mailboxID (which is accountID in our
	// case). We need to reconstruct the email from the inner vars pool OR store it. For now
	// we'll pass empty email and rely on mailboxID being set in the vars pool during GetEmail.
	email := a.inner.vars["email"]
	return a.inner.WaitForCode(ctx, email, mailboxID, nil, int(timeout.Seconds()))
}

func (a *GenericHTTPAdapter) DeleteEmail(ctx context.Context, mailboxID string) error {
	// Most HTTP mailbox APIs don't support explicit delete (they auto-expire). This is a no-op.
	return nil
}
