package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

func TestUserGroupModelInstructionsFilesBecomeResponsesInstructions(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_instr","status":"completed","output_text":"ok"}`))
	})
	save := func(name, content string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"name": name, "content": content})
		if code, raw := grpReq(t, h, http.MethodPost, "/admin/model-instructions", string(body)); code != http.StatusOK {
			t.Fatalf("save %s = %d: %s", name, code, raw)
		}
	}
	save("coding-style.md", "  Use concise Go.  \n")
	save("testing.txt", "\nPrefer regression tests.\n")
	routeKey := createTestAPIKeyForUserGroup(t, h, "codex-team", map[string]interface{}{
		"model_instruction_profiles": map[string]interface{}{
			"gpt":    map[string]interface{}{"enabled": true, "files": []string{"coding-style.md", "testing.txt"}},
			"claude": map[string]interface{}{"enabled": false, "files": []string{}},
			"gemini": map[string]interface{}{"enabled": false, "files": []string{}},
		},
	})
	acc := h.importAccount(t, "instr", "up-instr", "access-instr")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+acc+"/group", `{"group":"codex-team"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, acc, "gpt", 272000)

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","instructions":"downstream should not be prepended","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+routeKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var upstream map[string]interface{}
	if err := json.Unmarshal([]byte(reqs[0].Body), &upstream); err != nil {
		t.Fatalf("upstream json: %v\n%s", err, reqs[0].Body)
	}
	want := "Use concise Go.\n\nPrefer regression tests."
	if upstream["instructions"] != want {
		t.Fatalf("instructions = %#v, want %#v; body=%s", upstream["instructions"], want, reqs[0].Body)
	}
	for _, forbidden := range []string{"coding-style.md", "testing.txt", "instruction_bundle_hash"} {
		if strings.Contains(reqs[0].Body, forbidden) {
			t.Fatalf("diagnostic/file metadata leaked into prompt body: %s", reqs[0].Body)
		}
	}

	req, _ = http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses/compact", strings.NewReader(`{"model":"gpt","input":"compact me"}`))
	req.Header.Set("Authorization", "Bearer "+routeKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d", resp.StatusCode)
	}
	reqs = h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests after compact = %d", len(reqs))
	}
	if err := json.Unmarshal([]byte(reqs[1].Body), &upstream); err != nil {
		t.Fatalf("compact upstream json: %v\n%s", err, reqs[1].Body)
	}
	if upstream["instructions"] != want {
		t.Fatalf("compact instructions = %#v, want %#v", upstream["instructions"], want)
	}

	req, _ = http.NewRequest(http.MethodPost, h.pool.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"chat hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+routeKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	reqs = h.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests after chat = %d", len(reqs))
	}
	if err := json.Unmarshal([]byte(reqs[2].Body), &upstream); err != nil {
		t.Fatalf("chat upstream json: %v\n%s", err, reqs[2].Body)
	}
	if upstream["instructions"] != want {
		t.Fatalf("chat instructions = %#v, want %#v", upstream["instructions"], want)
	}
}

func TestSuperInstructSkillsBecomeResponsesInstructions(t *testing.T) {
	dir := t.TempDir()
	writeAPISuperSkill(t, dir, "anti-debug", `---
name: anti-debug
description: Anti debug workflow
---
# Anti Debug
ANTI DEBUG DIRECTIVE`)
	writeAPISuperSkill(t, dir, "rei-fallback", `---
