package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

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
	original := []byte(`{"model":"deepseek-v3","messages":[{"role":"assistant","reasoning":"keep"}]}`)
	unchanged, err := ensureDeepSeekV4ReasoningContent(original, "deepseek-v3")
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatalf("non-v4 request changed: %s err=%v", unchanged, err)
	}

	provided := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"assistant","reasoning":"alias","reasoning_content":"provider-signed"}]}`)
	normalized, err := ensureDeepSeekV4ReasoningContent(provided, "deepseek-v4-pro")
	if err != nil || gjson.GetBytes(normalized, "messages.0.reasoning_content").String() != "provider-signed" {
		t.Fatalf("provider reasoning_content was overwritten: %s err=%v", normalized, err)
	}
}
