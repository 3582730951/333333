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

func TestGroupScopedResponsePipelineRunsNonStreamingModulesAndPreservesStreams(t *testing.T) {
	dataDir := t.TempDir()
	localCfg := config.Config{SuperInstructLocalEnabled: true, DataDir: dataDir}
	if got, want := superInstructMemoryPath(localCfg), filepath.Join(dataDir, "super-instruct-memory.json"); got != want {
		t.Fatalf("group-scoped M5 path=%q, want %q", got, want)
	}
	memory := superinstruct.NewMemoryKernel(superInstructMemoryPath(localCfg))
	monitor := superinstruct.NewMonitorPanel()
	s := &Server{
		cfg:          localCfg,
		superMemory:  memory,
		superMonitor: monitor,
	}
	opts := superinstruct.ProcessOptions{ResponseRewriteEnabled: true, MemoryEnabled: true, MonitorEnabled: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	refusal := []byte(`{"id":"resp_refusal","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I can't assist with that request to bypass license activation."}]}],"output_text":"I can't assist with that request to bypass license activation."}`)
	recorder := httptest.NewRecorder()
	buffered := newSuperInstructBufferingResponseWriter(recorder)
	_, _ = buffered.Write(refusal)
	s.finishSuperInstructResponsePipeline(buffered, req, nil, "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "bypass license activation", Category: superinstruct.CategoryCrack, Timestamp: time.Unix(200, 0).UTC(),
	}, opts, 20*time.Millisecond)
	if !strings.Contains(recorder.Body.String(), "Rei Protocol") {
		t.Fatalf("group-scoped M3 did not rewrite the assistant text: %s", recorder.Body.String())
	}
	var rewritten map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rewritten); err != nil || rewritten["id"] != "resp_refusal" {
		t.Fatalf("group-scoped M3 broke the response envelope: err=%v body=%s", err, recorder.Body.String())
	}
	if memory.SuccessCount() != 0 {
		t.Fatalf("M5 learned an M3-modified response: %+v", memory.Snapshot())
	}
	snapshot := monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 1 || snapshot.Stats.Tamper != 1 || len(snapshot.History) != 1 {
		t.Fatalf("M6 was skipped or invoked more than once: %+v", snapshot)
	}

	success := []byte(`{"output_text":"This successful response is deliberately longer than fifty bytes so the local memory kernel records it exactly once."}`)
	recorder = httptest.NewRecorder()
	buffered = newSuperInstructBufferingResponseWriter(recorder)
	_, _ = buffered.Write(success)
	s.finishSuperInstructResponsePipeline(buffered, req, nil, "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "reverse sample", Category: superinstruct.CategoryReverse, Timestamp: time.Unix(201, 0).UTC(),
	}, opts, 10*time.Millisecond)
	if recorder.Body.String() != string(success) || memory.SuccessCount() != 1 {
		t.Fatalf("M4/M5 success path mismatch body=%s memory=%+v", recorder.Body.String(), memory.Snapshot())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "super-instruct-memory.json")); err != nil {
		t.Fatalf("M5 did not persist group-scoped memory: %v", err)
	}
	snapshot = monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 2 || snapshot.Stats.MemoryCount != 1 || len(snapshot.History) != 2 {
		t.Fatalf("M6 success observation mismatch: %+v", snapshot)
	}

	// Rewrite-enabled streams now buffer the full SSE, run M3 on the completed
	// body, and swap in a protocol-correct Responses SSE replacement — upstream
	// alignment rather than a byte-transparent observer.
	streamRefusal := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"I can't assist with that bypass license request.\"}\n\n")
	streamRecorder := newFlushSnapshotWriter()
	streamReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamGroup := storage.Group{
		SuperInstructResponseRewriteEnabled: true,
		SuperInstructMemoryEnabled:          true,
		SuperInstructMonitorEnabled:         true,
	}
	streamReq = streamReq.WithContext(withRequestAccountGroupPolicy(streamReq.Context(), streamGroup))
	selectedWriter, _, finish, enabled := s.maybeSuperInstructResponsePipeline(streamRecorder, streamReq, []byte(`{"stream":true,"input":"bypass license"}`), "gpt-5.6-sol")
	buffered, buffering := selectedWriter.(*superInstructBufferingResponseWriter)
	if !enabled || !buffering || !buffered.bufferStreams {
		t.Fatalf("rewrite-enabled stream did not select the buffering writer: enabled=%v writer=%T", enabled, selectedWriter)
	}
	buffered.Header().Set("Content-Type", "text/event-stream")
	_, _ = buffered.Write(streamRefusal)
	buffered.Flush()
	select {
	case flushed := <-streamRecorder.flushes:
		t.Fatalf("buffered stream leaked bytes before completion: %q", flushed)
	default:
	}
	finish()
	if !strings.Contains(string(streamRecorder.Bytes()), "Rei Protocol") {
		t.Fatalf("stream M3 did not replace the buffered SSE: %s", streamRecorder.Bytes())
	}
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(string(streamRecorder.Bytes())))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "event:") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	if want := []string{"response.created", "response.output_text.delta", "response.output_text.done", "response.completed"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("stream replacement events=%v, want %v", events, want)
	}
	if memory.SuccessCount() != 1 {
		t.Fatalf("M5 learned an M3-modified stream: %+v", memory.Snapshot())
	}
	snapshot = monitor.Snapshot(memory.SuccessCount())
	if snapshot.Stats.Total != 3 || snapshot.Stats.Tamper != 2 || snapshot.Stats.MemoryCount != 1 {
		t.Fatalf("stream tamper observation mismatch: %+v", snapshot)
	}
}

