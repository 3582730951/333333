package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type codexNativeIdentityCapture struct {
	Session string
	Thread  string
	Turn    string
	Window  string
	State   string
	Body    string
}

func enableCodexSessionMappingForTest(h *testHarness) {
	h.app.cfg.CodexSessionMappingEnabled = true
	h.app.cfg.CodexCPAStrict = true
}

func TestCodexSessionMappingKeepsNativeIdentityAndToolOutput(t *testing.T) {
	var mu sync.Mutex
	var captures []codexNativeIdentityCapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture := codexNativeIdentityCapture{
			Session: r.Header.Get("Session-Id"),
			Thread:  r.Header.Get("Thread-Id"),
			Turn:    turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")),
			Window:  r.Header.Get("X-Codex-Window-Id"),
			State:   r.Header.Get("X-Codex-Turn-State"),
			Body:    string(raw),
		}
		mu.Lock()
		captures = append(captures, capture)
		call := len(captures)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.Header().Set("X-Codex-Turn-State", "opaque-state-1")
			_, _ = w.Write([]byte(`{"id":"resp-map-1","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		w.Header().Set("X-Codex-Turn-State", "opaque-state-2")
		_, _ = w.Write([]byte(`{"id":"resp-map-2","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "mapping", "upstream-mapping", "access-mapping")

	post := func(body string, state string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "client-root-thread")
		req.Header.Set("Session-Id", "client-root-thread")
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := post(`{"model":"gpt","thread_id":"client-root-thread","session_id":"client-root-thread","input":[{"role":"user","content":"start"}]}`, "")
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || first.Header.Get("X-MiCliProxy-Context-Engine") != "cpa-v2" || first.Header.Get("X-Codex-Turn-State") != "opaque-state-1" || !bytes.Contains(firstBody, []byte("resp-map-1")) {
		t.Fatalf("first status=%d engine=%q turn_state=%q body=%s", first.StatusCode, first.Header.Get("X-MiCliProxy-Context-Engine"), first.Header.Get("X-Codex-Turn-State"), firstBody)
	}

	second := post(`{"model":"gpt","previous_response_id":"resp-map-1","input":[{"type":"custom_tool_call_output","call_id":"call-client-1","output":{"exact":900719925474099312345}}]}`, "opaque-state-1")
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || !bytes.Contains(secondBody, []byte("resp-map-2")) {
		t.Fatalf("second status=%d body=%s", second.StatusCode, secondBody)
	}

	mu.Lock()
	got := append([]codexNativeIdentityCapture(nil), captures...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d", len(got))
	}
	if got[0].Session == "" || got[0].Session != got[0].Thread || got[0].Thread == "client-root-thread" {
		t.Fatalf("root identity not internally mapped: %+v", got[0])
	}
	if strings.Contains(got[0].Body, "client-root-thread") {
		t.Fatalf("downstream session correlator leaked into upstream body: %s", got[0].Body)
	}
	if got[1].Session != got[0].Session || got[1].Thread != got[0].Thread || got[1].Turn == got[0].Turn {
		t.Fatalf("identity lifecycle mismatch first=%+v second=%+v", got[0], got[1])
	}
	if got[1].State != "opaque-state-1" || !strings.Contains(got[1].Body, `"previous_response_id":"resp-map-1"`) || !strings.Contains(got[1].Body, `"type":"custom_tool_call_output"`) || !strings.Contains(got[1].Body, "900719925474099312345") {
		t.Fatalf("state/tool output was not native: %+v", got[1])
	}
	if !strings.HasSuffix(got[0].Window, ":0") || got[1].Window != got[0].Window {
		t.Fatalf("window lifecycle first=%q second=%q", got[0].Window, got[1].Window)
	}

	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-map-1"})
	if err != nil || len(rows) != 1 || rows[0].AccountID == "" {
		t.Fatalf("durable response mapping rows=%+v err=%v", rows, err)
	}
	if _, err := h.store.GetAffinityBinding(context.Background(), routing.ResponseAffinityKey("resp-map-1").Hash); !storage.NotFound(err) {
		t.Fatalf("raw response id leaked into legacy affinity binding: %v", err)
	}
}

func TestCodexSessionMappingPersistsInstallationIDAcrossOSHints(t *testing.T) {
	var mu sync.Mutex
	installationIDs := []string{}
	userAgents := []string{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("upstream payload: %v", err)
		}
		var installationID string
		if err := json.Unmarshal(payload.ClientMetadata["x-codex-installation-id"], &installationID); err != nil || installationID == "" {
			t.Fatalf("mapped installation id=%q err=%v payload=%s", installationID, err, raw)
		}
		mu.Lock()
		installationIDs = append(installationIDs, installationID)
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		call := len(installationIDs)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"resp-installation-root","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-installation-next","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.IdentityOSSource = "downstream"
	h.importAccount(t, "installation", "upstream-installation", "access-installation")

	post := func(body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "installation-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// The root presents macOS; the tool-result turn explicitly presents Linux.
	// Without a persisted virtual installation identity those two OS hints derive
	// different devices and the upstream rejects the otherwise valid response id.
	first := post(`{"model":"gpt","input":[{"role":"user","content":"work in /Users/alice/project"}]}`)
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("root status=%d", first.StatusCode)
	}
	second := post(`{"model":"gpt","previous_response_id":"resp-installation-root","input":[{"type":"function_call_output","call_id":"call-installation","output":"Platform: linux"}]}`)
	_, _ = io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("continuation status=%d", second.StatusCode)
	}

	mu.Lock()
	got := append([]string(nil), installationIDs...)
	gotUserAgents := append([]string(nil), userAgents...)
	mu.Unlock()
	if len(got) != 2 || got[0] == "" || got[1] != got[0] {
		t.Fatalf("installation identity changed across strict continuation: %q", got)
	}
	if len(gotUserAgents) != 2 || gotUserAgents[0] == "" || gotUserAgents[1] != gotUserAgents[0] {
		t.Fatalf("device OS profile changed across strict continuation: %q", gotUserAgents)
	}
	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-installation-root"})
	if err != nil || len(rows) != 1 || rows[0].InstallationID != got[0] || rows[0].DeviceOSHint != "Mac OS" || !rows[0].DeviceOSHintSet {
		t.Fatalf("durable installation mapping rows=%+v err=%v", rows, err)
	}
}

func TestNativeCodexContinuePreservesInstructionSnapshotLitePrefix(t *testing.T) {
	original := setResponsesInstructions([]byte(`{"model":"gpt-5.6-sol","instructions":"must disappear","previous_response_id":"resp-old","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec","format":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"old user turn"}],"exact":900719925474099312345}]}`), "snapshotted administrator base")
	continued, err := nativeCodexContinueBody(original, "resp-latest")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(continued, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["previous_response_id"]) != `"resp-latest"` {
		t.Fatalf("previous response was not updated: %s", continued)
	}
	if _, exists := root["instructions"]; exists {
		t.Fatalf("Lite continuation regained top-level instructions: %s", continued)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(root["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 3 || !strings.Contains(string(input[0]), `900719925474099312345`) || !strings.Contains(string(input[1]), "snapshotted administrator base") || strings.Contains(string(continued), "old user turn") {
		t.Fatalf("Lite continuation did not retain only its stable prefix: %s", continued)
	}
	if !strings.Contains(string(input[2]), `"text":"continue"`) || !strings.Contains(string(root["stream"]), "true") {
		t.Fatalf("native continuation input missing: %s", continued)
	}
}

func TestCodexSessionMappingPersistsTurnStateFromStreamMetadata(t *testing.T) {
	var calls atomic.Int32
	var resumedState string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.metadata\ndata: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"opaque-metadata-state\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-metadata-state\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-metadata-state\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		resumedState = r.Header.Get("X-Codex-Turn-State")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-metadata-state-next","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "metadata-state", "upstream-metadata-state", "access-metadata-state")

	firstReq, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","stream":true,"input":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("Thread-Id", "metadata-root")
	first, err := http.DefaultClient.Do(firstReq)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || !bytes.Contains(firstBody, []byte("opaque-metadata-state")) || !bytes.Contains(firstBody, []byte("resp-metadata-state")) {
		t.Fatalf("metadata stream status=%d body=%s", first.StatusCode, firstBody)
	}

	secondReq, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","previous_response_id":"resp-metadata-state","input":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("X-Codex-Turn-State", "opaque-metadata-state")
	second, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || calls.Load() != 2 || resumedState != "opaque-metadata-state" || !bytes.Contains(secondBody, []byte("resp-metadata-state-next")) {
		t.Fatalf("metadata resume status=%d calls=%d upstream-state=%q body=%s", second.StatusCode, calls.Load(), resumedState, secondBody)
	}
}

func TestCodexSessionMappingAdvancesWindowAfterBodyTriggeredCompaction(t *testing.T) {
	var mu sync.Mutex
	var windows []string
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		windows = append(windows, r.Header.Get("X-Codex-Window-Id"))
		call := calls.Add(1)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-` + strconv.Itoa(int(call)) + `","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "compact-window", "upstream-compact-window", "access-compact-window")

	post := func(body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "compact-window-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, result)
		}
	}
	post(`{"model":"gpt","input":"before compact"}`)
	post(`{"model":"gpt","previous_response_id":"resp-compact-1","compaction_trigger":true,"input":"compact"}`)
	post(`{"model":"gpt","previous_response_id":"resp-compact-2","input":"after compact"}`)
	mu.Lock()
	got := append([]string(nil), windows...)
	mu.Unlock()
	if len(got) != 3 || !strings.HasSuffix(got[0], ":0") || got[1] != got[0] || !strings.HasSuffix(got[2], ":1") || strings.TrimSuffix(got[2], ":1") != strings.TrimSuffix(got[0], ":0") {
		t.Fatalf("compaction window lifecycle=%v", got)
	}
}

func TestCodexSessionMappingUsesOneIdentityAcrossDownstreamWebSocketTurns(t *testing.T) {
	var mu sync.Mutex
	var upstreamHeaders http.Header
	var payloads []map[string]interface{}
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		upstreamHeaders = r.Header.Clone()
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		for index, responseID := range []string{"resp-ws-map-1", "resp-ws-map-2"} {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				t.Fatalf("read upstream websocket payload %d: %v", index, readErr)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode upstream websocket payload: %v\n%s", err, raw)
			}
			mu.Lock()
			payloads = append(payloads, payload)
			mu.Unlock()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+responseID+`","object":"response","model":"gpt","status":"in_progress"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+responseID+`","object":"response","model":"gpt","status":"completed","output":[]}}`))
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "ws-map", "upstream-ws-map", "access-ws-map")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream websocket dial: %v body=%s", err, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	sendAndReadTerminal := func(raw string, responseID string) {
		t.Helper()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			_, event, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(event, []byte(responseID)) {
				t.Fatalf("downstream websocket event missing response id %q: %s", responseID, event)
			}
		}
	}
	sendAndReadTerminal(`{"type":"response.create","model":"gpt","thread_id":"client-ws-root","session_id":"client-ws-root","input":"start","stream":true}`, "resp-ws-map-1")
	sendAndReadTerminal(`{"type":"response.create","model":"gpt","previous_response_id":"resp-ws-map-1","input":"resume","stream":true}`, "resp-ws-map-2")

	mu.Lock()
	headers := upstreamHeaders.Clone()
	gotPayloads := append([]map[string]interface{}(nil), payloads...)
	mu.Unlock()
	if len(gotPayloads) != 2 {
		t.Fatalf("upstream websocket payloads=%d", len(gotPayloads))
	}
	sessionID, threadID := headers.Get("Session-Id"), headers.Get("Thread-Id")
	if sessionID == "" || sessionID != threadID || sessionID == "client-ws-root" || !isCodexUUIDv7(sessionID) {
		t.Fatalf("upstream websocket root headers session=%q thread=%q", sessionID, threadID)
	}
	metadata := func(payload map[string]interface{}) map[string]interface{} {
		value, _ := payload["client_metadata"].(map[string]interface{})
		return value
	}
	firstMetadata, secondMetadata := metadata(gotPayloads[0]), metadata(gotPayloads[1])
	firstSession, _ := firstMetadata["session_id"].(string)
	firstThread, _ := firstMetadata["thread_id"].(string)
	secondSession, _ := secondMetadata["session_id"].(string)
	secondThread, _ := secondMetadata["thread_id"].(string)
	firstTurn := turnIDFromMetadata(firstMetadata)
	secondTurn := turnIDFromMetadata(secondMetadata)
	if firstSession != sessionID || firstThread != threadID || secondSession != sessionID || secondThread != threadID || !isCodexUUIDv7(firstTurn) || !isCodexUUIDv7(secondTurn) || firstTurn == secondTurn {
		t.Fatalf("websocket identity metadata first=%+v second=%+v headers=%v", firstMetadata, secondMetadata, headers)
	}
	if _, present := gotPayloads[0]["thread_id"]; present {
		t.Fatalf("downstream root thread leaked as top-level upstream parameter: %+v", gotPayloads[0])
	}
	if rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-ws-map-2"}); err != nil || len(rows) != 1 || rows[0].RootSessionID != sessionID || rows[0].ThreadID != threadID {
		t.Fatalf("websocket terminal mapping rows=%+v err=%v", rows, err)
	}
}

