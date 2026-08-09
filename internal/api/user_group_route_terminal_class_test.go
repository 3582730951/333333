package api

import (
	"strings"
	"testing"

	"codex-account-pool/internal/leakfilter"
)

// other_terminal absorbed 442 of 445 terminal failures in one diagnostics export, all on
// HTTP 200. These pin that the refinement names a cause without widening what upstream
// text can reach the artifact, and that no existing class changed meaning.
func TestTerminalErrorClassNamesTheUpstreamCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure leakfilter.CodexFailureFrame
		want    string
	}{
		// A code that is not one of the leak signatures is non-retryable and statusless --
		// the shape the 442 rows had.
		{"structured code becomes the class", leakfilter.CodexFailureFrame{ErrorCode: "invalid_prompt"}, "invalid_prompt"},
		{"unknown code is still named", leakfilter.CodexFailureFrame{ErrorCode: "some_future_code_2"}, "some_future_code_2"},
		{"no code at all is distinguishable", leakfilter.CodexFailureFrame{EventType: "response.failed"}, "unclassified_stream_failure"},

		// Unchanged classes. retryable_terminal deliberately outranks the code so an
		// existing retry-semantics query keeps counting the same rows.
		{"retryable keeps precedence", leakfilter.CodexFailureFrame{ErrorCode: "server_error", BuiltinRetryable: true}, "retryable_terminal"},
		{"cyber policy unchanged", leakfilter.CodexFailureFrame{ErrorCode: "cyber_policy"}, "cyber_policy"},
		{"context length unchanged", leakfilter.CodexFailureFrame{ErrorCode: "context_length_exceeded"}, "context_length_exceeded"},
		{"context error unchanged", leakfilter.CodexFailureFrame{ContextError: leakfilter.ResponsesContextErrorOrphanedToolOutput}, "orphaned_tool_output"},

		// ErrorCode is upstream-controlled and only trimmed, so anything that is a message,
		// a URL or a blob rather than an identifier must stay behind the catch-all.
		{"prose is not a code", leakfilter.CodexFailureFrame{ErrorCode: "Rate limit reached for org-1234"}, "other_terminal"},
		{"url is not a code", leakfilter.CodexFailureFrame{ErrorCode: "https://api.example.com/v1/x"}, "other_terminal"},
		{"uppercase is not a code", leakfilter.CodexFailureFrame{ErrorCode: "Server_Error"}, "other_terminal"},
		{"non-ascii is not a code", leakfilter.CodexFailureFrame{ErrorCode: "请求超限"}, "other_terminal"},
		{"leading underscore is not a code", leakfilter.CodexFailureFrame{ErrorCode: "_internal"}, "other_terminal"},
		// 40 bytes is where diagnosticHighEntropyCandidate starts looking. A code at or above
		// it could ride the sanitizer's snake_case exemption into the export unaliased, so the
		// gate stops below the threshold rather than relying on that exemption.
		{"at the entropy threshold is not a code", leakfilter.CodexFailureFrame{ErrorCode: strings.Repeat("a", 40)}, "other_terminal"},
		{"just under the threshold is still a code", leakfilter.CodexFailureFrame{ErrorCode: strings.Repeat("a", 39)}, strings.Repeat("a", 39)},
		{"hex blob is not a code", leakfilter.CodexFailureFrame{ErrorCode: "a3f9c1d0e7b4a2f8c6d1e0b9a7f3c2d1e0b9a7f3"}, "other_terminal"},

		// A code equal to a reserved class would make a failure read as a different
		// failure, or as a success.
		{"none must never be emitted", leakfilter.CodexFailureFrame{ErrorCode: "none"}, "other_terminal"},
		{"reserved class is not adopted", leakfilter.CodexFailureFrame{ErrorCode: "retryable_terminal"}, "other_terminal"},
	} {
		if got := safeTerminalErrorClass(tc.failure); got != tc.want {
			t.Errorf("%s: class=%q want %q", tc.name, got, tc.want)
		}
	}
}

// The 442 rows were statusless response.failed frames inside a 200 response. Parsing one
// end to end proves the class now names the cause on the exact shape that produced them.
func TestStatuslessFailedFrameIsClassifiedByItsCode(t *testing.T) {
	// No leak signature in the frame, so the parser leaves it non-retryable and statusless
	// -- per its own comment, a genuine client error passed through unchanged. That is the
	// path that reached the catch-all.
	frame := []byte("event: response.failed\n" +
		`data: {"type":"response.failed","response":{"error":{"code":"invalid_prompt","message":"Your request was rejected."}}}` + "\n\n")
	failure, ok := leakfilter.ParseCodexFailureFrame(frame)
	if !ok {
		t.Fatal("frame did not parse as a terminal failure")
	}
	if failure.StatusCode != 0 || failure.BuiltinRetryable {
		t.Fatalf("status=%d retryable=%v want 0/false, the shape these rows had",
			failure.StatusCode, failure.BuiltinRetryable)
	}
	if got := safeTerminalErrorClass(failure); got != "invalid_prompt" {
		t.Fatalf("class=%q want invalid_prompt", got)
	}
	if strings.Contains(safeTerminalErrorClass(failure), "rejected") {
		t.Fatal("the upstream message must not reach the class")
	}
}
