package upstream

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/storage"
)

// TestMergeBetasPrefersClientBetas locks in the fix for the downstream
// "Failed to parse JSON" / "empty or malformed response (HTTP 200)" errors: when
// the client sends its own Anthropic-Beta set we forward THAT, and must NOT force
// our canonical capability betas (token-efficient-tools, structured-outputs, ...)
// on top — those change the response wire shape and break the client's parser.
func TestMergeBetasPrefersClientBetas(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "fast-mode-2099-01-01,some-new-capability-2099")
	got := mergeBetas(claudeOAuthBetas, h, false)

	// The client's own betas are forwarded verbatim.
	for _, want := range []string{"fast-mode-2099-01-01", "some-new-capability-2099"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dropped client beta %q: %s", want, got)
		}
	}
	// Required transport markers are guaranteed.
	if !strings.Contains(got, "oauth-2025-04-20") {
		t.Fatalf("oauth marker missing on OAuth path: %s", got)
	}
	if !strings.Contains(got, "interleaved-thinking-2025-05-14") {
		t.Fatalf("interleaved-thinking marker missing: %s", got)
	}
	// Canonical capability betas the client did NOT request must NOT be injected —
	// this is what previously corrupted the response format.
	for _, forbidden := range []string{
		"token-efficient-tools-2026-03-28",
		"structured-outputs-2025-12-15",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("forced canonical beta %q the client never requested: %s", forbidden, got)
		}
	}
	// No duplicates.
	if strings.Count(got, "interleaved-thinking-2025-05-14") != 1 {
		t.Fatalf("duplicate beta: %s", got)
	}
}

// TestMergeBetasFallsBackToCanonical: when the client sends no Anthropic-Beta
// (e.g. our own count_tokens / models probe), we present the canonical official
// Claude Code fingerprint so the request still looks first-party.
func TestMergeBetasFallsBackToCanonical(t *testing.T) {
	got := mergeBetas(claudeOAuthBetas, http.Header{}, false)
	for _, want := range []string{
		"claude-code-20250219",
		"oauth-2025-04-20",
		"token-efficient-tools-2026-03-28",
		"interleaved-thinking-2025-05-14",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("canonical fallback missing %q: %s", want, got)
		}
	}
}

func TestMergeBetasAPIKeyDropsOAuth(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "oauth-2025-04-20,x-extra-feature")
	got := mergeBetas(claudeAPIKeyBetas, h, true)
	if strings.Contains(got, "oauth") {
		t.Fatalf("api-key path must not carry an oauth beta: %s", got)
	}
	if !strings.Contains(got, "x-extra-feature") {
		t.Fatalf("dropped a non-oauth client beta on the api-key path: %s", got)
	}
}

// TestClaudePassthroughPreservesContentTypeAndBeta locks in the skills/Files-API
// passthrough contract: the client's own Content-Type (with the multipart boundary),
// Anthropic-Beta and Anthropic-Version ride upstream VERBATIM — we must not force the
// canonical messages-beta superset (which would make the Files upload be rejected) nor
// rewrite the multipart Content-Type to application/json (which would corrupt it).
// Account auth + the Claude Code identity fingerprint are still attached.
func TestClaudePassthroughPreservesContentTypeAndBeta(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill")

	in := http.Header{}
	in.Set("Content-Type", "multipart/form-data; boundary=----abc123XYZ")
	in.Set("Accept", "application/json")
	in.Set("Anthropic-Beta", "files-api-2025-04-14,skills-2025-10-02")
	in.Set("Anthropic-Version", "2099-09-09")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Headers:  in,
		Body:     []byte("------abc123XYZ\r\n"),
	}, id, false)

	if got := h.Get("Content-Type"); got != "multipart/form-data; boundary=----abc123XYZ" {
		t.Fatalf("Content-Type rewritten, got %q (multipart boundary must survive)", got)
	}
	// The client's exact beta set is forwarded; the canonical messages betas are NOT
	// injected on top (they would make the Files endpoint reject the request).
	if got := h.Get("Anthropic-Beta"); got != "files-api-2025-04-14,skills-2025-10-02" {
		t.Fatalf("Anthropic-Beta not forwarded verbatim, got %q", got)
	}
	for _, forbidden := range []string{"token-efficient-tools", "interleaved-thinking", "oauth-2025-04-20"} {
		if strings.Contains(h.Get("Anthropic-Beta"), forbidden) {
			t.Fatalf("passthrough injected canonical beta %q the client never sent: %s", forbidden, h.Get("Anthropic-Beta"))
		}
	}
	if got := h.Get("Anthropic-Version"); got != "2099-09-09" {
		t.Fatalf("Anthropic-Version not forwarded, got %q", got)
	}
	// Auth + identity attached (OAuth path).
	if got := h.Get("Authorization"); got != "Bearer sk-ant-oat-xyz" {
		t.Fatalf("Authorization = %q, want Bearer token", got)
	}
	if h.Get("X-App") != "cli" || h.Get("X-Stainless-OS") != id.StainlessOS {
		t.Fatalf("identity fingerprint missing: %+v", h)
	}
	if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "claude-cli/") {
		t.Fatalf("UA = %q, want claude-cli shape", ua)
	}
}

