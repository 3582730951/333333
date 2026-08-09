package cloak

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"codex-account-pool/internal/identity"
)

// real Claude Code 2.1.x metadata.user_id is a JSON string (captured ground truth).
var userIDShape = regexp.MustCompile(`^\{"device_id":"[a-f0-9]{64}","account_uuid":"[^"]*","session_id":"[0-9a-f-]{36}"\}$`)

func TestClaudeVirtualUserIDMatchesRealShape(t *testing.T) {
	id := identity.For(nil, "acc-uid")
	got := claudeVirtualUserID(id, true)
	if !userIDShape.MatchString(got) {
		t.Fatalf("oauth user_id does not match real Claude Code JSON shape: %q", got)
	}
	if bare := claudeVirtualUserID(id, false); bare != id.UserID {
		t.Fatalf("api-key user_id should be the bare id, got %q", bare)
	}
}

// billingRe extracts the current version, rotating three-hex suffix, and entrypoint.
var billingRe = regexp.MustCompile(`^x-anthropic-billing-header: cc_version=([0-9.]+)\.([0-9a-f]{3}); cc_entrypoint=(cli|sdk-cli);$`)

func firstSystemText(t *testing.T, body []byte) string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys, ok := root["system"].([]interface{})
	if !ok || len(sys) == 0 {
		t.Fatalf("system is not a non-empty array: %v", root["system"])
	}
	return sys[0].(map[string]interface{})["text"].(string)
}

func TestBillingHeaderInjectedForNonClaudeCodeClient(t *testing.T) {
	// An OpenAI-compat→Claude body has no billing header; we must prepend one.
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)
	out := EnsureClaudeCodeBillingHeader(body, "2.1.159")
	first := firstSystemText(t, out)
	m := billingRe.FindStringSubmatch(first)
	if m == nil {
		t.Fatalf("billing header not prepended/well-formed: %q", first)
	}
	if m[1] != "2.1.159" {
		t.Fatalf("cc_version not aligned to our version: %q", first)
	}
	// The billing block must carry neither cch nor cache_control.
	if strings.Contains(first, "cch=") {
		t.Fatalf("current Claude Code billing header must omit cch: %q", first)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(out, &root)
	if _, has := root["system"].([]interface{})[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("billing block must not carry cache_control")
	}
}

// TestBillingHeaderPreservesSubagentMarker: genuine Claude Code adds cc_is_subagent=true to
// the attribution block when the request comes from a subagent (a Task-tool invocation), as
// in the observed real block
//
//	x-anthropic-billing-header: cc_version=2.1.226.be8; cc_entrypoint=cli; cc_is_subagent=true;
//
// We rewrite that block to realign cc_version with our User-Agent, and used to drop the
// marker in the process. A subagent request is independently recognizable from its body, so
// dropping it left the body saying "subagent" while the block said "main agent", and pinned
// every account at a 0% subagent rate fleet-wide.
func TestBillingHeaderPreservesSubagentMarker(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run("preserves "+value, func(t *testing.T) {
			body := []byte(`{"model":"claude","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.226.be8; cc_entrypoint=cli; cc_is_subagent=` + value + `;"}],"messages":[]}`)
			got := firstSystemText(t, EnsureClaudeCodeBillingHeader(body, "2.1.159"))
			if !strings.Contains(got, "cc_is_subagent="+value+";") {
				t.Fatalf("cc_is_subagent=%s was dropped from the rewritten block: %q", value, got)
			}
			// cc_version is still realigned to ours, and field order is preserved.
			if !strings.Contains(got, "cc_version=2.1.159.") {
				t.Fatalf("cc_version was not realigned to our version: %q", got)
			}
			if got := strings.Index(got, "cc_entrypoint="); got < 0 {
				t.Fatal("cc_entrypoint missing")
			}
			if strings.Index(got, "cc_entrypoint=") > strings.Index(got, "cc_is_subagent=") {
				t.Fatalf("field order diverges from the real client (cc_entrypoint must precede cc_is_subagent): %q", got)
			}
		})
	}

	t.Run("never synthesized when downstream omits it", func(t *testing.T) {
		body := []byte(`{"model":"claude","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.100.123; cc_entrypoint=cli;"}],"messages":[]}`)
		got := firstSystemText(t, EnsureClaudeCodeBillingHeader(body, "2.1.159"))
		if strings.Contains(got, "cc_is_subagent") {
			t.Fatalf("cc_is_subagent must never be invented for a client that did not send it: %q", got)
		}
	})

	t.Run("garbage values are dropped, not forwarded", func(t *testing.T) {
		body := []byte(`{"model":"claude","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.100.123; cc_entrypoint=cli; cc_is_subagent=yes-injected;"}],"messages":[]}`)
		got := firstSystemText(t, EnsureClaudeCodeBillingHeader(body, "2.1.159"))
		if strings.Contains(got, "yes-injected") {
			t.Fatalf("an unrecognized cc_is_subagent value must not be forwarded into the block: %q", got)
		}
	})
}

