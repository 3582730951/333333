package cloak

import (
	"encoding/json"
	"strings"
	"testing"

	"codex-account-pool/internal/identity"
)

func mustRoot(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return m
}

func TestVirtualizeReplacesUserIDInjectsSystemAndRenamesTools(t *testing.T) {
	id := identity.For(nil, "acc-1")
	// Genuine Claude Code (leads with the identity line) → custom tool names are
	// normalized to TitleCase.
	body := []byte(`{"model":"claude","metadata":{"user_id":"real-user-123"},"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"hello"}],"tools":[{"name":"bash"},{"type":"web_search"}]}`)
	res := Virtualize(body, id, nil, true)
	root := mustRoot(t, res.Body)

	metadata, ok := root["metadata"].(map[string]interface{})
	if !ok || len(metadata) != 1 || !userIDShape.MatchString(metadata["user_id"].(string)) {
		t.Fatalf("metadata.user_id must use the captured Claude Code shape: %s", res.Body)
	}
	sys := root["system"].([]interface{})
	if sys[0].(map[string]interface{})["text"] != claudeCodeIdentityLine {
		t.Fatalf("identity block not injected first: %v", sys[0])
	}
	tools := root["tools"].([]interface{})
	if tools[0].(map[string]interface{})["name"] != "Bash" {
		t.Fatalf("custom tool not renamed: %v", tools[0])
	}
	if _, hasName := tools[1].(map[string]interface{})["name"]; hasName {
		t.Fatalf("typed built-in tool (web_search) should be untouched: %v", tools[1])
	}
	if out := res.Scrubber.ReplaceString("data: real-user-123\n\n"); strings.Contains(out, "real-user-123") {
		t.Fatalf("scrubber left the real user id: %q", out)
	}
}

func TestVirtualizeClaudeCodePreservesExitPlanModeTool(t *testing.T) {
	id := identity.For(nil, "acc-plan")
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"tools":[{"name":"ExitPlanMode","input_schema":{"type":"object"}},{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"plan"}]}`)
	res := Virtualize(body, id, nil, true)
	root := mustRoot(t, res.Body)
	tools := root["tools"].([]interface{})
	if got := tools[0].(map[string]interface{})["name"]; got != "ExitPlanMode" {
		t.Fatalf("ExitPlanMode tool must be preserved for Claude Code plan mode, got %v\n%s", got, res.Body)
	}
	if got := tools[1].(map[string]interface{})["name"]; got != "Bash" {
		t.Fatalf("normal Claude Code tools should still be normalized, got %v", got)
	}
}

func TestVirtualizeDoesNotRenameThirdPartyTools(t *testing.T) {
	id := identity.For(nil, "acc-tp")
	// A non-Claude-Code OAuth client (no identity block) that happens to define a tool
	// literally named "read": its tool name must be left ALONE (renaming to "Read"
	// would break the client's own tool-call matching), while the identity block is
	// still injected (Anthropic requires it on OAuth traffic).
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"You are a helpful assistant."}],"tools":[{"name":"read"}]}`)
	res := Virtualize(body, id, nil, true)
	root := mustRoot(t, res.Body)
	tools := root["tools"].([]interface{})
	if got := tools[0].(map[string]interface{})["name"]; got != "read" {
		t.Fatalf("third-party tool name should be preserved, got %v", got)
	}
	sys := root["system"].([]interface{})
	if sys[0].(map[string]interface{})["text"] != claudeCodeIdentityLine {
		t.Fatalf("identity block should still be injected for OAuth: %v", sys[0])
	}
}

func TestVirtualizeNormalizesSystemInfoButKeepsPaths(t *testing.T) {
	id := identity.For(nil, "acc-env")
	envText := "Here is the env:\n<env>\nWorking directory: /Users/realbob/code/app\nPlatform: win32\nOS Version: Windows 10\n</env>"
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude",
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": claudeCodeIdentityLine},
			map[string]interface{}{"type": "text", "text": envText},
		},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "open /Users/realbob/code/app/main.go"},
		},
	})
	res := Virtualize(body, id, nil, true)
	out := string(res.Body)

	// Directories/paths MUST be preserved — rewriting them would break tool use.
	if !strings.Contains(out, "/Users/realbob/code/app") {
		t.Fatalf("working directory must be preserved:\n%s", out)
	}
	if !strings.Contains(out, "/Users/realbob/code/app/main.go") {
		t.Fatalf("message file path must be preserved:\n%s", out)
	}
	// System descriptors are unified to the account identity.
	if want := nodePlatform(id.OSName); want != "" && !strings.Contains(out, "Platform: "+want) {
		t.Fatalf("platform not normalized to %q:\n%s", want, out)
	}
	if id.OSRelease != "" && !strings.Contains(out, "OS Version: "+id.OSRelease) {
		t.Fatalf("OS version not normalized to %q:\n%s", id.OSRelease, out)
	}
}

func TestVirtualizeSensitiveWords(t *testing.T) {
	id := identity.For(nil, "acc-x")
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"token is HUNTER2 ok"}]}`)
	res := Virtualize(body, id, []string{"HUNTER2"}, false)
	if strings.Contains(string(res.Body), "HUNTER2") {
		t.Fatalf("sensitive word not scrubbed in request: %s", res.Body)
	}
	if strings.Contains(res.Scrubber.ReplaceString("...HUNTER2..."), "HUNTER2") {
		t.Fatalf("sensitive word not scrubbed in response")
	}
}

func TestVirtualizeAPIKeyPathIsLightTouch(t *testing.T) {
	id := identity.For(nil, "acc-api")
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"custom"}],"tools":[{"name":"bash"}]}`)
	res := Virtualize(body, id, nil, false) // oauth=false → API key
	root := mustRoot(t, res.Body)
	if root["system"].([]interface{})[0].(map[string]interface{})["text"] != "custom" {
		t.Fatalf("API-key path must not inject the Claude Code system block")
	}
	if root["tools"].([]interface{})[0].(map[string]interface{})["name"] != "bash" {
		t.Fatalf("API-key path must not rename tools")
	}
}

