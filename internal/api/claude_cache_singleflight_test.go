package api

import (
	"testing"

	"codex-account-pool/internal/routing"
)

func TestClaudeCacheSingleflightShortBodyIgnoresOnlyBillingNonce(t *testing.T) {
	affinity := routing.AffinityFromKey("same-conversation", "conversation_id")
	build := func(suffix, question string) []byte {
		return []byte(`{"model":"claude-x","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.206.` + suffix + `; cc_entrypoint=cli;"},{"type":"text","text":"stable instructions"}],"messages":[{"role":"user","content":"` + question + `"}],"tools":[{"name":"Lookup","input_schema":{"type":"object","properties":{"id":{"const":900719925474099312345}}}}]}`)
	}

	first := claudeCacheSingleflightKey("account-a", "claude-x", build("a5e", "same request"), affinity)
	second := claudeCacheSingleflightKey("account-a", "claude-x", build("2e4", "same request"), affinity)
	if first == "" || first != second {
		t.Fatalf("rotating billing suffix split one exact request: %q vs %q", first, second)
	}
	changedContext := claudeCacheSingleflightKey("account-a", "claude-x", build("a5e", "different request"), affinity)
	if changedContext == first {
		t.Fatal("singleflight key ignored real message context")
	}
	if claudeCacheSingleflightKey("account-b", "claude-x", build("a5e", "same request"), affinity) == first {
		t.Fatal("singleflight crossed account boundary")
	}
	if claudeCacheSingleflightKey("account-a", "claude-y", build("a5e", "same request"), affinity) == first {
		t.Fatal("singleflight crossed model boundary")
	}
}
