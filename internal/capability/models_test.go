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

func TestBuildModelsResponseAdvertisesRealContextNoVirtual2M(t *testing.T) {
	caps, _ := Parse("acc-1", []byte(`{"data":[{"id":"gpt-x","context_window":128000,"max_context_window":272000,"effective_context_window_percent":90,"auto_compact_token_limit":240000}]}`), "")
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
	if model["context_window"].(float64) != 272000 || model["native_context_window"].(float64) != 272000 {
		t.Fatalf("context windows should be real native values: %v", model)
	}
	if model["window_mode"] == "virtual_2m" {
		t.Fatalf("virtual_2m must not be advertised: %v", model)
	}
	if model["auto_compact_token_limit"].(float64) != 240000 || model["effective_context_window_percent"].(float64) != 90 {
		t.Fatalf("compact/window metadata missing: %v", model)
	}
}

func TestBuildModelsResponsePreservesOfficialRawModelMetadata(t *testing.T) {
	raw := []byte(`{"data":[{
		"id":"gpt-future",
		"context_window":128000,
		"max_context_window":272000,
		"visibility":"list",
		"capabilities":{"responses":true,"tools":["function","web_search_preview"],"future_flag":"keep-me"},
		"supported_tool_types":["function","web_search_preview"],
		"feature_matrix":{"plugins":true,"browser_use":true}
	}]}`)
	caps, err := Parse("official-codex", raw, "etag-future")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(caps) != 1 {
		t.Fatalf("caps len = %d", len(caps))
	}
	if caps[0].RawModelJSON == "" {
		t.Fatalf("RawModelJSON was not stored: %+v", caps[0])
	}

	body, _, err := BuildModelsResponse(caps, config.Default())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	model := root["data"].([]interface{})[0].(map[string]interface{})
	capsObj, ok := model["capabilities"].(map[string]interface{})
	if !ok || capsObj["future_flag"] != "keep-me" {
		t.Fatalf("official raw capabilities were not preserved: %#v", model)
	}
	if _, ok := model["supported_tool_types"].([]interface{}); !ok {
		t.Fatalf("supported_tool_types missing from official raw metadata: %#v", model)
	}
	if feature, ok := model["feature_matrix"].(map[string]interface{}); !ok || feature["plugins"] != true {
		t.Fatalf("feature_matrix missing from official raw metadata: %#v", model)
	}
}

