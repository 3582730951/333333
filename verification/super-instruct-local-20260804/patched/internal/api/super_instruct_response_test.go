package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
)

func TestSuperInstructMonitorPublishesHeadlessM6Events(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.pool.URL+"/admin/super-instruct/monitor/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("monitor event stream status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	if event, _ := readSuperInstructTestEvent(t, reader); event != "snapshot" {
		t.Fatalf("first M6 event=%q, want snapshot", event)
	}
	h.app.superMonitor.Record(&superinstruct.ResponseContext{
		Meta:   superinstruct.RequestMeta{UserMessage: "inspect", Category: superinstruct.CategoryReverse, Timestamp: time.Unix(123, 0).UTC()},
		Parsed: superinstruct.ParsedResponse{Reply: "observed"},
	})
	event, interaction := readSuperInstructTestEvent(t, reader)
	if event != "interaction" || !strings.Contains(interaction, `"category":"reverse"`) {
		t.Fatalf("M6 interaction event=%q data=%s", event, interaction)
	}
	event, stats := readSuperInstructTestEvent(t, reader)
	if event != "stats" || !strings.Contains(stats, `"total":1`) || !strings.Contains(stats, `"reverse":1`) {
		t.Fatalf("M6 stats event=%q data=%s", event, stats)
	}
}

func TestHeadlessLocalResponsePipelineRunsM4M3M5M6Once(t *testing.T) {
	dataDir := t.TempDir()
	localCfg := config.Config{SuperInstructLocalEnabled: true, DataDir: dataDir}
	if got, want := superInstructMemoryPath(localCfg), filepath.Join(dataDir, "memory.json"); got != want {
		t.Fatalf("local M5 path=%q, want %q", got, want)
	}
	memory := superinstruct.NewMemoryKernel(superInstructMemoryPath(localCfg))
	monitor := superinstruct.NewMonitorPanel()
	s := &Server{
		cfg:          localCfg,
		superMemory:  memory,
		superMonitor: monitor,
	}
	selectionReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	selectedWriter, _, _, enabled := s.maybeSuperInstructResponsePipeline(
		httptest.NewRecorder(), selectionReq, []byte(`{"stream":true}`), "gpt-5.6-sol",
	)
	selectedBuffer, bufferedSelection := selectedWriter.(*superInstructBufferingResponseWriter)
	if !enabled || !bufferedSelection || !selectedBuffer.bufferStreams {
		t.Fatalf("local streamed response did not select the source buffering pipeline: enabled=%v writer=%T", enabled, selectedWriter)
	}
	opts := superinstruct.ProcessOptions{ResponseRewriteEnabled: true, MemoryEnabled: true, MonitorEnabled: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	refusal := []byte(`{"output_text":"I can't assist with that request to bypass license activation."}`)
	recorder := httptest.NewRecorder()
	buffered := newSuperInstructBufferingResponseWriter(recorder)
	_, _ = buffered.Write(refusal)
	s.finishSuperInstructResponsePipeline(buffered, req, nil, "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "bypass license activation", Category: superinstruct.CategoryCrack, Timestamp: time.Unix(200, 0).UTC(),
	}, opts, 20*time.Millisecond)
	if !strings.Contains(recorder.Body.String(), "Rei Protocol") || strings.HasPrefix(strings.TrimSpace(recorder.Body.String()), "{") {
		t.Fatalf("local M3 did not return the source replacement body: %s", recorder.Body.String())
	}
	if memory.SuccessCount() != 0 {
		t.Fatalf("local M5 learned an M3-modified response: %+v", memory.Snapshot())
	}
	snapshot := monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 1 || snapshot.Stats.Tamper != 1 || len(snapshot.History) != 1 {
		t.Fatalf("local M6 was skipped or invoked more than once: %+v", snapshot)
	}

	success := []byte(`{"output_text":"This successful response is deliberately longer than fifty bytes so the local memory kernel records it exactly once."}`)
	recorder = httptest.NewRecorder()
	buffered = newSuperInstructBufferingResponseWriter(recorder)
	_, _ = buffered.Write(success)
	s.finishSuperInstructResponsePipeline(buffered, req, nil, "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "reverse sample", Category: superinstruct.CategoryReverse, Timestamp: time.Unix(201, 0).UTC(),
	}, opts, 10*time.Millisecond)
	if recorder.Body.String() != string(success) || memory.SuccessCount() != 1 {
		t.Fatalf("local M4/M5 success path mismatch body=%s memory=%+v", recorder.Body.String(), memory.Snapshot())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "memory.json")); err != nil {
		t.Fatalf("local M5 did not persist memory.json: %v", err)
	}
	snapshot = monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 2 || snapshot.Stats.MemoryCount != 1 || len(snapshot.History) != 2 {
		t.Fatalf("local M6 success observation mismatch: %+v", snapshot)
	}

	streamRefusal := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"I can't assist with that bypass license request.\"}\n\n")
	recorder = httptest.NewRecorder()
	buffered = newSuperInstructBufferingResponseWriter(recorder)
	buffered.bufferStreams = true
	buffered.Header().Set("Content-Type", "text/event-stream")
	_, _ = buffered.Write(streamRefusal)
	buffered.Flush()
	if recorder.Body.Len() != 0 {
		t.Fatalf("local source-compatible stream leaked upstream chunks before M4: %s", recorder.Body.String())
	}
	s.finishSuperInstructResponsePipeline(buffered, req, []byte(`{"stream":true}`), "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "bypass license", Category: superinstruct.CategoryCrack, Timestamp: time.Unix(202, 0).UTC(),
	}, opts, 30*time.Millisecond)
	streamBody := recorder.Body.String()
	for _, event := range []string{"response.created", "response.output_text.delta", "response.output_text.done", "response.completed"} {
		if !strings.Contains(streamBody, "event: "+event) {
			t.Fatalf("local M3 SSE replacement missing %s: %s", event, streamBody)
		}
	}
	if !strings.Contains(streamBody, `"id":"resp_tamper"`) || memory.SuccessCount() != 1 {
		t.Fatalf("local streamed M3/M5 behavior mismatch body=%s memory=%+v", streamBody, memory.Snapshot())
	}
	snapshot = monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 3 || snapshot.Stats.Tamper != 2 || len(snapshot.History) != 3 {
		t.Fatalf("local streamed M6 observation mismatch: %+v", snapshot)
	}
}

func readSuperInstructTestEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	event, data := "", ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read monitor event: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" && event != "" {
			return event, data
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func TestSuperInstructResponseRewriteMemoryMonitor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), "bypass license") {
			_, _ = w.Write([]byte(`{"id":"resp_refusal","object":"response","created_at":1785830400,"status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg_refusal","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"I can't assist with that request to bypass license activation."}]}],"output_text":"I can't assist with that request to bypass license activation.","usage":{"input_tokens":11,"output_tokens":13,"total_tokens":24,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":5}},"metadata":{"trace":"keep-refusal"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_success","object":"response","created_at":1785830401,"status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg_success","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"This is a sufficiently long successful reverse engineering answer that should be learned by memory and left unchanged for the downstream client."}]}],"output_text":"This is a sufficiently long successful reverse engineering answer that should be learned by memory and left unchanged for the downstream client.","usage":{"input_tokens":17,"output_tokens":29,"total_tokens":46},"metadata":{"trace":"keep-success"}}`))
	})

	rewriteKey := createTestAPIKeyForUserGroup(t, h, "si-rewrite-pool", map[string]interface{}{
		"super_instruct_response_rewrite_enabled": true,
		"super_instruct_memory_enabled":           true,
		"super_instruct_monitor_enabled":          true,
	})
	rewriteAcc := h.importAccount(t, "si-rewrite", "up-si-rewrite", "access-si-rewrite")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+rewriteAcc+"/group", `{"group":"si-rewrite-pool"}`); code != http.StatusOK {
		t.Fatalf("assign rewrite group = %d: %s", code, raw)
	}
	setTestCapability(t, h, rewriteAcc, "gpt-5.6-sol", 272000)

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"please bypass license activation"}`))
	req.Header.Set("Authorization", "Bearer "+rewriteKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Rei Protocol") {
		t.Fatalf("rewrite response status=%d body=%s", resp.StatusCode, body)
	}
	var rewritten map[string]interface{}
	if err := json.Unmarshal(body, &rewritten); err != nil {
		t.Fatalf("decode rewritten response: %v (%s)", err, body)
	}
	if rewritten["id"] != "resp_refusal" || rewritten["status"] != "completed" || rewritten["metadata"].(map[string]interface{})["trace"] != "keep-refusal" {
		t.Fatalf("native response envelope changed: %+v", rewritten)
	}
	usage := rewritten["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(13) || usage["total_tokens"] != float64(24) {
		t.Fatalf("usage changed during rewrite: %+v", usage)
	}
	output := rewritten["output"].([]interface{})[0].(map[string]interface{})
	content := output["content"].([]interface{})[0].(map[string]interface{})
	if output["id"] != "msg_refusal" || content["text"] != rewritten["output_text"] || !strings.Contains(content["text"].(string), "Rei Protocol") {
		t.Fatalf("assistant text was not rewritten in place: %+v", rewritten)
	}

	monitor, memory := waitForSuperInstructState(t, h, 1, 0)
	if monitor.Stats.Total != 1 || monitor.Stats.Tamper != 1 || len(monitor.History) != 1 || !monitor.History[0].Tampered {
		t.Fatalf("monitor after rewrite mismatch: %+v", monitor)
	}
	if memory.Stats.Total != 0 || len(memory.Successes) != 0 {
		t.Fatalf("tampered response should not be learned: %+v", memory)
	}

	memoryKey := createTestAPIKeyForUserGroup(t, h, "si-memory-pool", map[string]interface{}{
		"super_instruct_memory_enabled":  true,
		"super_instruct_monitor_enabled": true,
	})
	memoryAcc := h.importAccount(t, "si-memory", "up-si-memory", "access-si-memory")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+memoryAcc+"/group", `{"group":"si-memory-pool"}`); code != http.StatusOK {
		t.Fatalf("assign memory group = %d: %s", code, raw)
	}
	setTestCapability(t, h, memoryAcc, "gpt-5.6-sol", 272000)

	req, _ = http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"reverse this binary safely"}`))
	req.Header.Set("Authorization", "Bearer "+memoryKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "Rei Protocol") || !strings.Contains(string(body), "sufficiently long successful reverse engineering answer") {
		t.Fatalf("memory response status=%d body=%s", resp.StatusCode, body)
	}
	var unchanged map[string]interface{}
	if err := json.Unmarshal(body, &unchanged); err != nil || unchanged["id"] != "resp_success" || unchanged["metadata"].(map[string]interface{})["trace"] != "keep-success" {
		t.Fatalf("memory-only response envelope changed: err=%v body=%s", err, body)
	}

	monitor, memory = waitForSuperInstructState(t, h, 2, 1)
	if memory.Stats.Total != 1 || memory.Stats.Reverse != 1 || len(memory.Successes) != 1 {
		t.Fatalf("memory after success mismatch: %+v", memory)
	}
	if monitor.Stats.Total != 2 || monitor.Stats.Tamper != 1 || monitor.Stats.MemoryCount != 1 || len(monitor.History) != 2 {
		t.Fatalf("monitor after success mismatch: %+v", monitor)
	}
}

