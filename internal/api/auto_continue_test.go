package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

// ---- helpers ----

func mustJSON(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json: %v\n%s", err, b)
	}
	return m
}

// parsedFrames splits an SSE payload into (event,type,data-map) tuples.
type parsedFrame struct {
	event string
	typ   string
	data  map[string]interface{}
}

func parseSSE(t *testing.T, payload string) []parsedFrame {
	t.Helper()
	var out []parsedFrame
	for _, chunk := range strings.Split(payload, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		ev, data := sseFrameEventData([]byte(chunk + "\n\n"))
		pf := parsedFrame{event: ev}
		if len(data) > 0 && strings.TrimSpace(string(data)) != "[DONE]" {
			var m map[string]interface{}
			if json.Unmarshal(data, &m) == nil {
				pf.data = m
				pf.typ = streamString(m["type"])
			}
		}
		if pf.typ == "" {
			pf.typ = ev
		}
		out = append(out, pf)
	}
	return out
}

func idxOf(frames []parsedFrame, typ string) int {
	for i, f := range frames {
		if f.typ == typ {
			return i
		}
	}
	return -1
}

// ---- body builders ----

func TestBuildClaudeContinueBody(t *testing.T) {
	orig := `{"model":"claude-x","system":"S","messages":[{"role":"user","content":"Q"}]}`
	out, ok := buildClaudeContinueBody([]byte(orig), "partial answer", "keep going")
	if !ok {
		t.Fatal("builder failed")
	}
	m := mustJSON(t, out)
	if m["system"] != "S" {
		t.Fatalf("stable prefix (system) lost: %v", m["system"])
	}
	if m["stream"] != true {
		t.Fatal("stream not forced on")
	}
	msgs, _ := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (orig user + assistant partial + user continue), got %d: %v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]interface{})
	if last["role"] != "user" || last["content"] != "keep going" {
		t.Fatalf("continue turn wrong: %v", last)
	}
	mid := msgs[1].(map[string]interface{})
	if mid["role"] != "assistant" || mid["content"] != "partial answer" {
		t.Fatalf("re-injected partial wrong: %v", mid)
	}
}

func TestBuildCodexContinueBody(t *testing.T) {
	resolved := `{"model":"gpt-x","previous_response_id":"resp_old","turn_state":{"opaque":"old"},"input":[{"role":"user","content":[{"type":"input_text","text":"Q"}]}]}`
	partial := []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "half"}}}}
	out, ok := buildCodexContinueBody([]byte(resolved), partial, "resume")
	if !ok {
		t.Fatal("builder failed")
	}
	m := mustJSON(t, out)
	if _, has := m["previous_response_id"]; has {
		t.Fatal("previous_response_id must be stripped for a stateless continuation")
	}
	if _, has := m["turn_state"]; has {
		t.Fatal("turn_state must be stripped for a stateless continuation")
	}
	if m["stream"] != true {
		t.Fatal("stream not forced on")
	}
	input, _ := m["input"].([]interface{})
	if len(input) != 3 {
		t.Fatalf("want 3 input items (orig user + assistant partial + user continue), got %d", len(input))
	}
	last := input[2].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("continue turn wrong: %v", last)
	}
}

// ---- taps / detectors ----

func TestClaudeStreamTapTruncatedVsComplete(t *testing.T) {
	truncated := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n"
	tap := &claudeStreamTap{}
	_, _ = tap.Write([]byte(truncated))
	if tap.reachedTerminal() {
		t.Fatal("truncated stream must not be terminal")
	}
	if tap.partialText() != "Hello" {
		t.Fatalf("partial text=%q", tap.partialText())
	}
	if tap.blocksOpened != 1 || !tap.openBlock || tap.openBlockIndex != 0 {
		t.Fatalf("block state wrong: opened=%d open=%v idx=%d", tap.blocksOpened, tap.openBlock, tap.openBlockIndex)
	}

	complete := tap2Feed()
	tap2 := &claudeStreamTap{}
	_, _ = tap2.Write([]byte(complete))
	if !tap2.reachedTerminal() {
		t.Fatal("complete stream must be terminal")
	}
	if tap2.openBlock {
		t.Fatal("complete stream should have no open block")
	}
	if !tap2.completedSuccessfully() {
		t.Fatal("message_stop must be recorded as a successful terminal")
	}

	errorTap := &claudeStreamTap{}
	_, _ = errorTap.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream failed\"}}\n\n"))
	if !errorTap.reachedTerminal() {
		t.Fatal("Claude error event must be recorded as terminal")
	}
	if errorTap.completedSuccessfully() {
		t.Fatal("Claude error event must not be recorded as successful")
	}
}