// TestClaudePassthroughAPIKeyPath: the api-key credential uses x-api-key (no Bearer),
// sets the browser-access header, and any oauth-* beta the client sent is stripped.
func TestClaudePassthroughAPIKeyPath(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill-api")

	in := http.Header{}
	in.Set("Anthropic-Beta", "files-api-2025-04-14,oauth-2025-04-20")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill-api"},
		Token:    storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-xyz"},
		Headers:  in,
	}, id, false)

	if h.Get("x-api-key") != "sk-ant-api03-xyz" {
		t.Fatalf("x-api-key not set on api-key path: %+v", h)
	}
	if h.Get("Authorization") != "" {
		t.Fatalf("api-key path must not send a Bearer Authorization")
	}
	if h.Get("Anthropic-Dangerous-Direct-Browser-Access") != "true" {
		t.Fatal("api-key path must set the browser-access header")
	}
	if strings.Contains(strings.ToLower(h.Get("Anthropic-Beta")), "oauth") {
		t.Fatalf("api-key path leaked an oauth beta: %q", h.Get("Anthropic-Beta"))
	}
	if !strings.Contains(h.Get("Anthropic-Beta"), "files-api-2025-04-14") {
		t.Fatalf("dropped the client's non-oauth beta: %q", h.Get("Anthropic-Beta"))
	}
}

// TestClaudePassthroughBetaFallback: a client that sent no Anthropic-Beta still gets a
// coherent first-party fingerprint (the canonical set), so an internal call isn't naked.
func TestClaudePassthroughBetaFallback(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill-bare")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill-bare"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Headers:  http.Header{},
	}, id, false)

	if !strings.Contains(h.Get("Anthropic-Beta"), "claude-code-20250219") {
		t.Fatalf("bare passthrough missing canonical fallback beta: %q", h.Get("Anthropic-Beta"))
	}
}

