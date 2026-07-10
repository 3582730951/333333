package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCodexHTTPFingerprintUsesCanonicalClientMetadata(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","status":"completed"}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	rawTurn := `{"installation_id":"real-install","session_id":"real-session","thread_id":"real-thread","turn_id":"real-turn","window_id":"real-thread:7","request_kind":"turn","thread_source":"user","sandbox":"workspace-write"}`
	body := []byte(`{"model":"gpt-5.6-sol","instructions":"keep exact context","store":false,"stream":true,"reasoning":{"effort":"ultra"},"previous_response_id":"resp_keep","tools":[{"name":"keep","schema":{"const":900719925474099312345}}],"input":[{"role":"user","content":"do not truncate","exact_id":900719925474099312345}],"client_metadata":{"session_id":"real-session","thread_id":"real-thread","turn_id":"real-turn","x-codex-window-id":"real-thread:7","x-codex-turn-metadata":` + strconvJSON(rawTurn) + `,"custom-safe-key":"keep","custom_exact_id":900719925474099312345}}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Headers: http.Header{
			"x-client-request-id": []string{"real-thread"},
			"x-codex-window-id":   []string{"real-thread:7"},
		},
		Body:    body,
		Account: storage.Account{ID: "acc-http", UpstreamAccountID: "workspace"},
		Token:   storage.AccountToken{AccessToken: "access-http"},
		Egress:  storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotHeader.Get(codexResponsesLiteHeader) != "true" {
		t.Fatalf("responses-lite header = %q", gotHeader.Get(codexResponsesLiteHeader))
	}
	if gotHeader.Get("session-id") == "" || gotHeader.Get("thread-id") == "" || gotHeader.Get("x-codex-window-id") == "" {
		t.Fatalf("canonical HTTP identity headers missing: %+v", gotHeader)
	}
	if gotHeader.Get("session-id") != gotHeader.Get("thread-id") || !looksLikeUUIDv7(gotHeader.Get("thread-id")) {
		t.Fatalf("Codex session/thread identity is not one shared UUIDv7: %+v", gotHeader)
	}
	if gotHeader.Get("x-codex-beta-features") != codexBetaFeaturesHeader || gotHeader.Get("version") != config.DefaultClientVersion {
		t.Fatalf("shipping Responses fingerprint missing: %+v", gotHeader)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, gotBody)
	}
	if payload["model"] != "gpt-5.6-sol" || payload["instructions"] != "keep exact context" {
		t.Fatalf("model/context changed: %+v", payload)
	}
	assertCodexContextFieldsUnchanged(t, body, gotBody)
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "max" {
		t.Fatalf("official wire must map client-side ultra to max: %+v", reasoning)
	}
	metadata, _ := payload["client_metadata"].(map[string]interface{})
	if metadata["session_id"] != gotHeader.Get("session-id") ||
		metadata["thread_id"] != gotHeader.Get("thread-id") ||
		metadata["x-codex-window-id"] != gotHeader.Get("x-codex-window-id") ||
		metadata["turn_id"] == "" || metadata["x-codex-installation-id"] == "" {
		t.Fatalf("HTTP body/header metadata drift: body=%+v headers=%+v", metadata, gotHeader)
	}
	if metadata["custom-safe-key"] != "keep" {
		t.Fatalf("non-reserved client metadata was lost: %+v", metadata)
	}
	for _, forbidden := range []string{"real-install", "real-session", "real-thread", "real-turn"} {
		if strings.Contains(string(gotBody), forbidden) {
			t.Fatalf("downstream identity %q leaked in body: %s", forbidden, gotBody)
		}
	}
	if metadata[codexWSResponsesLiteMetadata] != nil || metadata[codexWSRequestStartMetadata] != nil {
		t.Fatalf("WS-only metadata leaked into HTTP body: %+v", metadata)
	}
	turnRaw, _ := metadata["x-codex-turn-metadata"].(string)
	var turn map[string]interface{}
	if json.Unmarshal([]byte(turnRaw), &turn) != nil || turn["request_kind"] != "turn" || turn["turn_started_at_unix_ms"] == nil || turn["sandbox"] != "workspace-write" {
		t.Fatalf("canonical turn metadata malformed: %q", turnRaw)
	}
}

func TestCodexPrewarmOmitsTurnIDAndStartTimestamp(t *testing.T) {
	client := NewClient(config.Default())
	metadata := client.newCodexRequestMetadata(Request{
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{"x-client-request-id": []string{"019f4a5e-85d5-7ff2-b047-d392ac030c3d"}},
		Body:           []byte(`{"model":"gpt-5.6-sol","generate":false}`),
		Account:        storage.Account{ID: "acc-prewarm"},
	})
	if metadata.sessionID != metadata.threadID || !looksLikeUUIDv7(metadata.threadID) {
		t.Fatalf("prewarm session/thread IDs malformed: %+v", metadata)
	}
	if metadata.threadID[:13] != "019f4a5e-85d5" {
		t.Fatalf("downstream UUIDv7 timestamp was not preserved: %q", metadata.threadID)
	}
	if metadata.turnID != "" {
		t.Fatalf("prewarm turn ID = %q, want empty", metadata.turnID)
	}
	var turn map[string]interface{}
	if err := json.Unmarshal([]byte(metadata.turnMetadata), &turn); err != nil {
		t.Fatal(err)
	}
	if turn["request_kind"] != "prewarm" {
		t.Fatalf("prewarm turn metadata mismatch: %+v", turn)
	}
	if _, present := turn["turn_id"]; present {
		t.Fatalf("prewarm must omit turn_id: %+v", turn)
	}
	if _, present := turn["turn_started_at_unix_ms"]; present {
		t.Fatalf("prewarm must omit turn_started_at_unix_ms: %+v", turn)
	}
	original := []byte(`{"model":"gpt-5.6-sol","instructions":"keep prewarm","generate":false,"previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	body := applyCodexClientMetadata(original, metadata, true)
	assertCodexContextFieldsUnchanged(t, original, body)
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	clientMetadata, _ := payload["client_metadata"].(map[string]interface{})
	if _, present := clientMetadata["turn_id"]; present {
		t.Fatalf("prewarm client_metadata must omit turn_id: %+v", clientMetadata)
	}
	payloadBody, err := buildCodexWebSocketCreatePayload(body, metadata)
	if err != nil {
		t.Fatal(err)
	}
	assertCodexContextFieldsUnchanged(t, original, payloadBody)
	stamped := stampCodexWebSocketRequestStart(payloadBody)
	assertCodexContextFieldsUnchanged(t, original, stamped)
	var stampedRoot map[string]interface{}
	if err := json.Unmarshal(stamped, &stampedRoot); err != nil {
		t.Fatal(err)
	}
	stampedMetadata, _ := stampedRoot["client_metadata"].(map[string]interface{})
	if _, present := stampedMetadata["turn_id"]; present {
		t.Fatalf("prewarm WS payload must omit turn_id: %+v", stampedMetadata)
	}
	if stampedMetadata[codexWSRequestStartMetadata] == "" {
		t.Fatalf("prewarm WS payload missing request-start metadata: %+v", stampedMetadata)
	}
}

func TestStripCodexTopLevelTransportCorrelatorsPreservesContext(t *testing.T) {
	original := []byte(`{"model":"gpt-5.6-sol","thread_id":"thread-downstream","session_id":"session-downstream","conversation_id":"conversation-downstream","instructions":"keep","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	got := stripCodexTopLevelTransportCorrelators(original)
	assertCodexContextFieldsUnchanged(t, original, got)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"thread_id", "session_id", "conversation_id"} {
		if _, present := root[key]; present {
			t.Fatalf("unsupported top-level field %q survived: %s", key, got)
		}
	}
}

func TestCodexCompactFingerprintKeepsMetadataOutOfBody(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	original := []byte(`{"model":"gpt-5.6-sol","input":[],"instructions":"","parallel_tool_calls":false}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses/compact",
		Body:           original,
		Account:        storage.Account{ID: "acc-compact"},
		Token:          storage.AccountToken{AccessToken: "access-compact"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(gotBody) != string(original) {
		t.Fatalf("compact body gained normal-turn fields:\nwant %s\n got %s", original, gotBody)
	}
	if gotHeader.Get(codexInstallationIDMetadataKey) == "" || gotHeader.Get("session-id") == "" || gotHeader.Get("x-codex-turn-metadata") == "" {
		t.Fatalf("compact compatibility headers missing: %+v", gotHeader)
	}
	if gotHeader.Get(codexResponsesLiteHeader) != "true" {
		t.Fatalf("compact responses-lite header = %q", gotHeader.Get(codexResponsesLiteHeader))
	}
}

func strconvJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func looksLikeUUIDv7(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[14] == '7' && value[18] == '-' &&
		(value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b') && value[23] == '-'
}
