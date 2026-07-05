package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestPoolImportKeyCanOnlyImportAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })

	createBody := `{"label":"automation importer","key_type":"pool_import","expires_at":` + jsonNumber(time.Now().Add(time.Hour).Unix()) + `}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/api-keys", createBody)
	if code != http.StatusCreated {
		t.Fatalf("create pool import key = %d: %s", code, raw)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created key: %v (%s)", err, raw)
	}
	key, _ := created["key"].(string)
	keyHash, _ := created["key_hash"].(string)
	if key == "" || keyHash == "" {
		t.Fatalf("created key missing fields: %v", created)
	}
	if got := key[:8]; got != "poolimp_" {
		t.Fatalf("key prefix = %q, want poolimp_", got)
	}
	if created["key_type"] != "pool_import" {
		t.Fatalf("key_type = %v", created["key_type"])
	}

	importBody := []byte(`{"label":"imported","auth_json_text":"{\"access_token\":\"access-poolimp\",\"account_id\":\"acct-poolimp\",\"email\":\"poolimp@example.internal\"}"}`)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/api/account-pool/import", bytes.NewReader(importBody))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pool import = %d: %s", resp.StatusCode, raw)
	}
	var imported map[string]interface{}
	if err := json.Unmarshal(raw, &imported); err != nil {
		t.Fatalf("decode import: %v (%s)", err, raw)
	}
	if imported["email"] != "poolimp@example.internal" || imported["import_status"] != "imported" {
		t.Fatalf("unexpected import response: %v", imported)
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/admin/accounts", ""},
		{http.MethodGet, "/admin/accounts/export?format=json", ""},
		{http.MethodPost, "/v1/responses", `{"model":"gpt","input":"hi"}`},
		{http.MethodGet, "/v1/models", ""},
	} {
		req, _ := http.NewRequest(tc.method, h.pool.URL+tc.path, bytes.NewBufferString(tc.body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("pool import key should be rejected for %s %s, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}

	if code, raw := grpReq(t, h, http.MethodPatch, "/admin/api-keys/"+keyHash, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable key = %d: %s", code, raw)
	}
	req, _ = http.NewRequest(http.MethodPost, h.pool.URL+"/api/account-pool/import", bytes.NewReader(importBody))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled pool import key status = %d, want 401", resp.StatusCode)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
