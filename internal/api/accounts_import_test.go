package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestImportTopLevelSessionJSONAndDuplicateDoesNotOverwrite(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })

	body := `{"label":"first","auth_json_text":"{\"access_token\":\"access-one\",\"refresh_token\":\"refresh-one\",\"account_id\":\"acct-dup\",\"chatgpt_user_id\":\"user-dup\",\"email\":\"dup@example.internal\",\"plan_type\":\"plus\",\"last_refresh\":1750000000}"}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", body)
	if code != http.StatusOK {
		t.Fatalf("first import = %d: %s", code, raw)
	}
	var first map[string]interface{}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode first: %v (%s)", err, raw)
	}
	accountID, _ := first["id"].(string)
	if accountID == "" {
		t.Fatalf("first import missing id: %v", first)
	}
	if first["upstream_account_id"] != "acct-dup" || first["email"] != "dup@example.internal" || first["plan_type"] != "plus" {
		t.Fatalf("top-level metadata not stored: %v", first)
	}
	tok, err := h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-one" || tok.RefreshToken != "refresh-one" || tok.LastRefresh != 1750000000 {
		t.Fatalf("top-level token not stored: %+v", tok)
	}
	caps, err := h.store.ListCapabilities(context.Background(), accountID)
	if err != nil || len(caps) == 0 {
		t.Fatalf("auth.json import must synchronously seed model capabilities: caps=%+v err=%v", caps, err)
	}

	dupBody := strings.ReplaceAll(body, "first", "second")
	dupBody = strings.ReplaceAll(dupBody, "access-one", "access-two")
	code, raw = grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", dupBody)
	if code != http.StatusOK {
		t.Fatalf("duplicate import = %d: %s", code, raw)
	}
	var dup map[string]interface{}
	if err := json.Unmarshal(raw, &dup); err != nil {
		t.Fatalf("decode duplicate: %v (%s)", err, raw)
	}
	if dup["duplicate"] != true || dup["import_status"] != "duplicate" {
		t.Fatalf("duplicate response missing duplicate marker: %v", dup)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != "first" {
		t.Fatalf("duplicate import overwrote label: %+v", account)
	}
	tok, err = h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-one" {
		t.Fatalf("duplicate import overwrote token: %+v", tok)
	}
}
