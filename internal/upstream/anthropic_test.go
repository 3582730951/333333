package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
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
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"mid-conversation-system-2026-04-07",
		"effort-2025-11-24",
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

	cfg := sidecarEngineConfig()
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
	for _, forbidden := range []string{"betas", "thinking", "output_config"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("%s must be removed before Claude upstream: %s", forbidden, cap.body)
		}
	}
	metadata, _ := got["metadata"].(map[string]interface{})
	userID, _ := metadata["user_id"].(string)
	var userIdentity map[string]interface{}
	if len(metadata) != 1 || json.Unmarshal([]byte(userID), &userIdentity) != nil {
		t.Fatalf("Claude metadata.user_id does not match captured JSON-string shape: %s", cap.body)
	}
	if userIdentity["session_id"] != cap.headers.Get("X-Claude-Code-Session-Id") || userIdentity["device_id"] == "" || userIdentity["account_uuid"] != "" {
		t.Fatalf("Claude body/header identity drift: metadata=%+v headers=%+v", userIdentity, cap.headers)
	}
	if cap.headers.Get("x-client-request-id") != "" || cap.headers.Get("Accept") != "application/json" {
		t.Fatalf("Claude 2.1.206 header fingerprint mismatch: %+v", cap.headers)
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

	client := NewClient(sidecarEngineConfig())
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

	client := NewClient(sidecarEngineConfig())
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

	client := NewClient(sidecarEngineConfig())
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

func TestNormalizeClaudeMessagesSpecPreservesLargeToolInputInteger(t *testing.T) {
	const largeInteger = "900719925474099312345"
	const billing = "x-anthropic-billing-header: cc_version=2.1.206.abc; cc_entrypoint=cli;"
	body := []byte(`{"model":"claude-x","metadata":{"user_id":"downstream-user","device_id":"downstream-device"},` +
		`"system":[{"type":"text","text":"` + billing + `"},{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."}],` +
		`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_large","name":"Bash","input":{"request_id":` + largeInteger + `},"signature":"remove-me"}]}]}`)

	client, secret := fixedSecretClient(t, config.Default())
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
	spec := client.normalizeClaudeMessagesSpec(Request{
		Provider: "claude",
		Headers:  headers,
		Body:     body,
		Account:  storage.Account{ID: "acc-large-integer"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-test"},
	})

	if !bytes.Contains(spec.Body, []byte(largeInteger)) {
		t.Fatalf("large integer text changed during normalization: %s", spec.Body)
	}
	var root map[string]interface{}
	if err := decodeClaudeJSONObject(spec.Body, &root); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	content := root["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	toolUse := content[0].(map[string]interface{})
	input := toolUse["input"].(map[string]interface{})
	number, ok := input["request_id"].(json.Number)
	if !ok || number.String() != largeInteger {
		t.Fatalf("tool_use input integer = %T(%v), want exact %s", input["request_id"], input["request_id"], largeInteger)
	}
	if _, ok := toolUse["signature"]; ok {
		t.Fatalf("history sanitizer stopped running: %v", toolUse)
	}

	metadata := root["metadata"].(map[string]interface{})
	userID, _ := metadata["user_id"].(string)
	var userIdentity map[string]interface{}
	if len(metadata) != 1 || json.Unmarshal([]byte(userID), &userIdentity) != nil {
		t.Fatalf("metadata fingerprint changed: %v", metadata)
	}
	id := identity.For(secret, "acc-large-integer")
	if userIdentity["device_id"] != id.UserID || userIdentity["account_uuid"] != "" || userIdentity["session_id"] != claudeSessionID(headers, body, id) {
		t.Fatalf("metadata identity mismatch: %v", userIdentity)
	}
	system := root["system"].([]interface{})
	if len(system) != 2 || system[0].(map[string]interface{})["text"] != billing {
		t.Fatalf("billing/system fingerprint changed: %v", system)
	}
}
