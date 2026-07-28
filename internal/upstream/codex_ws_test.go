package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
	"github.com/gorilla/websocket"
)

type guardedWebSocketSource struct {
	body          []byte
	mu            sync.Mutex
	maxReadBuffer int
	opens         int
}

func (s *guardedWebSocketSource) Size() int64 { return int64(len(s.body)) }

func (s *guardedWebSocketSource) Open() (io.ReadCloser, error) {
	s.mu.Lock()
	s.opens++
	s.mu.Unlock()
	return io.NopCloser(&guardedWebSocketReader{source: s, reader: bytes.NewReader(s.body)}), nil
}

func (s *guardedWebSocketSource) Close() error { return nil }

type guardedWebSocketReader struct {
	source *guardedWebSocketSource
	reader *bytes.Reader
}

func (r *guardedWebSocketReader) Read(payload []byte) (int, error) {
	r.source.mu.Lock()
	if len(payload) > r.source.maxReadBuffer {
		r.source.maxReadBuffer = len(payload)
	}
	r.source.mu.Unlock()
	if len(payload) > bodysource.DefaultChunkSize {
		return 0, errors.New("request body was materialized instead of streamed")
	}
	return r.reader.Read(payload)
}

func TestCodexResponsesWebSocketBridge(t *testing.T) {
	var gotPath, gotMethod string
	var gotHeaders http.Header
	var gotPayload map[string]interface{}
	var gotPayloadRaw []byte

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket request: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d", messageType)
		}
		gotPayloadRaw = append([]byte(nil), raw...)
		if err := json.Unmarshal(raw, &gotPayload); err != nil {
			t.Fatalf("payload json: %v\n%s", err, raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Headers: http.Header{
			"Originator": []string{"codex_exec"}, "x-client-request-id": []string{"thread-123"},
			"x-codex-beta-features":                 []string{"network_proxy,remote_compaction_v2,network_proxy"},
			"x-responsesapi-include-timing-metrics": []string{"true"},
		},
		Body:                    testBody([]byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp_keep","skills":[{"id":"repo-review","future":{"n":900719925474099312345}}],"plugins":{"mcp":{"server":"workspace-skills"},"future":"keep"},"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}},{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"create"}]},{"type":"custom","name":"freeform","format":{"type":"text"}},{"type":"local_shell","name":"shell"},{"type":"mcp","server_label":"workspace-skills"},{"type":"tool_search","execution":"client"},{"type":"future_tool","future":{"x":1}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep WS context"}]},{"role":"user","content":"hi","exact_id":900719925474099312345}],"stream":true,"future_wire_field":{"n":900719925474099312345},"prompt_cache_retention":"24h"}`)),
		Account:                 storage.Account{ID: "acc-ws", UpstreamAccountID: "workspace-should-not-leak"},
		Token:                   storage.AccountToken{AccessToken: "access-ws"},
		Egress:                  storage.EgressProfile{Type: "direct", Health: "healthy"},
		CodexClientVersion:      config.DefaultClientVersion,
		CodexResponsesWebSocket: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotMethod != http.MethodGet || gotPath != "/backend-api/codex/responses" {
		t.Fatalf("handshake = %s %s", gotMethod, gotPath)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response status/header = %d %+v", resp.StatusCode, resp.Header)
	}
	out := string(body)
	for _, want := range []string{"event: response.created", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, out)
		}
	}
	if gotHeaders.Get("OpenAI-Beta") != codexResponsesWebSocketBeta {
		t.Fatalf("OpenAI-Beta = %q", gotHeaders.Get("OpenAI-Beta"))
	}
	if v := gotHeaders.Get("version"); v != config.DefaultClientVersion {
		t.Fatalf("version = %q", v)
	}
	if ua := gotHeaders.Get("User-Agent"); !strings.Contains(ua, "codex_exec/"+config.DefaultClientVersion) {
		t.Fatalf("UA = %q", ua)
	}
	if gotHeaders.Get("Originator") != "codex_exec" {
		t.Fatalf("Originator = %q", gotHeaders.Get("Originator"))
	}
	virtualIdentity := identity.For(client.identitySecret, "acc-ws")
	wantThread := identity.DerivedUUIDv7(virtualIdentity.MachineID+"\x00thread", "thread-123")
	if gotHeaders.Get("session-id") != wantThread || gotHeaders.Get("thread-id") != wantThread || gotHeaders.Get("x-codex-window-id") != wantThread+":0" {
		t.Fatalf("ws ids missing/mismatched: %+v", gotHeaders)
	}
	if gotHeaders.Get("x-codex-beta-features") != "network_proxy,remote_compaction_v2" {
		t.Fatalf("client/required beta features were not merged on handshake: %+v", gotHeaders)
	}
	if gotHeaders.Get("x-responsesapi-include-timing-metrics") != "true" {
		t.Fatalf("WS timing metrics opt-in missing from handshake: %+v", gotHeaders)
	}
	if gotHeaders.Get(codexResponsesLiteHeader) != "" {
		t.Fatalf("responses-lite is WS client metadata, not a handshake header: %+v", gotHeaders)
	}
	if gotHeaders.Get("Content-Type") != "" {
		t.Fatalf("WebSocket handshake must not carry HTTP JSON Content-Type: %+v", gotHeaders)
	}
	// The real WS handshake uses lowercase session-id/thread-id (never a capitalized
	// "Session_id") and DOES carry ChatGPT-Account-ID (it rides the auth headers).
	if gotHeaders.Get("Session_id") != "" {
		t.Fatalf("stale capitalized Session_id leaked into WS handshake: %+v", gotHeaders)
	}
	if gotHeaders.Get("ChatGPT-Account-ID") != "workspace-should-not-leak" {
		t.Fatalf("WS handshake must carry ChatGPT-Account-ID via auth: %+v", gotHeaders)
	}

	if gotPayload["type"] != "response.create" || gotPayload["model"] != "gpt-5.6-sol" || gotPayload["stream"] != true {
		t.Fatalf("bad response.create payload: %+v", gotPayload)
	}
	if gotPayload["previous_response_id"] != "resp_keep" {
		t.Fatalf("Responses Lite lost previous_response_id: %+v", gotPayload)
	}
	if !bytes.Contains(gotPayloadRaw, []byte(`900719925474099312345`)) {
		t.Fatalf("Responses Lite rounded tool/input context: %s", gotPayloadRaw)
	}
	if _, stale := gotPayload["prompt_cache_retention"]; stale {
		t.Fatalf("latest Codex WS payload must strip obsolete prompt_cache_retention: %+v", gotPayload)
	}
	if _, preserved := gotPayload["future_wire_field"]; !preserved {
		t.Fatalf("unknown future WS field was dropped: %+v", gotPayload)
	}
	if _, preserved := gotPayload["skills"]; !preserved {
		t.Fatalf("skill metadata was dropped over WS: %+v", gotPayload)
	}
	if _, preserved := gotPayload["plugins"]; !preserved {
		t.Fatalf("plugin metadata was dropped over WS: %+v", gotPayload)
	}
	metadata, _ := gotPayload["client_metadata"].(map[string]interface{})
	if metadata["x-codex-installation-id"] != virtualIdentity.MachineID ||
		metadata["session_id"] != gotHeaders.Get("session-id") ||
		metadata["thread_id"] != gotHeaders.Get("thread-id") ||
		metadata["x-codex-window-id"] != wantThread+":0" ||
		metadata["turn_id"] == "" || metadata["x-codex-turn-metadata"] == "" {
		t.Fatalf("missing websocket client metadata: %+v", metadata)
	}
	if metadata[codexWSResponsesLiteMetadata] != "true" || metadata[codexWSRequestStartMetadata] == "" {
		t.Fatalf("missing GPT-5.6 responses-lite/timing metadata: %+v", metadata)
	}
	if gotPayload["parallel_tool_calls"] != false {
		t.Fatalf("responses-lite must disable parallel_tool_calls: %+v", gotPayload)
	}
	if _, present := gotPayload["instructions"]; present {
		t.Fatalf("Responses Lite retained top-level instructions: %+v", gotPayload)
	}
	if _, present := gotPayload["tools"]; present {
		t.Fatalf("Responses Lite retained top-level tools: %+v", gotPayload)
	}
	items, _ := gotPayload["input"].([]interface{})
	if len(items) != 3 || items[0].(map[string]interface{})["type"] != "additional_tools" || items[1].(map[string]interface{})["role"] != "developer" || items[2].(map[string]interface{})["role"] != "user" {
		t.Fatalf("Responses Lite input envelope malformed: %+v", gotPayload)
	}
	additional := items[0].(map[string]interface{})["tools"].([]interface{})
	if len(additional) != 7 {
		t.Fatalf("stable/future Responses tools were not transparent over WS: %+v", additional)
	}
}

