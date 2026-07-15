package provider

import (
	"context"
	"net/http"
	"testing"
)

func TestBuildMailboxBuiltins(t *testing.T) {
	hc := &http.Client{}
	if got := buildMailbox("tempmail", map[string]interface{}{}, hc); got == nil {
		t.Fatal("built-in TempMail.lol provider must work without credentials")
	}
	if got := buildMailbox("cloudflare", map[string]interface{}{}, hc); got != nil {
		t.Fatal("cloudflare provider without API URL must not be considered ready")
	}
	if got := buildMailbox("cloudflare", map[string]interface{}{"api_url": "https://mail.example"}, hc); got == nil {
		t.Fatal("configured cloudflare provider was not built")
	}
	if got := buildMailbox("imap", map[string]interface{}{"host": "imap.example", "email": "a@example"}, hc); got != nil {
		t.Fatal("IMAP provider without password must not be considered ready")
	}
	if got := buildMailbox("imap", map[string]interface{}{
		"host": "imap.example", "email": "a@example", "password": "secret",
	}, hc); got == nil {
		t.Fatal("configured IMAP provider was not built")
	}
}

func TestGetMailboxFromProviderRecognizesTempMailAlias(t *testing.T) {
	m := &Manager{Mailbox: []MailboxProvider{buildMailbox("tempmail", map[string]interface{}{}, &http.Client{})}}
	// Selection must reach the configured provider. A cancelled context prevents a
	// real network request while still distinguishing alias lookup from not-found.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, err := m.GetMailboxFromProvider(ctx, "tempmail")
	if err == ErrNoProviderAvailable {
		t.Fatal("tempmail alias did not select the built-in tempmail_lol provider")
	}
}
