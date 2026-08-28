package upstream

import (
	"encoding/base64"
	"testing"
)

// eForm builds a single-layer Claude signature: base64 of a payload whose first byte
// is 0x12, which forces the leading base64 character to 'E'.
func eForm() string {
	return base64.StdEncoding.EncodeToString(append([]byte{0x12}, []byte("opaque-channel-block")...))
}

// cForm builds the CAIS envelope the newest Claude Code models use: first decoded
// byte 0x08 (top-level field 1 envelope-version varint), leading character 'C'.
func cForm() string {
	return base64.StdEncoding.EncodeToString(append([]byte{0x08}, []byte("opaque-cais-envelope")...))
}

// rForm wraps an E-form string in a second base64 layer, giving a leading 'R'.
func rForm() string {
	return base64.StdEncoding.EncodeToString([]byte(eForm()))
}

func TestClaudeThinkingSignatureEnvelopeRecognition(t *testing.T) {
	if got := eForm()[0]; got != 'E' {
		t.Fatalf("fixture broken: E-form starts with %q", got)
	}
	if got := cForm()[0]; got != 'C' {
		t.Fatalf("fixture broken: C-form starts with %q", got)
	}
	if got := rForm()[0]; got != 'R' {
		t.Fatalf("fixture broken: R-form starts with %q", got)
	}

	valid := map[string]string{
		"single layer E":      eForm(),
		"double layer R":      rForm(),
		"CAIS envelope C":     cForm(),
		"claude cache prefix": "claude#" + eForm(),
		"anthropic prefix":    "anthropic#" + rForm(),
		"CAIS with prefix":    "claude#" + cForm(),
	}
	for name, sig := range valid {
		if !isClaudeThinkingSignature(sig) {
			t.Errorf("%s was rejected; a genuine signature must never be dropped", name)
		}
	}

	invalid := map[string]string{
		"gemini channel":     "gemini#" + eForm(),
		"google channel":     "google#" + eForm(),
		"unknown provider":   "kimi#" + eForm(),
		"not base64":         "!!!not-base64!!!",
		"empty":              "",
		"wrong first byte":   base64.StdEncoding.EncodeToString([]byte{0x99, 0x01, 0x02}),
		"R wrapping non-E":   base64.StdEncoding.EncodeToString([]byte("Xnot-an-e-form")),
		"deepseek reasoning": "reasoning:some-provider-encoded-blob",
	}
	for name, sig := range invalid {
		if isClaudeThinkingSignature(sig) {
			t.Errorf("%s was accepted as a Claude signature", name)
		}
	}
}

// The pool routes one conversation across providers, so replayed history can carry a
// thinking signature another backend produced. Sending it to api.anthropic.com fails
// the turn on an invalid signature, and a first-party client never sends a foreign
// signature, so it is a tampering signal too. Emptiness alone used to be the only
// check, which let every non-empty foreign value through.
func TestSanitizeClaudeHistoryDropsForeignThinkingSignatures(t *testing.T) {
	thinking := func(text, signature string) map[string]interface{} {
		return map[string]interface{}{"type": "thinking", "thinking": text, "signature": signature}
	}

	root := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					thinking("kept, genuine E", eForm()),
					thinking("kept, genuine CAIS", cForm()),
					thinking("dropped, gemini channel", "gemini#"+eForm()),
					thinking("dropped, garbage", "!!!"),
					map[string]interface{}{"type": "text", "text": "visible answer"},
				},
			},
		},
	}

	if !sanitizeClaudeHistory(root) {
		t.Fatal("sanitizer reported no change despite two unusable blocks")
	}

	msgs := root["messages"].([]interface{})
	blocks := msgs[0].(map[string]interface{})["content"].([]interface{})
	if len(blocks) != 3 {
		t.Fatalf("expected 3 surviving blocks, got %d: %+v", len(blocks), blocks)
	}

	// Surviving signatures must be byte-identical: a valid signature is opaque and
	// must never be rewritten.
	first := blocks[0].(map[string]interface{})
	if first["signature"] != eForm() {
		t.Errorf("genuine E-form signature was rewritten: %v", first["signature"])
	}
	second := blocks[1].(map[string]interface{})
	if second["signature"] != cForm() {
		t.Errorf("genuine CAIS signature was rewritten: %v", second["signature"])
	}
	if third := blocks[2].(map[string]interface{}); third["type"] != "text" {
		t.Errorf("the plain text block did not survive: %+v", third)
	}
}

// A thinking block with both empty text and empty signature keeps its previous
// treatment, and a valid block is left completely alone.
func TestSanitizeClaudeHistoryLeavesValidHistoryUntouched(t *testing.T) {
	root := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "thinking": "reasoning", "signature": rForm()},
					map[string]interface{}{"type": "text", "text": "answer"},
				},
			},
		},
	}
	if sanitizeClaudeHistory(root) {
		t.Error("a fully valid history was modified")
	}
	blocks := root["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("valid history lost a block: %+v", blocks)
	}
}
