package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
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

func TestAdminSuperInstructSkillsAndUserGroupPolicy(t *testing.T) {
	dir := t.TempDir()
	writeAPISuperSkill(t, dir, "anti-debug", `---
name: anti-debug
description: Anti debug workflow
---
# Anti Debug
ANTI DEBUG DIRECTIVE`)
	writeAPISuperSkill(t, dir, "reverse-engineering", `---
name: reverse-engineering
description: Reverse workflow
---
# Reverse
REVERSE DIRECTIVE`)
	t.Setenv(superinstruct.EnvDir, dir)

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/super-instruct/skills", "")
	if code != http.StatusOK {
		t.Fatalf("GET super instruct skills = %d: %s", code, raw)
	}
	var catalog struct {
		Directory string                `json:"directory"`
		Skills    []superinstruct.Skill `json:"skills"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("decode skills: %v (%s)", err, raw)
	}
	if catalog.Directory != dir || len(catalog.Skills) != 2 || catalog.Skills[0].ID != "anti-debug" {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"super-team",
		"super_instruct_enabled":true,
		"super_instruct_skill_ids":["anti-debug","anti-debug"],
		"super_instruct_profiles":{
			"chatgpt":{"enabled":true,"skill_ids":["anti-debug","anti-debug"],"response_rewrite_enabled":true},
			"claude":{"monitor_enabled":true}
		},
		"super_instruct_response_rewrite_enabled":true,
		"super_instruct_memory_enabled":true,
		"super_instruct_monitor_enabled":true,
		"targets":[{"kind":"account_pool_group","id":"cyber"}]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("POST super user group = %d: %s", code, raw)
	}
	var created storage.UserGroup
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if !created.SuperInstructEnabled || len(created.SuperInstructSkillIDs) != 1 || created.SuperInstructSkillIDs[0] != "anti-debug" {
		t.Fatalf("super instruct policy not round-tripped: %+v", created)
	}
	if !created.SuperInstructResponseRewriteEnabled || !created.SuperInstructMemoryEnabled || !created.SuperInstructMonitorEnabled {
		t.Fatalf("super instruct response submodules not round-tripped: %+v", created)
	}
	if profile := created.SuperInstructProfiles[storage.ModelInstructionFamilyGPT]; !profile.Enabled || !profile.ResponseRewriteEnabled || len(profile.SkillIDs) != 1 || profile.SkillIDs[0] != "anti-debug" {
		t.Fatalf("gpt super instruct profile not round-tripped: %+v", created.SuperInstructProfiles)
	}
	if profile := created.SuperInstructProfiles[storage.ModelInstructionFamilyClaude]; !profile.MonitorEnabled {
		t.Fatalf("claude super instruct profile not round-tripped: %+v", created.SuperInstructProfiles)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"missing-super-skill",
		"super_instruct_enabled":true,
		"super_instruct_skill_ids":["missing"],
		"targets":[{"kind":"account_pool_group","id":"cyber"}]
	}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST missing super skill = %d, want 422: %s", code, raw)
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

func TestAdminUserGroupsTrafficFallbackRoundTrip(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	fallback := storage.UserGroup{
		ID:      "ug_api_fallback",
		Name:    "api-fallback",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}},
	}
	if err := h.store.CreateUserGroupDefinition(t.Context(), fallback); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"api-fallback-source",
		"targets":[{"kind":"account_pool_group","id":"cyber"}],
		"traffic_fallback_groups":{"gpt":["ug_api_fallback"],"claude":[],"gemini":[]},
		"traffic_fallback_model_mappings":[
			{"family":"gpt","source_model":"gpt-5.6-sol","target_user_group_id":"ug_api_fallback","target_model":"gpt-5.5"},
			{"family":"gpt","source_model":"gpt-5.*","target_user_group_id":"ug_api_fallback","target_model":"gpt-5.4"}
		]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("POST traffic fallback user group = %d: %s", code, raw)
	}
	var created storage.UserGroup
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.TrafficFallbackGroups.GPT) != 1 ||
		created.TrafficFallbackGroups.GPT[0] != fallback.ID ||
		len(created.TrafficFallbackModelMappings) != 2 {
		t.Fatalf("traffic fallback policy missing from response: %+v", created)
	}

	code, raw = grpReq(t, h, http.MethodDelete, "/admin/user-groups/"+fallback.ID, "")
	if code != http.StatusConflict {
		t.Fatalf("DELETE referenced fallback user group = %d, want 409: %s", code, raw)
	}
}

func TestAdminUserGroupsRejectsIncompleteTrafficFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	fallback := storage.UserGroup{
		ID:      "ug_api_incomplete_fallback",
		Name:    "api-incomplete-fallback",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}},
	}
	if err := h.store.CreateUserGroupDefinition(t.Context(), fallback); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"api-incomplete-source",
		"targets":[{"kind":"account_pool_group","id":"cyber"}],
		"traffic_fallback_groups":{"gpt":["ug_api_incomplete_fallback"],"claude":[],"gemini":[]},
		"traffic_fallback_model_mappings":[]
	}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST incomplete traffic fallback = %d, want 422: %s", code, raw)
	}
}

func writeAPISuperSkill(t *testing.T, root, id, content string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
