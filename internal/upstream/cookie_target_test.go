package upstream

import (
	"net/url"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestImportedClaudeCookieUsesClaudeRequestJar(t *testing.T) {
	cfg := config.Default()
	cfg.ClaudeUpstreamBaseURL = "https://api.anthropic.com"
	client := NewClient(cfg)
	const fallback = "acc-claude:egress-1:cf-verify"
	if err := client.ImportCookies("acc-claude", "egress-1", cfg.ClaudeUpstreamBaseURL, fallback, "cf_clearance=verified"); err != nil {
		t.Fatal(err)
	}
	spec := Request{
		Provider:       "claude",
		DownstreamPath: "/v1/models",
		Account:        storage.Account{ID: "acc-claude"},
		Egress:         storage.EgressProfile{ID: "egress-1"},
		CookieJarKey:   fallback,
	}
	target, err := url.Parse(client.cookieTargetForRequest(spec))
	if err != nil {
		t.Fatal(err)
	}
	cookies := client.cookieJarFor(spec).Cookies(target)
	if len(cookies) != 1 || cookies[0].Name != "cf_clearance" || cookies[0].Value != "verified" {
		t.Fatalf("Claude cookie was not replayable from the Claude jar: %+v", cookies)
	}
}
