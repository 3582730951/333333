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
	if model["context_window"].(float64) != 128000 || model["native_context_window"].(float64) != 128000 || model["native_max_context_window"].(float64) != 272000 {
		t.Fatalf("standard and technical context windows were conflated: %v", model)
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
		{AccountID: "rich", ModelSlug: "gpt-5-codex", AvailabilityState: AvailabilityVerified, NativeMaxContextWindow: 400000},
		{AccountID: "rich", ModelSlug: "gpt-5.1-codex", AvailabilityState: AvailabilityVerified, NativeMaxContextWindow: 400000},
		{AccountID: "rich", ModelSlug: "gpt-5.1-codex-max", AvailabilityState: AvailabilityVerified, NativeMaxContextWindow: 400000},
		{AccountID: "poor", ModelSlug: "gpt-4o-mini", AvailabilityState: AvailabilityVerified, NativeMaxContextWindow: 128000},
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

func TestClaudeOneMillionRequiresLiveEvidenceOrEligibleOAuthPlan(t *testing.T) {
	withoutEvidence, err := ParseClaudeModels("api", []byte(`{"data":[{"id":"claude-opus-4-8"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	withoutEvidence = ApplyClaudeAccountPolicy(withoutEvidence, storage.Account{Provider: "claude", PlanType: "api"}, storage.AccountToken{AuthMethod: "api_key", AccessToken: "sk-ant-api"})
	if got := withoutEvidence[0]; got.NativeContextWindow != 200000 || got.NativeMaxContextWindow != 1000000 || got.Context1MState != Context1MUnknown {
		t.Fatalf("technical model maximum became API-key entitlement: %+v", got)
	}

	withEvidence, err := ParseClaudeModels("api", []byte(`{"data":[{"id":"claude-opus-4-8","max_input_tokens":1000000}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	withEvidence = ApplyClaudeAccountPolicy(withEvidence, storage.Account{Provider: "claude", PlanType: "api"}, storage.AccountToken{AuthMethod: "api_key", AccessToken: "sk-ant-api"})
	if withEvidence[0].Context1MState != Context1MSupported || withEvidence[0].Context1MSource != "live_models" {
		t.Fatalf("explicit live API-key 1M evidence was lost: %+v", withEvidence[0])
	}

	pro := ApplyClaudeAccountPolicy(ParseStaticCopy("pro"), storage.Account{Provider: "claude", PlanType: "Pro"}, storage.AccountToken{AuthMethod: "oauth", AccessToken: "oauth"})
	max := ApplyClaudeAccountPolicy(ParseStaticCopy("max"), storage.Account{Provider: "claude", PlanType: "Max"}, storage.AccountToken{AuthMethod: "oauth", AccessToken: "oauth"})
	if pro[0].Context1MState != Context1MUnsupported || pro[0].NativeContextWindow != 200000 {
		t.Fatalf("Pro was not permanently limited to standard context: %+v", pro[0])
	}
	if max[0].Context1MState != Context1MSupported {
		t.Fatalf("eligible Max Opus capability was not enabled: %+v", max[0])
	}
}

func ParseStaticCopy(accountID string) []storage.ModelCapability {
	for _, c := range StaticClaudeModels(accountID) {
		if c.ModelSlug == "claude-opus-4-8" {
			return []storage.ModelCapability{c}
		}
	}
	return nil
}

func TestStaticClaudeModelsNonEmpty(t *testing.T) {
	caps := StaticClaudeModels("acc2")
	if len(caps) == 0 {
		t.Fatal("static Claude model set must not be empty")
	}
	for _, c := range caps {
		if c.AccountID != "acc2" || c.ModelSlug == "" || c.NativeContextWindow != 200000 || c.NativeMaxContextWindow == 0 || c.Source != "claude_static_unverified" || c.AvailabilityState != AvailabilityUnverified || c.Context1MState != Context1MUnknown {
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

func TestKiroGPTModelsAreExactAndKeepTheirNativeWindow(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "GPT-5.6-TERRA", " gpt-5.6-luna "} {
		canonical, ok := KiroCanonicalModel(model)
		if !ok || !KiroSupportsGPTModel(model) {
			t.Fatalf("Kiro GPT model %q was not recognized", model)
		}
		if got := KiroEffectiveContextWindow(canonical, "", 0); got != 372000 {
			t.Fatalf("%s standard window=%d, want 372000", canonical, got)
		}
	}
	for _, model := range []string{"gpt-5.6", "gpt-5.6-sol-preview", "gpt-5.5-sol", "gpt-4.1"} {
		if canonical, ok := KiroCanonicalModel(model); ok || canonical != "" || KiroSupportsGPTModel(model) {
			t.Fatalf("non-Kiro GPT model %q was accepted as %q", model, canonical)
		}
	}
}

func TestBuildAnthropicModelsResponseUsesClaudeFacingKiroModelIDs(t *testing.T) {
	caps := StaticKiroModels("kiro-account")
	for i := range caps {
		caps[i].AvailabilityState = AvailabilityVerified
		caps[i].Source = "kiro_runtime"
	}
	body, _, err := BuildAnthropicModelsResponse(caps)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, model := range response.Data {
		ids[model.ID] = true
	}
	if !ids["claude-opus-4-8"] {
		t.Fatalf("Claude-facing Kiro model alias missing: %s", body)
	}
	if ids["claude-opus-4.8"] {
		t.Fatalf("Kiro-native dotted model leaked into Claude Code catalog: %s", body)
	}
}

func TestParseRequestedClaudeModelContextSuffix(t *testing.T) {
	parsed, err := ParseRequestedClaudeModel("Claude-Opus-4.8[1M]")
	if err != nil || parsed.RequestedModel != "Claude-Opus-4.8[1M]" || parsed.BaseModel != "claude-opus-4.8" || parsed.ContextMode != "1m" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	if _, err := ParseRequestedClaudeModel("claude-opus-4.8[fast]"); err == nil {
		t.Fatal("unknown bracket suffix was silently removed")
	}
	if parsed, err := ParseRequestedClaudeModel("claude-opus-4-8"); err != nil || parsed.BaseModel != "claude-opus-4.8" || parsed.ContextMode != "" {
		t.Fatalf("plain model parsed=%+v err=%v", parsed, err)
	}
}

func TestClaudeFallbackStaysInFamilyAndOnlyMovesLower(t *testing.T) {
	caps := []storage.ModelCapability{
		{ModelSlug: "claude-opus-4-7", AvailabilityState: AvailabilityVerified, Source: "claude_probe"},
		{ModelSlug: "claude-opus-4-6", AvailabilityState: AvailabilityVerified, Source: "claude_probe"},
		{ModelSlug: "claude-sonnet-4-9", AvailabilityState: AvailabilityVerified, Source: "claude_probe"},
		{ModelSlug: "claude-opus-4-9", AvailabilityState: AvailabilityVerified, Source: "claude_probe"},
	}
	if got := SuggestedClaudeFallback("claude-opus-4-8", caps); got != "claude-opus-4-7" {
		t.Fatalf("fallback=%q, want same-family lower Opus 4.7", got)
	}
}

func TestKiroEffectiveContextWindowRequires1MMode(t *testing.T) {
	if got := KiroEffectiveContextWindow("claude-opus-4.8", "", 900000); got != 200000 {
		t.Fatalf("standard context = %d, want 200000", got)
	}
	if got := KiroEffectiveContextWindow("claude-opus-4.8", "1m", 640000); got != 640000 {
		t.Fatalf("measured 1m context = %d, want 640000", got)
	}
}

func TestStaticKiroSeparatesStandardAndTechnicalContextWindows(t *testing.T) {
	for _, c := range StaticKiroModels("account") {
		wantNative := int64(200000)
		if KiroSupportsGPTModel(c.ModelSlug) {
			wantNative = 372000
		}
		if c.NativeContextWindow != wantNative {
			t.Fatalf("%s standard window=%d, want %d", c.ModelSlug, c.NativeContextWindow, wantNative)
		}
		if KiroSupportsGPTModel(c.ModelSlug) && c.NativeMaxContextWindow != 372000 {
			t.Fatalf("%s GPT maximum window=%d, want 372000", c.ModelSlug, c.NativeMaxContextWindow)
		}
		if KiroContextWindow(c.ModelSlug) == 1000000 && c.NativeMaxContextWindow != 1000000 {
			t.Fatalf("%s technical window=%d, want 1000000", c.ModelSlug, c.NativeMaxContextWindow)
		}
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

func TestKiroFreePlanCannotBootstrapOpus(t *testing.T) {
	if KiroPlanAllowsBootstrap("KIRO FREE", "claude-opus-4-8") {
		t.Fatal("KIRO FREE must not bootstrap Opus from a stale static capability")
	}
	if !KiroPlanAllowsBootstrap("KIRO FREE", "claude-sonnet-4-6") {
		t.Fatal("KIRO FREE non-Opus concrete model was over-restricted")
	}
	if !KiroPlanAllowsBootstrap("KIRO PRO", "claude-opus-4-8") {
		t.Fatal("KIRO PRO Opus bootstrap was rejected")
	}
}

func TestKiroPlanAllows1MRequiresKnownPaidPlanAndTechnicalWindow(t *testing.T) {
	for _, plan := range []string{"", "KIRO FREE"} {
		if KiroPlanAllows1M(plan, "claude-opus-4-8") {
			t.Fatalf("plan %q unexpectedly received Kiro 1M entitlement", plan)
		}
	}
	for _, plan := range []string{"KIRO PRO", "KIRO PRO+", "KIRO ENTERPRISE"} {
		if !KiroPlanAllows1M(plan, "claude-opus-4-8") {
			t.Fatalf("known paid plan %q was rejected for Kiro 1M", plan)
		}
	}
	if KiroPlanAllows1M("KIRO PRO", "claude-haiku-4.5") {
		t.Fatal("200K model unexpectedly received Kiro 1M entitlement")
	}
}

// TestStaticClaudeModelsIncludesOpus48 locks in the user-reported fix: the current
// flagship claude-opus-4-8 stays discoverable as an unverified hint. Its standard
// window remains 200K and the 1M technical maximum is not entitlement evidence.
func TestStaticClaudeModelsIncludesOpus48(t *testing.T) {
	var found bool
	for _, c := range StaticClaudeModels("acc") {
		if c.ModelSlug == "claude-opus-4-8" {
			found = true
			if c.NativeContextWindow != 200000 || c.NativeMaxContextWindow != 1000000 || c.AvailabilityState != AvailabilityUnverified || c.Context1MState != Context1MUnknown {
				t.Fatalf("claude-opus-4-8 static hint conflated standard/entitled windows: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("claude-opus-4-8 missing from static Claude model set")
	}
}

// A successful live list is authoritative. Models missing from it must not be
// reintroduced from a static catalog as fake verified capabilities.
func TestMergeClaudeLiveCatalogIsAuthoritative(t *testing.T) {
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
	if _, ok := bySlug["claude-opus-4-8"]; ok {
		t.Fatalf("missing live model was reintroduced from static data: %+v", merged)
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

func TestBuildModelsResponseColdStartIsEmpty(t *testing.T) {
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
	if len(response.Data) != 0 {
		t.Fatalf("cold-start catalog injected unverified static models: %s", raw)
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

func TestMergeCodexLiveCatalogIsAuthoritative(t *testing.T) {
	probe, err := Parse("acc", []byte(`{"models":[{"slug":"gpt-5.4","max_context_window":1000000,"visibility":"list"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeCodexStatic("acc", probe)
	bySlug := map[string]storage.ModelCapability{}
	for _, c := range merged {
		bySlug[c.ModelSlug] = c
	}
	if len(merged) != 1 {
		t.Fatalf("live catalog was padded with static models: %+v", merged)
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
