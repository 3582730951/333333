package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
)

func TestCaptureRequestBodyIgnoresLegacyFixedReserveAndReplays(t *testing.T) {
	payload := strings.Repeat("x", (1<<20)+17)
	cfg := config.Default()
	cfg.MaxBodyBytes = 2 << 20
	cfg.BodyMemoryThresholdBytes = 1 << 20
	// Existing installations may still contain the former 10 GiB default. API
	// request capture must follow CLIProxyAPI-style actual-byte admission instead
	// of rejecting a fitting body because of this legacy value.
	cfg.BodyDiskReserveBytes = 1 << 62
	cfg.BodySpoolDir = t.TempDir()
	budget := bodysource.NewBudget(2<<20, 2<<20)
	body, err := captureRequestBody(context.Background(), strings.NewReader(payload), cfg, budget)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bodysource.ReadAll(body)
	if err != nil || string(raw) != payload {
		t.Fatalf("spooled body mismatch: len=%d err=%v", len(raw), err)
	}
	if got := budget.Snapshot().SpoolUsed; got != int64(len(payload)) {
		t.Fatalf("spool reservation=%d", got)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot().SpoolUsed; got != 0 {
		t.Fatalf("reservation leaked: %d", got)
	}
}

func TestCaptureRequestBodyStopsAtSpoolCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.MaxBodyBytes = 1 << 20
	cfg.BodyMemoryThresholdBytes = 1
	cfg.BodyDiskReserveBytes = 0
	cfg.BodySpoolDir = t.TempDir()
	_, err := captureRequestBody(context.Background(), strings.NewReader(strings.Repeat("x", 128<<10)), cfg, bodysource.NewBudget(1, 64<<10))
	if !errors.Is(err, bodysource.ErrSpoolBudget) {
		t.Fatalf("err=%v", err)
	}
}