name: rei-fallback
description: Fallback workflow
---
# Rei
REI DIRECTIVE MUST NOT LOAD`)
	t.Setenv("CODEX_POOL_SUPER_INSTRUCT_DIR", dir)

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_super_instr","status":"completed","output_text":"ok"}`))
	})
	routeKey := createTestAPIKeyForUserGroup(t, h, "super-codex-team", map[string]interface{}{
		"super_instruct_enabled":   true,
		"super_instruct_skill_ids": []string{"anti-debug"},
	})
	acc := h.importAccount(t, "super-instr", "up-super-instr", "access-super-instr")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+acc+"/group", `{"group":"super-codex-team"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, acc, "gpt", 272000)

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","instructions":"downstream should be replaced","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+routeKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var upstream map[string]interface{}
	if err := json.Unmarshal([]byte(reqs[0].Body), &upstream); err != nil {
		t.Fatalf("upstream json: %v\n%s", err, reqs[0].Body)
	}
	instructions, _ := upstream["instructions"].(string)
	for _, want := range []string{"downstream should be replaced", "Super-Instruct Codex 5.6", "ANTI DEBUG DIRECTIVE"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions missing %q:\n%s", want, instructions)
		}
	}
	for _, forbidden := range []string{"REI DIRECTIVE MUST NOT LOAD"} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("instructions contains forbidden %q:\n%s", forbidden, instructions)
		}
	}
}

func TestHeadlessLocalSuperInstructEnablesFullFallbackAndRespectsProfiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex-skills")
	writeAPISuperSkill(t, dir, "alpha", "---\nname: alpha\ndescription: Alpha workflow\n---\nALPHA LOCAL DIRECTIVE")
	writeAPISuperSkill(t, dir, "beta", "---\nname: beta\ndescription: Beta workflow\n---\nBETA LOCAL DIRECTIVE")
	bridgePath := filepath.Join(root, "bridge.md")
	if err := os.WriteFile(bridgePath, []byte("LOCAL M1 BRIDGE"), 0o600); err != nil {
		t.Fatal(err)
	}
	instructionDir := filepath.Join(root, "model-instructions")
	if err := os.MkdirAll(instructionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instructionDir, "group.md"), []byte("GROUP FILE ADDITION"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_POOL_SUPER_INSTRUCT_DIR", dir)
	t.Setenv("CODEX_POOL_SUPER_INSTRUCT_BRIDGE_FILE", bridgePath)

	s := &Server{cfg: config.Config{SuperInstructLocalEnabled: true, DatabasePath: filepath.Join(root, "pool.sqlite3")}}
	effective := s.effectiveSuperInstructPolicy(storage.Group{
		Name:                     "local-default",
		SystemPrompt:             "GROUP CONFIG ADDITION",
		ModelInstructionsEnabled: true,
		ModelInstructionsFiles:   []string{"group.md"},
	})
	profile, family := superInstructPolicyForModel(effective, "gpt-5.6-sol")
	if family != "legacy" || !profile.Enabled || !profile.ResponseRewriteEnabled || !profile.MemoryEnabled || !profile.MonitorEnabled {
		t.Fatalf("local fallback profile = %+v family=%q", profile, family)
	}
	compiled, _, err := s.compileGroupSuperInstructForModel(t.Context(), effective, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ALPHA LOCAL DIRECTIVE", "BETA LOCAL DIRECTIVE"} {
		if !strings.Contains(compiled, want) {
			t.Fatalf("local fallback did not load every installed skill; missing %q:\n%s", want, compiled)
		}
	}
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"OLD DIRECTIVE","input":[{"role":"system","content":"OLD SYSTEM"},{"role":"user","content":"keep me"}]}`)
	injected, err := s.applyModelInstructionsForEntrypoint(t.Context(), effective, "gpt-5.6-sol", "/v1/responses", raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(injected, &envelope); err != nil {
		t.Fatal(err)
	}
	wantM1 := envelope["instructions"].(string)
	for _, want := range []string{"LOCAL M1 BRIDGE", "GROUP CONFIG ADDITION", "GROUP FILE ADDITION", "ALPHA LOCAL DIRECTIVE", "BETA LOCAL DIRECTIVE"} {
		if !strings.Contains(wantM1, want) {
			t.Fatalf("M1 did not append resolved group content %q:\n%s", want, wantM1)
		}
	}
	if strings.Contains(wantM1, "OLD DIRECTIVE") {
		t.Fatalf("M1 retained replaced downstream instructions: %s", wantM1)
	}
	input := envelope["input"].([]interface{})
	if input[0].(map[string]interface{})["content"] != wantM1 || input[1].(map[string]interface{})["content"] != "keep me" {
		t.Fatalf("M1 system carrier replacement mismatch: %s", injected)
	}
	messageRaw := []byte(`{"model":"gemini-3-flash","system":"OLD ANTHROPIC SYSTEM","messages":[{"role":"user","content":"first"},{"role":"system","content":"OLD MESSAGE SYSTEM"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`)
	messageInjected, err := s.applyModelInstructionsForEntrypoint(t.Context(), effective, "gemini-3-flash", "/v1/messages", messageRaw)
	if err != nil {
		t.Fatal(err)
	}
	var messageEnvelope map[string]interface{}
	if err := json.Unmarshal(messageInjected, &messageEnvelope); err != nil {
		t.Fatal(err)
	}
	messageSystem, _ := messageEnvelope["system"].(string)
	if !strings.Contains(messageSystem, "LOCAL M1 BRIDGE") || strings.Contains(messageSystem, "OLD ANTHROPIC SYSTEM") {
		t.Fatalf("M1 Anthropic top-level system replacement mismatch: %s", messageInjected)
	}
	for index, value := range messageEnvelope["messages"].([]interface{}) {
		if value.(map[string]interface{})["role"] == "system" {
			t.Fatalf("M1 inserted unsupported messages[%d] system role: %s", index, messageInjected)
		}
	}
	if strings.Contains(string(messageInjected), "OLD MESSAGE SYSTEM") || len(messageEnvelope["messages"].([]interface{})) != 3 {
		t.Fatalf("M1 retained a replaced Anthropic message system carrier: %s", messageInjected)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attached, ok := s.attachUserGroupPolicy(httptest.NewRecorder(), req, downstreamPolicy{})
	if !ok {
		t.Fatal("local fallback was not attached to an ungrouped request")
	}
	attachedProfile, _ := superInstructPolicyForModel(requestUserGroupPolicy(attached.Context()), "gpt-5.6-sol")
	if !attachedProfile.Enabled || !attachedProfile.ResponseRewriteEnabled || !attachedProfile.MemoryEnabled || !attachedProfile.MonitorEnabled {
		t.Fatalf("ungrouped request did not receive the local fallback: %+v", attachedProfile)
	}

	explicit := storage.Group{SuperInstructProfiles: storage.SuperInstructProfiles{
		storage.ModelInstructionFamilyGPT: {Enabled: false},
	}}
	got := s.effectiveSuperInstructPolicy(explicit)
	gotProfile, _ := superInstructPolicyForModel(got, "gpt-5.6-sol")
	if gotProfile.Enabled || gotProfile.ResponseRewriteEnabled || gotProfile.MemoryEnabled || gotProfile.MonitorEnabled {
		t.Fatalf("explicit disabled profile was overwritten by local fallback: %+v", gotProfile)
	}
	responseOpts := s.effectiveSuperInstructResponseFeatures(got, "gpt-5.6-sol")
	if !responseOpts.ResponseRewriteEnabled || !responseOpts.MemoryEnabled || !responseOpts.MonitorEnabled {
		t.Fatalf("local M3/M5/M6 chain was gated by group policy: %+v", responseOpts)
	}
}

func TestSuperInstructProfilesSelectSkillsByModelFamily(t *testing.T) {
	dir := t.TempDir()
	writeAPISuperSkill(t, dir, "gpt-skill", `---
name: gpt-skill
description: GPT workflow
---
# GPT
GPT ONLY DIRECTIVE`)
	writeAPISuperSkill(t, dir, "claude-skill", `---
name: claude-skill
description: Claude workflow
---
# Claude
CLAUDE ONLY DIRECTIVE`)
	t.Setenv("CODEX_POOL_SUPER_INSTRUCT_DIR", dir)

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	group := storage.Group{
		Name: "super-profile-team",
		SuperInstructProfiles: storage.SuperInstructProfiles{
			storage.ModelInstructionFamilyGPT:    {Enabled: true, SkillIDs: []string{"gpt-skill"}},
			storage.ModelInstructionFamilyClaude: {Enabled: true, SkillIDs: []string{"claude-skill"}},
		},
	}
	gptCompiled, _, err := h.app.compileGroupModelInstructionsForModel(t.Context(), group, "chatgpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gptCompiled, "GPT ONLY DIRECTIVE") || strings.Contains(gptCompiled, "CLAUDE ONLY DIRECTIVE") {
		t.Fatalf("gpt compiled profile mismatch:\n%s", gptCompiled)
	}
	claudeCompiled, _, err := h.app.compileGroupModelInstructionsForModel(t.Context(), group, "claude-sonnet-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(claudeCompiled, "CLAUDE ONLY DIRECTIVE") || strings.Contains(claudeCompiled, "GPT ONLY DIRECTIVE") {
		t.Fatalf("claude compiled profile mismatch:\n%s", claudeCompiled)
	}
	geminiCompiled, _, err := h.app.compileGroupModelInstructionsForModel(t.Context(), group, "gemini-3.2-pro")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(geminiCompiled) != "" {
		t.Fatalf("gemini should be disabled without a profile, got:\n%s", geminiCompiled)
	}
}

func TestCodexInstructionPlanLocalM1ReplacesSystemCarriers(t *testing.T) {
	snapshot := encodeCodexInstructionSnapshot(codexInstructionSnapshotV2{
		Bridge:        "SNAPSHOT BRIDGE",
		GroupPrompt:   "SNAPSHOT GROUP CONFIG",
		Administrator: "SNAPSHOT FILE CONFIG",
		SuperInstruct: "SNAPSHOT SKILL CONFIG",
	})
	plan := newCodexInstructionPlan(snapshot, "revision", "tree", "snapshot", true)
	plan.LocalM1 = true
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"old","input":[{"type":"additional_tools","role":"developer","tools":[]},{"role":"developer","content":"native base"},{"role":"user","content":"keep"}]}`)
	got := plan.apply(raw)
	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	want := joinInstructionParts("SNAPSHOT BRIDGE", "SNAPSHOT GROUP CONFIG", "SNAPSHOT FILE CONFIG", "SNAPSHOT SKILL CONFIG")
	if root["instructions"] != want {
		t.Fatalf("M1 top-level replacement = %#v, want %#v", root["instructions"], want)
	}
	input := root["input"].([]interface{})
	if len(input) != 4 || input[0].(map[string]interface{})["type"] != "additional_tools" ||
		input[1].(map[string]interface{})["role"] != "system" ||
		input[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"] != want {
		t.Fatalf("M1 did not insert the source-compatible Responses system carrier: %s", got)
	}
	if input[3].(map[string]interface{})["content"] != "keep" {
		t.Fatalf("M1 changed non-system Codex Lite items: %s", got)
	}
}

func TestCodexInstructionPlanLocalM1KeepsLiteToolsThroughCustomBridge(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex_lite_initial_plan.json")
	if err != nil {
		t.Fatal(err)
	}
	plan := newCodexInstructionPlan("BRIDGE", "revision", "tree", "snapshot", true)
	plan.LocalM1 = true
	got := plan.apply(raw)
	if !upstream.CodexRequestUsesResponsesLite(got) {
		t.Fatalf("M1 changed the request out of the Responses Lite envelope: %s", got)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]interface{})
	if input[0].(map[string]interface{})["type"] != "additional_tools" || input[1].(map[string]interface{})["role"] != "system" {
		t.Fatalf("Lite tool/system ordering changed: %s", got)
	}
	converted, err := prompt.ResponsesRequestToChatCompletionBridge(got)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(converted.Body, &chat); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, value := range chat["tools"].([]interface{}) {
		function := value.(map[string]interface{})["function"].(map[string]interface{})
		seen[function["name"].(string)] = true
	}
	if !seen["exec"] || !seen["request_user_input"] {
		t.Fatalf("Lite tools disappeared in Responses-to-custom bridge: %s", converted.Body)
	}
}

func TestModelInstructionProfilesSelectFilesByModelFamily(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for name, content := range map[string]string{
		"gpt.md":    "GPT instructions",
		"claude.md": "Claude instructions",
		"gemini.md": "Gemini instructions",
	} {
		body, _ := json.Marshal(map[string]string{"name": name, "content": content})
		if code, raw := grpReq(t, h, http.MethodPost, "/admin/model-instructions", string(body)); code != http.StatusOK {
			t.Fatalf("save %s = %d: %s", name, code, raw)
		}
	}
	group := storage.Group{Name: "family-policy", ModelInstructionProfiles: storage.ModelInstructionProfiles{
		storage.ModelInstructionFamilyGPT:    {Enabled: true, Files: []string{"gpt.md"}},
		storage.ModelInstructionFamilyClaude: {Enabled: true, Files: []string{"claude.md"}},
		storage.ModelInstructionFamilyGemini: {Enabled: true, Files: []string{"gemini.md"}},
	}}
	for model, want := range map[string]string{
		"gpt-5.6-sol":     "GPT instructions",
		"claude-sonnet-5": "Claude instructions",
		"gemini-3.1-pro":  "Gemini instructions",
	} {
		compiled, _, err := h.app.compileGroupModelInstructionsForModel(t.Context(), group, model)
		if err != nil || compiled != want {
			t.Fatalf("model %s compiled=%q want=%q err=%v", model, compiled, want, err)
		}
	}
	compiled, _, err := h.app.compileGroupModelInstructionsForModel(t.Context(), group, "custom-unknown")
	if err != nil || compiled != "" {
		t.Fatalf("unknown model must not receive a cross-family profile: compiled=%q err=%v", compiled, err)
	}
}

func TestAdminUserGroupRejectsEnabledInstructionProfileWithoutFiles(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"broken-instructions",
		"targets":[{"kind":"account_pool_group","id":"cyber"}],
		"model_instruction_profiles":{"claude":{"enabled":true,"files":[]}}
	}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "invalid_model_instruction_policy") {
		t.Fatalf("enabled empty profile = %d, want 422 invalid_model_instruction_policy: %s", code, raw)
	}
}

func TestSetResponsesInstructionsReplacesLiteDeveloperBase(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","opaque":{"exact":900719925474099312345},"instructions":"top-level old","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec","format":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"exact":900719925474099312345}]}`)
	got := setResponsesInstructions(raw, "new compiled instructions")
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if _, present := root["instructions"]; present {
		t.Fatalf("Lite override leaked a top-level instructions field: %s", got)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(root["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 3 {
		t.Fatalf("Lite override changed input item count: %s", got)
	}
	if !strings.Contains(string(input[0]), `"const":900719925474099312345`) || !strings.Contains(string(root["opaque"]), `900719925474099312345`) {
		t.Fatalf("Lite prefix or unrelated large integer changed: %s", got)
	}
	var developer struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(input[1], &developer); err != nil {
		t.Fatal(err)
	}
	if len(developer.Content) != 1 || developer.Content[0].Text != "new compiled instructions" {
		t.Fatalf("Lite developer base was not replaced: %s", got)
	}
	if strings.Contains(string(got), "old base") || strings.Contains(string(got), "top-level old") ||
		!strings.Contains(string(input[2]), `"exact":900719925474099312345`) {
		t.Fatalf("Lite base replacement or top-level conversion mismatch: %s", got)
	}
}

func TestSetResponsesInstructionsAppendsSuperInstructToLiteNativeBase(t *testing.T) {
	const bundle = "# Super-Instruct Codex 5.6\n\nACTIVE SKILL DIRECTIVE"
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"stale top level","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec","format":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"native agent baseline"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"native tool policy"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"exact":900719925474099312345}]}`)
	got := setResponsesInstructions(raw, bundle)
	// Retries/recovery may shape the same body again. The additive bundle must be
	// idempotent rather than growing on every attempt.
	got = setResponsesInstructions(got, bundle)

	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if _, present := root["instructions"]; present {
		t.Fatalf("Lite request retained top-level instructions: %s", got)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(root["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 6 {
		t.Fatalf("Lite native messages were collapsed or additions missing: %s", got)
	}
	for index, want := range map[int]string{
		1: "native agent baseline",
		2: "stale top level",
		3: bundle,
		4: "native tool policy",
	} {
		if gotText := developerItemText(t, input[index]); gotText != want {
			t.Fatalf("developer item %d=%q, want %q: %s", index, gotText, want, got)
		}
	}
	if countDeveloperText(input, bundle) != 1 || !strings.Contains(string(input[5]), `"exact":900719925474099312345`) {
		t.Fatalf("Super-Instruct append was not idempotent or exact user JSON changed: %s", got)
	}
}

func TestSetResponsesInstructionsReplacesOnlyLiteBaseMessage(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"first stale base"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"second stale base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"keep user turn"}]}]}`)
	got := setResponsesInstructions(raw, "the only administrator base")
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(root["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 4 || !strings.Contains(string(input[1]), "the only administrator base") ||
		strings.Contains(string(input[1]), "first stale base") || !strings.Contains(string(input[2]), "second stale base") ||
		!strings.Contains(string(input[3]), "keep user turn") {
		t.Fatalf("Lite base replacement changed a dynamic developer message: %s", got)
	}
}

func TestSetResponsesInstructionsDoesNotInventLitePrefix(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"user","tools":[]},{"role":"user","content":"hi"}]}`)
	got := setResponsesInstructions(raw, "operator base")
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["instructions"]) != `"operator base"` {
		t.Fatalf("unrecognized envelope should remain classic: %s", got)
	}
	if strings.Contains(string(got), `"role":"developer","content":[{"type":"input_text","text":"operator base"}]`) {
		t.Fatalf("unrecognized envelope gained Lite developer prefix: %s", got)
	}
}

func TestSetResponsesInstructionPartsPreservesOfficialLiteInitialGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex_lite_initial_plan.json")
	if err != nil {
		t.Fatal(err)
	}
	const groupPrompt = "GROUP SYSTEM PROMPT"
	const superCompiled = "# Super-Instruct Codex 5.6\n\nSUPER SKILL DIRECTIVE"
	got := setResponsesInstructionParts(raw, groupPrompt, "", superCompiled)
	got = setResponsesInstructionParts(got, groupPrompt, "", superCompiled)

	beforeFields := decodeRawObject(t, raw)
	afterFields := decodeRawObject(t, got)
	if _, present := afterFields["instructions"]; present {
		t.Fatalf("Lite top-level instructions were not converted: %s", got)
	}
	for _, key := range []string{
		"model", "store", "stream", "parallel_tool_calls", "reasoning", "text",
		"service_tier", "prompt_cache_key", "client_metadata",
	} {
		if !bytes.Equal(beforeFields[key], afterFields[key]) {
			t.Fatalf("official Lite field %q changed\nbefore=%s\n after=%s", key, beforeFields[key], afterFields[key])
		}
	}
	beforeInput := decodeRawItems(t, beforeFields["input"])
	afterInput := decodeRawItems(t, afterFields["input"])
	if len(afterInput) != len(beforeInput)+3 {
		t.Fatalf("initial Lite item count=%d, want %d: %s", len(afterInput), len(beforeInput)+3, got)
	}
	if !bytes.Equal(afterInput[0], beforeInput[0]) || !bytes.Equal(afterInput[1], beforeInput[1]) {
		t.Fatalf("additional_tools or native base changed: %s", got)
	}
	for index := 2; index < len(beforeInput); index++ {
		if !bytes.Equal(afterInput[index+3], beforeInput[index]) {
			t.Fatalf("native dynamic item %d changed or moved incorrectly\nbefore=%s\n after=%s", index, beforeInput[index], afterInput[index+3])
		}
	}
	for index, want := range []string{"native Lite top-level instructions", groupPrompt, superCompiled} {
		if gotText := developerItemText(t, afterInput[index+2]); gotText != want {
			t.Fatalf("injected developer item %d=%q, want %q", index+2, gotText, want)
		}
	}
	additional := string(afterInput[0])
	if !strings.Contains(additional, `"name": "request_user_input"`) || strings.Contains(additional, `"name": "update_plan"`) {
		t.Fatalf("Plan Mode tool policy changed: %s", additional)
	}
	if countDeveloperText(afterInput, superCompiled) != 1 || countDeveloperText(afterInput, groupPrompt) != 1 {
		t.Fatalf("retry injection was not idempotent: %s", got)
	}
}

func TestSetResponsesInstructionPartsPreservesOfficialLiteContinuationGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex_lite_continuation_plan.json")
	if err != nil {
		t.Fatal(err)
	}
	const groupPrompt = "GROUP CONTINUATION PROMPT"
	const superCompiled = "# Super-Instruct Codex 5.6\n\nCONTINUATION SKILL"
	got := setResponsesInstructionParts(raw, groupPrompt, "", superCompiled)
	got = setResponsesInstructionParts(got, groupPrompt, "", superCompiled)

	beforeFields := decodeRawObject(t, raw)
	afterFields := decodeRawObject(t, got)
	if _, present := afterFields["instructions"]; present {
		t.Fatalf("continuation retained top-level instructions: %s", got)
	}
	for _, key := range []string{
		"model", "stream", "parallel_tool_calls", "reasoning", "text", "service_tier",
		"prompt_cache_key", "previous_response_id", "client_metadata",
	} {
		if !bytes.Equal(beforeFields[key], afterFields[key]) {
			t.Fatalf("continuation field %q changed\nbefore=%s\n after=%s", key, beforeFields[key], afterFields[key])
		}
	}
	beforeInput := decodeRawItems(t, beforeFields["input"])
	afterInput := decodeRawItems(t, afterFields["input"])
	if len(afterInput) != len(beforeInput)+3 || !bytes.Equal(afterInput[0], beforeInput[0]) {
		t.Fatalf("continuation base/order mismatch: %s", got)
	}
	for index := 1; index < len(beforeInput); index++ {
		if !bytes.Equal(afterInput[index+3], beforeInput[index]) {
			t.Fatalf("continuation native item %d changed: %s", index, got)
		}
	}
	for index, want := range []string{"continuation top-level instructions", groupPrompt, superCompiled} {
		if gotText := developerItemText(t, afterInput[index+1]); gotText != want {
			t.Fatalf("continuation injected item %d=%q, want %q", index+1, gotText, want)
		}
	}
	if countDeveloperText(afterInput, superCompiled) != 1 || countDeveloperText(afterInput, groupPrompt) != 1 {
		t.Fatalf("continuation retry injection was not idempotent: %s", got)
	}
}

func TestSetResponsesInstructionsClassicAppendAndAdministratorReplacement(t *testing.T) {
	const superCompiled = "# Super-Instruct Codex 5.6\n\nCLASSIC SKILL"
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"native classic instructions","reasoning":{"effort":"high"},"text":{"verbosity":"low"},"service_tier":"priority","input":"hi","exact":900719925474099312345}`)
	got := setResponsesInstructions(raw, superCompiled)
	got = setResponsesInstructions(got, superCompiled)
	fields := decodeRawObject(t, got)
	var instructions string
	if err := json.Unmarshal(fields["instructions"], &instructions); err != nil {
		t.Fatal(err)
	}
	if instructions != "native classic instructions\n\n"+superCompiled || strings.Count(instructions, superCompiled) != 1 {
		t.Fatalf("classic Super-Instruct was not appended idempotently: %q", instructions)
	}
	before := decodeRawObject(t, raw)
	for _, key := range []string{"reasoning", "text", "service_tier", "input", "exact"} {
		if !bytes.Equal(before[key], fields[key]) {
			t.Fatalf("classic field %q changed: %s", key, got)
		}
	}

	replaced := setResponsesInstructions(raw, "administrator replacement")
	fields = decodeRawObject(t, replaced)
	if string(fields["instructions"]) != `"administrator replacement"` || strings.Contains(string(replaced), "native classic instructions") {
		t.Fatalf("administrator replacement semantics changed: %s", replaced)
	}
}

func decodeRawObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func decodeRawItems(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func developerItemText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	if item.Type != "message" || item.Role != "developer" || len(item.Content) != 1 || item.Content[0].Type != "input_text" {
		t.Fatalf("not a single developer input_text item: %s", raw)
	}
	return item.Content[0].Text
}

func countDeveloperText(items []json.RawMessage, want string) int {
	count := 0
	for _, raw := range items {
		var item struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" || item.Role != "developer" {
			continue
		}
		for _, block := range item.Content {
			if block.Type == "input_text" && block.Text == want {
				count++
			}
		}
	}
	return count
}

func TestUserGroupModelInstructionsMissingFileIsConfigurationError(t *testing.T) {
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	routeKey := createTestAPIKeyForUserGroup(t, h, "broken-instr", map[string]interface{}{
		"model_instructions_enabled": true,
		"model_instructions_files":   []string{"missing.md"},
	})
	acc := h.importAccount(t, "broken", "up-broken", "access-broken")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+acc+"/group", `{"group":"broken-instr"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, acc, "gpt", 272000)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+routeKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing file status = %d body=%s", resp.StatusCode, raw)
	}
	if upstreamCalls != 0 {
		t.Fatalf("configuration error should not call upstream, calls=%d", upstreamCalls)
	}
	if !strings.Contains(string(raw), "missing.md") || !strings.Contains(string(raw), "codex_instruction_configuration_error") {
		t.Fatalf("error should mention missing file: %s", raw)
	}
}

func TestStrictCPAModelInstructionsAreTreeSnapshots(t *testing.T) {
	var captured []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, string(body))
		w.Header().Set("Content-Type", "application/json")
		switch len(captured) {
		case 1:
			_, _ = w.Write([]byte(`{"id":"resp-instruction-snapshot-1","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			_, _ = w.Write([]byte(`{"id":"resp-instruction-snapshot-2","object":"response","model":"gpt","status":"completed","output":[]}`))
		default:
			_, _ = w.Write([]byte(`{"id":"resp-instruction-snapshot-new-root","object":"response","model":"gpt","status":"completed","output":[]}`))
		}
	})
	enableCodexSessionMappingForTest(h)
	save := func(content string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"name": "strict.md", "content": content})
		if code, raw := grpReq(t, h, http.MethodPost, "/admin/model-instructions", string(body)); code != http.StatusOK {
			t.Fatalf("save instructions = %d: %s", code, raw)
		}
	}
	save("first immutable administrator instructions")
	key := createTestAPIKeyForUserGroup(t, h, "strict-snapshot", map[string]interface{}{
		"model_instructions_enabled": true,
		"model_instructions_files":   []string{"strict.md"},
	})
	accountID := h.importAccount(t, "strict-snapshot", "up-strict-snapshot", "access-strict-snapshot")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/group", `{"group":"strict-snapshot"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, accountID, "gpt", 272000)
	post := func(body string) ([]byte, *http.Response) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "strict-instruction-root")
		req.Header.Set("Session-Id", "strict-instruction-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return bodyBytes, resp
	}
	if body, resp := post(`{"model":"gpt","instructions":"downstream must be replaced","input":"root"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-instruction-snapshot-1") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "pending_root" {
		t.Fatalf("root status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}
	// A file update is deliberately visible only to a later root. The continuation
	// must use the snapshot committed with the completed first response.
	save("second mutable administrator instructions")
	if body, resp := post(`{"model":"gpt","previous_response_id":"resp-instruction-snapshot-1","input":"continue"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-instruction-snapshot-2") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "snapshot" {
		t.Fatalf("continuation status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}
	if len(captured) != 2 {
		t.Fatalf("upstream calls before rotation=%d", len(captured))
	}
	for i, body := range captured {
		if !strings.Contains(body, "first immutable administrator instructions") || strings.Contains(body, "second mutable administrator instructions") || strings.Contains(body, "downstream must be replaced") {
			t.Fatalf("request %d did not use immutable tree instructions: %s", i+1, body)
		}
	}
	// Retiring an epoch turns a later self-contained request into a fresh root.
	// It must read the current operator configuration rather than inherit the old
	// tree snapshot through the retired anchor used only to allocate the next epoch.
	namespace := codexNativeNamespaceForTest(t, hashAPIKey(key), "strict-instruction-root")
	rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "root", Value: "strict-instruction-root"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("root mapping before rotation rows=%+v err=%v", rows, err)
	}
	if _, err := h.store.RetireCodexSessionTree(context.Background(), rows[0].ID, rows[0].Epoch); err != nil {
		t.Fatalf("retire root tree: %v", err)
	}
	if body, resp := post(`{"model":"gpt","input":"fresh root after epoch rotation"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-instruction-snapshot-new-root") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "pending_root" {
		t.Fatalf("new root status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}
	if len(captured) != 3 || !strings.Contains(captured[2], "second mutable administrator instructions") || strings.Contains(captured[2], "first immutable administrator instructions") {
		t.Fatalf("fresh root did not compile current configuration: %v", captured)
	}
	var snapshotCount int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM codex_instruction_snapshot`).Scan(&snapshotCount); err != nil {
		t.Fatal(err)
	}
	if snapshotCount != 2 {
		t.Fatalf("snapshot rows=%d, want 2", snapshotCount)
	}
}

func TestStrictCPASuperInstructAndGroupPromptAreTreeSnapshots(t *testing.T) {
	dir := t.TempDir()
	writeAPISuperSkill(t, dir, "snapshot-skill", `---
name: snapshot-skill
description: Snapshot workflow
---
# Snapshot Skill
FIRST SUPER INSTRUCT DIRECTIVE`)
	t.Setenv("CODEX_POOL_SUPER_INSTRUCT_DIR", dir)

	var captured []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, string(body))
		w.Header().Set("Content-Type", "application/json")
		ids := []string{"resp-super-snapshot-1", "resp-super-snapshot-2", "resp-super-snapshot-new-root"}
		index := min(len(captured)-1, len(ids)-1)
		_, _ = w.Write([]byte(`{"id":"` + ids[index] + `","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	key := createTestAPIKeyForUserGroup(t, h, "strict-super-snapshot", map[string]interface{}{
		"system_prompt":                     "FIRST GROUP PROMPT",
		"super_instruct_enabled":            true,
		"super_instruct_skill_ids":          []string{"snapshot-skill"},
		"prompt_mode":                       "always",
		"system_prompt_apply_to_compaction": true,
	})
	accountID := h.importAccount(t, "strict-super-snapshot", "up-strict-super-snapshot", "access-strict-super-snapshot")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/group", `{"group":"strict-super-snapshot"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, accountID, "gpt", 272000)

	groups, err := h.store.ListUserGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var policy storage.UserGroup
	for _, candidate := range groups {
		if candidate.Name == "strict-super-snapshot users" {
			policy = candidate
			break
		}
	}
	if policy.ID == "" {
		t.Fatalf("created user group not found: %+v", groups)
	}
	post := func(body string) ([]byte, *http.Response) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "strict-super-snapshot-root")
		req.Header.Set("Session-Id", "strict-super-snapshot-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return responseBody, resp
	}
	if body, resp := post(`{"model":"gpt","instructions":"native root instructions","input":"root"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-super-snapshot-1") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "pending_root" {
		t.Fatalf("root status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}

	writeAPISuperSkill(t, dir, "snapshot-skill", `---
name: snapshot-skill
description: Snapshot workflow
---
# Snapshot Skill
SECOND SUPER INSTRUCT DIRECTIVE`)
	policy.SystemPrompt = "SECOND GROUP PROMPT"
	if err := h.store.ReplaceUserGroupDefinition(context.Background(), policy); err != nil {
		t.Fatalf("update user group: %v", err)
	}
	if body, resp := post(`{"model":"gpt","previous_response_id":"resp-super-snapshot-1","input":"continue"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-super-snapshot-2") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "snapshot" {
		t.Fatalf("continuation status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}
	if len(captured) != 2 {
		t.Fatalf("upstream calls before rotation=%d", len(captured))
	}
	for index, body := range captured {
		for _, want := range []string{"FIRST GROUP PROMPT", "FIRST SUPER INSTRUCT DIRECTIVE"} {
			if !strings.Contains(body, want) {
				t.Fatalf("request %d missing snapshotted %q: %s", index+1, want, body)
			}
		}
		for _, forbidden := range []string{"SECOND GROUP PROMPT", "SECOND SUPER INSTRUCT DIRECTIVE"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("request %d used mutable %q: %s", index+1, forbidden, body)
			}
		}
	}

	namespace := codexNativeNamespaceForTest(t, hashAPIKey(key), "strict-super-snapshot-root")
	rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "root", Value: "strict-super-snapshot-root"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("root mapping before rotation rows=%+v err=%v", rows, err)
	}
	if _, err := h.store.RetireCodexSessionTree(context.Background(), rows[0].ID, rows[0].Epoch); err != nil {
		t.Fatalf("retire root tree: %v", err)
	}
	if body, resp := post(`{"model":"gpt","input":"fresh root after epoch rotation"}`); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-super-snapshot-new-root") || resp.Header.Get("X-MiCliProxy-CPA-Instructions") != "pending_root" {
		t.Fatalf("new root status=%d instruction-state=%q body=%s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-CPA-Instructions"), body)
	}
	if len(captured) != 3 || !strings.Contains(captured[2], "SECOND GROUP PROMPT") || !strings.Contains(captured[2], "SECOND SUPER INSTRUCT DIRECTIVE") || strings.Contains(captured[2], "FIRST GROUP PROMPT") || strings.Contains(captured[2], "FIRST SUPER INSTRUCT DIRECTIVE") {
		t.Fatalf("fresh root did not compile current group/Super-Instruct policy: %v", captured)
	}
}