func TestSuperInstructRewriteSingleAssistantTextPreservesEnvelope(t *testing.T) {
	raw := []byte(`{"id":"resp_native","object":"response","created_at":1785830402,"status":"completed","model":"gpt-5.6-sol","previous_response_id":"resp_previous","output":[{"id":"msg_native","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[{"type":"url_citation","start_index":0,"end_index":4,"url":"https://example.invalid","title":"kept"}],"logprobs":[],"text":"original assistant text"}]}],"output_text":"original assistant text","usage":{"input_tokens":9007199254740993,"output_tokens":9,"total_tokens":9007199254741002},"metadata":{"trace":"keep","nested":{"n":7}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	got, valid := superInstructRewriteSingleAssistantText(req, raw, "replacement assistant text")
	if !valid {
		t.Fatal("official single-message Responses envelope was not recognized")
	}
	before := decodeJSONUseNumber(t, raw)
	after := decodeJSONUseNumber(t, got)
	for _, key := range []string{"id", "object", "created_at", "status", "model", "previous_response_id", "usage", "metadata"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("field %s changed:\nwant %#v\n got %#v", key, before[key], after[key])
		}
	}
	message := after["output"].([]interface{})[0].(map[string]interface{})
	block := message["content"].([]interface{})[0].(map[string]interface{})
	if message["id"] != "msg_native" || block["text"] != "replacement assistant text" || after["output_text"] != block["text"] {
		t.Fatalf("rewrite did not stay inside the assistant text fields: %s", got)
	}
	beforeBlock := before["output"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if !reflect.DeepEqual(beforeBlock["annotations"], block["annotations"]) || !reflect.DeepEqual(beforeBlock["logprobs"], block["logprobs"]) {
		t.Fatalf("assistant content metadata changed: %s", got)
	}
}

func TestSuperInstructRewriteChatAndAnthropicPreserveEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		raw  string
		want string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			raw:  `{"id":"chat_native","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"original"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10},"system_fingerprint":"fp_keep"}`,
			want: `{"id":"chat_native","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"replacement"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10},"system_fingerprint":"fp_keep"}`,
		},
		{
			name: "anthropic messages",
			path: "/v1/messages",
			raw:  `{"id":"msg_native","type":"message","role":"assistant","model":"claude-native","content":[{"type":"text","text":"original","citations":[]}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`,
			want: `{"id":"msg_native","type":"message","role":"assistant","model":"claude-native","content":[{"type":"text","text":"replacement","citations":[]}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			got, valid := superInstructRewriteSingleAssistantText(req, []byte(tc.raw), "replacement")
			if !valid || string(got) != tc.want {
				t.Fatalf("in-place rewrite mismatch: valid=%v\nwant %s\n got %s", valid, tc.want, got)
			}
		})
	}
}

func TestSuperInstructRewriteSkipsAmbiguousToolAndReasoningResponses(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"text"}]}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "reasoning plus message", raw: `{"id":"resp_reasoning","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"reasoning"}]},` + message + `]}`},
		{name: "tool plus message", raw: `{"id":"resp_tool","output":[` + message + `,{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}`},
		{name: "multiple text blocks", raw: `{"id":"resp_multi","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one"},{"type":"output_text","text":"two"}]}]}`},
		{name: "mismatched convenience text", raw: `{"id":"resp_mismatch","output":[` + message + `],"output_text":"different"}`},
		{name: "unknown structure", raw: `{"id":"resp_unknown","output":[{"type":"future_output","text":"text"}]}`},
		{name: "trailing second value", raw: `{"id":"resp_trailing","output":[` + message + `]} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := superInstructRewriteSingleAssistantText(req, []byte(tc.raw), "replacement")
			if valid || !bytes.Equal(got, []byte(tc.raw)) {
				t.Fatalf("ambiguous response was modified: valid=%v\nwant %s\n got %s", valid, tc.raw, got)
			}
		})
	}
}

func TestSuperInstructObservationIsBoundedAndQueueIsNonBlocking(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), superInstructObservationLimit+4096)
	dst := httptest.NewRecorder()
	w := newSuperInstructObservingResponseWriter(dst)
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("observing write n=%d err=%v", n, err)
	}
	if !bytes.Equal(dst.Body.Bytes(), payload) {
		t.Fatal("bounded observation changed downstream bytes")
	}
	if w.body.Len() != superInstructObservationLimit || !bytes.Equal(w.body.Bytes(), payload[:superInstructObservationLimit]) {
		t.Fatalf("observation capture len=%d, want %d", w.body.Len(), superInstructObservationLimit)
	}

	queue := make(chan superInstructObservation, 1)
	first := superInstructObservation{status: http.StatusOK, body: []byte("first")}
	if !enqueueSuperInstructObservation(queue, first) {
		t.Fatal("first observation was not enqueued")
	}
	started := time.Now()
	if enqueueSuperInstructObservation(queue, superInstructObservation{body: []byte("second")}) {
		t.Fatal("full observation queue accepted another item")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("congested observation enqueue blocked for %s", elapsed)
	}
	if got := <-queue; string(got.body) != "first" {
		t.Fatalf("queued observation changed: %+v", got)
	}
}

func TestSuperInstructObservingStreamIsByteIdenticalAndImmediate(t *testing.T) {
	first := []byte("event: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_native\"}}\r\n\r\n")
	second := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"This is a sufficiently long streamed answer for asynchronous Memory and Monitor observation.\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_native\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n")
	dst := newFlushSnapshotWriter()
	w := newSuperInstructObservingResponseWriter(dst)
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	select {
	case snapshot := <-dst.flushes:
		if !bytes.Equal(snapshot, first) {
			t.Fatalf("first stream flush changed:\nwant %q\n got %q", first, snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("transparent Super-Instruct writer delayed the first stream frame")
	}
	if _, err := w.Write(second); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	wire := append(append([]byte(nil), first...), second...)
	if got := dst.Bytes(); !bytes.Equal(got, wire) {
		t.Fatalf("stream bytes changed:\nwant %q\n got %q", wire, got)
	}

	memory := superinstruct.NewMemoryKernel("")
	monitor := superinstruct.NewMonitorPanel()
	processor := superinstruct.NewProcessor(memory, monitor)
	startSuperInstructObservationWorker()
	finishSuperInstructObservation(processor, http.StatusOK, w.body.Bytes(), false, superinstruct.RequestMeta{
		UserMessage: "reverse this stream",
		Category:    superinstruct.CategoryReverse,
		Timestamp:   time.Now().UTC(),
	}, superinstruct.ProcessOptions{MemoryEnabled: true, MonitorEnabled: true}, time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for monitor.Stats(memory.SuccessCount()).Total < 1 || memory.SuccessCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("asynchronous stream observation did not finish: monitor=%+v memory=%+v", monitor.Snapshot(memory.SuccessCount()), memory.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := monitor.Snapshot(memory.SuccessCount())
	if len(snapshot.History) != 1 || snapshot.History[0].Bytes != len(wire) {
		t.Fatalf("stream observation mismatch: %+v", snapshot)
	}
}

func decodeJSONUseNumber(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out map[string]interface{}
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("decode JSON: %v (%s)", err, raw)
	}
	return out
}

func waitForSuperInstructState(t *testing.T, h *testHarness, monitorTotal, memoryTotal uint64) (superinstruct.MonitorSnapshot, superinstruct.MemoryData) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var monitor superinstruct.MonitorSnapshot
	var memory superinstruct.MemoryData
	for {
		monitorCode, monitorRaw := grpReq(t, h, http.MethodGet, "/admin/super-instruct/monitor", "")
		memoryCode, memoryRaw := grpReq(t, h, http.MethodGet, "/admin/super-instruct/memory", "")
		if monitorCode != http.StatusOK || memoryCode != http.StatusOK {
			t.Fatalf("super-instruct state status monitor=%d memory=%d", monitorCode, memoryCode)
		}
		if err := json.Unmarshal(monitorRaw, &monitor); err != nil {
			t.Fatalf("decode monitor: %v (%s)", err, monitorRaw)
		}
		if err := json.Unmarshal(memoryRaw, &memory); err != nil {
			t.Fatalf("decode memory: %v (%s)", err, memoryRaw)
		}
		if monitor.Stats.Total >= monitorTotal && memory.Stats.Total >= memoryTotal {
			return monitor, memory
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for monitor=%d memory=%d; got monitor=%+v memory=%+v", monitorTotal, memoryTotal, monitor, memory)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSuperInstructResponseProfilesSelectByModelFamily(t *testing.T) {
	group := storage.Group{SuperInstructProfiles: storage.SuperInstructProfiles{
		storage.ModelInstructionFamilyGPT: {
			ResponseRewriteEnabled: true,
			MonitorEnabled:         true,
		},
		storage.ModelInstructionFamilyClaude: {
			MonitorEnabled: true,
		},
	}}
	gpt := superInstructResponseFeatures(group, "chatgpt-5")
	if !gpt.ResponseRewriteEnabled || !gpt.MonitorEnabled || gpt.MemoryEnabled {
		t.Fatalf("gpt features mismatch: %+v", gpt)
	}
	claude := superInstructResponseFeatures(group, "claude-sonnet-4.6")
	if claude.ResponseRewriteEnabled || !claude.MonitorEnabled || claude.MemoryEnabled {
		t.Fatalf("claude features mismatch: %+v", claude)
	}
	gemini := superInstructResponseFeatures(group, "gemini-3.2-pro")
	if gemini.Enabled() {
		t.Fatalf("gemini should be disabled without a profile: %+v", gemini)
	}
}