func TestBuildModelsResponseDoesNotForgeOfficialMetadataForCustomModels(t *testing.T) {
	caps := []storage.ModelCapability{{
		AccountID:                     "custom-1",
		ModelSlug:                     "deepseek-chat",
		NativeMaxContextWindow:        64000,
		EffectiveContextWindowPercent: 100,
		Visibility:                    "list",
		Source:                        "custom:deepseek",
	}}
	body, _, err := BuildModelsResponse(caps, config.Default())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	model := root["data"].([]interface{})[0].(map[string]interface{})
	for _, forbidden := range []string{"capabilities", "supported_tool_types", "feature_matrix"} {
		if _, ok := model[forbidden]; ok {
			t.Fatalf("custom model must not receive official metadata %q: %#v", forbidden, model)
		}
	}
	if model["provider_window_mode"] != "custom_native" {
		t.Fatalf("custom marker missing: %#v", model)
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

func TestKiroConcreteVersionsNeverDrift(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":          "claude-opus-4.8",
		"claude-opus-4.8":          "claude-opus-4.8",
		"claude-opus-4-8-20260701": "claude-opus-4.8",
		"claude-sonnet-4-9":        "claude-sonnet-4.9",
	}
	for input, want := range cases {
		got, ok := KiroCanonicalModel(input)
		if !ok || got != want {
			t.Fatalf("KiroCanonicalModel(%q)=(%q,%v), want %q", input, got, ok, want)
		}
	}
	if got, ok := KiroCanonicalModel("claude-opus-4-9-preview"); ok || got != "" {
		t.Fatalf("unknown suffix was attracted to a known version: %q %v", got, ok)
	}
}

func TestKiroAliasesUseOnlyVerifiedModels(t *testing.T) {
	if _, ok := ResolveKiroModel("auto", nil); ok {
		t.Fatal("auto resolved without a verified model")
	}
	verified := []string{"claude-sonnet-4.6", "claude-opus-4.7", "claude-opus-4.8"}
	if got, ok := ResolveKiroModel("auto", verified); !ok || got != "claude-opus-4.8" {
		t.Fatalf("auto=(%q,%v)", got, ok)
	}
	if got, ok := ResolveKiroModel("sonnet", verified); !ok || got != "claude-sonnet-4.6" {
		t.Fatalf("sonnet=(%q,%v)", got, ok)
	}
	if got, ok := ResolveKiroModel("claude-opus-4-9", verified); !ok || got != "claude-opus-4.9" {
		t.Fatalf("concrete version drifted: (%q,%v)", got, ok)
	}
	for _, capability := range StaticKiroModels("account") {
		if capability.Source != "kiro_static_unknown" {
			t.Fatalf("static Kiro capability is schedulable-looking: %+v", capability)
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
// current-generation catalog (gpt-5.6-sol the flagship) with source-accurate windows.
func TestStaticCodexModelsCurrent(t *testing.T) {
	caps := StaticCodexModels("acc")
	if len(caps) == 0 {
		t.Fatal("static Codex model set must not be empty")
	}
	bySlug := map[string]storage.ModelCapability{}
	for _, c := range caps {
		if c.AccountID != "acc" || c.NativeMaxContextWindow == 0 || c.EffectiveContextWindowPercent != 95 || c.Source != "codex_static" {
			t.Fatalf("malformed static Codex capability: %+v", c)
		}
		bySlug[c.ModelSlug] = c
	}
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if got, ok := bySlug[slug]; !ok || got.NativeContextWindow != 372000 || got.NativeMaxContextWindow != 372000 {
			t.Fatalf("current model %s missing or wrong window: %+v", slug, got)
		}
	}
	if _, stale := bySlug["gpt-5.3-codex"]; stale {
		t.Fatalf("removed gpt-5.3-codex must not remain in static catalog: %v", bySlug)
	}
}

func TestBuildModelsResponseColdStartUsesCurrentCodexCatalog(t *testing.T) {
	raw, _, err := BuildModelsResponse(nil, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range response.Data {
		if model["id"] != "gpt-5.6-sol" {
			continue
		}
		found = true
		if model["context_window"] != float64(372000) || model["native_context_window"] != float64(372000) || model["effective_context_window_percent"] != float64(95) {
			t.Fatalf("cold-start GPT-5.6 catalog entry is stale: %+v", model)
		}
	}
	if !found {
		t.Fatalf("cold-start catalog omitted gpt-5.6-sol: %s", raw)
	}
	if string(raw) == "" || containsModelID(response.Data, "gpt-5.4-codex") {
		t.Fatalf("obsolete unknown-window placeholder leaked: %s", raw)
	}
}

func containsModelID(models []map[string]interface{}, id string) bool {
	for _, model := range models {
		if model["id"] == id {
			return true
		}
	}
	return false
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
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"} {
		if _, ok := bySlug[slug]; !ok {
			t.Fatalf("%s was not floored into a gpt-5.4-only probe: %+v", slug, merged)
		}
	}
	if bySlug["gpt-5.4"].Source != "probe" {
		t.Fatalf("probe entry must win on conflict, got %+v", bySlug["gpt-5.4"])
	}

	probe, err = Parse("acc", []byte(`{"models":[{"slug":"gpt-5.6-sol","max_context_window":372000,"visibility":"list"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	merged = MergeCodexStatic("acc", probe)
	for _, c := range merged {
		if c.ModelSlug == "gpt-5.5" || c.ModelSlug == "gpt-5.4" {
			t.Fatalf("older static models must not be re-added when live probe has gpt-5.6-sol: %+v", merged)
		}
	}
}

func TestCodexRequiresCurrentClientVersion(t *testing.T) {
	if !CodexRequiresCurrentClientVersion("gpt-5.6-sol") {
		t.Fatal("gpt-5.6-sol must use the current Codex client version on live requests")
	}
	if CodexRequiresCurrentClientVersion("gpt-5.4") {
		t.Fatal("gpt-5.4 should keep the normal live request version")
	}
}

func TestCodexStaticTransportAndReasoningFingerprint(t *testing.T) {
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.2"} {
		if !CodexPrefersWebSocket(slug) {
			t.Fatalf("%s should prefer WebSocket", slug)
		}
	}
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if !CodexUsesResponsesLite(slug) || CodexMinimumClientVersion(slug) != "0.144.0" {
			t.Fatalf("%s responses-lite/minimum-version fingerprint is stale", slug)
		}
	}
	if !CodexSupportsReasoningEffort("gpt-5.6-sol", "ultra") || !CodexSupportsReasoningEffort("gpt-5.6-terra", "ultra") {
		t.Fatal("Sol and Terra must preserve ultra reasoning")
	}
	if CodexSupportsReasoningEffort("gpt-5.6-luna", "ultra") || !CodexSupportsReasoningEffort("gpt-5.6-luna", "max") {
		t.Fatal("Luna supports max but not ultra")
	}
}
