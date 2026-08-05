package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestStripAgentMessageEncryptedContentIsNarrow(t *testing.T) {
	body := []byte(`{
  "model":"gpt-5.6-sol",
  "large":900719925474099312345,
  "input":[
    {"type":"agent_message","author":"/root/a","content":[
      {"type":"input_text","text":"keep subagent summary"},
      {"type":"encrypted_content","encrypted_content":"foreign-a"},
      {"type":"input_text","text":"keep trailing summary"}
    ]},
    {"type":"reasoning","encrypted_content":"keep-reasoning"},
    {"type":"function_call_output","call_id":"call-1","output":"keep tool","encrypted_content":"keep-tool-ciphertext"},
    {"type":"agent_message","author":"/root/b","content":[
      {"type":"encrypted_content","encrypted_content":"foreign-b"}
    ]},
    {"role":"user","content":[{"type":"input_text","text":"keep user history"}]}
  ]
}`)

	got, removed := stripAgentMessageEncryptedContent(body)
	if removed != 2 {
		t.Fatalf("removed=%d want 2: %s", removed, got)
	}
	text := string(got)
	for _, want := range []string{
		"keep subagent summary", "keep trailing summary", "keep-reasoning",
		"keep tool", "keep-tool-ciphertext", "keep user history", "900719925474099312345",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("ordinary history %q was changed: %s", want, got)
		}
	}
	for _, rejected := range []string{"foreign-a", "foreign-b", `"author":"/root/b"`} {
		if strings.Contains(text, rejected) {
			t.Fatalf("rejected agent ciphertext/container %q remained: %s", rejected, got)
		}
	}

	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("sanitized request is invalid JSON: %v", err)
	}
	input := root["input"].([]interface{})
	if len(input) != 4 {
		t.Fatalf("input len=%d want 4: %s", len(input), got)
	}
}

func TestPendingRootAgentCiphertextGateIsExact(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"agent_message","content":[{"type":"input_text","text":"keep"},{"type":"encrypted_content","encrypted_content":"foreign"}]}]}`)
	root := func() *codexSessionMapping {
		return &codexSessionMapping{
			enabled:  true,
			identity: codexDownstreamIdentity{RootID: "root-a", ThreadID: "root-a"},
		}
	}
	if got, removed := root().sanitizePendingRootAgentEncryptedContent(body, nil); removed != 1 || strings.Contains(string(got), "foreign") {
		t.Fatalf("pending root was not sanitized: removed=%d body=%s", removed, got)
	}

	stateful := root()
	stateful.identity.ResponseID = "resp-bound"
	if got, removed := stateful.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 0 || string(got) != string(body) {
		t.Fatalf("stateful request was rewritten: removed=%d body=%s", removed, got)
	}

	child := root()
	child.identity = codexDownstreamIdentity{ThreadID: "child", ParentID: "root-a"}
	if got, removed := child.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 0 || string(got) != string(body) {
		t.Fatalf("child request was rewritten: removed=%d body=%s", removed, got)
	}

	durable := root()
	durable.binding = &storage.CodexSessionBinding{RootSessionID: "upstream-root", ThreadID: "upstream-root"}
	if got, removed := durable.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 0 || string(got) != string(body) {
		t.Fatalf("durable root was rewritten: removed=%d body=%s", removed, got)
	}

	activeAnchor := root()
	activeAnchor.anchor = &storage.CodexSessionBinding{
		State: "active", RootSessionID: "upstream-root", ThreadID: "upstream-root",
	}
	if got, removed := activeAnchor.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 0 || string(got) != string(body) {
		t.Fatalf("active parent/root anchor was rewritten: removed=%d body=%s", removed, got)
	}

	retiredRoot := root()
	retiredRoot.anchor = &storage.CodexSessionBinding{
		State: "retired", RootSessionID: "retired-upstream-root", ThreadID: "retired-upstream-root",
	}
	if got, removed := retiredRoot.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 1 || strings.Contains(string(got), "foreign") {
		t.Fatalf("retired-root fresh epoch was not sanitized: removed=%d body=%s", removed, got)
	}

	retiredFork := root()
	retiredFork.identity.ForkedFromID = "source-root"
	retiredFork.anchor = &storage.CodexSessionBinding{
		State: "retired", RootSessionID: "retired-upstream-root", ThreadID: "retired-upstream-root",
	}
	if got, removed := retiredFork.sanitizePendingRootAgentEncryptedContent(body, nil); removed != 0 || string(got) != string(body) {
		t.Fatalf("retired fork was rewritten: removed=%d body=%s", removed, got)
	}
}

func TestCodexPendingRootStripsForeignAgentCiphertextBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		payload := string(raw)
		if call > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"unexpected retry loop"}}}`+"\n\n")
			return
		}
		valid := r.Header.Get("Session-Id") != "" && r.Header.Get("X-Codex-Turn-State") == "" &&
			!strings.Contains(payload, "foreign-agent-ciphertext") &&
			!strings.Contains(payload, `"type":"encrypted_content"`) &&
			strings.Contains(payload, "subagent readable summary") &&
			strings.Contains(payload, "ordinary user history") &&
			strings.Contains(payload, "reasoning-ciphertext-must-stay")
		w.Header().Set("Content-Type", "text/event-stream")
		if !valid {
			// This reproduces the observed upstream shape: HTTP 200 followed by a
			// terminal SSE decryption failure after accepting the request.
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","status":400,"response":{"status":"failed","error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."}}}`+"\n\n"+
				"data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-agent-history-ok","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}}`+"\n\n"+
			"data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "agent-history", "upstream-agent-history", "access-agent-history")

	body := `{"model":"gpt-5.6-sol","stream":true,"input":[` +
		`{"type":"agent_message","author":"/root/subagent","recipient":"/root","content":[` +
		`{"type":"input_text","text":"subagent readable summary"},` +
		`{"type":"encrypted_content","encrypted_content":"foreign-agent-ciphertext"}]},` +
		`{"type":"reasoning","encrypted_content":"reasoning-ciphertext-must-stay"},` +
		`{"role":"user","content":[{"type":"input_text","text":"ordinary user history"}]}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "agent-history-root")
	req.Header.Set("Session-Id", "agent-history-root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp-agent-history-ok") ||
		strings.Contains(string(responseBody), "Encrypted function output") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1 (no replay loop)", got)
	}

	audits, err := h.store.ListAuditLog(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, audit := range audits {
		if audit.Action == "codex_agent_message_ciphertext_stripped" {
			found = audit.State == "sanitized" && audit.Reason == "prospective_root_foreign_ciphertext" && audit.Detail == "removed_blocks=1"
			break
		}
	}
	if !found {
		t.Fatalf("missing sanitized-block audit count: %+v", audits)
	}
}