func TestStrictCPAModelInstructionsRejectBrokenNewRootBeforeUpstream(t *testing.T) {
	var calls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id":"unexpected"}`))
	})
	enableCodexSessionMappingForTest(h)
	key := createTestAPIKeyForUserGroup(t, h, "strict-broken", map[string]interface{}{
		"model_instructions_enabled": true,
		"model_instructions_files":   []string{"missing.md"},
	})
	accountID := h.importAccount(t, "strict-broken", "up-strict-broken", "access-strict-broken")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/group", `{"group":"strict-broken"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, accountID, "gpt", 272000)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":"new root"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "strict-broken-root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls != 0 || !strings.Contains(string(body), "missing.md") {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls, body)
	}
}

func createTestAPIKeyForGroup(t *testing.T, h *testHarness, group string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"label": "route-" + group, "group_name": group})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/api-keys", string(body))
	if code != http.StatusCreated {
		t.Fatalf("create api key for group %s = %d: %s", group, code, raw)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode api key: %v (%s)", err, raw)
	}
	key, _ := out["key"].(string)
	if key == "" {
		t.Fatalf("api key response missing key: %v", out)
	}
	return key
}

func createTestAPIKeyForUserGroup(t *testing.T, h *testHarness, accountPoolGroup string, policy map[string]interface{}) string {
	t.Helper()
	groupBody, _ := json.Marshal(map[string]interface{}{"name": accountPoolGroup})
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/groups", string(groupBody)); code != http.StatusOK {
		t.Fatalf("create account pool group %s = %d: %s", accountPoolGroup, code, raw)
	}
	definition := map[string]interface{}{
		"name": accountPoolGroup + " users",
		"targets": []map[string]string{{
			"kind": storage.TargetKindAccountPoolGroup,
			"id":   accountPoolGroup,
		}},
	}
	for key, value := range policy {
		definition[key] = value
	}
	definitionBody, _ := json.Marshal(definition)
	code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", string(definitionBody))
	if code != http.StatusCreated {
		t.Fatalf("create user group for %s = %d: %s", accountPoolGroup, code, raw)
	}
	var userGroup storage.UserGroup
	if err := json.Unmarshal(raw, &userGroup); err != nil || userGroup.ID == "" {
		t.Fatalf("decode user group for %s: %+v err=%v (%s)", accountPoolGroup, userGroup, err, raw)
	}
	keyBody, _ := json.Marshal(map[string]string{"label": "route-" + accountPoolGroup, "user_group_id": userGroup.ID})
	code, raw = grpReq(t, h, http.MethodPost, "/admin/api-keys", string(keyBody))
	if code != http.StatusCreated {
		t.Fatalf("create api key for user group %s = %d: %s", userGroup.ID, code, raw)
	}
	var keyPayload map[string]interface{}
	if err := json.Unmarshal(raw, &keyPayload); err != nil {
		t.Fatalf("decode api key for user group %s: %v (%s)", userGroup.ID, err, raw)
	}
	plain, _ := keyPayload["key"].(string)
	if plain == "" {
		t.Fatalf("api key for user group %s missing plaintext: %s", userGroup.ID, raw)
	}
	return plain
}
