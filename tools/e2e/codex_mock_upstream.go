// Command codex-mock-upstream is a deterministic local Responses server used by
// the Docker fingerprint/E2E check. It supports the exact WebSocket transport the
// shipping Codex CLI and pool_server prefer for GPT-5.6, plus an HTTP SSE fallback.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

type capture struct {
	Transport         string                 `json:"transport"`
	Headers           map[string]string      `json:"headers"`
	Body              map[string]interface{} `json:"body_summary"`
	TotalRequests     int                    `json:"total_requests"`
	WarmupRequests    int                    `json:"warmup_requests"`
	InferenceRequests int                    `json:"inference_requests"`
	Valid             bool                   `json:"valid"`
	Errors            []string               `json:"errors,omitempty"`
}

var (
	lastMu              sync.RWMutex
	last                capture
	history             []capture
	up                  = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	matrixMode          bool
	matrixAnswerPattern = regexp.MustCompile(`[A-Z][A-Z0-9_]*\|73\|ZWYX`)
)

func main() {
	addr := flag.String("listen", ":9300", "listen address")
	flag.BoolVar(&matrixMode, "matrix", false, "echo deterministic CLI-matrix markers and synthetic cache usage")
	flag.Parse()
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	http.HandleFunc("/capture", serveCapture)
	http.HandleFunc("/captures", serveCaptures)
	http.HandleFunc("/models", serveModels)
	http.HandleFunc("/backend-api/codex/models", serveModels)
	http.HandleFunc("/v1/responses", serveResponses)
	http.HandleFunc("/backend-api/codex/responses", serveResponses)
	log.Printf("codex mock upstream listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func serveCapture(w http.ResponseWriter, _ *http.Request) {
	lastMu.RLock()
	defer lastMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(last)
}

func serveCaptures(w http.ResponseWriter, _ *http.Request) {
	lastMu.RLock()
	defer lastMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}

func serveModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"models":[` +
		`{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","context_window":372000,"max_context_window":372000,"effective_context_window_percent":95,"visibility":"list","supported_reasoning_efforts":["low","medium","high","xhigh","max","ultra"]},` +
		`{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra","context_window":372000,"max_context_window":372000,"effective_context_window_percent":95,"visibility":"list","supported_reasoning_efforts":["low","medium","high","xhigh","max","ultra"]},` +
		`{"slug":"gpt-5.6-luna","display_name":"GPT-5.6 Luna","context_window":372000,"max_context_window":372000,"effective_context_window_percent":95,"visibility":"list","supported_reasoning_efforts":["low","medium","high","xhigh","max","ultra"]},` +
		`{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":272000,"max_context_window":272000,"effective_context_window_percent":95,"visibility":"list","supported_reasoning_efforts":["low","medium","high","xhigh"]}` +
		`]}`))
}

