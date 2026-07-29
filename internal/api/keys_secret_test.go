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
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized api-key create = %d, want 413: %s", code, body)
	}
	if !strings.Contains(string(body), `"code":"request_too_large"`) {
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

func TestAdminAPIKeyRoutingPolicyCanBeEditedAtomically(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"key-import-pool"}`); code != http.StatusOK {
		t.Fatalf("create account pool group = %d: %s", code, body)
	}
	code, body := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{"name":"key-inference-users","targets":[{"kind":"account_pool_group","id":"key-import-pool"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create user group = %d: %s", code, body)
	}
	var userGroup map[string]interface{}
	if err := json.Unmarshal(body, &userGroup); err != nil {
		t.Fatal(err)
	}
	userGroupID, _ := userGroup["id"].(string)
	if userGroupID == "" {
		t.Fatalf("created user group missing id: %v", userGroup)
	}

	code, body = grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"cli","user_group_id":"`+userGroupID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create inference key = %d: %s", code, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	keyHash, _ := created["key_hash"].(string)
	patchBody := `{"label":"cli-edited","user_group_id":"` + userGroupID + `","group_name":"","force_model":"gpt-5.6-sol","force_effort":"high"}`
	if code, body = grpReq(t, h, http.MethodPatch, "/admin/api-keys/"+keyHash, patchBody); code != http.StatusOK {
		t.Fatalf("edit inference key = %d: %s", code, body)
	}
	key, found, err := h.store.LookupAPIKey(t.Context(), keyHash)
	if err != nil || !found {
		t.Fatalf("lookup edited key: found=%v err=%v", found, err)
	}
	if key.Label != "cli-edited" || key.UserGroupID != userGroupID || key.GroupName != "" || key.ForceModel != "gpt-5.6-sol" || key.ForceEffort != "high" {
		t.Fatalf("unexpected edited key: %+v", key)
	}

	code, body = grpReq(t, h, http.MethodPatch, "/admin/api-keys/"+keyHash, `{"label":"must-not-persist","user_group_id":"missing-user-group"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid user group patch = %d, want 400: %s", code, body)
	}
	key, found, err = h.store.LookupAPIKey(t.Context(), keyHash)
	if err != nil || !found || key.Label != "cli-edited" || key.UserGroupID != userGroupID {
		t.Fatalf("invalid patch partially persisted: found=%v err=%v key=%+v", found, err, key)
	}

	code, body = grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"importer","key_type":"pool_import","group_name":"key-import-pool"}`)
	if code != http.StatusCreated {
		t.Fatalf("create pool import key = %d: %s", code, body)
	}
	if code, body = grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"bad-inference","group_name":"key-import-pool"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("inference key with account pool group = %d, want 422: %s", code, body)
	}
	if code, body = grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"bad-import","key_type":"pool_import","user_group_id":"`+userGroupID+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("pool import key with user group = %d, want 422: %s", code, body)
	}
}
