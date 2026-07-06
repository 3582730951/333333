package accountprovider

import (
	"testing"

	"codex-account-pool/internal/storage"
)

func TestEffectiveProviderPrefersDeclaredThenTokenShapeThenUnknown(t *testing.T) {
	if got := EffectiveProvider("deepseek", storage.AccountToken{}, false); got != "deepseek" {
		t.Fatalf("declared provider = %q, want deepseek", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{AccessToken: "sk-ant-oat-123"}, true); got != "claude" {
		t.Fatalf("claude token provider = %q, want claude", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{AccessToken: "access-token"}, true); got != "codex" {
		t.Fatalf("codex token provider = %q, want codex", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{}, false); got != "unknown" {
		t.Fatalf("missing token provider = %q, want unknown", got)
	}
}
