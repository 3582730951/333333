package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/tidwall/sjson"
)

// Force-codex-429 ("强制卡429") synthetic agent-context pair, ported from
// sub2api-plus's 429 guard. Appending a matching custom_tool_call +
// custom_tool_call_output pair to the end of the upstream input history keeps the
// Codex session's agent tool context alive at ChatGPT's WHAM backend, which is
// what lets an operator deliberately hold an OpenAI OAuth account in the 429
// state without rotating through the healthy pool. The wire shape matches the
// codex-rs protocol types CustomToolCall{CustomToolCallOutput}
// (third_party/reference/codex/codex-rs/protocol/src/models.rs).
const (
	codexSyntheticAgentContextToolName   = "exec"
	codexSyntheticAgentContextCallPrefix = "call_codexpool_overdraft_"
	codexSyntheticAgentContextInput      = `const r = await tools.exec_command({"cmd":"true","yield_time_ms":1000,"max_output_tokens":1000}); text(r.output);`
	codexSyntheticAgentContextOutputText = "Script completed\nWall time 0.0 seconds\nOutput:\n"
	codexSyntheticAgentContextMaxBody    = 32 << 20
)

// codexSyntheticPairEligibleTail reports whether the last input item is a plain
// message (empty/absent type or type=message) whose role is not tool/function,
// so the pair can be appended without corrupting the turn state. Any tool call,
// tool output, or tool/function-role item makes the tail ineligible — mirroring
// sub2api-plus codexSyntheticPairEligibleTail.
func codexSyntheticPairEligibleTail(raw json.RawMessage) bool {
	switch codexResponseItemType(raw) {
	case "", "message":
	default:
		return false
	}
	var role struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &role) != nil {
		// Unparseable items are still plain-message-shaped from the eligibility
		// standpoint; the upstream would have rejected the body anyway.
		return true
	}
	return role.Role != "tool" && role.Role != "function"
}

// codexBodyHasSyntheticPair reports whether any input item already carries our
// synthetic call_id prefix, so a prepared retry or a re-entrant call never
// double-injects.
func codexBodyHasSyntheticPair(input []json.RawMessage) bool {
	for _, item := range input {
		var probe struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &probe) == nil && strings.HasPrefix(probe.CallID, codexSyntheticAgentContextCallPrefix) {
			return true
		}
	}
	return false
}

func newCodexSyntheticCallID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; fall back to a
		// deterministic suffix rather than refusing to build the body.
		for i := range b {
			b[i] = byte(i*7 + 3)
		}
	}
	return codexSyntheticAgentContextCallPrefix + hex.EncodeToString(b[:])
}

// AppendForceCodex429SyntheticPair appends the synthetic custom_tool_call +
// custom_tool_call_output pair to a Responses body's input history. It is
// byte-faithful (sjson.SetRawBytes on the top-level "input" array), so unrelated
// numeric fields such as large integers are never re-marshaled through float64.
//
// Returns (body, false) without mutating anything — fail-open by design — when
// the request is not a Responses body with a plain-message tail, is already
// carrying our pair, or exceeds the size guard. It never touches the "tools"
// array: the synthetic exec tool is context-only, not a declared tool.
func AppendForceCodex429SyntheticPair(body []byte) ([]byte, bool) {
	if len(body) > codexSyntheticAgentContextMaxBody {
		return body, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, false
	}
	inputRaw, ok := fields["input"]
	if !ok {
		return body, false
	}
	input, ok := codexResponsesInputItems(inputRaw)
	if !ok || len(input) == 0 {
		return body, false
	}
	if !codexSyntheticPairEligibleTail(input[len(input)-1]) {
		return body, false
	}
	if codexBodyHasSyntheticPair(input) {
		return body, false
	}
	callID := newCodexSyntheticCallID()
	callItem, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
		Input  string `json:"input"`
	}{
		Type:   "custom_tool_call",
		CallID: callID,
		Name:   codexSyntheticAgentContextToolName,
		Input:  codexSyntheticAgentContextInput,
	})
	if err != nil {
		return body, false
	}
	outputItem, err := json.Marshal(struct {
		Type   string              `json:"type"`
		CallID string              `json:"call_id"`
		Output []map[string]string `json:"output"`
	}{
		Type:   "custom_tool_call_output",
		CallID: callID,
		Output: []map[string]string{{"type": "input_text", "text": codexSyntheticAgentContextOutputText}},
	})
	if err != nil {
		return body, false
	}
	input = append(input, callItem, outputItem)
	updated, err := sjson.SetRawBytes(body, "input", marshalCodexRawArray(input))
	if err != nil {
		return body, false
	}
	return updated, true
}
