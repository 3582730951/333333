package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// groups_test.go guards Phase ④: multi-group CRUD + account reassignment + the delete
// guards (default group, non-empty group). The per-group ForceModel/ForceEffort routing
// already existed (override.resolveDownstreamPolicy); what's new is creating/deleting
// arbitrary groups and moving accounts between them.

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

	// Create a new group with a forced model + effort.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-a","force_model":"gpt-5","force_effort":"high","virtual_2m_enabled":false}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, body)
	}
	// Duplicate create → 409.
	if code, _ := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-a"}`); code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", code)
	}
	// List includes team-a with the forced fields.
	if g := findGroup(listGroups(t, h), "team-a"); g == nil || g["force_model"] != "gpt-5" || g["force_effort"] != "high" {
		t.Fatalf("team-a not created with forced fields: %v", g)
	}
	// Generic PATCH updates + normalizes effort on the new group ("max" → "xhigh").
	if code, body := grpReq(t, h, http.MethodPatch, "/admin/groups/team-a", `{"force_effort":"max"}`); code != http.StatusOK {
		t.Fatalf("patch group = %d: %s", code, body)
	}
	if g := findGroup(listGroups(t, h), "team-a"); g["force_effort"] != "xhigh" {
		t.Fatalf("force_effort not normalized/updated: %v", g["force_effort"])
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
