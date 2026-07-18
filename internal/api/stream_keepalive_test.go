package api

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/streamrewrite"
)

// chunkReader returns the source bytes in fixed-size pieces so a test can exercise
// read-boundary handling (a frame — or a sensitive word — split across reads).
type chunkReader struct {
	data []byte
	size int
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.size
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	return n, nil
}

// runLegacyRelay reproduces exactly what streamSSE does when no keepalive is
// configured: streamCopyRewrite for leak-off, leakfilter.Copy for leak-on.
func runLegacyRelay(leak bool, provider string, words *streamrewrite.Matcher, src io.Reader) (string, error) {
	rec := httptest.NewRecorder()
	var err error
	if !leak {
		err = streamCopyRewrite(rec, src, words)
	} else {
		err = leakfilter.NewSSEFilter(provider, words).Copy(rec, src)
	}
	return rec.Body.String(), err
}

// runPumpRelay runs the frame-aligned keepalive pump (rf=nil) with an interval large
// enough that no heartbeat fires, so its output must match the legacy relay byte for
// byte. This is the substitution streamSSE makes when stream_keepalive_seconds > 0.
func runPumpRelay(leak bool, provider string, words *streamrewrite.Matcher, src io.Reader) (string, error) {
	rec := httptest.NewRecorder()
	err := newRuleSSECopyWithHeartbeat(context.Background(), rec, src, nil, leak, words, provider, time.Hour)
	return rec.Body.String(), err
}

// TestStreamKeepAliveOutputParity is the guard the followup plan flagged as the ONE
// RISK for the general keepalive: routing the no-rule stream through the per-frame
// pump must produce byte-identical output to the legacy streamCopyRewrite /
// leakfilter.Copy relay. If this ever diverges, enabling stream_keepalive_seconds
// would silently alter downstream bytes, so we assert exact equality here.
func TestStreamKeepAliveOutputParity(t *testing.T) {
	scrub := streamrewrite.NewFromMap(map[string]string{"/home/secretuser": "/home/user", "hunter2": "REDACTED"})
	empty := streamrewrite.New(nil)

	codexContent := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello world"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"

	// A pool-internal rate-limit frame the leak filter must drop, wrapped by content.
	codexWithLeak := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: codex.rate_limits\n" +
		`data: {"type":"codex.rate_limits","primary_used_percent":42}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"working in /home/secretuser now"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"

	// A Claude error frame that carries a limit signature (neutralized in place) plus
	// ordinary content frames.
	claudeWithError := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"my key is hunter2"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":{"type":"rate_limit_error","message":"This account has hit its usage limit."}}` + "\n\n"

	// A trailing frame with no terminating blank line, to exercise the EOF-tail path.
	codexNoFinalTerminator := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"tail"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"unterminated"}`

	cases := []struct {
		name     string
		leak     bool
		provider string
		words    *streamrewrite.Matcher
		stream   string
		chunk    int // 0 = one strings.Reader; >0 = chunkReader of that piece size
	}{
		{"codex-leakoff-empty-passthrough", false, "codex", empty, codexContent, 0},
		{"codex-leakoff-scrub", false, "codex", scrub, codexWithLeak, 0},
		{"codex-leakon-drop-ratelimit", true, "codex", empty, codexWithLeak, 0},
		{"codex-leakon-drop-and-scrub", true, "codex", scrub, codexWithLeak, 0},
		{"claude-leakon-neutralize", true, "claude", empty, claudeWithError, 0},
		{"claude-leakon-neutralize-and-scrub", true, "claude", scrub, claudeWithError, 0},
		{"codex-leakoff-tail-no-terminator", false, "codex", empty, codexNoFinalTerminator, 0},
		{"codex-leakon-tail-no-terminator", true, "codex", empty, codexNoFinalTerminator, 0},
		// Split reads: a sensitive word and frame boundaries land mid-read. Both the
		// streaming rewriter (via carry) and the pump (via frame reassembly before
		// ReplaceAll) must still agree.
		{"codex-leakoff-scrub-chunked-3", false, "codex", scrub, codexWithLeak, 3},
		{"codex-leakon-drop-scrub-chunked-7", true, "codex", scrub, codexWithLeak, 7},
		{"claude-leakon-neutralize-chunked-5", true, "claude", scrub, claudeWithError, 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var legacySrc, pumpSrc io.Reader
			if tc.chunk > 0 {
				legacySrc = &chunkReader{data: []byte(tc.stream), size: tc.chunk}
				pumpSrc = &chunkReader{data: []byte(tc.stream), size: tc.chunk}
			} else {
				legacySrc = strings.NewReader(tc.stream)
				pumpSrc = strings.NewReader(tc.stream)
			}
			legacyOut, err := runLegacyRelay(tc.leak, tc.provider, tc.words, legacySrc)
			if err != nil {
				t.Fatalf("legacy relay error: %v", err)
			}
			pumpOut, err := runPumpRelay(tc.leak, tc.provider, tc.words, pumpSrc)
			if err != nil {
				t.Fatalf("pump relay error: %v", err)
			}
			if legacyOut != pumpOut {
				t.Fatalf("output parity broken\n legacy: %q\n pump:   %q", legacyOut, pumpOut)
			}
		})
	}
}

// TestStreamKeepAliveEmitsHeartbeatDuringSilence confirms the pump actually bridges an
// idle upstream with the provider-appropriate keepalive frame — response.in_progress
// for Codex, ping for Claude — without disturbing the terminal event.
func TestStreamKeepAliveEmitsHeartbeatDuringSilence(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		heartbeat string
	}{
		{"codex", "response.in_progress"},
		{"claude", "event: ping"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			reader, writer := io.Pipe()
			rec := httptest.NewRecorder()
			done := make(chan error, 1)
			go func() {
				done <- newRuleSSECopyWithHeartbeat(context.Background(), rec, reader, nil, true, nil, tc.provider, 10*time.Millisecond)
			}()
			go func() {
				_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
				time.Sleep(45 * time.Millisecond) // upstream silent long enough for >=1 heartbeat
				_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
				_ = writer.Close()
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("keepalive relay did not finish")
			}
			got := rec.Body.String()
			if !strings.Contains(got, tc.heartbeat) {
				t.Fatalf("expected keepalive frame %q during idle, got: %q", tc.heartbeat, got)
			}
			if !strings.Contains(got, "response.completed") {
				t.Fatalf("terminal event missing after heartbeat: %q", got)
			}
		})
	}
}
