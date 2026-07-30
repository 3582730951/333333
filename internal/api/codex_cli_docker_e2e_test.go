package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCodexCLIDockerLongContextAndSkills exercises the installed official Codex
// CLI in a container against the real gateway stack and a strict mock of the
// Codex Responses backend. In addition to long-context/Skill fidelity it proves
// that command auth refreshes /v1/models, installs the exact 372K/334.8K contract,
// and automatically emits RemoteCompactionV2 before a resumed turn can overflow.
// It is opt-in because it requires a local Docker daemon and native CLI distribution:
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
	type capturedRequest struct {
		path string
		body []byte
	}
	requests := make(chan capturedRequest, 16)
	var normalResponses atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read official Codex upstream request: %v", err)
		}
		select {
		case requests <- capturedRequest{path: r.URL.Path, body: append([]byte(nil), raw...)}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if codexRemoteCompactionV2Request(r, raw) {
			_, _ = io.WriteString(w, codexCLICompactionSSE())
			return
		}
		call := normalResponses.Add(1)
		if call == 1 {
			// The client trusts completed usage for auto-compaction. This crosses
			// 334.8K while remaining beneath the fixed 372K hard window.
			_, _ = io.WriteString(w, codexCLITextSSE(
				"resp_codex_docker_first",
				"CODEX_DOCKER_E2E_OK",
				340000,
			))
			return
		}
		_, _ = io.WriteString(w, codexCLITextSSE(
			fmt.Sprintf("resp_codex_docker_resume_%d", call),
			"CODEX_DOCKER_E2E_RESUME_OK",
			12000,
		))
	})
	accountID := h.importAccount(t, "codex-cli-docker", "upstream-codex-cli-docker", "access-codex-cli-docker")
	setTestCapability(t, h, accountID, "gpt-5.6-sol", 372000)

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
	// Exercise the exact URL+key setup artifact rather than a hand-written test
	// configuration.  A pre-existing table catches the TOML ordering regression
	// where generated root keys were accidentally nested under [features].
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[features]\nremote_compaction_v2 = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setupPath := filepath.Join(root, "setup-pool-cli.sh")
	setupScript := buildCodexConfigScript(h.pool.URL, "docker-e2e-key", "gpt-5.6-sol", "xhigh", "never", "read-only")
	if err := os.WriteFile(setupPath, []byte(setupScript), 0o700); err != nil {
		t.Fatal(err)
	}
	setup := exec.Command("bash", setupPath)
	setup.Env = append(os.Environ(),
		"HOME="+root,
		"CODEX_HOME="+codexHome,
		"POOL_CLIENT=codex",
		"POOL_INSTALL_RTK=0",
		"POOL_CODEX_WEBSOCKETS=0",
	)
	if output, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("generated URL+key Codex setup failed: %v\n%s", err, output)
	}
	configRaw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(configRaw)
	if modelPos, tablePos := strings.Index(config, `model = "gpt-5.6-sol"`), strings.Index(config, "[features]"); modelPos < 0 || tablePos < 0 || modelPos > tablePos {
		t.Fatalf("generated Codex root model key is nested below a preserved table:\n%s", config)
	}
	if !strings.Contains(config, `command = "/bin/cat"`) || !strings.Contains(config, `args = ["`+codexHome+`/pool-token"]`) {
		t.Fatalf("generated Codex config did not install command auth:\n%s", config)
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

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	baseArgs := []string{
		"run", "--rm", "--network", "host", "--user", "0:0", "-i",
		"-e", "CODEX_HOME=" + codexHome,
		"-v", vendor + ":/opt/codex:ro",
		"-v", codexHome + ":" + codexHome,
		"-v", workdir + ":" + workdir,
		"-w", workdir,
		"--entrypoint", "/opt/codex/bin/codex",
		image,
	}
	firstArgs := append(append([]string(nil), baseArgs...),
		"exec", "--skip-git-repo-check", "--ephemeral", "--color", "never",
		"-m", "gpt-5.6-sol", "-C", workdir, "-",
	)
	// This session must persist so the next invocation exercises the real resume
	// and pre-turn auto-compaction path.
	for i, arg := range firstArgs {
		if arg == "--ephemeral" {
			firstArgs = append(firstArgs[:i], firstArgs[i+1:]...)
			break
		}
	}
	cmd := exec.CommandContext(ctx, "docker", firstArgs...)
	cmd.Stdin = strings.NewReader(longPrompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("official Codex Docker run failed: %v\n%s", err, compactE2ECommandOutput(output))
	}
	if !strings.Contains(string(output), "CODEX_DOCKER_E2E_OK") {
		t.Fatalf("official Codex output missing completion sentinel:\n%s", output)
	}

	cacheRaw, err := os.ReadFile(filepath.Join(codexHome, "models_cache.json"))
	if err != nil {
		t.Fatalf("command-auth model refresh did not create models_cache.json: %v", err)
	}
	var cache struct {
		Models []struct {
			Slug                          string `json:"slug"`
			ContextWindow                 int64  `json:"context_window"`
			MaxContextWindow              int64  `json:"max_context_window"`
			AutoCompactTokenLimit         int64  `json:"auto_compact_token_limit"`
			EffectiveContextWindowPercent int64  `json:"effective_context_window_percent"`
		} `json:"models"`
	}
	if err := json.Unmarshal(cacheRaw, &cache); err != nil {
		t.Fatalf("decode official Codex model cache: %v\n%s", err, cacheRaw)
	}
	var fixed56 bool
	for _, model := range cache.Models {
		if model.Slug == "gpt-5.6-sol" {
			fixed56 = model.ContextWindow == 372000 &&
				model.MaxContextWindow == 372000 &&
				model.AutoCompactTokenLimit == 334800 &&
				model.EffectiveContextWindowPercent == 100
		}
	}
	if !fixed56 {
		t.Fatalf("official Codex cache did not install the fixed 372K/334.8K contract:\n%s", cacheRaw)
	}

	resumeArgs := append(append([]string(nil), baseArgs...),
		"exec", "--skip-git-repo-check", "--color", "never",
		"resume", "--last", "Reply with CODEX_DOCKER_E2E_RESUME_OK and do not call a tool.",
	)
	resume := exec.CommandContext(ctx, "docker", resumeArgs...)
	resumeOutput, err := resume.CombinedOutput()
	if err != nil {
		t.Fatalf("official Codex Docker resume failed: %v\n%s", err, compactE2ECommandOutput(resumeOutput))
	}
	if !strings.Contains(string(resumeOutput), "CODEX_DOCKER_E2E_RESUME_OK") {
		t.Fatalf("official Codex resume output missing completion sentinel:\n%s", resumeOutput)
	}

	captured := make([]capturedRequest, 0, 4)
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case request := <-requests:
			captured = append(captured, request)
		case <-deadline:
			break collect
		}
	}
	var matched, compactRequest, postCompactRequest []byte
	for _, request := range captured {
		switch {
		case strings.Contains(string(request.body), beginMarker):
			matched = request.body
		case codexRemoteCompactionV2Request(&http.Request{URL: mustURLForCodexE2E(t, request.path)}, request.body):
			compactRequest = request.body
		case strings.Contains(string(request.body), "REMOTE_COMPACTION_372K_SENTINEL"):
			postCompactRequest = request.body
		}
	}
	if len(matched) == 0 {
		t.Fatalf("no upstream request contained the long-context beginning marker; requests=%d", len(captured))
	}
	for _, marker := range []string{beginMarker, midMarker, endMarker, skillMarker} {
		if !strings.Contains(string(matched), marker) {
			t.Fatalf("official Codex request lost marker %q; bytes=%d", marker, len(matched))
		}
	}
	if len(compactRequest) == 0 {
		t.Fatalf("official Codex never emitted RemoteCompactionV2 after 340K usage; requests=%d", len(captured))
	}
	if len(postCompactRequest) == 0 || !strings.Contains(string(postCompactRequest), `"type":"compaction"`) {
		t.Fatalf("official Codex did not install/replay the compacted checkpoint before resume; requests=%d", len(captured))
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

func codexCLITextSSE(responseID, text string, inputTokens int64) string {
	messageID := "msg_" + responseID
	return "event: response.created\n" +
		fmt.Sprintf(`data: {"type":"response.created","response":{"id":%q,"model":"gpt-5.6-sol"}}`, responseID) + "\n\n" +
		"event: response.output_item.added\n" +
		fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","id":%q,"content":[]}}`, messageID) + "\n\n" +
		"event: response.output_text.delta\n" +
		fmt.Sprintf(`data: {"type":"response.output_text.delta","output_index":0,"delta":%q}`, text) + "\n\n" +
		"event: response.output_item.done\n" +
		fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","id":%q,"content":[{"type":"output_text","text":%q}]}}`, messageID, text) + "\n\n" +
		"event: response.completed\n" +
		fmt.Sprintf(`data: {"type":"response.completed","response":{"id":%q,"model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","role":"assistant","id":%q,"content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":%d,"input_tokens_details":null,"output_tokens":4,"output_tokens_details":null,"total_tokens":%d}}}`, responseID, messageID, text, inputTokens, inputTokens+4) + "\n\n"
}

func codexCLICompactionSSE() string {
	const item = `{"type":"compaction","id":"cmp_codex_docker","encrypted_content":"REMOTE_COMPACTION_372K_SENTINEL"}`
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_codex_docker_compact","model":"gpt-5.6-sol"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_codex_docker_compact","model":"gpt-5.6-sol","status":"completed","output":[` + item + `],"usage":{"input_tokens":12000,"output_tokens":1,"total_tokens":12001}}}` + "\n\n"
}

func mustURLForCodexE2E(t *testing.T, path string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse captured Codex path: %v", err)
	}
	return parsed
}

func compactE2ECommandOutput(output []byte) string {
	const limit = 16 << 10
	if len(output) <= limit {
		return string(output)
	}
	return string(output[:2<<10]) + "\n...[command output truncated]...\n" + string(output[len(output)-(limit-2<<10):])
}
