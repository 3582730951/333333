package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/admission"
)

func TestSpoolUnknownBodyReservesProgressivelyAndSpills(t *testing.T) {
	payload := strings.Repeat("x", (1<<20)+17)
	var reserved atomic.Int64
	body, cleanup, err := spoolUnknownBody(context.Background(), io.NopCloser(strings.NewReader(payload)), func(_ context.Context, n int64) (func(), error) {
		reserved.Add(n)
		return func() { reserved.Add(-n) }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	if err != nil || string(raw) != payload {
		t.Fatalf("spooled body mismatch: len=%d err=%v", len(raw), err)
	}
	if reserved.Load() != 5*int64(len(payload)) {
		t.Fatalf("reservation=%d", reserved.Load())
	}
	cleanup()
	if reserved.Load() != 0 {
		t.Fatalf("reservation leaked: %d", reserved.Load())
	}
}

func TestSpoolUnknownBodyStopsAtCapacity(t *testing.T) {
	_, cleanup, err := spoolUnknownBody(context.Background(), io.NopCloser(strings.NewReader(strings.Repeat("x", 128<<10))), func(_ context.Context, _ int64) (func(), error) {
		return nil, admission.ErrCapacity
	})
	cleanup()
	if !errors.Is(err, admission.ErrCapacity) {
		t.Fatalf("err=%v", err)
	}
}
