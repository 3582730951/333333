package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	body := functionBody(t, source, "adminAccountsExport") + functionBody(t, source, "accountExportRecords")
	if strings.Contains(body, ".GetToken(") {
		t.Fatal("adminAccountsExport must use ListTokensByAccountIDs instead of per-account GetToken")
	}
	if !strings.Contains(body, ".ListTokensByAccountIDs(") {
		t.Fatal("adminAccountsExport must use ListTokensByAccountIDs")
	}
}

func TestCodexOfficialAndCLIProxyCompatibilityExports(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	account := storage.Account{ID: "local-a", UpstreamAccountID: "workspace-a", Label: "A", Email: "a@example.com", PlanType: "Plus Team", GroupName: "cyber", Provider: "codex", Status: "active"}
	token := storage.AccountToken{AuthMethod: "oauth", IDTokenRaw: "id.jwt.value", AccessToken: "access-a", RefreshToken: "refresh-a", LastRefresh: 1_700_000_000, ExpiresAt: 1_700_003_600}
	if err := h.store.UpsertAccount(t.Context(), account, token); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts/export?format=codex-auth&id=local-a", "")
	if code != http.StatusOK {
		t.Fatalf("Codex auth export = %d: %s", code, raw)
	}
	var official map[string]any
	if err := json.Unmarshal(raw, &official); err != nil {
		t.Fatal(err)
	}
	if official["auth_mode"] != "chatgpt" {
		t.Fatalf("auth_mode = %#v", official["auth_mode"])
	}
	if value, present := official["OPENAI_API_KEY"]; !present || value != nil {
		t.Fatalf("OPENAI_API_KEY = %#v, present=%v; want explicit null", value, present)
	}
	tokens := official["tokens"].(map[string]any)
	if tokens["id_token"] != "id.jwt.value" || tokens["account_id"] != "workspace-a" {
		t.Fatalf("official tokens = %#v", tokens)
	}

	response, err := http.Get(h.pool.URL + "/admin/accounts/export?format=cliproxyapi&id=local-a")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CLIProxy export = %d: %s", response.StatusCode, raw)
	}
	digest := sha256.Sum256([]byte("workspace-a"))
	wantName := fmt.Sprintf("codex-%x-a@example.com-plus-team.json", digest[:4])
	if disposition := response.Header.Get("Content-Disposition"); !strings.Contains(disposition, wantName) {
		t.Fatalf("Content-Disposition = %q, want %q", disposition, wantName)
	}
	var cli map[string]any
	if err := json.Unmarshal(raw, &cli); err != nil {
		t.Fatal(err)
	}
	if cli["type"] != "codex" || cli["account_id"] != "workspace-a" || cli["expired"] == "" {
		t.Fatalf("CLIProxy document = %#v", cli)
	}
}

func TestCodexMultiAuthExportUsesEmailTimestampFiles(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, email := range []string{"one@example.com", "two@example.com"} {
		id := strings.Split(email, "@")[0]
		if err := h.store.UpsertAccount(t.Context(), storage.Account{ID: id, Email: email, Provider: "codex", Status: "active"}, storage.AccountToken{
			AuthMethod: "oauth", IDTokenRaw: "id-" + id, AccessToken: "access-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := http.Get(h.pool.URL + "/admin/accounts/export?format=codex-auth")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("decode ZIP: %v", err)
	}
	if len(reader.File) != 2 {
		t.Fatalf("ZIP entries = %d", len(reader.File))
	}
	for _, file := range reader.File {
		if !strings.Contains(file.Name, "@example.com-") || !strings.HasSuffix(file.Name, ".json") {
			t.Fatalf("unexpected official multi-account filename %q", file.Name)
		}
	}
}

func TestOfficialCodexAPIKeyExportMatchesAuthDotJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: "api-key", Email: "key@example.com", Provider: "codex", Status: "active",
	}, storage.AccountToken{AuthMethod: "api_key", AccessToken: "legacy-copy", OpenAIAPIKey: "sk-official"}); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts/export?format=auth.json&id=api-key", "")
	if code != http.StatusOK {
		t.Fatalf("Codex API-key auth export = %d: %s", code, raw)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 2 || document["auth_mode"] != "apikey" || document["OPENAI_API_KEY"] != "sk-official" {
		t.Fatalf("official API-key auth.json = %#v", document)
	}
}

func TestCLIProxyFilenameNeverHashesLocalTokenAccountID(t *testing.T) {
	record := accountExportRecord{
		Account: storage.Account{Email: "fallback@example.com", PlanType: "Pro Plus"},
		// AccountToken.AccountID is the pool's local foreign key, not a ChatGPT
		// workspace identifier, so it must never appear in a compatibility name.
		Token: storage.AccountToken{AccountID: "local-pool-id"},
	}
	want := "codex-fallback@example.com-pro-plus.json"
	if got := cliProxyCredentialFilename(record); got != want {
		t.Fatalf("CLIProxy filename = %q, want %q", got, want)
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
