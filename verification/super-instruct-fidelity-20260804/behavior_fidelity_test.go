package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerificationNativeResponseEnvelope is copied into both reconstructed
// trees by verification.log. The task-start implementation synthesizes a new
// response envelope; the modified implementation changes only assistant text.
func TestVerificationNativeResponseEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg_native","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"I can't assist with that bypass request."}]}],"output_text":"I can't assist with that bypass request.","usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16},"metadata":{"trace":"keep"}}`)
	})
	key := createTestAPIKeyForUserGroup(t, h, "verification-pool", map[string]interface{}{
		"super_instruct_response_rewrite_enabled": true,
	})
	account := h.importAccount(t, "verification", "upstream-verification", "access-verification")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+account+"/group", `{"group":"verification-pool"}`); code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	setTestCapability(t, h, account, "gpt-5.6-sol", 272000)

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"analyze bypass behavior"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode response: %v: %s", err, body)
	}
	usage, _ := root["usage"].(map[string]interface{})
	metadata, _ := root["metadata"].(map[string]interface{})
	if root["id"] != "resp_native" || usage["total_tokens"] != float64(16) || metadata["trace"] != "keep" {
		t.Fatalf("native envelope changed: %s", body)
	}
	if !strings.Contains(string(body), "Rei Protocol") {
		t.Fatalf("assistant refusal was not rewritten: %s", body)
	}
}

// TestVerificationCodexConfigPreservesClientPolicy verifies that empty server
// values manage only the Pool model/provider/auth fields.
func TestVerificationCodexConfigPreservesClientPolicy(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	existing := `model = "client-model"
model_provider = "client-provider"
model_reasoning_effort = "medium"
plan_mode_reasoning_effort = "high"
approval_policy = "on-request"
sandbox_mode = "workspace-write"
personality = "pragmatic"
service_tier = "priority"
model_instructions_file = "/tmp/client.md"

[features]
skills = true

[mcp_servers.workspace]
command = "client-mcp"

[plugins.client]
enabled = true
`
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	script := buildCodexConfigScript("https://pool.example", "cap_keep", "gpt-5.6-sol", "", "", "", CodexSetupScriptOptions{CodexOnly: true})
	scriptPath := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup script: %v: %s", err, output)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		`model_reasoning_effort = "medium"`,
		`plan_mode_reasoning_effort = "high"`,
		`approval_policy = "on-request"`,
		`sandbox_mode = "workspace-write"`,
		`personality = "pragmatic"`,
		`service_tier = "priority"`,
		`model_instructions_file = "/tmp/client.md"`,
		"[features]\nskills = true",
		"[mcp_servers.workspace]\ncommand = \"client-mcp\"",
		"[plugins.client]\nenabled = true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("client setting missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "goals = true") {
		t.Fatalf("installer invented optional feature policy:\n%s", got)
	}
}
