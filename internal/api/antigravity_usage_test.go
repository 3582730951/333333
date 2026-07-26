package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestAntigravityGeminiHandlerRecordsAttributedCacheUsage(t *testing.T) {
	const (
		accountID = "antigravity-usage"
		model     = "gemini-2.5-pro"
		keyHash   = "downstream-key-hash"
		userID    = "portal-user"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" {
			t.Fatalf("antigravity upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer antigravity-access-token" {
			t.Fatalf("antigravity authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"response": {
				"candidates": [{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],
				"usageMetadata": {"promptTokenCount":12,"candidatesTokenCount":3,"cachedContentTokenCount":5}
			}
		}`)
	})
	ctx := context.Background()
	account := storage.Account{ID: accountID, Label: "Antigravity usage", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(ctx, storage.AntigravityCredentials{
		AccountID:   accountID,
		ProjectID:   "test-project",
		AccessToken: "antigravity-access-token",
		ExpiresAt:   time.Now().Add(2 * time.Hour).Unix(),
		BaseURL:     h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`{"model":"` + model + `","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "antigravity-thread")
	affinity := routing.ExtractAffinityKey(req, raw)
	if affinity.Hash == "" {
		t.Fatal("expected test request to produce an affinity hash")
	}
	req = req.WithContext(withDownstreamKey(req.Context(), downstreamPolicy{KeyHash: keyHash, UserID: userID}))
	w := httptest.NewRecorder()

	if got := h.app.antigravityMessagesWithLease(w, req, raw, model, scheduler.Lease{Account: account}); got != outcomeDone {
		t.Fatalf("handler outcome = %v", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("handler status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hello"`) {
		t.Fatalf("translated response = %s", w.Body.String())
	}

	var (
		gotRouteHash, gotKeyHash, gotUserID, gotProvider, gotSource, gotAffinitySource string
		gotCached, gotCacheRead                                                        int64
		gotCacheReadPresent                                                            int
	)
	if err := h.store.DB().QueryRowContext(ctx, `
		SELECT route_key_hash, api_key_hash, user_id, usage_provider, usage_source,
		       affinity_source, cached_tokens, cache_read_tokens, cache_read_present
		FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, accountID).Scan(
		&gotRouteHash, &gotKeyHash, &gotUserID, &gotProvider, &gotSource,
		&gotAffinitySource, &gotCached, &gotCacheRead, &gotCacheReadPresent,
	); err != nil {
		t.Fatal(err)
	}
	if gotRouteHash != affinity.Hash || gotKeyHash != keyHash || gotUserID != userID {
		t.Fatalf("usage identity = route %q key %q user %q", gotRouteHash, gotKeyHash, gotUserID)
	}
	if gotProvider != "antigravity" || gotSource != "upstream" || gotAffinitySource != affinity.Source {
		t.Fatalf("usage diagnostics = provider %q source %q affinity %q", gotProvider, gotSource, gotAffinitySource)
	}
	if gotCached != 5 || gotCacheRead != 5 || gotCacheReadPresent != 1 {
		t.Fatalf("cache usage = cached %d read %d present %d", gotCached, gotCacheRead, gotCacheReadPresent)
	}

	binding, err := h.store.GetAffinityBinding(ctx, affinity.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RouteKey != affinity.Key || binding.AccountID != accountID {
		t.Fatalf("affinity binding = %+v", binding)
	}
}