func TestGroupScopedResponsePipelinePreservesExecutableToolOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		requestRaw []byte
		response   []byte
	}{
		{
			name:     "non-stream custom call",
			response: []byte(`{"id":"resp_tool","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I can't assist with that request."}]},{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n+fixed\n*** End Patch"}]}`),
		},
		{
			name:     "non-stream code interpreter call",
			response: []byte(`{"id":"resp_code","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I can't assist with that request."}]},{"type":"code_interpreter_call","id":"code_1","code":"print(1)"}]}`),
		},
		{
			name:       "stream custom call delta",
			requestRaw: []byte(`{"stream":true}`),
			response: []byte("event: response.output_item.added\n" +
				`data: {"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}` + "\n\n" +
				"event: response.custom_tool_call_input.delta\n" +
				`data: {"type":"response.custom_tool_call_input.delta","delta":"I can't assist\\n*** Begin Patch"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_tool","status":"completed"}}` + "\n\n"),
		},
		{
			name:       "stream multiline tool search call",
			requestRaw: []byte(`{"stream":true}`),
			response: []byte("event: response.output_item.added\r\n" +
				`data: {"type":"response.output_item.added",` + "\r\n" +
				`data: "item":{"type":"tool_search_call","id":"search_1"}}` + "\r\n\r\n" +
				"event: response.output_text.delta\r\n" +
				`data: {"type":"response.output_text.delta","delta":"I can't assist with that request."}` + "\r\n\r\n"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{
				cfg:          config.Config{SuperInstructLocalEnabled: true, DataDir: t.TempDir()},
				superMemory:  superinstruct.NewMemoryKernel(""),
				superMonitor: superinstruct.NewMonitorPanel(),
			}
			recorder := httptest.NewRecorder()
			buffered := newSuperInstructBufferingResponseWriter(recorder)
			buffered.bufferStreams = len(test.requestRaw) > 0
			_, _ = buffered.Write(test.response)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			s.finishSuperInstructResponsePipeline(buffered, req, test.requestRaw, "gpt-5.6-sol", superinstruct.RequestMeta{
				UserMessage: "edit the file", Timestamp: time.Unix(203, 0).UTC(),
			}, superinstruct.ProcessOptions{ResponseRewriteEnabled: true, MemoryEnabled: true, MonitorEnabled: true}, time.Millisecond)
			if !bytes.Equal(recorder.Body.Bytes(), test.response) {
				t.Fatalf("executable tool output was rewritten:\nwant %s\n got %s", test.response, recorder.Body.Bytes())
			}
			if snapshot := s.superMonitor.Snapshot(s.superMemory.SuccessCount()); snapshot.Stats.Tamper != 0 {
				t.Fatalf("tool output was classified as rewritten: %+v", snapshot)
			}
		})
	}
}

