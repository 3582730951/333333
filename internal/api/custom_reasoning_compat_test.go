package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/storage"
	"github.com/tidwall/gjson"
)

func TestDeepSeekV4ToolReplayAddsRequiredReasoningContent(t *testing.T) {
	const reasoning = "private reasoning already returned to this client"
	var capturedMu sync.Mutex
	var captured []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedMu.Lock()
		captured = append([]byte(nil), body...)
		capturedMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-deepseek-v4","object":"chat.completion","model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	})
	setupDeepSeek(t, h, []string{"deepseek/deepseek-v4-flash"}, false)

	request := `{
  "model":"deepseek/deepseek-v4-flash",
  "messages":[
    {"role":"user","content":"use the write tool"},
    {"role":"assistant","content":null,"reasoning":"` + reasoning + `","reasoning_details":[{"type":"reasoning.text","text":"keep detail","future":900719925474099312345}],"tool_calls":[{"id":"call_v4","type":"function","function":{"name":"write","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"call_v4","content":"ok"}
  ]
}`
	response, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	result, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("DeepSeek V4 replay status=%d body=%s err=%v", response.StatusCode, result, readErr)
	}

	capturedMu.Lock()
	upstreamBody := append([]byte(nil), captured...)
	capturedMu.Unlock()
	if got := gjson.GetBytes(upstreamBody, "messages.1.reasoning_content").String(); got != reasoning {
		t.Fatalf("reasoning_content=%q want=%q body=%s", got, reasoning, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.1.content"); got.Type != gjson.String || got.String() != "" {
		t.Fatalf("DeepSeek tool-call content must be a non-null empty string: %s", upstreamBody)
	}
	for _, preserved := range [][]byte{
		[]byte(`"reasoning":"` + reasoning + `"`),
		[]byte(`"tool_call_id":"call_v4"`),
		[]byte(`900719925474099312345`),
	} {
		if !bytes.Contains(upstreamBody, preserved) {
			t.Fatalf("DeepSeek compatibility rewrite changed %s: %s", preserved, upstreamBody)
		}
	}
}

func TestDeepSeekReasoningAliasIsNarrowAndNeverOverwritesProviderValue(t *testing.T) {
	original := []byte(`{"model":"deepseek-v3","messages":[{"role":"assistant","reasoning":"ignored-without-tools","content":"answer"}]}`)
	unchanged, err := ensureDeepSeekV4ReasoningContent(original, "deepseek-v3")
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatalf("tool-free DeepSeek request changed: %s err=%v", unchanged, err)
	}

	nonDeepSeek := []byte(`{"model":"other-reasoner","messages":[{"role":"assistant","content":null,"reasoning":"keep","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	unchanged, err = ensureDeepSeekV4ReasoningContent(nonDeepSeek, "other-reasoner")
	if err != nil || !bytes.Equal(unchanged, nonDeepSeek) {
		t.Fatalf("non-DeepSeek request changed: %s err=%v", unchanged, err)
	}

	provided := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"assistant","content":"","reasoning":"alias","reasoning_content":"provider-signed","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	normalized, err := ensureDeepSeekV4ReasoningContent(provided, "deepseek-v4-pro")
	if err != nil || gjson.GetBytes(normalized, "messages.0.reasoning_content").String() != "provider-signed" {
		t.Fatalf("provider reasoning_content was overwritten: %s err=%v", normalized, err)
	}
}

func TestOfficialDeepSeekChatCompatibilityIsLosslessAndNarrow(t *testing.T) {
	input := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"ultra","tool_choice":"auto","messages":[{"role":"user","content":"work"}]}`)
	normalized, err := normalizeOfficialDeepSeekChatRequest(input, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(normalized, "reasoning_effort").String(); got != "max" {
		t.Fatalf("ultra effort mapped to %q, want provider maximum", got)
	}
	if gjson.GetBytes(normalized, "tool_choice").Exists() {
		t.Fatalf("equivalent auto tool choice was not removed: %s", normalized)
	}
	if got := gjson.GetBytes(normalized, "messages.0.content").String(); got != "work" {
		t.Fatalf("message content changed: %s", normalized)
	}

	forced := []byte(`{"model":"deepseek-v4-pro","tool_choice":{"type":"function","function":{"name":"write"}},"messages":[]}`)
	if _, err := normalizeOfficialDeepSeekChatRequest(forced, "deepseek-v4-pro"); err == nil {
		t.Fatal("explicit unsupported DeepSeek tool choice must not be silently downgraded")
	}
	disabled := []byte(`{"model":"deepseek-v4-pro","thinking":{"type":"disabled"},"reasoning_effort":"ultra","tool_choice":"required","messages":[]}`)
	disabledResult, err := normalizeOfficialDeepSeekChatRequest(disabled, "deepseek-v4-pro")
	if err != nil || !bytes.Equal(disabledResult, disabled) {
		t.Fatalf("non-thinking DeepSeek request changed: %s err=%v", disabledResult, err)
	}
	nullChoice := []byte(`{"model":"deepseek-v4-flash","tool_choice":null,"messages":[]}`)
	nullResult, err := normalizeOfficialDeepSeekChatRequest(nullChoice, "deepseek-v4-flash")
	if err != nil || gjson.GetBytes(nullResult, "tool_choice").Exists() {
		t.Fatalf("null/default DeepSeek tool choice was not removed: %s err=%v", nullResult, err)
	}

	other := []byte(`{"model":"deepseek-v3.2","reasoning_effort":"ultra","tool_choice":"required"}`)
	unchanged, err := normalizeOfficialDeepSeekChatRequest(other, "deepseek-v3.2")
	if err != nil || !bytes.Equal(unchanged, other) {
		t.Fatalf("non-V4 compatibility surface changed: %s err=%v", unchanged, err)
	}
}

