package capability

import (
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// Request dispatch decides "is this a Claude model" from the slug's spelling
// (isClaudeModel in internal/api). That heuristic is only safe while the Codex
// catalog owns no claude-spelled slug. Lock the invariant here, at the catalog, so
// adding such a slug fails loudly instead of silently routing it to a Claude account
// that cannot serve it.
func TestNoBundledCodexSlugIsClaudeSpelled(t *testing.T) {
	SetRemoteCodexModels(nil)
	t.Cleanup(func() { SetRemoteCodexModels(nil) })

	slugs := make([]string, 0, 8)
	for _, c := range StaticCodexModels("acc") {
		slugs = append(slugs, c.ModelSlug)
	}
	if len(slugs) == 0 {
		t.Fatal("StaticCodexModels returned nothing; this test would prove nothing")
	}
	for _, slug := range slugs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(slug)), "claude") {
			t.Errorf("Codex catalog slug %q is claude-spelled: dispatch would divert it to the Claude bridge", slug)
		}
		if !IsCatalogCodexModel(slug) {
			t.Errorf("IsCatalogCodexModel(%q) = false for a slug the catalog advertises", slug)
		}
	}
	t.Logf("verified %d bundled Codex slugs: %s", len(slugs), strings.Join(slugs, ", "))
}

// SetRemoteCodexModels normalizes and dedupes slugs but reserves no prefix, so a
// signed remote manifest can publish a claude-spelled slug that the Codex channel
// owns. IsCatalogCodexModel must report it, which is what lets dispatch follow the
// catalog instead of the spelling.
func TestRemoteManifestClaudeSpelledSlugIsStillCatalogCodex(t *testing.T) {
	SetRemoteCodexModels(nil)
	t.Cleanup(func() { SetRemoteCodexModels(nil) })

	const spoofed = "claude-shaped-codex-model"
	if IsCatalogCodexModel(spoofed) {
		t.Fatalf("%q must not be in the catalog before the manifest is published", spoofed)
	}
	SetRemoteCodexModels([]RemoteCodexModel{{
		Slug: spoofed, ContextWindow: 272000, MaxContextWindow: 272000,
		ReasoningLevels: []string{"low", "medium", "high"},
	}})
	if !IsCatalogCodexModel(spoofed) {
		t.Fatalf("IsCatalogCodexModel(%q) = false after the manifest published it; dispatch would "+
			"then route a Codex-owned model to a Claude account", spoofed)
	}
	// A bundled slug must survive the partial manifest.
	if !IsCatalogCodexModel("gpt-5.6-sol") {
		t.Fatal("publishing a remote manifest erased a bundled Codex slug")
	}
	SetRemoteCodexModels(nil)
	if IsCatalogCodexModel(spoofed) {
		t.Fatalf("clearing the manifest did not remove %q from the catalog", spoofed)
	}
}

// The Kiro channel legitimately advertises claude-* slugs and must keep Anthropic
// semantics. They are distinguished by capability source, not by spelling, so they
// must NOT be reported as catalog Codex models.
func TestKiroClaudeSlugsAreNotCatalogCodexModels(t *testing.T) {
	SetRemoteCodexModels(nil)
	t.Cleanup(func() { SetRemoteCodexModels(nil) })

	var claudeSlugs []storage.ModelCapability
	for _, c := range StaticKiroModels("acc") {
		if strings.HasPrefix(strings.ToLower(c.ModelSlug), "claude") {
			claudeSlugs = append(claudeSlugs, c)
		}
	}
	if len(claudeSlugs) == 0 {
		t.Skip("no claude-spelled Kiro slugs in the static catalog")
	}
	for _, c := range claudeSlugs {
		if IsCatalogCodexModel(c.ModelSlug) {
			t.Errorf("Kiro slug %q was reported as a catalog Codex model", c.ModelSlug)
		}
	}
	t.Logf("verified %d claude-spelled Kiro slugs stay outside the Codex catalog", len(claudeSlugs))
}
