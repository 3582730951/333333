package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

func TestApplyOpenAICompatHeadersUsesAnthropicAuthForMessages(t *testing.T) {
	headers := http.Header{}
	applyOpenAICompatHeaders(headers, Request{
		Headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}, "Anthropic-Beta": []string{"test-beta"}},
		Account: storage.Account{Provider: "claude-relay"},
		Token:   storage.AccountToken{AccessToken: "relay-key"},
	}, false)
	if headers.Get("Authorization") != "Bearer relay-key" || headers.Get("X-Api-Key") != "relay-key" {
		t.Fatalf("anthropic relay auth headers = %#v", headers)
	}
	if headers.Get("Anthropic-Version") != "2023-06-01" || headers.Get("Anthropic-Beta") != "test-beta" {
		t.Fatalf("anthropic headers not preserved: %#v", headers)
	}
	if got, want := headers.Get("User-Agent"), "claude-cli/"+identity.ClaudeCLIVersion+" (external, cli)"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestIsCustomProviderExcludesBuiltIns(t *testing.T) {
	for _, provider := range []string{"", "codex", "claude", "kiro", "antigravity"} {
		if IsCustomProvider(provider) {
			t.Fatalf("built-in provider %q classified as custom", provider)
		}
	}
	if !IsCustomProvider("openrouter") {
		t.Fatal("real custom provider was not classified as custom")
	}
}

func TestCustomCodexCLISidecarSuppressesBrowserDefaultHeaders(t *testing.T) {
	var capture sidecarCapture
	sidecar := newFakeSidecar(t, &capture)
	defer sidecar.Close()

	client := NewClient(sidecarEngineConfig())
	response, err := client.Do(nilContext(t), Request{
		Provider:         "custom-codex",
		BaseURL:          "https://custom-codex.example/v1",
		TransportProfile: storage.CustomProviderTransportCodexCLI,
		DownstreamPath:   "/responses",
		Body:             testBody([]byte(`{"model":"gpt","stream":true}`)),
		Account:          storage.Account{ID: "acc-custom-codex", Provider: "custom-codex"},
		Token:            storage.AccountToken{OpenAIAPIKey: "custom-provider-key"},
		Egress:           storage.EgressProfile{ID: "sidecar", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if capture.defaultHeaders == nil || *capture.defaultHeaders {
		t.Fatalf("custom Codex CLI sidecar must pin default_headers=false, got %v", capture.defaultHeaders)
	}
	if ua := capture.headers.Get("User-Agent"); !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("custom Codex CLI User-Agent = %q", ua)
	}
}

func TestCustomCodexResponsesProfileNormalizesBodyAndCanonicalIdentity(t *testing.T) {
	type capture struct {
		path   string
		header http.Header
		body   []byte
	}
	captured := make(chan capture, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured <- capture{path: r.URL.Path, header: r.Header.Clone(), body: raw}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-custom","object":"response","status":"completed","output":[]}`)
	}))
	defer relay.Close()

	client := NewClient(config.Default())
	response, err := client.Do(t.Context(), Request{
		Provider:         "custom-codex-wire",
		BaseURL:          relay.URL + "/v1",
		TransportProfile: storage.CustomProviderTransportCodexCLI,
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		DownstreamPath:   "/responses",
		Headers: http.Header{
			"Originator": []string{"Codex Desktop"},
		},
		Body: testBody([]byte(`{
			"model":"gpt-5.6-sol","stream":true,
			"reasoning":{"effort":"ultra"},
			"prompt_cache_retention":"24h",
			"prompt_cache_key":"018f47a0-7b5a-7b21-8a32-123456789abc",
			"thread_id":"downstream-thread","session_id":"downstream-session","window_id":"downstream-window",
			"generate":true,
			"client_metadata":{"session_id":"stale-session","custom":"preserved"},
			"input":[{"role":"user","content":"hi","future_integer":900719925474099312345}]
		}`)),
		Account: storage.Account{ID: "acc-custom-codex-wire", Provider: "custom-codex-wire"},
		Token:   storage.AccountToken{OpenAIAPIKey: "custom-codex-key"},
		Egress:  storage.EgressProfile{ID: "egress_direct", Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	got := <-captured
	if got.path != "/v1/responses" {
		t.Fatalf("custom Codex path = %q", got.path)
	}
	if got.header.Get("Authorization") != "Bearer custom-codex-key" ||
		got.header.Get("Accept") != "text/event-stream" ||
		got.header.Get("Version") == "" ||
		strings.Contains(got.header.Get("User-Agent"), openAICompatUserAgent) {
		t.Fatalf("custom Codex transport headers = %#v", got.header)
	}
	var body map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(got.body)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if instructions, _ := body["instructions"].(string); strings.TrimSpace(instructions) == "" {
		t.Fatalf("normalized body has no instructions: %s", got.body)
	}
	if store, ok := body["store"].(bool); !ok || store {
		t.Fatalf("normalized store = %#v, want false", body["store"])
	}
	reasoning, _ := body["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "max" {
		t.Fatalf("normalized reasoning = %#v", reasoning)
	}
	for _, forbidden := range []string{"prompt_cache_retention", "thread_id", "session_id", "window_id", "generate"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("unsupported top-level field %q survived: %s", forbidden, got.body)
		}
	}
	metadata, _ := body["client_metadata"].(map[string]interface{})
	if metadata["session_id"] != got.header.Get("Session-Id") ||
		metadata["thread_id"] != got.header.Get("Thread-Id") ||
		metadata["x-codex-window-id"] != got.header.Get("X-Codex-Window-Id") ||
		metadata["custom"] != "preserved" {
		t.Fatalf("body/header identity diverged metadata=%#v headers=%#v", metadata, got.header)
	}
	input := body["input"].([]interface{})[0].(map[string]interface{})
	if gotInteger := input["future_integer"].(json.Number).String(); gotInteger != "900719925474099312345" {
		t.Fatalf("large future integer changed to %q in %s", gotInteger, got.body)
	}
}