func tap2Feed() string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// ---- Claude stitch ----

func TestStitchClaudeContinuationOffsetsAndSuppresses(t *testing.T) {
	continuation := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m2\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" rest\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var buf strings.Builder
	tap, err := stitchClaudeContinuation(&buf, strings.NewReader(continuation), 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !tap.reachedTerminal() {
		t.Fatal("continuation reached message_stop; tap should report terminal")
	}
	frames := parseSSE(t, buf.String())

	// First emitted frame must close the still-open prior block (index 0).
	if frames[0].typ != "content_block_stop" || jsonIntValue(frames[0].data["index"]) != 0 {
		t.Fatalf("expected leading content_block_stop{index:0}, got %+v", frames[0])
	}
	// The continuation's own message_start must be suppressed.
	if idxOf(frames, "message_start") != -1 {
		t.Fatal("second message_start leaked downstream")
	}
	// Continuation content blocks must be offset to index 1.
	cbs := -1
	for _, f := range frames {
		if f.typ == "content_block_start" {
			cbs = jsonIntValue(f.data["index"])
		}
		if f.typ == "content_block_delta" && jsonIntValue(f.data["index"]) != 1 {
			t.Fatalf("continuation delta not offset to index 1: %+v", f)
		}
	}
	if cbs != 1 {
		t.Fatalf("continuation block_start not offset to index 1 (got %d)", cbs)
	}
	if idxOf(frames, "message_stop") == -1 {
		t.Fatal("terminal message_stop missing")
	}
}

// ---- Codex stitch ----

func TestStitchCodexContinuationOffsetsSuppressesAndRewritesCompleted(t *testing.T) {
	priorItems := []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "Part"}}}}
	continuation := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\"}}\n\n" +
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"i2\",\"type\":\"message\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"more\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"i2\",\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"more\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"more\"}]}],\"output_text\":\"more\"}}\n\n"
	var buf strings.Builder
	rec, err := stitchCodexContinuation(&buf, strings.NewReader(continuation), priorItems, "Part", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.reachedTerminal() {
		t.Fatal("continuation reached response.completed; recorder should report terminal")
	}
	frames := parseSSE(t, buf.String())
	if idxOf(frames, "response.created") != -1 {
		t.Fatal("continuation response.created leaked downstream")
	}
	progressIndex := idxOf(frames, "response.in_progress")
	if progressIndex == -1 || len(frames[progressIndex].data) != 1 {
		t.Fatal("minimal continuation heartbeat was not preserved")
	}
	// output_index offset by priorItemCount (1).
	for _, f := range frames {
		if oi, ok := f.data["output_index"]; ok {
			if jsonIntValue(oi) != 1 {
				t.Fatalf("output_index not offset to 1: %+v", f)
			}
		}
	}
	ci := idxOf(frames, "response.completed")
	if ci == -1 {
		t.Fatal("terminal response.completed missing")
	}
	resp := frames[ci].data["response"].(map[string]interface{})
	out, _ := resp["output"].([]interface{})
	if len(out) != 2 {
		t.Fatalf("completed.output should carry prior + continuation items (2), got %d", len(out))
	}
	if resp["output_text"] != "Partmore" {
		t.Fatalf("completed.output_text should be merged 'Partmore', got %v", resp["output_text"])
	}
}

// ---- orchestrators (with a synthetic re-issue) ----

func TestAutoContinueClaudeStitchesToTerminal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := &claudeStreamTap{}
	_, _ = first.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n"))

	var capturedBody []byte
	reissue := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
		capturedBody = body
		return io.NopCloser(strings.NewReader(tap2Feed())), nil
	}
	var buf strings.Builder
	if _, err := h.app.autoContinueClaude(context.Background(), &buf, []byte(`{"model":"c","messages":[{"role":"user","content":"hi"}]}`), first, reissue, nil); err != nil {
		t.Fatal(err)
	}
	// The re-issue must carry the partial answer + a continue turn.
	cb := mustJSON(t, capturedBody)
	msgs := cb["messages"].([]interface{})
	if len(msgs) != 3 || msgs[1].(map[string]interface{})["content"] != "Hello" {
		t.Fatalf("continuation body did not re-inject partial: %v", msgs)
	}
	frames := parseSSE(t, buf.String())
	if idxOf(frames, "message_stop") == -1 {
		t.Fatalf("stitched stream missing terminal: %s", buf.String())
	}
	if idxOf(frames, "message_start") != -1 {
		t.Fatal("duplicate message_start leaked")
	}
}

