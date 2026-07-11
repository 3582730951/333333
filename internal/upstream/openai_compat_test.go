package upstream

import "testing"

func TestIsCustomProviderExcludesBuiltIns(t *testing.T) {
	for _, provider := range []string{"", "codex", "claude", "kiro"} {
		if IsCustomProvider(provider) {
			t.Fatalf("built-in provider %q classified as custom", provider)
		}
	}
	if !IsCustomProvider("openrouter") {
		t.Fatal("real custom provider was not classified as custom")
	}
}
