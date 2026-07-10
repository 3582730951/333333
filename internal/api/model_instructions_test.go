package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGroupModelInstructionsFilesBecomeResponsesInstructions(t *testing.T) {
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
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"codex-team","model_instructions_enabled":true,"model_instructions_files":["coding-style.md","testing.txt"]}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, raw)
	}
	routeKey := createTestAPIKeyForGroup(t, h, "codex-team")
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

func TestSetResponsesInstructionsReplacesLiteDeveloperBase(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	got := setResponsesInstructions(raw, "new compiled instructions")
	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if _, present := root["instructions"]; present {
		t.Fatalf("Lite override leaked a top-level instructions field: %s", got)
	}
	input, _ := root["input"].([]interface{})
	if len(input) != 3 {
		t.Fatalf("Lite override changed input item count: %s", got)
	}
	developer, _ := input[1].(map[string]interface{})
	content, _ := developer["content"].([]interface{})
	if len(content) != 1 || content[0].(map[string]interface{})["text"] != "new compiled instructions" {
		t.Fatalf("Lite developer base was not replaced: %s", got)
	}
	if strings.Contains(string(got), "old base") {
		t.Fatalf("old Lite base instructions survived override: %s", got)
	}
}

func TestGroupModelInstructionsMissingFileIsConfigurationError(t *testing.T) {
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"broken-instr","model_instructions_enabled":true,"model_instructions_files":["missing.md"]}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, raw)
	}
	routeKey := createTestAPIKeyForGroup(t, h, "broken-instr")
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
	if !strings.Contains(string(raw), "missing.md") {
		t.Fatalf("error should mention missing file: %s", raw)
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
