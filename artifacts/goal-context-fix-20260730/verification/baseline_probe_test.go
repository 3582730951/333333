package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestBaselineProbeNativeSessionNamespaceIsolation(t *testing.T) {
	policy := downstreamPolicy{KeyHash: "shared-key-hash"}
	first, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	second, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	first.Header.Set("Session-Id", "native-client-a")
	second.Header.Set("Session-Id", "native-client-b")
	firstNamespace := codexSessionNamespace(policy, first)
	secondNamespace := codexSessionNamespace(policy, second)
	if firstNamespace == secondNamespace {
		t.Fatalf("BASELINE_BEHAVIOR namespace collision: %q", firstNamespace)
	}
}

func TestBaselineProbeClaudeCompactionReplacement(t *testing.T) {
	durable := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"}]}`)
	compacted := []byte(`{"model":"claude","messages":[{"role":"user","content":"summary of the retained task"},{"role":"assistant","content":"summary acknowledged"},{"role":"user","content":"continue after compact"}]}`)
	body, replace := incrementalGoalRequestWithMode(compacted, durable)
	if !replace {
		t.Fatalf("BASELINE_BEHAVIOR compact history appended: replace=%v body=%s", replace, body)
	}
}

func TestBaselineProbeCodexInstallerAllowlist(t *testing.T) {
	script := buildCodexConfigScript(
		"https://pool.example",
		"API_KEY",
		"gpt-5.6-sol",
		"ultra",
		"never",
		"danger-full-access",
	)
	for _, forbidden := range []string{
		"X-Pool-Client-ID",
		"pool-token",
		"supports_websockets =",
		"models_cache.json",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("BASELINE_BEHAVIOR installer added %q", forbidden)
		}
	}
}