func TestCurrentBillingHeaderMatchesCapturedBuildAndSDKEntrypoint(t *testing.T) {
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.100.123; cc_entrypoint=sdk-cli;"}],"messages":[]}`)
	got := firstSystemText(t, EnsureClaudeCodeBillingHeader(body, identity.ClaudeCLIVersion))
	m := billingRe.FindStringSubmatch(got)
	if m == nil || m[1] != identity.ClaudeCLIVersion || m[3] != "sdk-cli" {
		t.Fatalf("shipping billing fingerprint changed: %q", got)
	}
}

func TestBillingHeaderSuffixRotatesForSameVersion(t *testing.T) {
	body := []byte(`{"model":"claude","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		got := firstSystemText(t, EnsureClaudeCodeBillingHeader(body, "2.1.159"))
		m := billingRe.FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("invalid rotating billing header: %q", got)
		}
		seen[m[2]] = true
	}
	if len(seen) < 2 {
		t.Fatalf("billing suffix did not rotate across requests: %v", seen)
	}
}

func TestBillingHeaderRealignedInPlaceForRealClaudeCode(t *testing.T) {
	// Real Claude Code already carries a billing header reporting the DOWNSTREAM's
	// version; we must realign cc_version to ours (no duplicate block).
	body := []byte(`{"model":"claude","system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.180.abc; cc_entrypoint=cli; cch=deadb;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}` +
		`],"messages":[]}`)
	out := EnsureClaudeCodeBillingHeader(body, "2.1.160")
	var root map[string]interface{}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sys := root["system"].([]interface{})
	if len(sys) != 2 {
		t.Fatalf("expected no duplicate block, got %d system blocks", len(sys))
	}
	m := billingRe.FindStringSubmatch(sys[0].(map[string]interface{})["text"].(string))
	if m == nil || m[1] != "2.1.160" {
		t.Fatalf("billing header not realigned to our version: %v", sys[0])
	}
	if strings.Contains(sys[0].(map[string]interface{})["text"].(string), "cch=") {
		t.Fatalf("legacy cch must be removed: %v", sys[0])
	}
}

func TestEnsureClaudeCodeSystemDoesNotDoubleInjectOnBillingHeader(t *testing.T) {
	// A genuine Claude Code request leads with the billing block; Virtualize must NOT
	// prepend a second identity block.
	body := []byte(`{"model":"claude","metadata":{"user_id":"real-user-abc123"},"system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.953; cc_entrypoint=cli; cch=d4baa;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}` +
		`]}`)
	res := Virtualize(body, identity.For(nil, "acc-real"), nil, true)
	var root map[string]interface{}
	_ = json.Unmarshal(res.Body, &root)
	sys := root["system"].([]interface{})
	// Count identity-line blocks: must be exactly one (no duplicate).
	n := 0
	for _, b := range sys {
		if t, _ := b.(map[string]interface{})["text"].(string); strings.HasPrefix(strings.TrimSpace(t), claudeCodeIdentityLine) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one identity block (no double injection), got %d in %d blocks", n, len(sys))
	}
	if first := sys[0].(map[string]interface{})["text"].(string); !strings.HasPrefix(first, claudeBillingHeaderPrefix) {
		t.Fatalf("the original billing block should remain first, got %q", first)
	}
}

// canonicalizeForCompare strips the synthetic fingerprint fields from a billing
// block so two bodies can compare only their structural request shape. Everything
// else must match byte-for-byte.
func canonicalizeForCompare(t *testing.T, body []byte) string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sys, ok := root["system"].([]interface{}); ok {
		for _, blk := range sys {
			bm, ok := blk.(map[string]interface{})
			if !ok {
				continue
			}
			if txt, _ := bm["text"].(string); strings.HasPrefix(strings.TrimSpace(txt), claudeBillingHeaderPrefix) {
				bm["text"] = billingRe.ReplaceAllString(txt, "x-anthropic-billing-header: cc_version=$1.NNN; cc_entrypoint=$3;")
			}
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

// TestVirtualizeClaudeCodeFoldsBillingEquivalently is the regression guard for the
// performance refactor: folding the billing-header stamp into Virtualize's single
// parse/marshal pass (VirtualizeClaudeCode) must produce a body byte-identical to
// the old two-pass sequence
// (Virtualize then EnsureClaudeCodeBillingHeader). If they ever diverge in structure,
// the "pure performance" claim is false.
func TestVirtualizeClaudeCodeFoldsBillingEquivalently(t *testing.T) {
	id := identity.For(nil, "acc-fold")
	bodies := [][]byte{
		// Genuine Claude Code (leads with billing + identity blocks).
		[]byte(`{"model":"claude","metadata":{"user_id":"real-user"},"system":[` +
			`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.180.abc; cc_entrypoint=cli; cch=deadb;"},` +
			`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.\nPlatform: linux\nOS Version: x"}` +
			`],"messages":[{"role":"user","content":"hi"}]}`),
		// OpenAI-compat→Claude (no billing block, string system).
		[]byte(`{"model":"claude","system":"be helpful","messages":[{"role":"user","content":"hi"}]}`),
		// No system at all.
		[]byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`),
	}
	for i, body := range bodies {
		words := []string{"secret"}
		twoPass := Virtualize(body, id, words, true)
		twoPassBody := EnsureClaudeCodeBillingHeader(twoPass.Body, "2.1.159")
		onePass := VirtualizeClaudeCode(body, id, words, true, "2.1.159")

		got := canonicalizeForCompare(t, onePass.Body)
		want := canonicalizeForCompare(t, twoPassBody)
		if got != want {
			t.Fatalf("case %d: one-pass body diverges from two-pass\n one-pass=%s\n two-pass=%s", i, got, want)
		}
	}
}

// TestVirtualizeClaudeCodeNoBillingMatchesVirtualize confirms an empty billingVersion
// (or non-OAuth) keeps VirtualizeClaudeCode identical to plain Virtualize — no billing
// block is injected, so the API-key / probe paths are unaffected.
func TestVirtualizeClaudeCodeNoBillingMatchesVirtualize(t *testing.T) {
	id := identity.For(nil, "acc-nobill")
	body := []byte(`{"model":"claude","system":"hello","messages":[{"role":"user","content":"hi"}]}`)
	plain := Virtualize(body, id, nil, true).Body
	folded := VirtualizeClaudeCode(body, id, nil, true, "").Body
	if string(plain) != string(folded) {
		t.Fatalf("empty billingVersion must equal Virtualize\n plain =%s\n folded=%s", plain, folded)
	}
}

func TestClaudeBodyRewritersPreserveLargeToolInputInteger(t *testing.T) {
	const largeInteger = "900719925474099312345"
	body := []byte(`{"model":"claude","metadata":{"user_id":"real-user","device_id":"real-device"},` +
		`"system":[{"type":"text","text":"` + claudeCodeIdentityLine + `"}],` +
		`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_large","name":"Bash","input":{"request_id":` + largeInteger + `}}]}]}`)
	id := identity.For(nil, "acc-large-integer")

	tests := []struct {
		name        string
		body        []byte
		virtualized bool
	}{
		{
			name:        "virtualize and billing in one pass",
			body:        VirtualizeClaudeCode(body, id, nil, true, "2.1.206").Body,
			virtualized: true,
		},
		{
			name: "billing-only pass",
			body: EnsureClaudeCodeBillingHeader(body, "2.1.206"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !bytes.Contains(tt.body, []byte(largeInteger)) {
				t.Fatalf("large integer text changed during rewrite: %s", tt.body)
			}
			var root map[string]interface{}
			if err := decodeClaudeJSONObject(tt.body, &root); err != nil {
				t.Fatalf("decode rewritten body: %v", err)
			}
			content := root["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
			input := content[0].(map[string]interface{})["input"].(map[string]interface{})
			number, ok := input["request_id"].(json.Number)
			if !ok || number.String() != largeInteger {
				t.Fatalf("tool_use input integer = %T(%v), want exact %s", input["request_id"], input["request_id"], largeInteger)
			}

			system := root["system"].([]interface{})
			if len(system) < 2 || billingRe.FindStringSubmatch(system[0].(map[string]interface{})["text"].(string)) == nil {
				t.Fatalf("billing block missing or malformed: %v", system)
			}
			if system[1].(map[string]interface{})["text"] != claudeCodeIdentityLine {
				t.Fatalf("Claude Code identity system block changed: %v", system)
			}

			metadata := root["metadata"].(map[string]interface{})
			if tt.virtualized {
				if len(metadata) != 1 || !userIDShape.MatchString(metadata["user_id"].(string)) {
					t.Fatalf("virtualized metadata fingerprint changed: %v", metadata)
				}
			} else if metadata["user_id"] != "real-user" || metadata["device_id"] != "real-device" {
				t.Fatalf("billing-only pass changed metadata: %v", metadata)
			}
		})
	}
}
