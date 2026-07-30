package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	claudeCodeE2EVersion       = "2.1.220"
	claudeCodeE2EDefaultBytes  = 896 << 10
	claudeCodeE2EMaxBytes      = 8 << 20
	claudeCodeE2EBeginSentinel = "CLAUDE_CODE_LONG_CONTEXT_BEGIN_71A0E0F3"
	claudeCodeE2EMidSentinel   = "CLAUDE_CODE_LONG_CONTEXT_MIDDLE_5B329DC1"
	claudeCodeE2EEndSentinel   = "CLAUDE_CODE_LONG_CONTEXT_END_CE94822A"
	claudeCodeE2EReadSentinel  = "CLAUDE_CODE_READ_RESULT_5F1C79B8"
	claudeCodeE2ESkillName     = "gateway-e2e-discovery"
	claudeCodeE2ESkillSentinel = "CLAUDE_CODE_SKILL_DISCOVERY_3A726F09"
)

// TestClaudeCodeDockerEndToEnd runs the real, version-pinned Claude Code CLI
// against the in-process gateway and a strict Codex Responses mock.
//
// It is intentionally opt-in because it needs Docker and npm registry access:
//
//	CLAUDE_CODE_DOCKER_E2E=1 go test ./internal/api \
//	  -run '^TestClaudeCodeDockerEndToEnd$' -count=1 -v
//
// CLAUDE_CODE_E2E_CONTEXT_BYTES may raise the default 896 KiB prompt up to
// 8 MiB for dedicated large-context hosts. The default leaves room beneath the
// gateway's 372K GPT context ceiling for Claude Code's system prompt and tool
// schemas.
func TestClaudeCodeDockerEndToEnd(t *testing.T) {
	if os.Getenv("CLAUDE_CODE_DOCKER_E2E") != "1" {
		t.Skip("set CLAUDE_CODE_DOCKER_E2E=1 to run the Docker Claude Code end-to-end test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker is required: %v", err)
	}

	contextBytes := claudeCodeE2EDefaultBytes
	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_E2E_CONTEXT_BYTES")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 64<<10 || n > claudeCodeE2EMaxBytes {
			t.Fatalf("CLAUDE_CODE_E2E_CONTEXT_BYTES must be between 65536 and %d, got %q", claudeCodeE2EMaxBytes, raw)
		}
		contextBytes = n
	}
	longContext := claudeCodeE2ELongContext(contextBytes)

	var capture claudeCodeE2ECapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		serveClaudeCodeStrictCodexMock(w, r, func(raw []byte) (int, string) {
			return capture.respond(raw, longContext)
		})
	})
	h.importAccount(t, "claude-code-docker-e2e", "upstream-claude-code-docker-e2e", "access-claude-code-docker-e2e")

	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "fixture.txt"), []byte(claudeCodeE2EReadSentinel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(workDir, ".claude", "skills", claudeCodeE2ESkillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := `---
name: gateway-e2e-discovery
description: A Docker E2E discovery skill used to verify Claude Code skill propagation.
---

# Gateway E2E discovery

` + claudeCodeE2ESkillSentinel + `
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := longContext + "\n\nUse the Read tool exactly once on /work/fixture.txt, then report its exact content."
	if err := os.WriteFile(filepath.Join(workDir, "long-prompt.txt"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}

	shell := `
set -eu
CC='@anthropic-ai/claude-code@` + claudeCodeE2EVersion + `'
npx -y "$CC" --version > /work/claude-version.txt
npx -y "$CC" -p \
  --model gpt-5.5 \
  --effort xhigh \
  --tools "Read,Bash,Edit,Write,Grep,Glob,Agent,Skill" \
  --dangerously-skip-permissions \
  --setting-sources project \
  --no-session-persistence \
  --no-chrome \
  --output-format json \
  < /work/long-prompt.txt > /work/full-tools-output.json
npx -y "$CC" -p \
  --model gpt-5.5 \
  --effort xhigh \
  --tools "" \
  --dangerously-skip-permissions \
  --setting-sources project \
  --no-session-persistence \
  --no-chrome \
  --output-format json \
  "CLAUDE_CODE_NO_TOOLS_PROBE_37BEA1D9" > /work/no-tools-output.json
`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker",
		"run", "--rm",
		"--network", "host",
		"--user", "node",
		"--workdir", "/work",
		"-v", workDir+":/work",
		"-e", "HOME=/home/node",
		"-e", "CLAUDE_CONFIG_DIR=/tmp/claude-config",
		"-e", "ANTHROPIC_BASE_URL="+h.pool.URL,
		"-e", "ANTHROPIC_AUTH_TOKEN=claude-code-docker-e2e-key",
		"-e", "ANTHROPIC_MODEL=gpt-5.5",
		"-e", "ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.5",
		"-e", "ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.5",
		"-e", "ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.5",
		"-e", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		"-e", "CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1",
		"-e", "NO_PROXY=127.0.0.1,localhost",
		"node:22-bookworm-slim",
		"sh", "-lc", shell,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Claude Code Docker E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Claude Code Docker E2E failed: %v\n%s", err, output)
	}

	version, err := os.ReadFile(filepath.Join(workDir, "claude-version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(version), claudeCodeE2EVersion) {
		t.Fatalf("Claude Code version = %q, want %s", strings.TrimSpace(string(version)), claudeCodeE2EVersion)
	}
	for file, want := range map[string]string{
		"full-tools-output.json": "CLAUDE_CODE_E2E_COMPLETE",
		"no-tools-output.json":   "CLAUDE_CODE_NO_TOOLS_OK",
	} {
		raw, readErr := os.ReadFile(filepath.Join(workDir, file))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s missing %q:\n%s", file, want, raw)
		}
	}

	snapshot := capture.snapshot()
	if len(snapshot.errors) > 0 {
		t.Fatalf("strict Codex mock violations:\n%s", strings.Join(snapshot.errors, "\n"))
	}
	if snapshot.strict400 {
		t.Fatal("strict Codex mock returned 400 for an orphan parallel_tool_calls field")
	}
	if !snapshot.noToolsSeen {
		t.Fatal("real Claude Code --tools \"\" request did not reach the strict Codex mock")
	}
	if snapshot.fullTurns < 2 || !snapshot.longFirst || !snapshot.longSecond {
		t.Fatalf("long context was not preserved exactly across the Read round trip: turns=%d first=%v second=%v",
			snapshot.fullTurns, snapshot.longFirst, snapshot.longSecond)
	}
	if !snapshot.readResultSeen {
		t.Fatal("the real Read result did not return to Codex on the second turn")
	}
	if !snapshot.skillSeen {
		t.Fatalf("project skill %q was not discoverable in Claude Code's request", claudeCodeE2ESkillName)
	}
	if !snapshot.xhighPreserved {
		t.Fatal("Claude Code --effort xhigh was lowered or omitted before the Codex mock")
	}
	requiredTools := []string{"Read", "Bash", "Edit", "Write", "Grep", "Glob", "Agent", "Skill"}
	for _, name := range requiredTools {
		if !snapshot.tools[name] {
			t.Errorf("Claude Code %s official tool definition was not visible upstream", name)
		}
	}
	t.Logf("Claude Code %s official tools observed (%d): %s",
		claudeCodeE2EVersion, len(snapshot.tools), strings.Join(sortedClaudeCodeToolNames(snapshot.tools), ", "))
	t.Logf("long context preserved across %d turns: %d bytes", snapshot.fullTurns, len(longContext))
}

type claudeCodeE2ECapture struct {
	mu             sync.Mutex
	errors         []string
	tools          map[string]bool
	fullTurns      int
	longFirst      bool
	longSecond     bool
	readResultSeen bool
	skillSeen      bool
	xhighPreserved bool
	noToolsSeen    bool
	strict400      bool
}

type claudeCodeE2ESnapshot struct {
	errors         []string
	tools          map[string]bool
	fullTurns      int
	longFirst      bool
	longSecond     bool
	readResultSeen bool
	skillSeen      bool
	xhighPreserved bool
	noToolsSeen    bool
	strict400      bool
}

func (c *claudeCodeE2ECapture) respond(raw []byte, longContext string) (int, string) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		c.addError(fmt.Sprintf("invalid Codex request JSON: %v", err))
		return http.StatusBadRequest, `{"error":{"message":"invalid JSON"}}`
	}
	tools := claudeCodeToolDefinitions(root["tools"])
	_, hasParallel := root["parallel_tool_calls"]
	if len(tools) == 0 {
		c.mu.Lock()
		c.noToolsSeen = true
		if hasParallel {
			c.strict400 = true
			c.errors = append(c.errors, "no-tools request contained parallel_tool_calls")
			c.mu.Unlock()
			return http.StatusBadRequest, `{"error":{"message":"parallel_tool_calls requires at least one tool"}}`
		}
		c.mu.Unlock()
		return http.StatusOK, claudeCodeE2ETextResponse("resp_no_tools", "CLAUDE_CODE_NO_TOOLS_OK")
	}

	allStrings := collectClaudeCodeJSONStrings(root, nil)
	hasLong := containsClaudeCodeExactString(allStrings, longContext)
	hasReadResult := containsClaudeCodeString(allStrings, claudeCodeE2EReadSentinel)
	skillSeen := containsClaudeCodeString(allStrings, claudeCodeE2ESkillName) ||
		containsClaudeCodeString(allStrings, claudeCodeE2ESkillSentinel)
	effort, _ := root["reasoning"].(map[string]interface{})["effort"].(string)

	c.mu.Lock()
	if c.tools == nil {
		c.tools = make(map[string]bool)
	}
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name != "" {
			c.tools[name] = true
		}
	}
	c.fullTurns++
	turn := c.fullTurns
	if turn == 1 {
		c.longFirst = hasLong
	} else {
		c.longSecond = c.longSecond || hasLong
	}
	c.readResultSeen = c.readResultSeen || hasReadResult
	c.skillSeen = c.skillSeen || skillSeen
	if effort == "xhigh" {
		c.xhighPreserved = true
	} else {
		c.errors = append(c.errors, fmt.Sprintf("reasoning.effort=%q, want xhigh", effort))
	}
	c.mu.Unlock()

	if !hasLong {
		c.addError(fmt.Sprintf("full-tools turn %d did not contain the exact %d-byte long context", turn, len(longContext)))
	}
	if turn == 1 {
		return http.StatusOK, claudeCodeE2EToolResponse()
	}
	if !hasReadResult {
		c.addError("second full-tools turn did not contain the Read result")
	}
	return http.StatusOK, claudeCodeE2ETextResponse("resp_after_read", "CLAUDE_CODE_E2E_COMPLETE")
}

func (c *claudeCodeE2ECapture) addError(message string) {
	c.mu.Lock()
	c.errors = append(c.errors, message)
	c.mu.Unlock()
}

func (c *claudeCodeE2ECapture) snapshot() claudeCodeE2ESnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	tools := make(map[string]bool, len(c.tools))
	for name, seen := range c.tools {
		tools[name] = seen
	}
	return claudeCodeE2ESnapshot{
		errors:         append([]string(nil), c.errors...),
		tools:          tools,
		fullTurns:      c.fullTurns,
		longFirst:      c.longFirst,
		longSecond:     c.longSecond,
		readResultSeen: c.readResultSeen,
		skillSeen:      c.skillSeen,
		xhighPreserved: c.xhighPreserved,
		noToolsSeen:    c.noToolsSeen,
		strict400:      c.strict400,
	}
}

func claudeCodeE2ELongContext(size int) string {
	if size < len(claudeCodeE2EBeginSentinel)+len(claudeCodeE2EMidSentinel)+len(claudeCodeE2EEndSentinel)+2 {
		panic("Claude Code E2E context size is too small")
	}
	headBytes := (size - len(claudeCodeE2EBeginSentinel) - len(claudeCodeE2EMidSentinel) - len(claudeCodeE2EEndSentinel) - 2) / 2
	tailBytes := size - len(claudeCodeE2EBeginSentinel) - len(claudeCodeE2EMidSentinel) - len(claudeCodeE2EEndSentinel) - 2 - headBytes
	return claudeCodeE2EBeginSentinel + "\n" +
		claudeCodeE2EPattern(headBytes) +
		claudeCodeE2EMidSentinel + "\n" +
		claudeCodeE2EPattern(tailBytes) +
		claudeCodeE2EEndSentinel
}

func claudeCodeE2EPattern(size int) string {
	const unit = "0123456789abcdef "
	var b strings.Builder
	b.Grow(size)
	for b.Len()+len(unit) <= size {
		b.WriteString(unit)
	}
	b.WriteString(unit[:size-b.Len()])
	return b.String()
}

func claudeCodeToolDefinitions(value interface{}) []map[string]interface{} {
	list, _ := value.([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		if tool, ok := item.(map[string]interface{}); ok {
			out = append(out, tool)
		}
	}
	return out
}

func collectClaudeCodeJSONStrings(value interface{}, out []string) []string {
	switch typed := value.(type) {
	case string:
		out = append(out, typed)
	case []interface{}:
		for _, item := range typed {
			out = collectClaudeCodeJSONStrings(item, out)
		}
	case map[string]interface{}:
		for _, item := range typed {
			out = collectClaudeCodeJSONStrings(item, out)
		}
	}
	return out
}

func containsClaudeCodeExactString(values []string, exact string) bool {
	for _, value := range values {
		if strings.Contains(value, exact) {
			return true
		}
	}
	return false
}

func containsClaudeCodeString(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func sortedClaudeCodeToolNames(tools map[string]bool) []string {
	names := make([]string, 0, len(tools))
	for name, seen := range tools {
		if seen {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func claudeCodeE2EToolResponse() string {
	const arguments = `{"file_path":"/work/fixture.txt"}`
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_read","model":"gpt-5.5"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"Read","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_read","output_index":0,"delta":"{\"file_path\":\"/work/fixture.txt\"}"}` + "\n\n" +
		"event: response.function_call_arguments.done\n" +
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_read","output_index":0,"arguments":` + strconv.Quote(arguments) + `}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"Read","arguments":` + strconv.Quote(arguments) + `}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_read","model":"gpt-5.5","status":"completed","usage":{"input_tokens":220000,"output_tokens":8,"total_tokens":220008}}}` + "\n\n"
}

func claudeCodeE2ETextResponse(id, text string) string {
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":` + strconv.Quote(id) + `,"model":"gpt-5.5"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":` + strconv.Quote(text) + `}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":` + strconv.Quote(id) + `,"model":"gpt-5.5","status":"completed","usage":{"input_tokens":16,"output_tokens":4,"total_tokens":20}}}` + "\n\n"
}

func serveClaudeCodeStrictCodexMock(w http.ResponseWriter, r *http.Request, respond func([]byte) (int, string)) {
	if !websocket.IsWebSocketUpgrade(r) {
		raw, _ := io.ReadAll(r.Body)
		status, body := respond(raw)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	status, body := respond(raw)
	if status != http.StatusOK {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_request_error","message":"strict mock rejected request"}}}`))
		return
	}
	for _, frame := range strings.Split(body, "\n\n") {
		_, data := sseFrameEventData([]byte(frame))
		if len(data) == 0 {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}