func serveResponses(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		serveResponsesWS(w, r)
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	warmup := record("http", r, body)
	w.Header().Set("Content-Type", "text/event-stream")
	events := responseEvents(modelOf(body), responseText(body))
	if warmup {
		events = warmupEvents(modelOf(body))
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func serveResponsesWS(w http.ResponseWriter, r *http.Request) {
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var body map[string]interface{}
		if json.Unmarshal(raw, &body) != nil {
			return
		}
		if body["type"] == "response.processed" {
			continue
		}
		warmup := record("websocket", r, body)
		events := responseEvents(modelOf(body), responseText(body))
		if warmup {
			events = warmupEvents(modelOf(body))
		}
		for _, event := range events {
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		}
	}
}

func record(transport string, r *http.Request, body map[string]interface{}) bool {
	h := map[string]string{}
	for _, key := range []string{"User-Agent", "Accept", "Content-Type", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "X-Codex-Window-Id", "X-Client-Request-Id", "Session-Id", "Thread-Id"} {
		h[strings.ToLower(key)] = r.Header.Get(key)
	}
	generate, hasGenerate := body["generate"].(bool)
	warmup := hasGenerate && !generate
	metadata, _ := body["client_metadata"].(map[string]interface{})
	headerTurnMetadata := map[string]interface{}{}
	headerTurnMetadataValid := json.Unmarshal([]byte(h["x-codex-turn-metadata"]), &headerTurnMetadata) == nil
	bodyTurnMetadata := map[string]interface{}{}
	bodyTurnMetadataRaw, _ := metadata["x-codex-turn-metadata"].(string)
	bodyTurnMetadataValid := json.Unmarshal([]byte(bodyTurnMetadataRaw), &bodyTurnMetadata) == nil
	bodyTurnID, _ := bodyTurnMetadata["turn_id"].(string)
	_, bodyMetadataTurnIDPresent := bodyTurnMetadata["turn_id"]
	_, bodyTurnIDPresent := metadata["turn_id"]
	_, toolsPresent := body["tools"]
	reasoning, _ := body["reasoning"].(map[string]interface{})
	summary := map[string]interface{}{
		"model":                   modelOf(body),
		"warmup":                  warmup,
		"parallel_tool_calls":     body["parallel_tool_calls"],
		"instructions_present":    body["instructions"] != nil,
		"top_level_tools_present": toolsPresent,
		"client_metadata_present": metadata != nil,
		"installation_id_present": metadata != nil && metadata["x-codex-installation-id"] != nil,
		"turn_metadata_present":   metadata != nil && metadata["x-codex-turn-metadata"] != nil,
		"responses_lite":          metadata != nil && metadata["ws_request_header_x_openai_internal_codex_responses_lite"] == "true",
		"ws_timing_present":       metadata != nil && metadata["x-codex-ws-stream-request-start-ms"] != nil,
		"session_equals_thread":   h["session-id"] != "" && h["session-id"] == h["thread-id"],
		"thread_id_is_uuidv7":     isUUIDv7(h["thread-id"]),
		"turn_id_is_uuidv7":       isUUIDv7(bodyTurnID),
		"prewarm_turn_id_omitted": warmup && !bodyMetadataTurnIDPresent && !bodyTurnIDPresent,
		"reasoning_effort":        reasoning["effort"],
	}
	c := capture{Transport: transport, Headers: h, Body: summary, Valid: true}
	require := func(ok bool, msg string) {
		if !ok {
			c.Valid = false
			c.Errors = append(c.Errors, msg)
		}
	}
	if matrixMode {
		require(strings.HasPrefix(modelOf(body), "gpt-5."), "matrix model is not a GPT-5 family model")
	} else {
		require(modelOf(body) == "gpt-5.6-sol", "model is not gpt-5.6-sol")
	}
	require(h["version"] == "0.144.1", "version header is not 0.144.1")
	require(h["x-codex-beta-features"] == "remote_compaction_v2", "remote_compaction_v2 missing")
	require(strings.Contains(h["user-agent"], "codex_exec/0.144.1"), "codex_exec user-agent missing")
	require(strings.Contains(h["user-agent"], "(codex_exec; 0.144.1)"), "codex_exec user-agent suffix missing")
	require(h["x-codex-turn-metadata"] != "", "turn metadata missing")
	require(isUUIDv7(h["thread-id"]), "thread-id is not UUIDv7")
	require(h["session-id"] == h["thread-id"], "session-id does not equal thread-id")
	require(h["x-client-request-id"] == h["thread-id"], "x-client-request-id does not equal thread-id")
	require(h["x-codex-window-id"] == h["thread-id"]+":0", "window id does not match thread-id")
	require(headerTurnMetadataValid, "handshake turn metadata is not valid JSON")
	require(headerTurnMetadata["session_id"] == h["session-id"] && headerTurnMetadata["thread_id"] == h["thread-id"], "handshake turn metadata session/thread mismatch")
	require(bodyTurnMetadataValid, "body turn metadata is not valid JSON")
	require(bodyTurnMetadata["session_id"] == h["session-id"] && bodyTurnMetadata["thread_id"] == h["thread-id"], "body turn metadata session/thread mismatch")
	if warmup {
		require(headerTurnMetadata["request_kind"] == "prewarm", "warmup handshake request kind is not prewarm")
		require(bodyTurnMetadata["request_kind"] == "prewarm", "warmup body request kind is not prewarm")
		require(!bodyMetadataTurnIDPresent, "warmup x-codex-turn-metadata must omit turn_id")
		require(!bodyTurnIDPresent, "warmup client_metadata must omit turn_id")
		_, hasStartedAt := bodyTurnMetadata["turn_started_at_unix_ms"]
		require(!hasStartedAt, "prewarm contains turn_started_at_unix_ms")
	} else {
		// The handshake can remain tagged as prewarm on a persistent connection;
		// per-frame body metadata is authoritative for the actual inference turn.
		require(bodyTurnMetadata["request_kind"] == "turn", "inference body request kind is not turn")
		require(isUUIDv7(bodyTurnID), "inference body turn_id is not UUIDv7")
		require(isUUIDv7(fmt.Sprint(metadata["turn_id"])), "inference body turn_id is not UUIDv7")
		require(bodyTurnMetadata["turn_started_at_unix_ms"] != nil, "inference turn start timestamp missing")
	}
	if transport == "websocket" {
		require(h["openai-beta"] == "responses_websockets=2026-02-06", "websocket beta missing")
		require(h["accept"] == "", "websocket handshake must omit Accept")
		require(h["content-type"] == "", "websocket handshake must omit Content-Type")
		require(!toolsPresent, "Responses Lite must omit top-level tools")
	}
	if metadata != nil {
		require(metadata["x-codex-installation-id"] != nil, "installation id missing")
		require(metadata["x-codex-turn-metadata"] != nil, "body turn metadata missing")
		if transport == "websocket" {
			require(metadata["ws_request_header_x_openai_internal_codex_responses_lite"] == "true", "Responses Lite metadata missing")
			require(metadata["x-codex-ws-stream-request-start-ms"] != nil, "WS request timing missing")
		}
	} else {
		require(false, "client_metadata missing")
	}
	lastMu.Lock()
	c.TotalRequests = last.TotalRequests + 1
	c.WarmupRequests = last.WarmupRequests
	c.InferenceRequests = last.InferenceRequests
	if warmup {
		c.WarmupRequests++
	} else {
		c.InferenceRequests++
	}
	last = c
	history = append(history, c)
	if len(history) > 200 {
		history = append([]capture(nil), history[len(history)-200:]...)
	}
	lastMu.Unlock()
	raw, _ := json.Marshal(c)
	log.Printf("capture=%s", raw)
	return warmup
}

func modelOf(body map[string]interface{}) string {
	model, _ := body["model"].(string)
	return model
}

func isUUIDv7(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[14] != '7' || value[18] != '-' || value[23] != '-' {
		return false
	}
	if !strings.ContainsRune("89abAB", rune(value[19])) {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func responseText(body map[string]interface{}) string {
	if matrixMode {
		raw, _ := json.Marshal(body)
		if marker := matrixAnswerPattern.Find(raw); len(marker) > 0 {
			return string(marker)
		}
	}
	return "POOL_E2E_GPT_5_6_OK"
}

func responseEvents(model, text string) []map[string]interface{} {
	const responseID = "resp_pool_e2e"
	const messageID = "msg_pool_e2e"
	inputTokens := 32
	cachedTokens := 0
	if matrixMode {
		inputTokens = 7808
		cachedTokens = 6912
	}
	itemOpen := map[string]interface{}{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []interface{}{}}
	itemDone := map[string]interface{}{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}}}
	return []map[string]interface{}{
		{"type": "response.created", "response": map[string]interface{}{"id": responseID, "object": "response", "status": "in_progress", "model": model, "output": []interface{}{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": itemOpen},
		{"type": "response.content_part.added", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": ""}},
		{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": text},
		{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": text},
		{"type": "response.content_part.done", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": text}},
		{"type": "response.output_item.done", "output_index": 0, "item": itemDone},
		{"type": "response.completed", "response": map[string]interface{}{"id": responseID, "object": "response", "status": "completed", "model": model, "output": []interface{}{itemDone}, "usage": map[string]interface{}{"input_tokens": inputTokens, "input_tokens_details": map[string]interface{}{"cached_tokens": cachedTokens}, "output_tokens": 8, "total_tokens": inputTokens + 8}}},
	}
}

func warmupEvents(model string) []map[string]interface{} {
	const responseID = "resp_pool_e2e_warmup"
	return []map[string]interface{}{
		{"type": "response.created", "response": map[string]interface{}{"id": responseID, "object": "response", "status": "in_progress", "model": model, "output": []interface{}{}}},
		{"type": "response.completed", "response": map[string]interface{}{"id": responseID, "object": "response", "status": "completed", "model": model, "output": []interface{}{}, "usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}},
	}
}
