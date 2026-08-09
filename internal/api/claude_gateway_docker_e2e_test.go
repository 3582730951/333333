package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	claudeGatewayE2EHost          = "pool-gateway-e2e.invalid"
	claudeGatewayE2EKey           = "cap_gateway_e2e_valid"
	claudeGatewayE2EStaleKey      = "stale_official_key_must_not_escape"
	claudeGatewayE2ESkillName     = "bundled-gateway-e2e"
	claudeGatewayE2ESkillSentinel = "BUNDLED_GATEWAY_SKILL_8F791D2C"
	claudeGatewayE2EPrompt        = "BUNDLED_GATEWAY_PROMPT_1A5B8E77"
	claudeGatewayE2EReply         = "BUNDLED_GATEWAY_OK_7DC6A093"
)

// TestClaudeCodeBundledGatewayDockerEndToEnd covers the client path that the
// one-click installer actually creates:
//
//	gateway init -> trust-ca -> install-wrapper -> start-background
//	  -> gateway run-claude -> local proxy -> pool
//
// It intentionally poisons ANTHROPIC_API_KEY before launching the real,
// version-pinned Claude Code. The runtime must remove that direct credential,
// send only the bundled bearer token, and preserve the current beta
// query/header, streaming body, tool schemas and project Skill.
//
//	CLAUDE_GATEWAY_DOCKER_E2E=1 go test ./internal/api \
//	  -run '^TestClaudeCodeBundledGatewayDockerEndToEnd$' -count=1 -v
func TestClaudeCodeBundledGatewayDockerEndToEnd(t *testing.T) {
	if os.Getenv("CLAUDE_GATEWAY_DOCKER_E2E") != "1" {
		t.Skip("set CLAUDE_GATEWAY_DOCKER_E2E=1 to run the bundled gateway Docker end-to-end test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker is required: %v", err)
	}

	var capture claudeGatewayE2ECapture
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.serve(w, r)
	}))
	defer pool.Close()

	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(poolURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	aliasedPoolURL := "http://" + net.JoinHostPort(claudeGatewayE2EHost, port)

	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binDir, 0o777); err != nil {
		t.Fatal(err)
	}
	gatewayBin := filepath.Join(binDir, "gateway")
	buildGatewayForDockerE2E(t, gatewayBin)

	// InstallWrapper expects a real `claude` executable on PATH. This small
	// launcher remains the backed-up claude.real and pins the official client.
	realClaude := filepath.Join(binDir, "claude")
	launcher := fmt.Sprintf("#!/bin/sh\nexec npx -y '@anthropic-ai/claude-code@%s' \"$@\"\n", claudeCodeE2EVersion)
	if err := os.WriteFile(realClaude, []byte(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	countProbe := filepath.Join(binDir, "claude-count-probe")
	countProbeScript := `#!/usr/bin/env node
const http = require("http");
const target = new URL(process.env.ANTHROPIC_BASE_URL + "/v1/messages/count_tokens");
const body = JSON.stringify({model:"claude-opus-5",messages:[{role:"user",content:"count gateway probe"}]});
const request = http.request({
  host: "127.0.0.1",
  port: 8765,
  method: "POST",
  path: target.toString(),
  headers: {
    host: target.host,
    authorization: "Bearer " + process.env.ANTHROPIC_AUTH_TOKEN,
    "anthropic-version": "2023-06-01",
    "anthropic-beta": "claude-code-20250219",
    "content-type": "application/json",
    "content-length": Buffer.byteLength(body)
  }
}, response => {
  let output = "";
  response.on("data", chunk => output += chunk);
  response.on("end", () => {
    process.stdout.write(output);
    if (response.statusCode !== 200) process.exitCode = 1;
  });
});
request.on("error", error => { console.error(error); process.exit(1); });
request.end(body);
`
	if err := os.WriteFile(countProbe, []byte(countProbeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	skillDir := filepath.Join(workDir, ".claude", "skills", claudeGatewayE2ESkillName)
	if err := os.MkdirAll(skillDir, 0o777); err != nil {
		t.Fatal(err)
	}
	skill := `---
name: bundled-gateway-e2e
description: Verifies Skill discovery through the installed bundled gateway (` + claudeGatewayE2ESkillSentinel + `).
---

` + claudeGatewayE2ESkillSentinel + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	shell := `
set -eu
export PATH="/work/bin:$PATH"
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy
if ! command -v update-ca-certificates >/dev/null 2>&1; then
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates >/dev/null
fi
su -s /bin/sh node -c '
  set -eu
  export HOME=/home/node
  export PATH="/work/bin:$PATH"
  gateway init --pool-url "$POOL_E2E_URL" --key "$POOL_E2E_KEY"
'
HOME=/home/node gateway trust-ca
su -s /bin/sh node -c '
  set -eu
  export HOME=/home/node
  export PATH="/work/bin:$PATH"
  export CLAUDE_CONFIG_DIR=/tmp/claude-config
  export npm_config_cache=/tmp/npm-cache
  gateway install-wrapper
  gateway start-background
  trap "gateway stop >/dev/null 2>&1 || true" EXIT
  gateway status
  export ANTHROPIC_API_KEY="$STALE_ANTHROPIC_API_KEY"
  claude --version > /work/bundled-gateway-version.txt
  claude -p \
    --model claude-opus-5 \
    --tools "Read,Skill" \
    --dangerously-skip-permissions \
    --setting-sources project \
    --no-session-persistence \
    --no-chrome \
    --output-format json \
    "$POOL_E2E_PROMPT" > /work/bundled-gateway-output.json
  CLAUDE_REAL_BIN=/work/bin/claude-count-probe \
    gateway run-claude -- > /work/bundled-gateway-count.json
'
`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker",
		"run", "--rm",
		"--network", "host",
		"--add-host", claudeGatewayE2EHost+":127.0.0.1",
		"--workdir", "/work",
		"-v", workDir+":/work",
		"-e", "HOME=/root",
		"-e", "POOL_E2E_URL="+aliasedPoolURL,
		"-e", "POOL_E2E_KEY="+claudeGatewayE2EKey,
		"-e", "POOL_E2E_PROMPT="+claudeGatewayE2EPrompt,
		"-e", "STALE_ANTHROPIC_API_KEY="+claudeGatewayE2EStaleKey,
		"node:22-bookworm-slim",
		"sh", "-lc", shell,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bundled gateway Docker E2E timed out: %v\n%s", ctx.Err(), compactE2ECommandOutput(output))
	}
	if err != nil {
		t.Fatalf("bundled gateway Docker E2E failed: %v\n%s", err, compactE2ECommandOutput(output))
	}

	version, err := os.ReadFile(filepath.Join(workDir, "bundled-gateway-version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(version), claudeCodeE2EVersion) {
		t.Fatalf("Claude Code version = %q, want %s", strings.TrimSpace(string(version)), claudeCodeE2EVersion)
	}
	result, err := os.ReadFile(filepath.Join(workDir, "bundled-gateway-output.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), claudeGatewayE2EReply) {
		t.Fatalf("bundled gateway output missing %q:\n%s", claudeGatewayE2EReply, result)
	}
	countResult, err := os.ReadFile(filepath.Join(workDir, "bundled-gateway-count.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(countResult), `"input_tokens":128`) {
		t.Fatalf("bundled gateway count_tokens output invalid:\n%s", countResult)
	}

	snapshot := capture.snapshot()
	if len(snapshot.errors) > 0 {
		t.Fatalf("bundled gateway protocol violations:\n%s", strings.Join(snapshot.errors, "\n"))
	}
	if snapshot.identityRequests < 2 {
		t.Fatalf("identity requests = %d, want installer/status and runtime probes", snapshot.identityRequests)
	}
	if snapshot.messageRequests == 0 {
		t.Fatal("real Claude Code message did not traverse the bundled local gateway")
	}
	if snapshot.countTokenRequests == 0 {
		t.Fatal("count_tokens did not traverse the bundled local gateway")
	}
	if !snapshot.betaQuery || !snapshot.anthropicVersion || !snapshot.anthropicBeta || !snapshot.stream {
		t.Fatalf("latest Claude request fingerprint was not preserved: beta_query=%v version=%v beta=%v stream=%v",
			snapshot.betaQuery, snapshot.anthropicVersion, snapshot.anthropicBeta, snapshot.stream)
	}
	if !snapshot.skillSeen {
		t.Fatalf("project Skill sentinel %q was not preserved", claudeGatewayE2ESkillSentinel)
	}
	for _, tool := range []string{"Read", "Skill"} {
		if !snapshot.tools[tool] {
			t.Errorf("official Claude Code tool %q was not preserved through the bundled gateway", tool)
		}
	}
	t.Logf("Claude Code %s passed installed gateway path: identities=%d messages=%d count_tokens=%d tools=%v",
		claudeCodeE2EVersion, snapshot.identityRequests, snapshot.messageRequests, snapshot.countTokenRequests,
		sortedClaudeCodeToolNames(snapshot.tools))
}

type claudeGatewayE2ECapture struct {
	mu                 sync.Mutex
	errors             []string
	identityRequests   int
	messageRequests    int
	countTokenRequests int
	betaQuery          bool
	anthropicVersion   bool
	anthropicBeta      bool
	stream             bool
	skillSeen          bool
	tools              map[string]bool
}

type claudeGatewayE2ESnapshot struct {
	errors             []string
	identityRequests   int
	messageRequests    int
	countTokenRequests int
	betaQuery          bool
	anthropicVersion   bool
	anthropicBeta      bool
	stream             bool
	skillSeen          bool
	tools              map[string]bool
}

func (c *claudeGatewayE2ECapture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz", "/readyz":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ready":true}`)
		return
	case "/v1/gateway/identity":
		c.mu.Lock()
		c.identityRequests++
		c.checkGatewayHeadersLocked(r, false)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"session_id":"gateway-e2e-session",
			"user_id":"gateway-e2e-user",
			"machine_id":"gateway-e2e-machine",
			"os_name":"linux",
			"os_version":"bookworm",
			"os_release":"bookworm",
			"arch":"amd64",
			"terminal":"xterm-256color",
			"node_version":"22",
			"claude_cli_version":"2.1.226",
			"stainless_package_version":"0.94.0",
			"username":"root",
			"hostname":"gateway-e2e",
			"home_dir":"/root",
			"dns_servers":["1.1.1.1"],
			"gateway_policy":{"unknown_target_policy":"forward","disable_nonessential_env":false}
		}`)
		return
	case "/v1/messages/count_tokens":
		c.mu.Lock()
		c.countTokenRequests++
		c.checkGatewayHeadersLocked(r, true)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":128}`)
		return
	case "/v1/messages":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			c.addError("invalid message JSON: " + err.Error())
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.messageRequests++
		c.checkGatewayHeadersLocked(r, true)
		c.betaQuery = c.betaQuery || r.URL.Query().Get("beta") == "true"
		c.anthropicVersion = c.anthropicVersion || strings.TrimSpace(r.Header.Get("anthropic-version")) != ""
		c.anthropicBeta = c.anthropicBeta || strings.TrimSpace(r.Header.Get("anthropic-beta")) != ""
		if stream, _ := body["stream"].(bool); stream {
			c.stream = true
		}
		if strings.Contains(string(raw), claudeGatewayE2ESkillSentinel) {
			c.skillSeen = true
		}
		if c.tools == nil {
			c.tools = make(map[string]bool)
		}
		for _, tool := range claudeCodeToolDefinitions(body["tools"]) {
			if name, _ := tool["name"].(string); name != "" {
				c.tools[name] = true
			}
		}
		c.mu.Unlock()
		writeClaudeGatewayE2ESSE(w)
		return
	default:
		c.addError(fmt.Sprintf("unexpected pool request: %s %s", r.Method, r.URL.RequestURI()))
		http.NotFound(w, r)
	}
}

func (c *claudeGatewayE2ECapture) checkGatewayHeadersLocked(r *http.Request, expectClaudeCredential bool) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+claudeGatewayE2EKey {
		c.errors = append(c.errors, fmt.Sprintf("%s authorization = %q", r.URL.Path, got))
	}
	if expectClaudeCredential && strings.TrimSpace(r.Header.Get("x-api-key")) != "" {
		c.errors = append(c.errors, fmt.Sprintf("%s unexpected x-api-key = %q", r.URL.Path, r.Header.Get("x-api-key")))
	}
	if strings.Contains(r.Header.Get("x-api-key"), claudeGatewayE2EStaleKey) {
		c.errors = append(c.errors, fmt.Sprintf("%s leaked stale x-api-key", r.URL.Path))
	}
	if got := r.Header.Get("X-Gateway-Mode"); got != "local" {
		c.errors = append(c.errors, fmt.Sprintf("%s X-Gateway-Mode = %q", r.URL.Path, got))
	}
}

func (c *claudeGatewayE2ECapture) addError(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errors = append(c.errors, message)
}

func (c *claudeGatewayE2ECapture) snapshot() claudeGatewayE2ESnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	tools := make(map[string]bool, len(c.tools))
	for name, seen := range c.tools {
		tools[name] = seen
	}
	return claudeGatewayE2ESnapshot{
		errors:             append([]string(nil), c.errors...),
		identityRequests:   c.identityRequests,
		messageRequests:    c.messageRequests,
		countTokenRequests: c.countTokenRequests,
		betaQuery:          c.betaQuery,
		anthropicVersion:   c.anthropicVersion,
		anthropicBeta:      c.anthropicBeta,
		stream:             c.stream,
		skillSeen:          c.skillSeen,
		tools:              tools,
	}
}

func buildGatewayForDockerE2E(t *testing.T, output string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(goBin, "build", "-trimpath", "-o", output, "./cmd/gateway")
	cmd.Dir = repoRoot
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bundled gateway: %v\n%s", err, raw)
	}
	if err := os.Chmod(output, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeGatewayE2ESSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w,
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_gateway_e2e","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":128,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+claudeGatewayE2EReply+`"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}

`)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
