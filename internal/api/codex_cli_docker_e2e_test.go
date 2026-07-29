package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCodexCLIDockerLongContextAndSkills exercises the installed official Codex
// CLI in a container against the real gateway stack and a strict mock of the
// Codex Responses backend. It is opt-in because it requires a local Docker
// daemon and the native CLI distribution:
//
//	RUN_CODEX_CLI_DOCKER_E2E=1 go test ./internal/api \
//	  -run TestCodexCLIDockerLongContextAndSkills -count=1
func TestCodexCLIDockerLongContextAndSkills(t *testing.T) {
	if os.Getenv("RUN_CODEX_CLI_DOCKER_E2E") != "1" {
		t.Skip("set RUN_CODEX_CLI_DOCKER_E2E=1 to run the official Codex CLI Docker check")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the default mounted Codex distribution is linux/amd64")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is required: %v", err)
	}

	const (
		beginMarker = "CODEX-LONG-BEGIN-746C"
		midMarker   = "CODEX-LONG-MIDDLE-18A9"
		endMarker   = "CODEX-LONG-END-DB32"
		skillMarker = "CODEX_SKILL_SENTINEL_52EA"
	)
	requests := make(chan []byte, 8)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		stream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_codex_docker","model":"gpt-5.6-sol"}}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","id":"msg_codex_docker","content":[]}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"CODEX_DOCKER_E2E_OK"}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","id":"msg_codex_docker","content":[{"type":"output_text","text":"CODEX_DOCKER_E2E_OK"}]}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_codex_docker","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":220000,"input_tokens_details":null,"output_tokens":4,"output_tokens_details":null,"total_tokens":220004}}}` + "\n\n"
		raw := serveCodexResponsesFixture(t, w, r, stream)
		if len(raw) > 0 {
			select {
			case requests <- append([]byte(nil), raw...):
			default:
			}
		}
	})
	h.importAccount(t, "codex-cli-docker", "upstream-codex-cli-docker", "access-codex-cli-docker")

	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	workdir := filepath.Join(root, "work")
	skillDir := filepath.Join(codexHome, "skills", "context-probe")
	for _, dir := range []string{codexHome, workdir, skillDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf(`model = "gpt-5.6-sol"
model_provider = "pool"
model_context_window = 372000
model_reasoning_effort = "xhigh"
approval_policy = "never"
sandbox_mode = "read-only"

[model_providers.pool]
name = "Pool E2E"
base_url = %q
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
experimental_bearer_token = "docker-e2e-key"
`, h.pool.URL+"/v1")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o666); err != nil {
		t.Fatal(err)
	}
	skill := `---
name: context-probe
description: Verifies that the official Codex skill loader survives the relay.
---
Preserve this exact marker in model context: ` + skillMarker + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "probe.txt"), []byte("container probe\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	vendor := os.Getenv("CODEX_CLI_VENDOR_DIR")
	if vendor == "" {
		vendor = "/usr/local/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl"
	}
	if _, err := os.Stat(filepath.Join(vendor, "bin", "codex")); err != nil {
		t.Fatalf("Codex native distribution not found at %s: %v", vendor, err)
	}
	image := os.Getenv("CODEX_POOL_E2E_IMAGE")
	if image == "" {
		image = "codex-pool:verification"
	}

	longPrompt := "$context-probe\n" + beginMarker +
		strings.Repeat(" stable-codex-context-a", 18*1024) +
		midMarker +
		strings.Repeat(" stable-codex-context-b", 18*1024) +
		endMarker + "\nReply with CODEX_DOCKER_E2E_OK and do not call a tool."

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{
		"run", "--rm", "--network", "host", "--user", "0:0", "-i",
		"-e", "CODEX_HOME=/codex-home",
		"-v", vendor + ":/opt/codex:ro",
		"-v", codexHome + ":/codex-home",
		"-v", workdir + ":/work",
		"-w", "/work",
		"--entrypoint", "/opt/codex/bin/codex",
		image,
		"exec", "--skip-git-repo-check", "--ephemeral", "--color", "never",
		"-m", "gpt-5.6-sol", "-C", "/work", "-",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(longPrompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("official Codex Docker run failed: %v\n%s", err, compactE2ECommandOutput(output))
	}
	if !strings.Contains(string(output), "CODEX_DOCKER_E2E_OK") {
		t.Fatalf("official Codex output missing completion sentinel:\n%s", output)
	}

	var matched []byte
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case raw := <-requests:
			if strings.Contains(string(raw), beginMarker) {
				matched = raw
			}
		case <-deadline:
			break collect
		}
	}
	if len(matched) == 0 {
		t.Fatal("no upstream request contained the long-context beginning marker")
	}
	for _, marker := range []string{beginMarker, midMarker, endMarker, skillMarker} {
		if !strings.Contains(string(matched), marker) {
			t.Fatalf("official Codex request lost marker %q; bytes=%d", marker, len(matched))
		}
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(matched, &payload); err != nil {
		t.Fatalf("decode official Codex upstream request: %v", err)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("official Codex reasoning effort changed: %v", reasoning)
	}
	if !strings.Contains(string(matched), `"type":"additional_tools"`) {
		t.Fatalf("official Codex tools were absent from the Responses Lite envelope; bytes=%d", len(matched))
	}
}

func compactE2ECommandOutput(output []byte) string {
	const limit = 16 << 10
	if len(output) <= limit {
		return string(output)
	}
	return string(output[:2<<10]) + "\n...[command output truncated]...\n" + string(output[len(output)-(limit-2<<10):])
}