func turnIDFromMetadata(metadata map[string]interface{}) string {
	raw, _ := metadata["x-codex-turn-metadata"].(string)
	return turnIDFromHeader(raw)
}

func isCodexUUIDv7(value string) bool {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Version() == 7
}

func TestCodexSessionMappingRejectsUnknownStateBeforeUpstream(t *testing.T) {
	called := false
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	enableCodexSessionMappingForTest(h)
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","previous_response_id":"resp-unknown","input":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("codex_session_mapping_unidentified")) || called {
		t.Fatalf("status=%d called=%v body=%s", resp.StatusCode, called, body)
	}
}

func TestCodexSessionMappingPassesToolOutput400WithoutRotation(t *testing.T) {
	var calls atomic.Int32
	var secondBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-tool-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		secondBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"No tool output found for custom tool call call-native."},"status":400,"type":"error"}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "tool-400", "upstream-tool-400", "access-tool-400")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "tool-400-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-tool-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	status, body := post(`{"model":"gpt","previous_response_id":"resp-tool-origin","input":[{"type":"custom_tool_call_output","call_id":"call-native","output":{"exact":900719925474099312345}}]}`)
	if status != http.StatusBadRequest || calls.Load() != 2 || !strings.Contains(body, "No tool output found") || strings.Contains(body, "response.failed") {
		t.Fatalf("native tool 400 status=%d calls=%d body=%s", status, calls.Load(), body)
	}
	if !strings.Contains(secondBody, `"type":"custom_tool_call_output"`) || !strings.Contains(secondBody, "900719925474099312345") {
		t.Fatalf("tool result was changed before upstream: %s", secondBody)
	}
	resolved, err := h.store.ResolveCodexSessionAliases(context.Background(), "unauthenticated", []storage.CodexSessionAlias{{Type: "response", Value: "resp-tool-origin"}})
	if err != nil || resolved.State != "active" {
		t.Fatalf("ordinary tool 400 must retain active epoch binding=%+v err=%v", resolved, err)
	}
}

