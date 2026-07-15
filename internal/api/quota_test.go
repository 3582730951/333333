package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestParseQuotaSnapshotAnthropicUnified(t *testing.T) {
	now := int64(1_700_000_000)
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-limit", "1000")
	h.Set("anthropic-ratelimit-unified-remaining", "250")
	h.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-reset", "2023-11-14T22:15:00Z") // now+100s
	h.Set("anthropic-ratelimit-tokens-limit", "5000")
	h.Set("anthropic-ratelimit-tokens-remaining", "4000")

	snap, ok := parseQuotaSnapshot("acc", "claude", h, now)
	if !ok {
		t.Fatal("expected snapshot ok")
	}
	if snap.Source != "unified" {
		t.Fatalf("source = %q, want unified", snap.Source)
	}
	if snap.UsedPercent != 75 {
		t.Fatalf("used%% = %v, want 75", snap.UsedPercent)
	}
	if snap.Status != "allowed_warning" {
		t.Fatalf("status = %q", snap.Status)
	}
	if snap.ResetAt != now+100 {
		t.Fatalf("reset_at = %d, want %d", snap.ResetAt, now+100)
	}
	// dedicated token window is still captured for detail
	if snap.LimitTokens != 5000 || snap.RemainingTokens != 4000 {
		t.Fatalf("token window not captured: %+v", snap)
	}
	if snap.Raw == "" {
		t.Fatal("raw headers should be captured")
	}
}

func TestParseQuotaSnapshotOpenAI(t *testing.T) {
	now := int64(1_700_000_000)
	h := http.Header{}
	h.Set("x-ratelimit-limit-tokens", "10000")
	h.Set("x-ratelimit-remaining-tokens", "2500")
	h.Set("x-ratelimit-reset-tokens", "6m0s")
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "90")

	snap, ok := parseQuotaSnapshot("acc", "codex", h, now)
	if !ok {
		t.Fatal("expected snapshot ok")
	}
	if snap.Source != "tokens" {
		t.Fatalf("source = %q, want tokens", snap.Source)
	}
	if snap.UsedPercent != 75 {
		t.Fatalf("used%% = %v, want 75", snap.UsedPercent)
	}
	if snap.ResetAt != now+360 {
		t.Fatalf("reset_at = %d, want %d", snap.ResetAt, now+360)
	}
	if snap.LimitRequests != 100 || snap.RemainingRequests != 90 {
		t.Fatalf("request window not captured: %+v", snap)
	}
}

func TestParseQuotaSnapshotAnthropicInputOutputTokens(t *testing.T) {
	now := int64(1_700_000_000)
	h := http.Header{}
	h.Set("anthropic-ratelimit-input-tokens-limit", "1000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "100")
	h.Set("anthropic-ratelimit-input-tokens-reset", "2023-11-14T22:14:20Z") // now+60s
	h.Set("anthropic-ratelimit-input-tokens-status", "allowed_warning")
	h.Set("anthropic-ratelimit-output-tokens-limit", "2000")
	h.Set("anthropic-ratelimit-output-tokens-remaining", "1500")

	snap, ok := parseQuotaSnapshot("acc", "claude", h, now)
	if !ok {
		t.Fatal("expected snapshot ok")
	}
	if snap.Source != "input_tokens" {
		t.Fatalf("source = %q, want input_tokens", snap.Source)
	}
	if snap.UsedPercent != 90 {
		t.Fatalf("used%% = %v, want 90", snap.UsedPercent)
	}
	if snap.LimitTokens != 1000 || snap.RemainingTokens != 100 {
		t.Fatalf("input token window not captured: %+v", snap)
	}
	if snap.ResetAt != now+60 {
		t.Fatalf("reset_at = %d, want %d", snap.ResetAt, now+60)
	}
	if snap.Status != "allowed_warning" {
		t.Fatalf("status = %q", snap.Status)
	}
	if !strings.Contains(snap.Raw, "anthropic-ratelimit-output-tokens-remaining") {
		t.Fatalf("raw should retain output token dimension: %s", snap.Raw)
	}
}

func TestParseQuotaSnapshotRequestsOnly(t *testing.T) {
	now := int64(1_700_000_000)
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "200")
	h.Set("x-ratelimit-remaining-requests", "200")
	h.Set("x-ratelimit-reset-requests", "30s")

	snap, ok := parseQuotaSnapshot("acc", "codex", h, now)
	if !ok {
		t.Fatal("expected snapshot ok")
	}
	if snap.Source != "requests" {
		t.Fatalf("source = %q, want requests", snap.Source)
	}
	if snap.UsedPercent != 0 {
		t.Fatalf("used%% = %v, want 0", snap.UsedPercent)
	}
}

func TestParseQuotaSnapshotNoHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("content-type", "application/json")
	if _, ok := parseQuotaSnapshot("acc", "codex", h, 1); ok {
		t.Fatal("expected no snapshot when no rate-limit headers present")
	}
}

func TestUsedPercentFromUnknown(t *testing.T) {
	if v := usedPercentFrom(0, 5); v != -1 {
		t.Fatalf("limit 0 should be unknown, got %v", v)
	}
	if v := usedPercentFrom(100, -1); v != -1 {
		t.Fatalf("negative remaining should be unknown, got %v", v)
	}
	if v := usedPercentFrom(100, 0); v != 100 {
		t.Fatalf("fully consumed should be 100, got %v", v)
	}
}

func TestCaptureQuotaThrottlesCodexNoRateLimitHeaderAudit(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	app := &Server{cfg: config.Default(), store: store}
	ctx := context.Background()
	header := http.Header{}
	header.Set("content-type", "text/event-stream")

	app.captureQuota(ctx, "acc", "codex", "gpt-5.5", header)
	app.captureQuota(ctx, "acc", "codex", "gpt-5.5", header)
	rows, err := store.ListAuditLogForAccount(ctx, "acc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows after duplicate capture = %d, want 1: %+v", len(rows), rows)
	}

	app.missingLimitAudit.Store("acc\x00codex", storage.Now()/3600-1)
	app.captureQuota(ctx, "acc", "codex", "gpt-5.5", header)
	rows, err = store.ListAuditLogForAccount(ctx, "acc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows after hourly window = %d, want 2: %+v", len(rows), rows)
	}
}
