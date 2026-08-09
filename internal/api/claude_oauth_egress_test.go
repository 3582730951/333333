package api

import (
	"context"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestClaudeOAuthEgressKeepsBoundExitWhenSidecarBreaks is the regression guard for the
// silent-egress-fallback class on the credential lifecycle. The token endpoint is the same
// host as inference, so a refresh that leaves from the relay host while the account's
// inference leaves from its proxy puts one account on two networks on a fixed schedule.
// A broken sidecar row must cost the impersonation backend, not the exit IP.
func TestClaudeOAuthEgressKeepsBoundExitWhenSidecarBreaks(t *testing.T) {
	ctx := context.Background()
	store := apiTestStore(t)
	s := &Server{store: store}

	const proxyEndpoint = "socks5h://10.0.0.9:1080"
	if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID: "eg-primary", Name: "primary", Type: "socks5h_proxy", Endpoint: proxyEndpoint,
	}); err != nil {
		t.Fatalf("upsert primary egress: %v", err)
	}
	seedClaudeOAuthEgressAccount(t, store, "acct-1")
	// Bind a sidecar ID that does not exist, so ApplySidecarEgressBinding fails.
	if err := store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID: "acct-1", PrimaryEgressID: "eg-primary",
		SidecarEgressID: "eg-missing-sidecar", CookieJarKey: "jar-1",
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	egress, jar := s.claudeOAuthEgress(ctx, "acct-1")
	if egress.Endpoint != proxyEndpoint {
		t.Errorf("Endpoint = %q, want %q: a broken sidecar row must not discard the account's exit IP", egress.Endpoint, proxyEndpoint)
	}
	if !strings.EqualFold(egress.Type, "socks5h_proxy") {
		t.Errorf("Type = %q, want socks5h_proxy", egress.Type)
	}
	if jar != "jar-1" {
		t.Errorf("cookieJarKey = %q, want jar-1", jar)
	}

	// The degradation must be visible to an operator, not inferred from a ban.
	assertClaudeOAuthEgressAudited(t, store, "sidecar_wrapper_unavailable", "bound_exit_kept")
}

// TestClaudeOAuthEgressAuditsHostExitFallback: when nothing about the account's exit is
// knowable the refresh still runs (losing it loses the account), but the host-IP exposure
// must be recorded. Silence here is the actual defect: a missing binding row is persistent,
// so every scheduled refresh for that account would leak, indefinitely and unnoticed.
func TestClaudeOAuthEgressAuditsHostExitFallback(t *testing.T) {
	ctx := context.Background()
	store := apiTestStore(t)
	s := &Server{store: store}

	egress, jar := s.claudeOAuthEgress(ctx, "acct-with-no-binding")
	if !strings.EqualFold(strings.TrimSpace(egress.Type), "direct") {
		t.Errorf("Type = %q, want direct: an unknown binding has no exit to use", egress.Type)
	}
	if jar != "" {
		t.Errorf("cookieJarKey = %q, want empty", jar)
	}
	assertClaudeOAuthEgressAudited(t, store, "binding_lookup_failed", "host_exit")
}

// TestClaudeOAuthEgressUsesBoundExitWhenHealthy pins the non-degraded path so the two
// tests above cannot pass merely because everything degrades.
func TestClaudeOAuthEgressUsesBoundExitWhenHealthy(t *testing.T) {
	ctx := context.Background()
	store := apiTestStore(t)
	s := &Server{store: store}

	const proxyEndpoint = "http://10.0.0.5:3128"
	if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID: "eg-primary", Name: "primary", Type: "http_proxy", Endpoint: proxyEndpoint,
	}); err != nil {
		t.Fatalf("upsert primary egress: %v", err)
	}
	seedClaudeOAuthEgressAccount(t, store, "acct-ok")
	if err := store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID: "acct-ok", PrimaryEgressID: "eg-primary", CookieJarKey: "jar-ok",
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	egress, jar := s.claudeOAuthEgress(ctx, "acct-ok")
	if egress.Endpoint != proxyEndpoint {
		t.Errorf("Endpoint = %q, want %q", egress.Endpoint, proxyEndpoint)
	}
	if jar != "jar-ok" {
		t.Errorf("cookieJarKey = %q, want jar-ok", jar)
	}
	rows := claudeOAuthEgressAuditRows(t, store)
	if len(rows) != 0 {
		t.Errorf("healthy binding produced %d degradation audit rows, want 0", len(rows))
	}
}

// seedClaudeOAuthEgressAccount creates the account row the egress binding's foreign key
// requires. The token values are obvious non-secrets.
func seedClaudeOAuthEgressAccount(t *testing.T, store *storage.Store, accountID string) {
	t.Helper()
	err := store.UpsertAccount(context.Background(), storage.Account{
		ID: accountID, Label: "claude-egress-test", Provider: "claude", Status: "active",
		Email: accountID + "@example.internal",
	}, storage.AccountToken{
		AuthMethod: "oauth", AccessToken: "test-access-placeholder",
		RefreshToken: "test-refresh-placeholder", Scopes: "user:inference",
	})
	if err != nil {
		t.Fatalf("upsert account %s: %v", accountID, err)
	}
}

func claudeOAuthEgressAuditRows(t *testing.T, store *storage.Store) []storage.AuditLogRow {
	t.Helper()
	rows, err := store.ListAuditLog(context.Background(), 200)
	if err != nil {
		t.Fatalf("list audit logs: %v", err)
	}
	out := make([]storage.AuditLogRow, 0, len(rows))
	for _, row := range rows {
		if row.Action == "claude_oauth_egress_degraded" {
			out = append(out, row)
		}
	}
	return out
}

func assertClaudeOAuthEgressAudited(t *testing.T, store *storage.Store, wantReason, wantState string) {
	t.Helper()
	rows := claudeOAuthEgressAuditRows(t, store)
	if len(rows) == 0 {
		t.Fatalf("no claude_oauth_egress_degraded audit row; the fallback would be silent")
	}
	for _, row := range rows {
		if row.Reason == wantReason {
			if row.State != wantState {
				t.Errorf("audit state = %q, want %q", row.State, wantState)
			}
			// The audit payload must stay free of account identifiers and secrets.
			if strings.Contains(row.Detail, "acct-") || strings.Contains(row.Detail, "sk-") {
				t.Errorf("audit detail %q carries an identifier/credential", row.Detail)
			}
			return
		}
	}
	t.Errorf("no audit row with reason %q; got %+v", wantReason, rows)
}
