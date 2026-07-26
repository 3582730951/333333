package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// groups_test.go guards account-pool group CRUD, account reassignment, and delete
// guards. User policy belongs to user groups; account-pool group endpoints reject it.

func grpReq(t *testing.T, h *testHarness, method, path, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.pool.URL+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw
}

func listGroups(t *testing.T, h *testHarness) []map[string]interface{} {
	t.Helper()
	code, raw := grpReq(t, h, http.MethodGet, "/admin/groups", "")
	if code != http.StatusOK {
		t.Fatalf("list groups = %d: %s", code, raw)
	}
	var gs []map[string]interface{}
	if err := json.Unmarshal(raw, &gs); err != nil {
		t.Fatalf("decode groups: %v (%s)", err, raw)
	}
	return gs
}

func findGroup(gs []map[string]interface{}, name string) map[string]interface{} {
	for _, g := range gs {
		if g["name"] == name {
			return g
		}
	}
	return nil
}

func TestMultiGroupCRUDAndReassign(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })
	acc := h.importAccount(t, "a", "up-a", "tok-a")

	// Account-pool groups reject user policy instead of silently retaining it.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-a","force_model":"gpt-5","force_effort":"high"}`); code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "invalid_group_policy") {
		t.Fatalf("create group with user policy = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-a","egress_ids":[]}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, body)
	}
	// Duplicate create → 409.
	if code, _ := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-a"}`); code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", code)
	}
	// List includes team-a without user policy fields populated.
	if g := findGroup(listGroups(t, h), "team-a"); g == nil || g["force_model"] != "" || g["force_effort"] != "" {
		t.Fatalf("team-a retained user policy: %v", g)
	}
	if code, body := grpReq(t, h, http.MethodPatch, "/admin/groups/team-a", `{"force_effort":"max"}`); code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "invalid_group_policy") {
		t.Fatalf("patch group with user policy = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodPatch, "/admin/groups/team-a", `{"egress_ids":[]}`); code != http.StatusOK {
		t.Fatalf("patch group egresses = %d: %s", code, body)
	}

	// Single reassign → team-a.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+acc+"/group", `{"group":"team-a"}`); code != http.StatusOK {
		t.Fatalf("reassign = %d: %s", code, body)
	}
	// Delete guard: team-a now has a member → 409.
	if code, _ := grpReq(t, h, http.MethodDelete, "/admin/groups/team-a", ""); code != http.StatusConflict {
		t.Fatalf("delete non-empty group = %d, want 409", code)
	}
	// Bulk reassign back to cyber, then delete the now-empty group.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/assign-group", `{"ids":["`+acc+`"],"group":"cyber"}`); code != http.StatusOK {
		t.Fatalf("bulk reassign = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodDelete, "/admin/groups/team-a", ""); code != http.StatusOK {
		t.Fatalf("delete empty group = %d: %s", code, body)
	}
	if findGroup(listGroups(t, h), "team-a") != nil {
		t.Fatalf("team-a still present after delete")
	}

	// Guards: cannot delete the default group; cannot reassign to a non-existent group.
	if code, _ := grpReq(t, h, http.MethodDelete, "/admin/groups/cyber", ""); code != http.StatusBadRequest {
		t.Fatalf("delete default group = %d, want 400", code)
	}
	if code, _ := grpReq(t, h, http.MethodPost, "/admin/accounts/"+acc+"/group", `{"group":"ghost"}`); code != http.StatusBadRequest {
		t.Fatalf("reassign to missing group = %d, want 400", code)
	}
}

func TestGroupsExposeAccountCountsAndGroupMovesInvalidateSchedulerCache(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })
	accActive := h.importAccount(t, "active", "up-active", "tok-active")
	accDisabled := h.importAccount(t, "disabled", "up-disabled", "tok-disabled")
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-counts"}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accActive+"/group", `{"group":"team-counts"}`); code != http.StatusOK {
		t.Fatalf("move active = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accDisabled+"/group", `{"group":"team-counts"}`); code != http.StatusOK {
		t.Fatalf("move disabled = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accDisabled+"/disable", ``); code != http.StatusOK {
		t.Fatalf("disable = %d: %s", code, body)
	}
	g := findGroup(listGroups(t, h), "team-counts")
	if g == nil {
		t.Fatal("team-counts not listed")
	}
	if g["account_count"] != float64(2) || g["active_account_count"] != float64(1) {
		t.Fatalf("group counts = %#v, want account_count=2 active_account_count=1", g)
	}

	raw, err := os.ReadFile("admin_accounts.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, fn := range []string{"adminSetAccountGroup", "adminAccountsAssignGroup"} {
		body := functionBody(t, source, fn)
		if !strings.Contains(body, ".InvalidateAccountCache()") {
			t.Fatalf("%s must call scheduler.InvalidateAccountCache after group changes", fn)
		}
	}
}
