package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

func TestHeartbeatFinishForRuleEmitsOneProviderFrame(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"ok"}`)) })
	for _, tc := range []struct {
		provider string
		want     string
	}{
		{"claude", "event: ping"},
		{"codex", "response.in_progress"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			rec := &flushRecorder{header: http.Header{}}
			h.app.writeHeartbeatFinishForRule(rec, tc.provider)
			if rec.status != http.StatusOK {
				t.Fatalf("status=%d want 200", rec.status)
			}
			if ct := rec.header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
				t.Fatalf("content-type=%q", ct)
			}
			got := rec.body.String()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("body %q missing keepalive %q", got, tc.want)
			}
			// Exactly one frame, and never fabricated model content.
			if strings.Count(got, "\n\n") != 1 {
				t.Fatalf("expected exactly one SSE frame, got %q", got)
			}
			if strings.Contains(got, "output_text") || strings.Contains(got, "content_block") {
				t.Fatalf("heartbeat_finish must not fabricate content: %q", got)
			}
		})
	}
}

func TestWriteRuleDownstreamHeartbeatFinishDispatch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"ok"}`)) })
	decision := upstreamErrorRuleDecision{
		Rule:  storage.UpstreamErrorRule{Name: "hb", DownstreamAction: upstreamrules.DownstreamActionHeartbeatFinish},
		Match: upstreamrules.MatchResult{DownstreamAction: upstreamrules.DownstreamActionHeartbeatFinish},
	}

	// Streaming: emit the keepalive frame and close.
	streamRec := &flushRecorder{header: http.Header{}}
	if !h.app.writeRuleDownstream(context.Background(), streamRec, "codex", 500, http.Header{}, []byte("boom"), nil, decision, true) {
		t.Fatal("streaming heartbeat_finish should report handled=true")
	}
	if streamRec.status != http.StatusOK || !strings.Contains(streamRec.body.String(), "response.in_progress") {
		t.Fatalf("streaming dispatch wrong: status=%d body=%q", streamRec.status, streamRec.body.String())
	}

	// Non-streaming: an SSE heartbeat would be malformed for a JSON client, so it must
	// fall back to a neutral 503 like idle_stream does.
	jsonRec := &flushRecorder{header: http.Header{}}
	if !h.app.writeRuleDownstream(context.Background(), jsonRec, "codex", 500, http.Header{}, []byte("boom"), nil, decision, false) {
		t.Fatal("non-streaming heartbeat_finish should still report handled=true")
	}
	if jsonRec.status != http.StatusServiceUnavailable {
		t.Fatalf("non-streaming should neutralize to 503, got %d", jsonRec.status)
	}
}