func TestOfficialDeepSeekMessagesUsesNativeAnthropicRouteOnlyForExactHost(t *testing.T) {
	provider := storage.CustomProvider{
		ID: "deepseek", BaseURL: "https://api.deepseek.com/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		TransportProfile: storage.CustomProviderTransportGeneric,
	}
	resolved, routeID := resolveLiveCustomProviderRoute(provider, storage.CustomProviderDownstreamMessages)
	if routeID != "deepseek-official-anthropic" || resolved.BaseURL != "https://api.deepseek.com/anthropic" ||
		resolved.UpstreamProtocol != storage.CustomProviderProtocolAnthropicMessages ||
		resolved.TransportProfile != storage.CustomProviderTransportClaudeCode {
		t.Fatalf("official messages route not selected: id=%q provider=%+v", routeID, resolved)
	}

	responses, routeID := resolveLiveCustomProviderRoute(provider, storage.CustomProviderDownstreamResponses)
	if routeID != "default" || responses.BaseURL != provider.BaseURL || responses.UpstreamProtocol != provider.UpstreamProtocol {
		t.Fatalf("Codex Responses path must retain its bridge: id=%q provider=%+v", routeID, responses)
	}

	proxy := provider
	proxy.BaseURL = "https://proxy.example/deepseek/v1"
	proxied, routeID := resolveLiveCustomProviderRoute(proxy, storage.CustomProviderDownstreamMessages)
	if routeID != "default" || proxied.BaseURL != proxy.BaseURL || proxied.UpstreamProtocol != proxy.UpstreamProtocol {
		t.Fatalf("third-party proxy route changed: id=%q provider=%+v", routeID, proxied)
	}

	explicit := provider
	explicit.Routes = []storage.CustomProviderRoute{{
		ID: "operator-messages", DownstreamPath: storage.CustomProviderDownstreamMessages,
		BaseURL: "https://operator.example/claude", UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		TransportProfile: storage.CustomProviderTransportClaudeCode,
	}}
	operator, routeID := resolveLiveCustomProviderRoute(explicit, storage.CustomProviderDownstreamMessages)
	if routeID != "operator-messages" || operator.BaseURL != "https://operator.example/claude" {
		t.Fatalf("operator route lost precedence: id=%q provider=%+v", routeID, operator)
	}
}