func TestCodexSessionMappingRetiresOnlyAfterUpstreamContextLoss(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"previous_response_not_found","message":"Previous response resp-retired-origin was not found."}}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "retire", "upstream-retire", "access-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","previous_response_id":"resp-retired-origin","input":"resume"}`); status != http.StatusBadRequest || !strings.Contains(body, "previous_response_not_found") {
		t.Fatalf("context loss must stay native status=%d body=%s", status, body)
	}
	status, body := post(`{"model":"gpt","previous_response_id":"resp-retired-origin","input":"resume again"}`)
	if status != http.StatusConflict || calls.Load() != 2 || !strings.Contains(body, "codex_context_epoch_retired") {
		t.Fatalf("retired epoch status=%d calls=%d body=%s", status, calls.Load(), body)
	}
}

func TestCodexSessionMappingRetiresOnSoft200PreviousResponseNotFound(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-soft-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		// The backend sometimes returns this error envelope with HTTP 200. The
		// semantic status is nested in error.message and must still retire CPA.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp-soft-retired-origin' not found.\",\"param\":\"previous_response_id\"},\"status\":400}"},"status":400,"type":"error"}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "soft-retire", "upstream-soft-retire", "access-soft-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "soft-retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-soft-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","previous_response_id":"resp-soft-retired-origin","input":"resume"}`); status != http.StatusBadRequest || !strings.Contains(body, "previous_response_not_found") {
		t.Fatalf("soft context loss must surface as native 400 status=%d body=%s", status, body)
	}
	status, body := post(`{"model":"gpt","previous_response_id":"resp-soft-retired-origin","input":"resume again"}`)
	if status != http.StatusConflict || calls.Load() != 2 || !strings.Contains(body, "codex_context_epoch_retired") {
		t.Fatalf("soft-retired epoch status=%d calls=%d body=%s", status, calls.Load(), body)
	}
}