func TestClaudeMessagesFinalSanitizerBeforeSidecar(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := config.Default()
	client := NewClient(cfg)
	body := []byte(`{"model":"claude-x","stream":true,"metadata":{"user_id":"real-user"},"betas":["body-beta-2099"],"thinking":{"type":"enabled","budget_tokens":1000},"output_config":{"effort":"high"},"tool_choice":{"type":"any"},"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":[],"blocked_domains":[]},{"type":"web_search_20250305","name":"web_search_strict","allowed_domains":["example.com"],"blocked_domains":["bad.com"]}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           body,
		Account:        storage.Account{ID: "acc-final-sanitize"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(cap.body), &got); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"metadata", "betas", "thinking", "output_config"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("%s must be removed before Claude upstream: %s", forbidden, cap.body)
		}
	}
	tools := got["tools"].([]interface{})
	firstTool := tools[0].(map[string]interface{})
	if _, ok := firstTool["allowed_domains"]; ok {
		t.Fatalf("empty allowed_domains must be removed: %v", firstTool)
	}
	if _, ok := firstTool["blocked_domains"]; ok {
		t.Fatalf("empty blocked_domains must be removed: %v", firstTool)
	}
	secondTool := tools[1].(map[string]interface{})
	if len(secondTool["allowed_domains"].([]interface{})) != 1 || len(secondTool["blocked_domains"].([]interface{})) != 1 {
		t.Fatalf("non-empty web_search domains must be preserved: %v", secondTool)
	}
	if !strings.Contains(cap.headers.Get("Anthropic-Beta"), "body-beta-2099") {
		t.Fatalf("body betas must be extracted into Anthropic-Beta header: %q", cap.headers.Get("Anthropic-Beta"))
	}
}

func TestClaudeMessagesFinalSanitizerNormalizesThinkingSampling(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(config.Default())
	body := []byte(`{"model":"claude-x","stream":true,"temperature":0.2,"top_p":0.7,"top_k":20,"thinking":{"type":"enabled","budget_tokens":1000},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           body,
		Account:        storage.Account{ID: "acc-thinking-sampling"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(cap.body), &got); err != nil {
		t.Fatal(err)
	}
	if got["temperature"].(float64) != 1 {
		t.Fatalf("thinking requests must force temperature=1, body=%s", cap.body)
	}
	if _, ok := got["top_p"]; ok {
		t.Fatalf("thinking requests must not send top_p: %s", cap.body)
	}
	if _, ok := got["top_k"]; ok {
		t.Fatalf("thinking requests must not send top_k: %s", cap.body)
	}
	if got["thinking"].(map[string]interface{})["type"] != "enabled" {
		t.Fatalf("thinking block should remain enabled: %s", cap.body)
	}
}

func TestClaudeMessagesFinalSanitizerStripsHistoryProvenance(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(config.Default())
	body := []byte(`{"model":"claude-x","stream":true,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"},"signature":"sig","thoughtSignature":"ts","thought_signature":"ts2","model":"foreign","extra_content":{"google":{"thought_signature":"gts"}}},{"type":"thinking","text":"","signature":""},{"type":"text","text":"kept"}]}]}`)
	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           body,
		Account:        storage.Account{ID: "acc-history-sanitize"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(cap.body), &got); err != nil {
		t.Fatal(err)
	}
	content := got["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("empty thinking placeholder should be removed, content=%v body=%s", content, cap.body)
	}
	toolUse := content[0].(map[string]interface{})
	for _, forbidden := range []string{"signature", "thoughtSignature", "thought_signature", "model"} {
		if _, ok := toolUse[forbidden]; ok {
			t.Fatalf("tool_use provenance field %s not stripped: %v", forbidden, toolUse)
		}
	}
	if extra, ok := toolUse["extra_content"].(map[string]interface{}); ok {
		if google, ok := extra["google"].(map[string]interface{}); ok {
			if _, has := google["thought_signature"]; has {
				t.Fatalf("google thought_signature not stripped: %v", toolUse)
			}
		}
	}
	if toolUse["id"] != "toolu_1" || toolUse["name"] != "Bash" || toolUse["input"].(map[string]interface{})["cmd"] != "pwd" {
		t.Fatalf("sanitizer changed tool_use semantics: %v", toolUse)
	}
}

func TestClaudeMessagesFinalSanitizerNormalizesCacheControlTTL(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(config.Default())
	body := []byte(`{"model":"claude-x","stream":true,"tools":[{"name":"Bash","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           body,
		Account:        storage.Account{ID: "acc-cache-ttl"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(cap.body), &got); err != nil {
		t.Fatal(err)
	}
	sysCC := got["system"].([]interface{})[0].(map[string]interface{})["cache_control"].(map[string]interface{})
	if _, ok := sysCC["ttl"]; ok {
		t.Fatalf("system ttl should be downgraded after earlier default/5m tool marker: %s", cap.body)
	}
	msgCC := got["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["cache_control"].(map[string]interface{})
	if _, ok := msgCC["ttl"]; ok {
		t.Fatalf("message ttl should be downgraded after earlier default/5m marker: %s", cap.body)
	}
}

func TestClaudeCCHSigningIsDeterministicByDefaultAndConfigurable(t *testing.T) {
	body := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	signedA := captureClaudeBody(t, config.Default(), body)
	signedB := captureClaudeBody(t, config.Default(), body)

	cchA := extractCCH(t, signedA)
	cchB := extractCCH(t, signedB)
	if cchA == "fffff" {
		t.Fatalf("cch was not signed: %s", signedA)
	}
	if cchA != cchB {
		t.Fatalf("same final body must sign to the same cch: %q vs %q", cchA, cchB)
	}

	cfg := config.Default()
	cfg.ClaudeCCHSigning = false
	disabled := captureClaudeBody(t, cfg, body)
	if got := extractCCH(t, disabled); got != "fffff" {
		t.Fatalf("claude_cch_signing=false should leave cch unchanged, got %q body=%s", got, disabled)
	}
}

func TestClaudeCCHSigningUsesStableCachePrefix(t *testing.T) {
	bodyA := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"},{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"what changed in file A?"}]}]}`)
	bodyB := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"},{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"what changed in file B?"}]}]}`)
	bodyC := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"},{"type":"text","text":"different stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"what changed in file A?"}]}]}`)

	cchA := extractCCH(t, string(signClaudeBillingCCH(bodyA)))
	cchB := extractCCH(t, string(signClaudeBillingCCH(bodyB)))
	cchC := extractCCH(t, string(signClaudeBillingCCH(bodyC)))
	if cchA != cchB {
		t.Fatalf("same cacheable system prefix must sign to same cch despite final user tail: %q vs %q", cchA, cchB)
	}
	if cchA == cchC {
		t.Fatalf("different cacheable system prefix should change cch, got %q for both", cchA)
	}
}

func TestClaudeCCHSigningUsesStableMessageCachePrefix(t *testing.T) {
	bodyA := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},{"type":"text","text":"stable project context","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail A"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/workspace/a.go"}}]},{"role":"user","content":[{"type":"text","text":"final question A"}]}]}`)
	bodyB := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},{"type":"text","text":"stable project context","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail B"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/workspace/b.go"}}]},{"role":"user","content":[{"type":"text","text":"final question B"}]}]}`)
	bodyC := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"DIFFERENT"}},{"type":"text","text":"stable project context","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail A"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/workspace/a.go"}}]},{"role":"user","content":[{"type":"text","text":"final question A"}]}]}`)

	cchA := extractCCH(t, string(signClaudeBillingCCH(bodyA)))
	cchB := extractCCH(t, string(signClaudeBillingCCH(bodyB)))
	cchC := extractCCH(t, string(signClaudeBillingCCH(bodyC)))
	if cchA != cchB {
		t.Fatalf("same cacheable multimodal message prefix must sign to same cch despite volatile tail: %q vs %q", cchA, cchB)
	}
	if cchA == cchC {
		t.Fatalf("different cacheable multimodal prefix should change cch, got %q for both", cchA)
	}
}