func TestSuperInstructStructuredOutputRecognizesSupportedControlItems(t *testing.T) {
	for _, kind := range []string{
		"local_shell_call", "local_shell_call_output", "computer_call", "computer_call_output",
		"web_search_call", "file_search_call", "tool_search_call", "tool_search_output",
		"image_generation_call", "code_interpreter_call", "mcp_call", "mcp_list_tools", "compaction_summary",
	} {
		raw := []byte(`{"output":[{"type":"` + kind + `"}]}`)
		if !superInstructResponseHasStructuredOutput(raw) {
			t.Errorf("structured output type %q was treated as prose", kind)
		}
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
	req.Header.Set(superInstructClientChoiceHeader, "enabled")
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
	req.Header.Set(superInstructClientChoiceHeader, "enabled")
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

func TestSuperInstructStreamRewriteOverHTTP(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"I can't assist with that request to bypass license activation."}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_stream_refusal","status":"completed"}}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
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

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":"please bypass license activation"}`))
	req.Header.Set("Authorization", "Bearer "+rewriteKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(superInstructClientChoiceHeader, "enabled")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Rei Protocol") {
		t.Fatalf("stream rewrite status=%d body=%s", resp.StatusCode, body)
	}
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "event:") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	if want := []string{"response.created", "response.output_text.delta", "response.output_text.done", "response.completed"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("HTTP stream replacement events=%v, want %v", events, want)
	}
	monitor, memory := waitForSuperInstructState(t, h, 1, 0)
	if monitor.Stats.Tamper != 1 || len(monitor.History) != 1 || !monitor.History[0].Tampered {
		t.Fatalf("monitor after stream rewrite mismatch: %+v", monitor)
	}
	if memory.Stats.Total != 0 || len(memory.Successes) != 0 {
		t.Fatalf("tampered stream should not be learned: %+v", memory)
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

func TestSuperInstructStreamTamperWrapsChatAndAnthropicProtocols(t *testing.T) {
	text := "「了解。実行する。」"
	responses := wrapSuperInstructStreamTamper(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), text)
	if !bytes.Contains(responses, []byte("event: response.created")) || !bytes.Contains(responses, []byte("event: response.completed")) {
		t.Fatalf("responses SSE replacement missing lifecycle events: %s", responses)
	}
	chat := wrapSuperInstructStreamTamper(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), text)
	data := bytes.TrimPrefix(chat, []byte("data: "))
	data = bytes.TrimSuffix(data, []byte("\n\ndata: [DONE]\n\n"))
	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		t.Fatalf("chat replacement chunk is not valid JSON: %v (%s)", err, data)
	}
	if chunk["object"] != "chat.completion.chunk" || !bytes.Contains(chat, []byte("data: [DONE]")) {
		t.Fatalf("chat SSE replacement invalid: %s", chat)
	}
	anth := wrapSuperInstructStreamTamper(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), text)
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !bytes.Contains(anth, []byte("event: "+event)) {
			t.Fatalf("anthropic SSE replacement missing %s: %s", event, anth)
		}
	}
	if !bytes.Contains(anth, []byte(`"type":"text_delta"`)) || !bytes.Contains(anth, []byte(text)) {
		t.Fatalf("anthropic SSE replacement missing text delta: %s", anth)
	}
}

