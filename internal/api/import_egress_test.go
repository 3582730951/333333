package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
)

func createPoolImportTestKey(t *testing.T, h *testHarness) string {
	t.Helper()
	plain := "poolimp_test_key"
	if err := h.store.UpsertAPIKey(context.Background(), storage.APIKey{
		KeyHash: hashAPIKey(plain),
		KeyType: "pool_import",
		Label:   "test pool import",
		Enabled: true,
		Secret:  plain,
	}); err != nil {
		t.Fatalf("upsert pool import key: %v", err)
	}
	return plain
}

func postPoolImport(t *testing.T, h *testHarness, key string, body map[string]interface{}) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/api/account-pool/import", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func importedAccountID(t *testing.T, raw []byte) string {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode import response: %v (%s)", err, raw)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("import response missing id: %v", out)
	}
	return id
}

func assertPrimaryEgress(t *testing.T, h *testHarness, accountID, want string) {
	t.Helper()
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatalf("get egress binding for %s: %v", accountID, err)
	}
	if binding.PrimaryEgressID != want {
		t.Fatalf("primary egress for %s = %q, want %q", accountID, binding.PrimaryEgressID, want)
	}
	if binding.CookieJarKey != accountID+":"+want {
		t.Fatalf("cookie jar key for %s = %q, want account:primary", accountID, binding.CookieJarKey)
	}
}

func TestPoolImportBindsRequestedEgressAndFallsBackToDirect(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "egress_alt")
	key := createPoolImportTestKey(t, h)

	for _, tc := range []struct {
		name     string
		egressID string
		want     string
	}{
		{name: "existing", egressID: "egress_alt", want: "egress_alt"},
		{name: "missing", egressID: "egress_missing", want: storage.DefaultDirectEgressID},
		{name: "blank", egressID: "", want: storage.DefaultDirectEgressID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{
				"label":          "pool-" + tc.name,
				"group_name":     "cyber",
				"auth_json_text": fmt.Sprintf(`{"access_token":"tok-pool-%s","account_id":"up-pool-%s"}`, tc.name, tc.name),
			}
			if tc.egressID != "" {
				body["egress_id"] = tc.egressID
			}
			code, raw := postPoolImport(t, h, key, body)
			if code != http.StatusOK {
				t.Fatalf("pool import = %d: %s", code, raw)
			}
			assertPrimaryEgress(t, h, importedAccountID(t, raw), tc.want)
		})
	}
}

func TestAdminImportAuthJSONBindsEgressAliasAndDuplicatePreservesBinding(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "egress_alt")
	upsertTestEgressProfile(t, h, "egress_other")

	body := `{"label":"first","primary_egress_id":"egress_alt","auth_json_text":"{\"access_token\":\"access-one\",\"refresh_token\":\"refresh-one\",\"account_id\":\"up-admin-egress\"}"}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", body)
	if code != http.StatusOK {
		t.Fatalf("first import = %d: %s", code, raw)
	}
	accountID := importedAccountID(t, raw)
	assertPrimaryEgress(t, h, accountID, "egress_alt")

	dupBody := `{"label":"second","egress_id":"egress_other","auth_json_text":"{\"access_token\":\"access-two\",\"refresh_token\":\"refresh-two\",\"account_id\":\"up-admin-egress\"}"}`
	code, raw = grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", dupBody)
	if code != http.StatusOK {
		t.Fatalf("duplicate import = %d: %s", code, raw)
	}
	var dup map[string]interface{}
	if err := json.Unmarshal(raw, &dup); err != nil {
		t.Fatalf("decode duplicate: %v (%s)", err, raw)
	}
	if dup["duplicate"] != true || dup["import_status"] != "duplicate" {
		t.Fatalf("duplicate response missing marker: %v", dup)
	}
	assertPrimaryEgress(t, h, accountID, "egress_alt")
	tok, err := h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-one" {
		t.Fatalf("duplicate import overwrote token: %+v", tok)
	}
}

func TestSaveImportedAccountBindsRequestedEgressAndFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "egress_alt")

	account, err := h.app.saveImportedAccount(context.Background(), authparse.ParsedAuth{
		AccountID:         "acc-save-egress-alt",
		UpstreamAccountID: "up-save-egress-alt",
		AccessToken:       "tok-save-egress-alt",
	}, "save-alt", "", "", "codex", "egress_alt")
	if err != nil {
		t.Fatal(err)
	}
	assertPrimaryEgress(t, h, account.ID, "egress_alt")

	account, err = h.app.saveImportedAccount(context.Background(), authparse.ParsedAuth{
		AccountID:         "acc-save-egress-missing",
		UpstreamAccountID: "up-save-egress-missing",
		AccessToken:       "tok-save-egress-missing",
	}, "save-missing", "", "", "codex", "egress_missing")
	if err != nil {
		t.Fatal(err)
	}
	assertPrimaryEgress(t, h, account.ID, storage.DefaultDirectEgressID)
}

func TestAdminImportKeyBindsEgressID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "egress_alt")
	if err := h.store.UpsertCustomProvider(context.Background(), storage.CustomProvider{
		ID:                 "deepseek",
		Name:               "DeepSeek",
		BaseURL:            "https://api.deepseek.example/v1",
		UpstreamProtocol:   storage.CustomProviderProtocolResponses,
		Enabled:            true,
		AutoDiscoverModels: false,
		Models:             []string{"deepseek-chat"},
	}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-key", `{
		"provider_id":"deepseek",
		"api_key":"sk-test-import-key-egress",
		"label":"deepseek egress",
		"egress_id":"egress_alt"
	}`)
	if code != http.StatusOK {
		t.Fatalf("import key = %d: %s", code, raw)
	}
	assertPrimaryEgress(t, h, importedAccountID(t, raw), "egress_alt")
}
