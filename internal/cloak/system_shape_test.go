package cloak

import (
	"encoding/json"
	"testing"

	"codex-account-pool/internal/identity"
)

// TestClaudeSystemAlwaysLeadsWithIdentityBlockInEveryShape pins the invariant every
// upstream body mutation in this repo depends on.
//
// Requests reach the Claude upstream after several passes that can legitimately reshape or
// remove system content: the OpenAI->Anthropic bridge merges all system paragraphs into a
// single STRING, the superinstruct/M1 path deletes the system field outright before
// injecting operator instructions, and goal-continuity replay copies the current request's
// system over a stored one. Every one of those runs before this function. The block count
// and the leading text of `system` are the cheapest structural things an upstream can
// compare against real Claude Code, so whatever shape arrives here, the result must lead
// with the official identity line and must be an ARRAY of blocks — a bare string `system`
// is valid API but not a shape the real CLI emits.
func TestClaudeSystemAlwaysLeadsWithIdentityBlockInEveryShape(t *testing.T) {
	id := identity.For(nil, "acc-system-shape")

	cases := []struct {
		name string
		body string
		// wantExtraBlocks is how many caller blocks must survive after the identity block.
		wantExtraBlocks int
	}{
		{
			name:            "absent system (bridge produced none)",
			body:            `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 0,
		},
		{
			name:            "single merged string (ChatCompletionToAnthropic shape)",
			body:            `{"model":"claude-sonnet-4-5","system":"first paragraph\n\nsecond paragraph","messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 1,
		},
		{
			name:            "empty string system",
			body:            `{"model":"claude-sonnet-4-5","system":"","messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 0,
		},
		{
			name:            "array without the identity line (operator instructions injected)",
			body:            `{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"operator instructions"}],"messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 1,
		},
		{
			name:            "array already led by the identity line (real Claude Code)",
			body:            `{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"env"}],"messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 1,
		},
		{
			name:            "empty array",
			body:            `{"model":"claude-sonnet-4-5","system":[],"messages":[{"role":"user","content":"hi"}]}`,
			wantExtraBlocks: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := VirtualizeClaudeCode([]byte(tc.body), id, nil, true, "")
			root := mustRoot(t, res.Body)

			sys, ok := root["system"].([]interface{})
			if !ok {
				t.Fatalf("system is %T, want []interface{}: real Claude Code never sends a bare string system\n%s", root["system"], res.Body)
			}
			if len(sys) == 0 {
				t.Fatalf("system is empty; the identity block must always be present\n%s", res.Body)
			}
			first, ok := sys[0].(map[string]interface{})
			if !ok {
				t.Fatalf("system[0] is %T, want a text block", sys[0])
			}
			if first["text"] != claudeCodeIdentityLine {
				t.Errorf("system[0].text = %q, want the official identity line", first["text"])
			}
			if first["type"] != "text" {
				t.Errorf("system[0].type = %v, want \"text\"", first["type"])
			}
			if got := len(sys) - 1; got != tc.wantExtraBlocks {
				t.Errorf("caller blocks after the identity block = %d, want %d (content must be neither duplicated nor dropped)\n%s", got, tc.wantExtraBlocks, res.Body)
			}
			// No block may be a bare string: a mixed array is a shape the SDK cannot emit.
			for i, item := range sys {
				if _, isMap := item.(map[string]interface{}); !isMap {
					t.Errorf("system[%d] is %T, want a block object", i, item)
				}
			}
		})
	}
}

// TestClaudeSystemIdentityBlockIsNotDuplicatedOnRepeatedPasses: the pipeline can virtualize
// a body more than once (retry on another account, cache prewarm, quality probe). A second
// pass that prepends a second identity block would produce a block count no real client
// emits, and the duplication grows with every retry.
func TestClaudeSystemIdentityBlockIsNotDuplicatedOnRepeatedPasses(t *testing.T) {
	id := identity.For(nil, "acc-system-idempotent")
	body := []byte(`{"model":"claude-sonnet-4-5","system":"operator text","messages":[{"role":"user","content":"hi"}]}`)

	first := VirtualizeClaudeCode(body, id, nil, true, "").Body
	second := VirtualizeClaudeCode(first, id, nil, true, "").Body

	countIdentity := func(raw []byte) int {
		var root map[string]interface{}
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		sys, _ := root["system"].([]interface{})
		n := 0
		for _, item := range sys {
			if block, ok := item.(map[string]interface{}); ok && block["text"] == claudeCodeIdentityLine {
				n++
			}
		}
		return n
	}

	if got := countIdentity(first); got != 1 {
		t.Fatalf("first pass identity blocks = %d, want 1", got)
	}
	if got := countIdentity(second); got != 1 {
		t.Errorf("second pass identity blocks = %d, want 1: virtualization must be idempotent", got)
	}
}