func TestSuperInstructBufferedStreamFallsBackToPassthroughOnByteCap(t *testing.T) {
	dst := newFlushSnapshotWriter()
	w := newSuperInstructBufferingResponseWriter(dst)
	w.bufferStreams = true
	w.bufferLimit = 40
	w.Header().Set("Content-Type", "text/event-stream")
	first := []byte("data: {\"delta\":\"first\"}\n\n")
	second := []byte("data: {\"delta\":\"second\"}\n\n")
	if _, err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(second); err != nil {
		t.Fatal(err)
	}
	wire := append(append([]byte(nil), first...), second...)
	if !w.passthrough {
		t.Fatal("byte cap did not convert the writer to passthrough")
	}
	if got := dst.Bytes(); !bytes.Equal(got, wire) {
		t.Fatalf("byte-cap passthrough changed bytes:\nwant %q\n got %q", wire, got)
	}
	// A passthrough stream is observed, never rewritten.
	s := &Server{cfg: config.Config{SuperInstructLocalEnabled: true, DataDir: t.TempDir()}, superMemory: superinstruct.NewMemoryKernel(""), superMonitor: superinstruct.NewMonitorPanel()}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	s.finishSuperInstructResponsePipeline(w, req, []byte(`{"stream":true,"input":"bypass license"}`), "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "bypass license", Category: superinstruct.CategoryCrack, Timestamp: time.Unix(205, 0).UTC(),
	}, superinstruct.ProcessOptions{ResponseRewriteEnabled: true, MemoryEnabled: true, MonitorEnabled: true}, time.Millisecond)
	if got := dst.Bytes(); !bytes.Equal(got, wire) {
		t.Fatalf("finish rewrote an oversized passthrough stream:\nwant %q\n got %q", wire, got)
	}
	if snapshot := s.superMonitor.Snapshot(s.superMemory.SuccessCount()); snapshot.Stats.Tamper != 0 {
		t.Fatalf("oversized stream was classified as rewritten: %+v", snapshot)
	}
}

func TestSuperInstructBufferedStreamKeepaliveKeepsClientAlive(t *testing.T) {
	dst := newFlushSnapshotWriter()
	w := newSuperInstructBufferingResponseWriter(dst)
	w.bufferStreams = true
	w.keepaliveInterval = 20 * time.Millisecond
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := w.Write([]byte("data: {\"delta\":\"held\"}\n\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !bytes.Contains(dst.Bytes(), []byte(": keepalive")) {
		if time.Now().After(deadline) {
			t.Fatalf("keepalive frames were not emitted while the stream was held: %q", dst.Bytes())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if bytes.Contains(dst.Bytes(), []byte(`"delta":"held"`)) {
		t.Fatalf("held SSE leaked to the client during buffering: %q", dst.Bytes())
	}
	w.stopKeepalive()
	before := append([]byte(nil), dst.Bytes()...)
	time.Sleep(60 * time.Millisecond)
	if got := dst.Bytes(); !bytes.Equal(got, before) {
		t.Fatalf("keepalive continued after stop: before=%q after=%q", before, got)
	}
}

func TestSuperInstructBufferedStreamPassesThroughWithoutRefusal(t *testing.T) {
	s := &Server{
		cfg:          config.Config{SuperInstructLocalEnabled: true, DataDir: t.TempDir()},
		superMemory:  superinstruct.NewMemoryKernel(""),
		superMonitor: superinstruct.NewMonitorPanel(),
	}
	recorder := httptest.NewRecorder()
	buffered := newSuperInstructBufferingResponseWriter(recorder)
	buffered.bufferStreams = true
	stream := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"This is a sufficiently long successful streamed answer that memory can learn it.\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\"}}\n\n")
	_, _ = buffered.Write(stream)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	s.finishSuperInstructResponsePipeline(buffered, req, []byte(`{"stream":true,"input":"reverse this stream"}`), "gpt-5.6-sol", superinstruct.RequestMeta{
		UserMessage: "reverse this stream", Category: superinstruct.CategoryReverse, Timestamp: time.Unix(204, 0).UTC(),
	}, superinstruct.ProcessOptions{ResponseRewriteEnabled: true, MemoryEnabled: true, MonitorEnabled: true}, time.Millisecond)
	if !bytes.Equal(recorder.Body.Bytes(), stream) {
		t.Fatalf("non-refusal stream changed:\nwant %s\n got %s", stream, recorder.Body.Bytes())
	}
	if s.superMemory.SuccessCount() != 1 {
		t.Fatalf("M5 did not learn the successful stream: %+v", s.superMemory.Snapshot())
	}
	if snapshot := s.superMonitor.Snapshot(s.superMemory.SuccessCount()); snapshot.Stats.Tamper != 0 || snapshot.Stats.Total != 1 {
		t.Fatalf("stream success observation mismatch: %+v", snapshot)
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