func TestClaudeNativeCachePrefixStableAfterVirtualizationAndSigning(t *testing.T) {
	id := identity.For(nil, "acc-native-cache")
	build := func(question string) []byte {
		body := []byte(`{"model":"claude-x","stream":true,"system":[` +
			`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.180.abc; cc_entrypoint=cli; cch=deadb;"},` +
			`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},` +
			`{"type":"text","text":"stable repo instructions"}` +
			`],"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],` +
			`"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# Repo\nStable context.\n</system-reminder>\n\n"},` +
			`{"type":"text","text":` + strconv.Quote(question) + `}` +
			`]}]}`)
		res := cloak.VirtualizeClaudeCodeWithCache(body, id, nil, true, "2.1.160", cloak.ClaudeCodeCacheOptions{
			NativeBreakpoints: true,
			TTL:               "1h",
		})
		withCache := prompt.EnsureAnthropicCacheControl(res.Body, "1h")
		return signClaudeBillingCCH(withCache)
	}

	a := build("inspect file A")
	b := build("inspect file B")
	if got, want := extractCCH(t, string(a)), extractCCH(t, string(b)); got != want {
		t.Fatalf("native Claude stable prefix should keep cch stable across final questions: %q vs %q\na=%s\nb=%s", got, want, a, b)
	}
	if !strings.Contains(string(a), "inspect file A") || !strings.Contains(string(b), "inspect file B") {
		t.Fatalf("final user question was changed or removed\na=%s\nb=%s", a, b)
	}
}

func TestClaudeCCHSigningIgnoresNestedSchemaCacheControlField(t *testing.T) {
	bodyA := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"}],"tools":[{"name":"Custom","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"final question A"}]}]}`)
	bodyB := []byte(`{"model":"claude-x","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=fffff;"}],"tools":[{"name":"Custom","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"final question B"}]}]}`)

	cchA := extractCCH(t, string(signClaudeBillingCCH(bodyA)))
	cchB := extractCCH(t, string(signClaudeBillingCCH(bodyB)))
	if cchA == cchB {
		t.Fatalf("schema property named cache_control must not be treated as a prompt-cache breakpoint, got same cch %q", cchA)
	}
}

func captureClaudeBody(t *testing.T, cfg config.Config, body []byte) string {
	t.Helper()
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           body,
		Account:        storage.Account{ID: "acc-cch-sign"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return cap.body
}

var cchTestRe = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

func extractCCH(t *testing.T, body string) string {
	t.Helper()
	m := cchTestRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no cch in body: %s", body)
	}
	return m[1]
}
