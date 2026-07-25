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
// upstream is held open), the client must receive SSE keepalive comments so its
// idle-SSE timer never fires. It also asserts the keepalive stops synchronously —
// no keepalive comment may trail the first real emitted frame, since the emitter
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
			case strings.HasPrefix(trimmed, ": pool-keepalive"):
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
		t.Fatalf("no ': pool-keepalive' comment during the silent pre-first-token window; downstream would hit the idle-SSE timeout:\n%s", collected.String())
	}
	if keepaliveAfterContent {
		t.Fatalf("keepalive comment emitted after real content began; stopKeepalive did not stop synchronously:\n%s", collected.String())
	}
	if !strings.Contains(collected.String(), "first") {
		t.Fatalf("stream did not deliver upstream content after the keepalive window:\n%s", collected.String())
	}
}
