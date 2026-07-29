package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminUserGroupsCreatePersistsBaseAndRelayTargets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "relay-one", Name: "Relay One", BaseURL: "https://relay.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"mixed-targets",
		"block_claude_target_groups":["cyber"],
		"block_gpt_target_groups":["cyber"],
		"targets":[
			{"target_type":"base_group","target_ref":"cyber","affinity_weight":2},
			{"target_type":"relay","target_ref":"relay-one","affinity_weight":1}
		]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("POST user group = %d: %s", code, raw)
	}
	var created storage.UserGroup
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || len(created.Targets) != 2 {
		t.Fatalf("created user group missing targets: %+v", created)
	}
	if len(created.BlockClaudeTargetGroups) != 1 || created.BlockClaudeTargetGroups[0] != "cyber" ||
		len(created.BlockGPTTargetGroups) != 1 || created.BlockGPTTargetGroups[0] != "cyber" {
		t.Fatalf("created user group missing target-family policy: %+v", created)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/user-groups", "")
	if code != http.StatusOK {
		t.Fatalf("GET user groups = %d: %s", code, raw)
	}
	var groups []storage.UserGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, group := range groups {
		if group.ID == created.ID {
			found = len(group.Targets) == 2 &&
				len(group.BlockClaudeTargetGroups) == 1 &&
				len(group.BlockGPTTargetGroups) == 1
		}
	}
	if !found {
		t.Fatalf("list response did not hydrate targets: %+v", groups)
	}
}

func TestAdminUserGroupsRejectsBlockOutsideSelectedAccountPools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"invalid-block-target",
		"block_claude_target_groups":["not-selected"],
		"targets":[{"kind":"account_pool_group","id":"cyber"}]
	}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST invalid block target = %d, want 422: %s", code, raw)
	}
}

func TestAdminUserGroupsRejectsEmptyTargets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{"name":"empty","targets":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST empty targets = %d, want 400: %s", code, raw)
	}
}
