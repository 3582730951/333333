package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// TestRequestGuardReleasesContextOnCloseNotBefore is the deterministic unit guard:
// a request's context must stay alive while the caller reads a streaming body and be
// released only when the body is Closed — never the instant the upstream call returns.
// (Returning resp.Body from under a `defer cancel()` cancels the context before the
// body is read, truncating the stream.)
func TestRequestGuardReleasesContextOnCloseNotBefore(t *testing.T) {
	// A huge idle window so the watchdog never fires during the test; we are testing
	// the Close→cancel semantics, not the idle timeout.
	ctx, guard := newRequestGuard(context.Background(), time.Hour)
	body := guard.Wrap(io.NopCloser(strings.NewReader("hello")))

	if ctx.Err() != nil {
		t.Fatal("context cancelled before body Close: a streamed read would be truncated")
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q, want %q", got, "hello")
	}
	if ctx.Err() != nil {
		t.Fatal("context cancelled merely by reading; it must stay alive until Close")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("context not cancelled after body Close: the request context would leak")
	}
}

// TestClaudeDirectStreamNotTruncatedByPrematureCancel is the end-to-end guard for the
// user-reported bug: a real Claude Code account that works perfectly used directly
// broke FREQUENTLY through the relay with "API Error: Failed to parse JSON" / "API
// returned an empty or malformed response (HTTP 200)" on the DIRECT egress. Root cause:
// doClaude returned the streaming resp.Body under a `defer cancel()`, cancelling the
// request context the moment doClaude returned — before the caller streamed the body —
// resetting the upstream stream. Here the upstream flushes the SSE head, then streams
// the remainder only AFTER a delay (i.e. after doClaude has returned); the full body
// must still arrive intact once the caller drains it.
func TestClaudeDirectStreamNotTruncatedByPrematureCancel(t *testing.T) {
	const head = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	const tail = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, head)
		if fl != nil {
			fl.Flush()
		}
		// The remainder is sent only after the caller has returned from doClaude and
		// is actively draining the body. Under the old defer-cancel bug the stream is
		// already reset by now, so this write never reaches the client.
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, tail)
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	resp := doDirectClaudeStream(t, upstream.URL, config.Default())
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading streamed body failed (premature cancel truncated the stream?): %v", err)
	}
	if want := head + tail; string(got) != want {
		t.Fatalf("streamed body truncated by premature cancel:\n got = %q\nwant = %q", got, want)
	}
}

// TestIdleGuardSurvivesLongProgressingStream models the user's "long context /
// continuous waiting" worry: a response whose TOTAL duration far exceeds the
// configured timeout must NOT be cut as long as it keeps making progress. The guard
// uses an IDLE timeout (reset on every read), not an absolute deadline, so a steady
// stream of small frames survives indefinitely. Here request_timeout is 1s but the
// stream runs ~1.8s with 300ms inter-frame gaps; an absolute 1s deadline would
// truncate it, the idle guard does not.
func TestIdleGuardSurvivesLongProgressingStream(t *testing.T) {
	const frames = 6
	const gap = 300 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < frames; i++ {
			time.Sleep(gap)
			_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.RequestTimeoutSeconds = 1 // idle window; total stream (~1.8s) deliberately exceeds it
	resp := doDirectClaudeStream(t, upstream.URL, cfg)
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("long progressing stream was cut (idle guard behaving as an absolute deadline?): %v", err)
	}
	if n := strings.Count(string(got), "data:"); n != frames {
		t.Fatalf("expected %d frames to survive, got %d: %q", frames, n, got)
	}
}

// TestIdleGuardAbortsSilentStream is the safety half: a genuinely hung upstream — no
// bytes for the whole idle window — is still aborted, so a dead connection cannot
// hang the relay forever. request_timeout is 1s and the upstream goes silent for
// ~1.5s after the head; the read must fail before the tail arrives.
func TestIdleGuardAbortsSilentStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(1500 * time.Millisecond) // silence longer than the 1s idle window
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.RequestTimeoutSeconds = 1
	resp := doDirectClaudeStream(t, upstream.URL, cfg)
	defer resp.Body.Close()

	_, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("expected the idle watchdog to abort a stream silent past the idle window, but the read succeeded")
	}
}

// doDirectClaudeStream issues a streaming /v1/messages request through the direct
// Claude egress against the given test upstream and returns the response.
func doDirectClaudeStream(t *testing.T, baseURL string, cfg config.Config) *Response {
	t.Helper()
	cfg.ClaudeUpstreamBaseURL = baseURL
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           []byte(`{"model":"claude-3-5-sonnet","stream":true}`),
		Account:        storage.Account{ID: "acc-claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
