package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminContextJournalClearDeletesOnlyContextsAndReclaims(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	if err := h.store.PutContextJournal(ctx, storage.ContextJournal{ResponseID: "resp-clear", Payload: `{"input":[]}`, ExpiresAt: storage.Now() + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "must-survive"}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodDelete, "/admin/context-journal", "")
	if code != http.StatusOK {
		t.Fatalf("clear contexts status=%d body=%s", code, raw)
	}
	var response struct {
		DeletedContexts int64 `json:"deleted_contexts"`
		SpaceReclaimed  bool  `json:"space_reclaimed"`
		TTLSeconds      int   `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.DeletedContexts != 1 || !response.SpaceReclaimed || response.TTLSeconds != 3600 {
		t.Fatalf("clear response=%+v body=%s", response, raw)
	}
	var contexts, audits int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM context_journal`).Scan(&contexts); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='must-survive'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if contexts != 0 || audits != 1 {
		t.Fatalf("after clear contexts=%d audits=%d", contexts, audits)
	}
}

func TestAdminContextJournalRejectsNonDelete(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if code, _ := grpReq(t, h, http.MethodGet, "/admin/context-journal", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", code)
	}
}
