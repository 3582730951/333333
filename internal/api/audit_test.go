package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminAuditCanFilterByAccountID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	for _, row := range []storage.AuditLogRow{
		{AccountID: "acc-a", AccountLabel: "alpha", Action: "first", State: "alive"},
		{AccountID: "acc-b", AccountLabel: "beta", Action: "other", State: "banned"},
		{AccountID: "acc-a", AccountLabel: "alpha", Action: "latest", State: "alive"},
	} {
		if err := h.store.InsertAuditLog(ctx, row); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/audit?account_id=acc-a&limit=10", "")
	if code != http.StatusOK {
		t.Fatalf("admin audit = %d: %s", code, raw)
	}
	var rows []storage.AuditLogRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode audit rows: %v\n%s", err, raw)
	}
	if len(rows) != 2 {
		t.Fatalf("filtered audit rows = %d, want 2: %#v", len(rows), rows)
	}
	if rows[0].Action != "latest" || rows[1].Action != "first" {
		t.Fatalf("filtered audit order = %#v, want newest acc-a rows", rows)
	}
	for _, row := range rows {
		if row.AccountID != "acc-a" {
			t.Fatalf("filtered audit included unrelated row: %#v", row)
		}
	}
}

func TestAdminAuditEmptyReturnsJSONArray(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/audit?limit=10", "")
	if code != http.StatusOK {
		t.Fatalf("admin audit = %d: %s", code, raw)
	}
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty audit response = %s, want []", raw)
	}
}