func TestAutoContinueClaudeFailsTerminalWhenStillTruncated(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := &claudeStreamTap{}
	_, _ = first.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"X\"}}\n\n"))
	// Continuation is ALSO truncated (no message_stop); with max attempts 1 the loop then
	// emits a protocol error terminal so the client does not mistake a truncated
	// result for a completed long-running task.
	reissue := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Y\"}}\n\n")), nil
	}
	var buf strings.Builder
	if _, err := h.app.autoContinueClaude(context.Background(), &buf, []byte(`{"model":"c","messages":[{"role":"user","content":"hi"}]}`), first, reissue, nil); err != nil {
		t.Fatal(err)
	}
	frames := parseSSE(t, buf.String())
	if idxOf(frames, "message_stop") == -1 || idxOf(frames, "error") == -1 || idxOf(frames, "message_delta") != -1 {
		t.Fatalf("truncated stream must emit error+message_stop: %s", buf.String())
	}
}

func TestAutoContinueCodexStitchesToTerminal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := newCodexStreamLedgerRecorder()
	_, _ = first.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"i1\",\"type\":\"message\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"Part\"}\n\n"))
	if first.reachedTerminal() {
		t.Fatal("first stream is truncated")
	}
	reissue := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"i2\",\"type\":\"message\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"more\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"more\"}]}],\"output_text\":\"more\"}}\n\n")), nil
	}
	var buf strings.Builder
	if _, err := h.app.autoContinueCodex(context.Background(), &buf, []byte(`{"model":"gpt","input":[{"role":"user","content":[{"type":"input_text","text":"Q"}]}]}`), first, reissue); err != nil {
		t.Fatal(err)
	}
	frames := parseSSE(t, buf.String())
	if idxOf(frames, "response.created") != -1 {
		t.Fatal("duplicate response.created leaked from continuation")
	}
	ci := idxOf(frames, "response.completed")
	if ci == -1 {
		t.Fatalf("stitched stream missing terminal: %s", buf.String())
	}
	resp := frames[ci].data["response"].(map[string]interface{})
	if resp["output_text"] != "Partmore" {
		t.Fatalf("merged output_text wrong: %v", resp["output_text"])
	}
}

// ---- A#3: per-rule opt-in on safety-buffering ----

func TestValidateRulePreservesAutoContinueOnSafetyBuffering(t *testing.T) {
	r := &storage.UpstreamErrorRule{Name: "x", DownstreamAction: "hide_safety_buffering", AccountAction: "auto_continue"}
	if err := validateUpstreamErrorRule(r); err != nil {
		t.Fatal(err)
	}
	if r.AccountAction != upstreamrules.AccountActionAutoContinue {
		t.Fatalf("auto_continue must be preserved on a safety-buffering rule, got %q", r.AccountAction)
	}
	// Any other account action on a safety-buffering rule is still forced to none, so a
	// healthy account is never punished for an informational buffering signal.
	r2 := &storage.UpstreamErrorRule{Name: "y", DownstreamAction: "hide_safety_buffering", AccountAction: "cooldown"}
	if err := validateUpstreamErrorRule(r2); err != nil {
		t.Fatal(err)
	}
	if r2.AccountAction != upstreamrules.AccountActionNone {
		t.Fatalf("non-auto_continue account action must be forced to none, got %q", r2.AccountAction)
	}
}

func TestAutoContinueEnabledViaRuleDecision(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if h.app.autoContinueEnabled(ctx, nil) {
		t.Fatal("global auto-continue must default off")
	}
	dec := &upstreamErrorRuleDecision{Match: upstreamrules.MatchResult{AccountAction: upstreamrules.AccountActionAutoContinue}}
	if !h.app.autoContinueEnabled(ctx, dec) {
		t.Fatal("a rule opting in with auto_continue must enable it even when the global flag is off")
	}
	rf := &responseRuleFilter{Rule: &storage.UpstreamErrorRule{AccountAction: "auto_continue"}}
	if !h.app.autoContinueEnabled(ctx, autoContinueDecisionFromFilter(rf)) {
		t.Fatal("a safety-buffering filter carrying auto_continue must enable continuation")
	}
	if autoContinueDecisionFromFilter(nil) != nil {
		t.Fatal("nil filter must yield nil decision")
	}
}