func TestVirtualizeClaudeCodeWithCacheMarksAutoContextPrefix(t *testing.T) {
	id := identity.For(nil, "acc-cache")
	body := []byte(`{"model":"claude","metadata":{"user_id":"real-user-123"},"system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.953; cc_entrypoint=sdk-cli; cch=d4baa;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"stable instructions","cache_control":{"type":"ephemeral"}}` +
		`],"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday.\n</system-reminder>\n\n"},` +
		`{"type":"text","text":"real user request","cache_control":{"type":"ephemeral"}}` +
		`]}]}`)
	res := VirtualizeClaudeCodeWithCache(body, id, nil, true, "2.1.160", ClaudeCodeCacheOptions{NativeBreakpoints: true, TTL: "1h"})
	root := mustRoot(t, res.Body)
	msgs := root["messages"].([]interface{})
	blocks := msgs[0].(map[string]interface{})["content"].([]interface{})
	auto := blocks[0].(map[string]interface{})
	cc, ok := auto["cache_control"].(map[string]interface{})
	if !ok {
		t.Fatalf("auto-context prefix was not marked: %v", auto)
	}
	if cc["ttl"] != "1h" {
		t.Fatalf("native cache ttl not applied: %v", cc)
	}
	if _, has := blocks[1].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("real latest user request marker should be stripped: %v", blocks[1])
	}
	sys := root["system"].([]interface{})
	for _, idx := range []int{1, 2} {
		cc, ok := sys[idx].(map[string]interface{})["cache_control"].(map[string]interface{})
		if !ok || cc["ttl"] != "1h" {
			t.Fatalf("system marker %d should be upgraded to 1h: %v", idx, sys[idx])
		}
	}
	if got := countClaudeCacheControls(root); got != 3 {
		t.Fatalf("cache_control count = %d, want identity+system+auto-context", got)
	}
	if text := blocks[1].(map[string]interface{})["text"]; text != "real user request" {
		t.Fatalf("real user text changed: %v", text)
	}
	if _, has := sys[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("billing block must not carry cache_control")
	}
}

func TestVirtualizeClaudeCodeWithCacheCoarseSafeDoesNotMarkAutoContextPrefix(t *testing.T) {
	id := identity.For(nil, "acc-cache-coarse")
	body := []byte(`{"model":"claude","metadata":{"user_id":"real-user-123"},"system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.953; cc_entrypoint=sdk-cli; cch=d4baa;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"stable instructions","cache_control":{"type":"ephemeral"}}` +
		`],"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday.\n</system-reminder>\n\n"},` +
		`{"type":"text","text":"real user request","cache_control":{"type":"ephemeral"}}` +
		`]}]}`)

	res := VirtualizeClaudeCodeWithCache(body, id, nil, true, "2.1.160", ClaudeCodeCacheOptions{
		NativeBreakpoints: true,
		BreakpointPolicy:  "coarse_safe",
		TTL:               "1h",
	})
	root := mustRoot(t, res.Body)
	blocks := root["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if _, has := blocks[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("coarse_safe must not mark auto-context user block: %v", blocks[0])
	}
	if got := countClaudeCacheControls(root); got != 2 {
		t.Fatalf("coarse_safe cache_control count = %d, want stable system markers only", got)
	}
}

func TestVirtualizeClaudeCodeWithCacheSkipsUnknownShapeAndDisabled(t *testing.T) {
	id := identity.For(nil, "acc-cache-skip")
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.953; cc_entrypoint=sdk-cli; cch=d4baa;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"plain user request"}]}]}`)
	disabled := VirtualizeClaudeCodeWithCache(body, id, nil, true, "2.1.160", ClaudeCodeCacheOptions{NativeBreakpoints: false})
	if got := countClaudeCacheControls(mustRoot(t, disabled.Body)); got != 1 {
		t.Fatalf("disabled native injection changed marker count to %d", got)
	}
	unknown := VirtualizeClaudeCodeWithCache(body, id, nil, true, "2.1.160", ClaudeCodeCacheOptions{NativeBreakpoints: true})
	if got := countClaudeCacheControls(mustRoot(t, unknown.Body)); got != 1 {
		t.Fatalf("unknown message shape should be skipped, marker count = %d", got)
	}
}

func countClaudeCacheControls(root map[string]interface{}) int {
	n := 0
	for _, section := range []string{"system", "tools"} {
		if arr, ok := root[section].([]interface{}); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if _, has := m["cache_control"]; has {
						n++
					}
				}
			}
		}
	}
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			m, _ := msg.(map[string]interface{})
			if arr, ok := m["content"].([]interface{}); ok {
				for _, item := range arr {
					if bm, ok := item.(map[string]interface{}); ok {
						if _, has := bm["cache_control"]; has {
							n++
						}
					}
				}
			}
		}
	}
	return n
}

func TestScrubSensitive(t *testing.T) {
	res := ScrubSensitive([]byte(`{"input":"my key sk-leak-123 here"}`), []string{"sk-leak-123"})
	if strings.Contains(string(res.Body), "sk-leak-123") {
		t.Fatalf("sensitive word not scrubbed: %s", res.Body)
	}
	// Paths are never touched by ScrubSensitive.
	res2 := ScrubSensitive([]byte(`{"cwd":"/Users/real/x"}`), nil)
	if !strings.Contains(string(res2.Body), "/Users/real/x") {
		t.Fatalf("ScrubSensitive must not touch paths: %s", res2.Body)
	}
}
