package capability

import (
	"encoding/json"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestParseModelCapabilities(t *testing.T) {
	raw := []byte(`{"data":[{"id":"gpt-x","context_window":128000,"max_context_window":2000000,"effective_context_window_percent":90,"auto_compact_token_limit":120000,"visibility":"visible"}]}`)
	caps, err := Parse("acc-1", raw, "etag-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(caps) != 1 || caps[0].NativeMaxContextWindow != 2000000 || caps[0].AutoCompactTokenLimit != 120000 {
		t.Fatalf("unexpected caps: %+v", caps)
	}
}

// TestParseDropsHiddenModels locks in the codex-auto-review fix: the ChatGPT /models
// backend returns internal presets (visibility "hide") that the real Codex CLI never
// shows; the relay must drop them so they don't leak into the account's model list.
func TestParseDropsHiddenModels(t *testing.T) {
	raw := []byte(`{"models":[
		{"slug":"gpt-5.4","max_context_window":1000000,"visibility":"list"},
		{"slug":"codex-auto-review","max_context_window":1000000,"visibility":"hide"},
		{"slug":"gpt-5.4-mini","max_context_window":272000,"visibility":"list"}
	]}`)
	caps, err := Parse("acc", raw, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.ModelSlug == "codex-auto-review" {
			t.Fatalf("hidden model codex-auto-review must be filtered out: %+v", caps)
		}
	}
	if len(caps) != 2 {
		t.Fatalf("expected 2 visible models, got %d: %+v", len(caps), caps)
	}
}

func TestBuildModelsResponseVirtual2M(t *testing.T) {
	caps, _ := Parse("acc-1", []byte(`{"data":[{"id":"gpt-x","context_window":128000}]}`), "")
	body, etag, err := BuildModelsResponse(caps, config.Default())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if etag == "" {
		t.Fatalf("etag empty")
	}
	var root map[string]interface{}
	_ = json.Unmarshal(body, &root)
	data := root["data"].([]interface{})
	model := data[0].(map[string]interface{})
	if model["window_mode"] != "virtual_2m" {
		t.Fatalf("window mode = %#v", model["window_mode"])
	}
}

// TestBuildModelsResponseRichestAccount asserts /v1/models advertises the model
// set of the account that supports the MOST models, not the pool-wide union.
func TestBuildModelsResponseRichestAccount(t *testing.T) {
	caps := []storage.ModelCapability{
		{AccountID: "rich", ModelSlug: "gpt-5-codex", NativeMaxContextWindow: 400000},
		{AccountID: "rich", ModelSlug: "gpt-5.1-codex", NativeMaxContextWindow: 400000},
		{AccountID: "rich", ModelSlug: "gpt-5.1-codex-max", NativeMaxContextWindow: 400000},
		{AccountID: "poor", ModelSlug: "gpt-4o-mini", NativeMaxContextWindow: 128000},
	}
	body, _, err := BuildModelsResponse(caps, config.Default())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]bool{}
	for _, item := range root["data"].([]interface{}) {
		got[item.(map[string]interface{})["id"].(string)] = true
	}
	for _, want := range []string{"gpt-5-codex", "gpt-5.1-codex", "gpt-5.1-codex-max"} {
		if !got[want] {
			t.Fatalf("expected richest account model %q in %v", want, got)
		}
	}
	if got["gpt-4o-mini"] {
		t.Fatalf("lone-account model gpt-4o-mini must NOT be advertised (union semantics); got %v", got)
	}
}

func TestParseClaudeModelsFillsWindows(t *testing.T) {
	raw := []byte(`{"data":[{"id":"claude-opus-4-7"},{"id":"claude-sonnet-4-6"},{"id":"claude-unknown-9"}]}`)
	caps, err := ParseClaudeModels("acc1", raw, `W/"etag"`)
	if err != nil {
		t.Fatal(err)
	}
	win := map[string]int64{}
	for _, c := range caps {
		win[c.ModelSlug] = c.NativeMaxContextWindow
		if c.Source != "claude_probe" {
			t.Fatalf("source = %q", c.Source)
		}
	}
	if win["claude-opus-4-7"] != 1000000 {
		t.Fatalf("opus-4-7 window = %d", win["claude-opus-4-7"])
	}
	if win["claude-sonnet-4-6"] != 200000 {
		t.Fatalf("sonnet-4-6 window = %d", win["claude-sonnet-4-6"])
	}
	// Unknown models default to the standard 200k Claude window, not 0.
	if win["claude-unknown-9"] != 200000 {
		t.Fatalf("unknown model window = %d (want 200000 default)", win["claude-unknown-9"])
	}
}

func TestStaticClaudeModelsNonEmpty(t *testing.T) {
	caps := StaticClaudeModels("acc2")
	if len(caps) == 0 {
		t.Fatal("static Claude model set must not be empty")
	}
	for _, c := range caps {
		if c.AccountID != "acc2" || c.ModelSlug == "" || c.NativeMaxContextWindow == 0 || c.Source != "claude_static" {
			t.Fatalf("malformed static capability: %+v", c)
		}
	}
}

