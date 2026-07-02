package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminAccountsExportUsesBatchTokens(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx,
		storage.Account{ID: "acc-a", Label: "A", Email: "a@example.com", GroupName: "cyber", Provider: "codex", Status: "active"},
		storage.AccountToken{AccessToken: "at-a", RefreshToken: "rt-a"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(ctx,
		storage.Account{ID: "acc-b", Label: "B", Email: "b@example.com", GroupName: "cyber", Provider: "custom", Status: "active"},
		storage.AccountToken{OpenAIAPIKey: "key-b"}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts/export?format=json", "")
	if code != http.StatusOK {
		t.Fatalf("accounts export = %d: %s", code, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode export: %v\n%s", err, raw)
	}
	if len(rows) != 2 {
		t.Fatalf("export rows = %d, want 2: %#v", len(rows), rows)
	}
	byID := map[string]map[string]interface{}{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	if byID["acc-a"]["access_token"] != "at-a" || byID["acc-a"]["refresh_token"] != "rt-a" {
		t.Fatalf("acc-a export = %#v, want tokens", byID["acc-a"])
	}
	if byID["acc-b"]["openai_api_key"] != "key-b" {
		t.Fatalf("acc-b export = %#v, want api key", byID["acc-b"])
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/accounts/export?format=at", "")
	if code != http.StatusOK {
		t.Fatalf("access token export = %d: %s", code, raw)
	}
	if got := string(raw); got != "at-a\n" {
		t.Fatalf("access token export = %q, want only non-empty access tokens", got)
	}
}

func TestAdminAccountsExportDoesNotUsePerAccountTokenLookup(t *testing.T) {
	source := readAPISource(t, "accounts_export.go")
	body := functionBody(t, source, "adminAccountsExport")
	if strings.Contains(body, ".GetToken(") {
		t.Fatal("adminAccountsExport must use ListTokensByAccountIDs instead of per-account GetToken")
	}
	if !strings.Contains(body, ".ListTokensByAccountIDs(") {
		t.Fatal("adminAccountsExport must use ListTokensByAccountIDs")
	}
}

func readAPISource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func functionBody(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, "func (s *Server) "+name+"(")
	if start < 0 {
		t.Fatalf("%s handler not found", name)
	}
	rest := source[start+len("func (s *Server) "+name+"("):]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}
