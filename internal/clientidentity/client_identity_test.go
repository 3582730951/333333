package clientidentity

import (
	"net/http"
	"testing"
)

func TestResolveNativeAndConflict(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "codex_cli_rs/0.1")
	h.Set("X-Anthropic-Billing-Header", "cc_version=2")
	h.Set("X-OpenAI-Subagent", "true")
	id := Resolve(h, nil)
	if id.ClientFamily != ClientUnknown || id.AgentClass != AgentSubagent || !id.Conflict || id.Confidence != ConfidenceLow {
		t.Fatalf("identity=%+v", id)
	}
}

func TestBodyAdapter(t *testing.T) {
	h := http.Header{}
	id := Resolve(h, []byte(`{"originator":"codex_cli_rs","model":"gpt-5.6","agent_class":"root"}`))
	// Requested model family is intentionally provider-neutral. A GPT request
	// remains `gpt` regardless of whether its final route is Codex/OpenAI.
	if id.ClientFamily != ClientCodexCLI || id.AgentClass != AgentRoot || id.RequestedModelFamily != "gpt" {
		t.Fatalf("identity=%+v", id)
	}
}

func TestBodyClientFamilyAdapter(t *testing.T) {
	id := Resolve(http.Header{}, []byte(`{"client_family":"claude_code","agent_class":"subagent"}`))
	if id.ClientFamily != ClientClaudeCode || id.AgentClass != AgentSubagent {
		t.Fatalf("identity=%+v", id)
	}
}
