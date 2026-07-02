package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAPIKeySecretIsRecopyable guards the "API Key + install command 随时可复制" requirement:
// a created key's plaintext is stored and returned by the admin list (so the UI can copy
// it and build the one-shot install command at any time, not just once at creation).
func TestAPIKeySecretIsRecopyable(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, body := grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"k1"}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create key = %d: %s", code, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	plain, _ := created["key"].(string)
	hash, _ := created["key_hash"].(string)
	if !strings.HasPrefix(plain, "cap_") || hash == "" {
		t.Fatalf("unexpected create response: %v", created)
	}

	_, lb := grpReq(t, h, http.MethodGet, "/admin/api-keys", "")
	var list []map[string]interface{}
	if err := json.Unmarshal(lb, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range list {
		if k["key_hash"] == hash {
			found = true
			if k["secret"] != plain {
				t.Fatalf("stored secret not re-copyable: got %v, want %s", k["secret"], plain)
			}
		}
	}
	if !found {
		t.Fatalf("created key missing from /admin/api-keys list")
	}
}

func TestAPIKeyCreateRejectsOversizedJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, body := grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"too-big"}`+strings.Repeat(" ", adminJSONBodyLimit))
	if code != http.StatusBadRequest {
		t.Fatalf("oversized api-key create = %d, want 400: %s", code, body)
	}
	if !strings.Contains(string(body), "request body too large") {
		t.Fatalf("oversized api-key create body = %s, want size error", body)
	}

	code, body = grpReq(t, h, http.MethodGet, "/admin/api-keys", "")
	if code != http.StatusOK {
		t.Fatalf("list api keys = %d: %s", code, body)
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("oversized api-key create should not persist keys: %v", list)
	}
}