// TestStaticClaudeModelsIncludesOpus48 locks in the user-reported fix: the current
// flagship claude-opus-4-8 must be in the static set with its 1M context window, so
// an OAuth account whose /v1/models probe is rejected still advertises it.
func TestStaticClaudeModelsIncludesOpus48(t *testing.T) {
	var found bool
	for _, c := range StaticClaudeModels("acc") {
		if c.ModelSlug == "claude-opus-4-8" {
			found = true
			if c.NativeMaxContextWindow != 1000000 {
				t.Fatalf("claude-opus-4-8 window = %d, want 1000000 (1M default)", c.NativeMaxContextWindow)
			}
		}
	}
	if !found {
		t.Fatal("claude-opus-4-8 missing from static Claude model set")
	}
}

// TestMergeClaudeStaticFloorsNewestModels confirms a live probe missing the newest
// model (Anthropic /v1/models lags a fresh release) still surfaces it via the static
// floor, while the probe's own entries are kept on conflict.
func TestMergeClaudeStaticFloorsNewestModels(t *testing.T) {
	probe, err := ParseClaudeModels("acc", []byte(`{"data":[{"id":"claude-opus-4-7"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeClaudeStatic("acc", probe)
	bySlug := map[string]storage.ModelCapability{}
	for _, c := range merged {
		bySlug[c.ModelSlug] = c
	}
	// Probe entry preserved (and marked as a live probe, not static).
	if got := bySlug["claude-opus-4-7"]; got.Source != "claude_probe" {
		t.Fatalf("probe entry source = %q, want claude_probe (probe must win on conflict)", got.Source)
	}
	// Newest model floored in even though the probe omitted it, with the 1M window.
	op48, ok := bySlug["claude-opus-4-8"]
	if !ok {
		t.Fatal("claude-opus-4-8 not floored into merged set")
	}
	if op48.NativeMaxContextWindow != 1000000 || op48.Source != "claude_static" {
		t.Fatalf("floored opus-4-8 = %+v, want 1M window + claude_static source", op48)
	}
}

// TestStaticCodexModelsCurrent confirms the Codex static fallback advertises the
// current-generation catalog (gpt-5.5 the flagship) with sane windows.
func TestStaticCodexModelsCurrent(t *testing.T) {
	caps := StaticCodexModels("acc")
	if len(caps) == 0 {
		t.Fatal("static Codex model set must not be empty")
	}
	bySlug := map[string]storage.ModelCapability{}
	for _, c := range caps {
		if c.AccountID != "acc" || c.NativeMaxContextWindow == 0 || c.Source != "codex_static" {
			t.Fatalf("malformed static Codex capability: %+v", c)
		}
		bySlug[c.ModelSlug] = c
	}
	if _, ok := bySlug["gpt-5.5"]; !ok {
		t.Fatalf("current flagship gpt-5.5 missing from static Codex set: %v", bySlug)
	}
}

// TestMergeCodexStaticFloorsNewerModels locks in the version-gated Codex /models
// fix: if a live probe only returns up to gpt-5.4, the current flagship from the
// static catalog is added; if the live probe already returns gpt-5.5, older static
// entries are not re-added as stale capabilities.
func TestMergeCodexStaticFloorsNewerModels(t *testing.T) {
	probe, err := Parse("acc", []byte(`{"models":[{"slug":"gpt-5.4","max_context_window":1000000,"visibility":"list"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeCodexStatic("acc", probe)
	bySlug := map[string]storage.ModelCapability{}
	for _, c := range merged {
		bySlug[c.ModelSlug] = c
	}
	if _, ok := bySlug["gpt-5.5"]; !ok {
		t.Fatalf("gpt-5.5 was not floored into a gpt-5.4-only probe: %+v", merged)
	}
	if bySlug["gpt-5.4"].Source != "probe" {
		t.Fatalf("probe entry must win on conflict, got %+v", bySlug["gpt-5.4"])
	}

	probe, err = Parse("acc", []byte(`{"models":[{"slug":"gpt-5.5","max_context_window":272000,"visibility":"list"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	merged = MergeCodexStatic("acc", probe)
	for _, c := range merged {
		if c.ModelSlug == "gpt-5.4" {
			t.Fatalf("older static gpt-5.4 must not be re-added when live probe has gpt-5.5: %+v", merged)
		}
	}
}

func TestCodexRequiresCurrentClientVersion(t *testing.T) {
	if !CodexRequiresCurrentClientVersion("gpt-5.5") {
		t.Fatal("gpt-5.5 must use the current Codex client version on live requests")
	}
	if CodexRequiresCurrentClientVersion("gpt-5.4") {
		t.Fatal("gpt-5.4 should keep the normal live request version")
	}
}
