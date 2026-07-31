package mailbox

import (
	"context"
	"net/url"
	"testing"
)

func parseMailboxTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestMailboxNetworkGuardRejectsPrivateAndInsecureTargets(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"https://169.254.169.254/latest/meta-data",
		"https://10.1.2.3/mail",
		"https://127.0.0.1/mail",
		"http://1.1.1.1/mail",
		"file:///tmp/mail",
	} {
		if err := validateMailboxNetworkURL(ctx, parseMailboxTestURL(t, raw), "", false); err == nil {
			t.Fatalf("target %q was accepted", raw)
		}
	}
	if err := validateMailboxNetworkURL(
		ctx, parseMailboxTestURL(t, "https://1.1.1.1/mail"), "", false,
	); err != nil {
		t.Fatalf("public HTTPS target rejected: %v", err)
	}
}

func TestMailboxNetworkGuardAllowsOnlyConfiguredLoopbackFixture(t *testing.T) {
	ctx := context.Background()
	if err := validateMailboxNetworkURL(
		ctx, parseMailboxTestURL(t, "http://127.0.0.1:8080/mail"), "127.0.0.1", true,
	); err != nil {
		t.Fatalf("configured loopback fixture rejected: %v", err)
	}
	for _, raw := range []string{
		"http://localhost:8080/mail",
		"http://127.0.0.2:8080/mail",
		"https://127.0.0.1:8080/mail",
	} {
		if err := validateMailboxNetworkURL(ctx, parseMailboxTestURL(t, raw), "127.0.0.1", true); err == nil {
			t.Fatalf("different loopback target %q was accepted", raw)
		}
	}
}
