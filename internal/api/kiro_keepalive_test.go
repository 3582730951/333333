package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestKiroStreamingEmitsKeepaliveDuringSilentPreTokenWindow proves the fix for
// "Stream disconnected before completion: idle timeout waiting for SSE": while a
// streaming Kiro attempt is stalled in its silent pre-first-token window (here the
// upstream is held open), the client must receive protocol-visible SSE ping events
// so its idle-SSE timer never fires. It also asserts the keepalive stops synchronously —
// no ping may trail the first real emitted frame, since the emitter
// writes straight to the ResponseWriter and would otherwise race a late tick.
func TestKiroStreamingEmitsKeepaliveDuringSilentPreTokenWindow(t *testing.T) {
	release := make(chan struct{})
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			// Hold the response open to simulate the silent window (cache-singleflight
			// wait + token refresh + upstream time-to-first-byte).
			<-release
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"first"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":7,"outputTokens":2,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"totalTokens":9}}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0.25}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	if err := h.store.SetSetting(context.Background(), "stream_keepalive_seconds", "1"); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "kiro-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	imported, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, imported.Body)
	imported.Body.Close()

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "keepalive-session")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	// Safety net: if the keepalive is broken the upstream would block forever, so
	// release after a bounded wait to let the test complete and assert on what it saw.
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	safety := time.AfterFunc(3*time.Second, doRelease)
	defer safety.Stop()

	lines := make(chan string, 512)
	go func() {
		reader := bufio.NewReader(response.Body)
		for {
			line, readErr := reader.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if readErr != nil {
				close(lines)
				return
			}
		}
	}()

	var collected strings.Builder
	sawKeepalive := false
	firstEventSeen := false
	keepaliveAfterContent := false
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case line, ok := <-lines:
			if !ok {
				done = true
				break
			}
			collected.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "event: ping":
				sawKeepalive = true
				if firstEventSeen {
					keepaliveAfterContent = true
				}
				doRelease() // upstream may now deliver the real response
			case strings.HasPrefix(trimmed, "event:"):
				firstEventSeen = true
			}
		case <-timeout:
			t.Fatal("timed out reading Kiro stream")
		}
	}

	if !sawKeepalive {
		t.Fatalf("no protocol-visible ping event during the silent pre-first-token window; downstream would hit the idle-SSE timeout:\n%s", collected.String())
	}
	if keepaliveAfterContent {
		t.Fatalf("keepalive ping emitted after real content began; stopKeepalive did not stop synchronously:\n%s", collected.String())
	}
	if !strings.Contains(collected.String(), "first") {
		t.Fatalf("stream did not deliver upstream content after the keepalive window:\n%s", collected.String())
	}
}

func TestKiroContextLimitAfterKeepaliveKeepsTypedCompactionSignal(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	requests := 0
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			requests++
			mu.Unlock()
			<-release
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	defer doRelease()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	if err := h.store.SetSetting(context.Background(), "stream_keepalive_seconds", "1"); err != nil {
		t.Fatal(err)
	}
	importKiroEndpointForTest(t, h, kiroMock.URL, "context-after-keepalive-key")
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"preserve delayed context request"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "context-after-keepalive")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// Do returns after the proxy's first keepalive commits HTTP 200 while the Kiro
	// endpoint is still silent. The later rejection must therefore use typed SSE.
	doRelease()
	body, _ := io.ReadAll(response.Body)
	mu.Lock()
	requestCount := requests
	mu.Unlock()
	if response.StatusCode != http.StatusOK || requestCount != 1 {
		t.Fatalf("status=%d requests=%d body=%s", response.StatusCode, requestCount, body)
	}
	for _, want := range []string{
		"event: ping", "event: error", `"type":"invalid_request_error"`,
		`"code":"context_length_exceeded"`, "Prompt is too long", "retry_target=",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("typed context-limit stream missing %q: %s", want, body)
		}
	}
	if bytes.Contains(body, []byte(publicRetryMessage)) || bytes.Contains(body, []byte("333301 tokens > 372000")) {
		t.Fatalf("typed context-limit stream was replaced or fabricated a comparison: %s", body)
	}
	if response.Trailer.Get("X-MiCliProxy-Context-Limit-Source") != "upstream_unreported" ||
		response.Trailer.Get("X-MiCliProxy-Context-Retry-Target") == "" ||
		response.Trailer.Get("X-MiCliProxy-Auto-Compact") != "client_retry" {
		t.Fatalf("context-limit trailers=%v body=%s", response.Trailer, body)
	}
}

func TestKiroSummaryFallbackAfterCommittedKeepaliveCompletesStream(t *testing.T) {
	releaseInitial := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(releaseInitial) }) }
	var mu sync.Mutex
	requests := 0
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests++
			mu.Unlock()
			if !bytes.Contains(body, []byte("sequential transcript fragment")) && !bytes.Contains(body, []byte("ordered intermediate summaries")) {
				<-releaseInitial
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "exception", ":exception-type": "ContentLengthExceededException"}, []byte(`{"message":"Input is too long."}`)))
				return
			}
			content := "committed-map-summary"
			if bytes.Contains(body, []byte("ordered intermediate summaries")) {
				content = "committed-final-summary"
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"`+content+`"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":80,"outputTokens":12,"totalTokens":92}}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	defer doRelease()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	if err := h.store.SetSetting(context.Background(), "stream_keepalive_seconds", "1"); err != nil {
		t.Fatal(err)
	}
	importKiroEndpointForTest(t, h, kiroMock.URL, "committed-summary-key")
	payload := `{"model":"claude-sonnet-4-6","stream":true,"system":"You are a helpful AI assistant tasked with summarizing conversations.","messages":[{"role":"user","content":"committed history marker"},{"role":"assistant","content":"old answer"},{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. REMINDER: Do NOT call any tools. Respond with plain text only"}]}`
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "committed-summary")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// The first ping commits HTTP 200 before the initial Kiro rejection. The proxy
	// must keep the same stream open while it performs the bounded map/reduce.
	doRelease()
	body, _ := io.ReadAll(response.Body)
	mu.Lock()
	requestCount := requests
	mu.Unlock()
	if response.StatusCode != http.StatusOK || requestCount != 3 ||
		!bytes.Contains(body, []byte("event: ping")) ||
		!bytes.Contains(body, []byte("committed-final-summary")) ||
		bytes.Contains(body, []byte("event: error")) {
		t.Fatalf("status=%d requests=%d trailers=%v body=%s", response.StatusCode, requestCount, response.Trailer, body)
	}
	if response.Trailer.Get("X-MiCliProxy-Auto-Compact") != "kiro_summary_fallback" ||
		response.Trailer.Get("X-MiCliProxy-Context-Status") != "compacted" ||
		response.Trailer.Get("X-MiCliProxy-Kiro-Summary-Passes") != "1" {
		t.Fatalf("fallback trailers=%v body=%s", response.Trailer, body)
	}
}