func TestCodexSessionMappingRetiresAfterNativeStreamContextLoss(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-stream-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"status\":400,\"response\":{\"id\":\"resp-stream-retired-origin\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"type\":\"previous_response_not_found\",\"message\":\"Previous response was not found.\"}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "stream-retire", "upstream-stream-retire", "access-stream-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "stream-retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-stream-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","stream":true,"previous_response_id":"resp-stream-retired-origin","input":"resume"}`); status != http.StatusOK || !strings.Contains(body, "previous_response_not_found") || strings.Contains(body, "codex_native_continue_failed") {
		t.Fatalf("native stream context loss status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","previous_response_id":"resp-stream-retired-origin","input":"retry"}`); status != http.StatusConflict || calls.Load() != 2 || !strings.Contains(body, "codex_context_epoch_retired") {
		t.Fatalf("retired stream epoch status=%d calls=%d body=%s", status, calls.Load(), body)
	}
}

func TestCodexSessionMappingEOFUsesOneNativeContinue(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var sessions []string
	var turns []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		sessions = append(sessions, r.Header.Get("Session-Id")+"/"+r.Header.Get("Thread-Id"))
		turns = append(turns, turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-eof-source\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-eof-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-eof-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "eof", "upstream-eof", "access-eof")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"original turn must not be replayed"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("resp-eof-final")) || bytes.Contains(body, []byte("codex_native_continue_failed")) {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, body)
	}
	if got := bytes.Count(body, []byte("data: [DONE]")); got != 1 {
		t.Fatalf("truncated stream DONE leaked before native continuation: count=%d body=%s", got, body)
	}
	mu.Lock()
	gotBodies := append([]string(nil), bodies...)
	gotSessions := append([]string(nil), sessions...)
	gotTurns := append([]string(nil), turns...)
	mu.Unlock()
	if len(gotBodies) != 2 || gotSessions[0] == "" || gotSessions[0] != gotSessions[1] || gotTurns[0] == "" || gotTurns[0] == gotTurns[1] {
		t.Fatalf("native continue identity bodies=%d sessions=%v turns=%v", len(gotBodies), gotSessions, gotTurns)
	}
	var continuation map[string]interface{}
	if err := json.Unmarshal([]byte(gotBodies[1]), &continuation); err != nil {
		t.Fatal(err)
	}
	if continuation["previous_response_id"] != "resp-eof-source" || strings.Contains(gotBodies[1], "original turn must not be replayed") {
		t.Fatalf("continuation must use only native upstream state: %s", gotBodies[1])
	}
	input, _ := continuation["input"].([]interface{})
	if len(input) != 1 || !strings.Contains(gotBodies[1], `"text":"continue"`) {
		t.Fatalf("continuation input=%v body=%s", input, gotBodies[1])
	}
	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-eof-final"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("continuation terminal not mapped: rows=%+v err=%v", rows, err)
	}
}

