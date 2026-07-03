package api

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/usage"
)

// writeUpstreamHeaders copies upstream response headers to the downstream client
// using the same hop-by-hop / relay-internal denylist as copyDownstreamHeaders,
// and additionally drops pool-internal account/quota leak headers (x-codex-*,
// openai-model, x-ratelimit-*, anthropic-ratelimit-*, anthropic-organization-*)
// when leak-scrub is enabled. This is the single place every gateway response
// path filters headers.
func (s *Server) writeUpstreamHeaders(ctx context.Context, dst, src http.Header) {
	stripLeak := s.leakScrubEnabled(ctx)
	for k, values := range src {
		lower := strings.ToLower(k)
		if lower == "authorization" ||
			lower == "chatgpt-account-id" ||
			lower == "x-openai-fedramp" ||
			strings.HasPrefix(lower, "x-pool-") ||
			strings.HasPrefix(lower, "x-sidecar-") ||
			lower == "content-length" ||
			// The body we forward is always plaintext: the direct path requests
			// Accept-Encoding: identity, and the sidecar auto-decompresses (and
			// already drops these). Never propagate an encoding/length that would
			// describe the upstream's wire bytes rather than what we actually send.
			lower == "content-encoding" ||
			lower == "transfer-encoding" ||
			lower == "connection" {
			continue
		}
		if stripLeak && leakfilter.IsLeakHeader(lower) {
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

// scannerResponseWriter wraps an http.ResponseWriter so that every byte written
// to the downstream client is simultaneously fed into a usage.StreamScanner. This
// eliminates the io.TeeReader on the read side — and its per-byte copy into the
// scanner's Write buffer — by instead feeding the scanner from the write side where
// the bytes already exist. The StreamScanner only cares about complete SSE frames
// (it parses line-by-line), so feeding it post-scrub/rewrite bytes is equivalent:
// sensitive-word scrubbing replaces in-band text (e.g. a pseudo-username embedded
// in a tool result), never the JSON usage fields; leak-filter drops rate-limit
// frames entirely — those frames carry no usage data by definition.
type scannerResponseWriter struct {
	http.ResponseWriter
	scanner *usage.StreamScanner
}

func (sw *scannerResponseWriter) Write(p []byte) (int, error) {
	sw.scanner.Write(p)
	return sw.ResponseWriter.Write(p)
}

// streamSSE forwards an upstream SSE stream to the client. When leak-scrub is on
// it routes through the SSE frame filter (drops pool-internal rate-limit frames,
// scrubs sensitive words); otherwise it uses the plain boundary-safe scrubbing
// copy. provider is "claude" for the Anthropic protocol, "codex" otherwise.
//
// It also feeds the stream through a usage.StreamScanner and records the token usage
// once the stream finishes. Streaming is the common case for real CLI traffic, so
// without this the usage tables (and the whole admin overview built on them) would
// only ever reflect the rare non-streaming response. Usage is recorded even when the
// copy ends in an error, so a client that disconnects mid-stream after the upstream
// already reported its counts is still billed/metered for what was produced.
func (s *Server) streamSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, words *streamrewrite.Matcher, provider, accountID, routeHash string) error {
	scanner := usage.NewStreamScanner(provider)
	// Wrap the client writer so every byte forwarded to the downstream also feeds
	// the scanner — no TeeReader copy on the read side.
	sw := &scannerResponseWriter{ResponseWriter: w, scanner: scanner}
	var err error
	if !s.leakScrubEnabled(ctx) {
		err = streamCopyRewrite(sw, body, words)
	} else {
		err = leakfilter.NewSSEFilter(provider, words).Copy(sw, body)
	}
	if parsed, ok := scanner.Parsed(); ok {
		log.Printf("[STREAM-USAGE] provider=%s, account=%s, model=%s, prompt=%d, completion=%d, total=%d, cached=%d",
			provider, accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens, parsed.CachedTokens)
		s.recordParsedUsage(ctx, accountID, routeHash, parsed)
	} else {
		log.Printf("[STREAM-USAGE] provider=%s, account=%s: NO USAGE EXTRACTED", provider, accountID)
	}
	return err
}

const (
	earlySSEMaxBytes  = 64 * 1024
	earlySSEMaxFrames = 8
)

func probeEarlyCodexSSEFailure(body io.Reader) ([]byte, bool, error) {
	return probeEarlySSEFailure(body, leakfilter.IsRetryableCodexFailureFrame, codexSSEFrameCommitsContent)
}

func probeEarlyClaudeSSEFailure(body io.Reader) ([]byte, bool, error) {
	return probeEarlySSEFailure(body, leakfilter.IsRetryableClaudeErrorFrame, claudeSSEFrameCommitsContent)
}

func probeEarlySSEFailure(body io.Reader, retryableFrame func([]byte) bool, contentFrame func([]byte) bool) ([]byte, bool, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	processed := 0
	frames := 0
	for len(buf) < earlySSEMaxBytes && frames < earlySSEMaxFrames {
		for {
			idx := bytes.Index(buf[processed:], []byte("\n\n"))
			if idx < 0 {
				break
			}
			end := processed + idx + 2
			frame := buf[processed:end]
			frames++
			if retryableFrame(frame) {
				return buf, true, nil
			}
			if contentFrame(frame) {
				return buf, false, nil
			}
			processed = end
			if frames >= earlySSEMaxFrames {
				return buf, false, nil
			}
		}
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				if n > 0 {
					continue
				}
				return buf, false, nil
			}
			return buf, false, err
		}
	}
	return buf, false, nil
}

func codexSSEFrameCommitsContent(frame []byte) bool {
	lower := strings.ToLower(string(frame))
	for _, marker := range []string{
		"response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.audio.delta",
		`"delta"`,
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func claudeSSEFrameCommitsContent(frame []byte) bool {
	lower := strings.ToLower(string(frame))
	for _, marker := range []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"text_delta",
		"input_json_delta",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// writeFilteredError writes an upstream error response to the downstream client,
// neutralizing pool-internal limit/quota/overload/billing bodies into a generic
// account-agnostic error when leak-scrub is on, and always filtering headers.
// provider is "claude" for the Anthropic protocol, "codex" otherwise.
func (s *Server) writeFilteredError(ctx context.Context, w http.ResponseWriter, provider string, status int, header http.Header, body []byte, words *streamrewrite.Matcher) {
	out := body
	if s.leakScrubEnabled(ctx) {
		if ns, nb, changed := leakfilter.NeutralizeErrorBody(provider, status, body); changed {
			status = ns
			out = nb
		}
	}
	if words != nil && !words.Empty() {
		out = words.ReplaceAll(out)
	}
	s.writeUpstreamHeaders(ctx, w.Header(), header)
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
