package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestPrewarmSkipsShapesAnthropicRejectsWithZeroMaxTokens is the regression test for the
// pre-warm firing requests that were GUARANTEED to 400.
//
// Anthropic documents four shapes that make max_tokens:0 an invalid_request_error: stream:true
// (the pre-warm overrides that itself), extended thinking (thinking.type "enabled"),
// structured outputs (output_config.format), and tool_choice {"type":"tool"|"any"}. Claude
// Code sets thinking.type=enabled on every extended-thinking turn, so the pre-warm used to
// send a duplicate of the real body that always failed validation — no cache written, and an
// elevated 4xx rate per pooled account on a duplicate-request pattern the real client never
// produces.
func TestPrewarmSkipsShapesAnthropicRejectsWithZeroMaxTokens(t *testing.T) {
	base := `"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}]`
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"plain body prewarms", `{` + base + `,"max_tokens":1024}`, true},
		{"thinking enabled is rejected by anthropic", `{` + base + `,"max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":4096}}`, false},
		{"thinking disabled is fine", `{` + base + `,"max_tokens":1024,"thinking":{"type":"disabled"}}`, true},
		{"adaptive thinking is fine", `{` + base + `,"max_tokens":1024,"thinking":{"type":"adaptive"}}`, true},
		{"structured output format is rejected", `{` + base + `,"max_tokens":1024,"output_config":{"format":{"type":"json_schema"}}}`, false},
		{"output effort alone is fine", `{` + base + `,"max_tokens":1024,"output_config":{"effort":"high"}}`, true},
		{"tool_choice tool is rejected", `{` + base + `,"max_tokens":1024,"tool_choice":{"type":"tool","name":"x"}}`, false},
		{"tool_choice any is rejected", `{` + base + `,"max_tokens":1024,"tool_choice":{"type":"any"}}`, false},
		{"tool_choice auto is fine", `{` + base + `,"max_tokens":1024,"tool_choice":{"type":"auto"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := claudeCachePrewarmBody([]byte(tc.body))
			if ok != tc.want {
				t.Fatalf("claudeCachePrewarmBody ok = %v, want %v; a false positive here sends Anthropic a request that is guaranteed to 400", ok, tc.want)
			}
			if !ok {
				return
			}
			var root map[string]interface{}
			if err := json.Unmarshal(out, &root); err != nil {
				t.Fatalf("prewarm body is not JSON: %v", err)
			}
			// The documented pre-warm shape.
			if got, isNumber := root["max_tokens"].(float64); !isNumber || got != 0 {
				t.Errorf("prewarm max_tokens = %v, want 0 (the documented cache pre-warm value)", root["max_tokens"])
			}
			if got, isBool := root["stream"].(bool); !isBool || got {
				t.Errorf("prewarm stream = %v, want false (max_tokens:0 with stream:true is rejected)", root["stream"])
			}
		})
	}
}

func TestPrewarmPreservesClaudeJSONWireShape(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4","messages":[{"role":"user","content":"<session>&"}],"metadata":{"user_id":"virtual"},"max_tokens":1024,"stream":true}`)
	out, ok := claudeCachePrewarmBody(body)
	if !ok {
		t.Fatal("valid body did not produce a prewarm request")
	}
	if !bytes.HasPrefix(out, []byte(`{"model":"claude-opus-4","messages":`)) {
		t.Fatalf("prewarm changed Claude root order: %s", out)
	}
	if !bytes.Contains(out, []byte(`"content":"<session>&"`)) {
		t.Fatalf("prewarm introduced Go HTML escaping: %s", out)
	}
}