func TestCodexSessionMappingQuietLongPollUsesKeepalive(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatal("a live upstream long poll must not trigger native continuation")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-quiet-native\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Longer than the configured heartbeat interval. Strict CPA must start the
		// relay immediately instead of holding response.created in a retry probe.
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-quiet-native\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.StreamKeepAliveSeconds = 1
	h.importAccount(t, "quiet-native", "upstream-quiet-native", "access-quiet-native")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || !bytes.Contains(body, []byte("response.in_progress")) || !bytes.Contains(body, []byte("response.completed")) || bytes.Contains(body, []byte("codex_native_continue_failed")) {
		t.Fatalf("quiet strict stream status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func TestCodexSessionMappingEOFPendingClientToolDoesNotContinue(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatalf("pending client tool call must not trigger native continue")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-pending-tool\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call-pending\",\"name\":\"patch\",\"input\":\"{}\"}}\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "pending", "upstream-pending", "access-pending")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || !bytes.Contains(body, []byte("codex_native_continue_failed")) || bytes.Count(body, []byte("data: [DONE]")) != 1 {
		t.Fatalf("pending tool EOF status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func TestCodexSessionMappingCreatesDistinctChildAndForkTrees(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	root, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: "unauthenticated",
		Binding: storage.CodexSessionBinding{
			ID: "root-binding", TreeID: "root-tree", AccountID: "account-root", EgressID: storage.DefaultDirectEgressID, State: "active",
			InstallationID:  "install-root-device",
			DeviceOSHint:    "Mac OS",
			DeviceOSHintSet: true,
			RootSessionID:   "019f0000-0000-7000-8000-000000000101",
			ThreadID:        "019f0000-0000-7000-8000-000000000101",
		},
		Aliases: []storage.CodexSessionAlias{{Type: "root", Value: "client-root"}, {Type: "session", Value: "client-root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := scheduler.Lease{Account: storage.Account{ID: "account-root"}, Egress: storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}}

	childRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child"}`))
	childMapping, err := h.app.resolveCodexSessionMapping(ctx, childRequest, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := childMapping.instructionTreeID(); got != root.TreeID {
		t.Fatalf("child instruction tree=%q, want parent tree %q", got, root.TreeID)
	}
	child, err := childMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if child.InstallationID != root.InstallationID || child.DeviceOSHint != root.DeviceOSHint || child.SessionID != root.RootSessionID || child.ThreadID == child.SessionID || child.ParentThreadID != root.ThreadID || !strings.HasSuffix(child.WindowID(), ":0") {
		t.Fatalf("child snapshot=%+v root=%+v", child, root)
	}
	if err := h.app.commitCodexSessionMapping(ctx, childMapping, lease, lease.Egress, "resp-child", "", false); err != nil {
		t.Fatal(err)
	}
	repeatChild, err := h.app.resolveCodexSessionMapping(ctx, childRequest, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child again"}`), downstreamPolicy{})
	if err != nil || repeatChild.binding == nil || repeatChild.binding.ThreadID != child.ThreadID {
		t.Fatalf("self-contained child turn must reuse its branch binding=%+v err=%v", repeatChild.binding, err)
	}
	childResume, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","previous_response_id":"resp-child","input":"next"}`))
	resumedChild, err := h.app.resolveCodexSessionMapping(ctx, childResume, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","previous_response_id":"resp-child","input":"next"}`), downstreamPolicy{})
	if err != nil || resumedChild.binding == nil || resumedChild.binding.ThreadID != child.ThreadID {
		t.Fatalf("child stateful resume binding=%+v err=%v", resumedChild.binding, err)
	}

	// Some clients omit session_id on a child follow-up and identify it solely by
	// thread_id + parent_thread_id. That must still become a child branch rather
	// than a second root session.
	noSessionBody := []byte(`{"model":"gpt","thread_id":"client-child-no-session","parent_thread_id":"client-root","input":"child"}`)
	noSessionRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(noSessionBody))
	noSessionMapping, err := h.app.resolveCodexSessionMapping(ctx, noSessionRequest, noSessionBody, downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	noSessionChild, err := noSessionMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil || noSessionChild.InstallationID != root.InstallationID || noSessionChild.DeviceOSHint != root.DeviceOSHint || noSessionChild.SessionID != root.RootSessionID || noSessionChild.ThreadID == noSessionChild.SessionID || noSessionChild.ParentThreadID != root.ThreadID {
		t.Fatalf("child without session snapshot=%+v err=%v", noSessionChild, err)
	}
	if err := h.app.commitCodexSessionMapping(ctx, noSessionMapping, lease, lease.Egress, "resp-child-no-session", "", false); err != nil {
		t.Fatal(err)
	}
	repeatNoSession, err := h.app.resolveCodexSessionMapping(ctx, noSessionRequest, noSessionBody, downstreamPolicy{})
	if err != nil || repeatNoSession.binding == nil || repeatNoSession.binding.ThreadID != noSessionChild.ThreadID {
		t.Fatalf("child without session did not persist branch binding=%+v err=%v", repeatNoSession.binding, err)
	}

	forkRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork"}`))
	forkMapping, err := h.app.resolveCodexSessionMapping(ctx, forkRequest, []byte(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := forkMapping.instructionTreeID(); got != "" {
		t.Fatalf("fork must create a fresh instruction tree, got inherited %q", got)
	}
	fork, err := forkMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if fork.SessionID == root.RootSessionID || fork.SessionID != fork.ThreadID || fork.ParentThreadID != "" || fork.ForkedFromThreadID != child.ThreadID || !strings.HasSuffix(fork.WindowID(), ":0") {
		t.Fatalf("fork snapshot=%+v child=%+v root=%+v", fork, child, root)
	}
	if err := h.app.commitCodexSessionMapping(ctx, forkMapping, lease, lease.Egress, "resp-fork", "", false); err != nil {
		t.Fatal(err)
	}
	// Codex retains the original fork marker on later self-contained turns. That
	// marker identifies ancestry, not a request to create a fresh fork every time;
	// the second turn must reuse the durable fork root and commit another response
	// alias without colliding with its own root alias.
	repeatForkBody := []byte(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork follow-up"}`)
	repeatForkRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(repeatForkBody))
	repeatForkMapping, err := h.app.resolveCodexSessionMapping(ctx, repeatForkRequest, repeatForkBody, downstreamPolicy{})
	if err != nil || repeatForkMapping.binding == nil || repeatForkMapping.binding.ID != forkMapping.binding.ID {
		t.Fatalf("persistent fork marker did not reuse its root binding=%+v err=%v", repeatForkMapping.binding, err)
	}
	repeatFork, err := repeatForkMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil || repeatFork.SessionID != fork.SessionID || repeatFork.ThreadID != fork.ThreadID || repeatFork.ForkedFromThreadID != fork.ForkedFromThreadID {
		t.Fatalf("persistent fork marker changed upstream identity=%+v err=%v", repeatFork, err)
	}
	if err := h.app.commitCodexSessionMapping(ctx, repeatForkMapping, lease, lease.Egress, "resp-fork-follow-up", "", false); err != nil {
		t.Fatalf("persistent fork marker collided during terminal commit: %v", err)
	}
	if _, err := h.store.RetireCodexSessionTree(ctx, root.ID, root.Epoch); err != nil {
		t.Fatal(err)
	}
	newRootRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-root","session_id":"client-root","input":"fresh root"}`))
	newRootMapping, err := h.app.resolveCodexSessionMapping(ctx, newRootRequest, []byte(`{"model":"gpt","thread_id":"client-root","session_id":"client-root","input":"fresh root"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := newRootMapping.instructionTreeID(); got != "" {
		t.Fatalf("new root after retirement inherited old instruction tree %q", got)
	}
}

func turnIDFromHeader(raw string) string {
	var metadata map[string]interface{}
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return ""
	}
	value, _ := metadata["turn_id"].(string)
	return value
}