func TestCodexResponsesWebSocketStreamsBodySourceWithoutMaterializing(t *testing.T) {
	largeInput := strings.Repeat("x", 2<<20)
	raw := []byte(`{"model":"gpt-5.5","instructions":"keep","input":"` + largeInput + `","stream":true}`)
	meta, err := bodysource.ScanJSON(context.Background(), bodysource.Bytes(raw), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	source := &guardedWebSocketSource{body: raw}
	received := make(chan []byte, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		defer conn.Close()
		messageType, body, readErr := conn.ReadMessage()
		if readErr != nil || messageType != websocket.TextMessage {
			return
		}
		received <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_streamed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses", Body: source, BodyMeta: &meta,
		Account: storage.Account{ID: "acc-source-ws"}, Token: storage.AccountToken{AccessToken: "access-ws"},
		Egress: storage.EgressProfile{Type: "direct", Health: "healthy"}, CodexResponsesWebSocket: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	var payload map[string]json.RawMessage
	if err = json.Unmarshal(<-received, &payload); err != nil {
		t.Fatal(err)
	}
	var input string
	if err = json.Unmarshal(payload["input"], &input); err != nil || input != largeInput {
		t.Fatalf("large input changed: len=%d err=%v", len(input), err)
	}
	if string(payload["type"]) != `"response.create"` || string(payload["stream"]) != "true" || string(payload["tools"]) != "[]" || string(payload["tool_choice"]) != `"auto"` || string(payload["parallel_tool_calls"]) != "false" || string(payload["include"]) != "[]" {
		t.Fatalf("source websocket defaults missing: %s", payload)
	}
	var clientMetadata map[string]json.RawMessage
	if err = json.Unmarshal(payload["client_metadata"], &clientMetadata); err != nil || len(clientMetadata[codexWSRequestStartMetadata]) == 0 {
		t.Fatalf("source websocket metadata missing: %s err=%v", payload["client_metadata"], err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.maxReadBuffer > bodysource.DefaultChunkSize || source.opens < 1 {
		t.Fatalf("body source was not replayed in bounded chunks: opens=%d max_read=%d", source.opens, source.maxReadBuffer)
	}
}

func TestCodexWebSocketResponsesLiteDoesNotSynthesizeInstructionsOrTools(t *testing.T) {
	payload, err := buildCodexWebSocketCreatePayload([]byte(`{"model":"gpt-5.6-sol","input":"hi"}`), codexWebSocketIDs{responsesLite: true})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	if _, present := root["instructions"]; present {
		t.Fatalf("Responses Lite synthesized instructions: %+v", root)
	}
	if _, present := root["tools"]; present {
		t.Fatalf("Responses Lite synthesized top-level tools: %+v", root)
	}
}

func TestCodexWebSocketClassicResponsesStillDefaultsTools(t *testing.T) {
	payload, err := buildCodexWebSocketCreatePayload([]byte(`{"model":"gpt-5.5","input":"hi"}`), codexWebSocketIDs{})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	tools, present := root["tools"].([]interface{})
	if !present || len(tools) != 0 {
		t.Fatalf("classic Responses must retain the tools:[] default: %+v", root)
	}
}

func TestCodexWebSocketClassicHostedToolDoesNotOptIntoResponsesLite(t *testing.T) {
	client := NewClient(config.Default())
	spec := Request{
		DownstreamPath:          "/v1/responses",
		Body:                    testBody([]byte(`{"model":"gpt-5.6-sol","instructions":"classic client","tools":[{"type":"web_search"}],"input":"search"}`)),
		Account:                 storage.Account{ID: "acc-classic-hosted-ws"},
		Token:                   storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
		CodexResponsesWebSocket: true,
	}
	metadata := client.newCodexRequestMetadata(spec)
	if metadata.responsesLite {
		t.Fatal("classic hosted-tool request was classified as Responses Lite")
	}
	spec.codexMetadata = &metadata
	_, headers, payload, err := client.prepareCodexResponsesWebSocket(spec)
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get(codexResponsesLiteHeader) != "" {
		t.Fatalf("classic hosted-tool WS handshake received Lite header: %+v", headers)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	tools, _ := root["tools"].([]interface{})
	if root["instructions"] != "classic client" || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "web_search" {
		t.Fatalf("classic hosted-tool WS body was rewritten as Lite: %s", payload)
	}
	clientMetadata, _ := root["client_metadata"].(map[string]interface{})
	if clientMetadata[codexWSResponsesLiteMetadata] != nil {
		t.Fatalf("classic hosted-tool WS body received Lite metadata: %s", payload)
	}
}

func TestCodexResponsesWebSocketTerminalErrorFrameClosesCleanly(t *testing.T) {
	for _, eventType := range []string{"error", "response.error"} {
		t.Run(eventType, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Fatalf("upgrade: %v", err)
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Fatalf("read request: %v", err)
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"`+eventType+`","error":{"message":"invalid cache request"}}`))
			}))
			defer server.Close()

			cfg := config.Default()
			cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
			client := NewClient(cfg)
			resp, err := client.Do(context.Background(), Request{
				DownstreamPath:          "/v1/responses",
				Body:                    testBody([]byte(`{"model":"gpt-5.6-sol","store":false,"stream":true,"parallel_tool_calls":false,"reasoning":{"effort":"low"},"input":"hi"}`)),
				Account:                 storage.Account{ID: "acc-ws-error", UpstreamAccountID: "workspace"},
				Token:                   storage.AccountToken{AccessToken: "access"},
				Egress:                  storage.EgressProfile{Type: "direct", Health: "healthy"},
				CodexResponsesWebSocket: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("terminal %s frame became a transport error: %v", eventType, err)
			}
			if !strings.Contains(string(body), `"type":"`+eventType+`"`) || !strings.Contains(string(body), "data: [DONE]") {
				t.Fatalf("terminal %s frame was not preserved: %s", eventType, body)
			}
		})
	}
}

func TestCodexWebSocketPayloadPreservesPreviousResponseID(t *testing.T) {
	client := NewClient(config.Default())
	original := []byte(`{"model":"gpt","instructions":"keep","previous_response_id":"resp_real","session_id":"downstream-session","thread_id":"downstream-thread","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	payload, err := buildCodexWebSocketCreatePayload(original, codexWebSocketIDs{
		installationID: "install-1",
		sessionID:      "session-derived",
		threadID:       "thread-derived",
		windowID:       "thread-derived:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := client.codexWSNamespacePayload(Request{Account: storage.Account{ID: "acc"}}, codexWebSocketIDs{
		sessionID: "session-derived",
		threadID:  "thread-derived",
	}, payload)
	var root map[string]interface{}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("payload json: %v\n%s", err, out)
	}
	if _, present := root["session_id"]; present {
		t.Fatalf("top-level session_id is not a Responses API parameter: %+v", root)
	}
	if _, present := root["thread_id"]; present {
		t.Fatalf("top-level thread_id is not a Responses API parameter: %+v", root)
	}
	if root["previous_response_id"] != "resp_real" {
		t.Fatalf("previous_response_id must pass through unchanged, got %+v", root)
	}
	assertCodexContextFieldsUnchanged(t, original, out)
}

func TestWriteSSEEventPrefixesEveryMultilineJSONLine(t *testing.T) {
	raw := []byte("{\n  \"type\": \"response.completed\",\n  \"response\": {\"id\": \"resp_multiline\"}\n}")
	var out bytes.Buffer
	if err := writeSSEEvent(&out, "response.completed", raw); err != nil {
		t.Fatal(err)
	}
	want := "event: response.completed\n" +
		"data: {\n" +
		"data:   \"type\": \"response.completed\",\n" +
		"data:   \"response\": {\"id\": \"resp_multiline\"}\n" +
		"data: }\n\n"
	if out.String() != want {
		t.Fatalf("multiline SSE event = %q, want %q", out.String(), want)
	}
}
