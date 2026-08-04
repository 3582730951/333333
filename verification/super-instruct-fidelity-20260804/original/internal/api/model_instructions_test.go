package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
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
	for _, want := range []string{"Super-Instruct Codex 5.6", "ANTI DEBUG DIRECTIVE"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions missing %q:\n%s", want, instructions)
		}
	}
	for _, forbidden := range []string{"downstream should be replaced", "REI DIRECTIVE MUST NOT LOAD"} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("instructions contains forbidden %q:\n%s", forbidden, instructions)
		}
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
	if strings.Contains(string(got), "old base") || !strings.Contains(string(input[2]), `"exact":900719925474099312345`) {
		t.Fatalf("old Lite base instructions survived override: %s", got)
	}
}

func TestSetResponsesInstructionsKeepsExactlyOneLiteBaseMessage(t *testing.T) {
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
	if len(input) != 3 || !strings.Contains(string(input[1]), "the only administrator base") || strings.Contains(string(got), "stale base") || !strings.Contains(string(input[2]), "keep user turn") {
		t.Fatalf("Lite prefix must contain exactly one replacement base message: %s", got)
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
