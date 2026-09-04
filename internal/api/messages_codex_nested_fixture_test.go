package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/storage"
)

const nestedDepthTwoFixtureCallID = "call_q3pzFM5pC1hlIiDXlUIKJjbe"

// TestMessagesViaCodexNestedDepthTwoFixtureRoundTripsMockUpstream covers the
// actual Claude Code 2.1.226 child-resume request captured after a grandchild
// Agent completed. It exercises the whole Messages -> Responses -> Messages
// bridge against a mock Responses upstream, rather than a synthetic one-hop
// tool exchange.
func TestMessagesViaCodexNestedDepthTwoFixtureRoundTripsMockUpstream(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "prompt", "testdata", "claude_code_2_1_226_nested_depth2_child_resume.json"))
	if err != nil {
		t.Fatal(err)
	}

	var (
		upstreamMu sync.Mutex
		postBodies [][]byte
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream path=%q, want Responses endpoint", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("upstream method=%q, want POST", r.Method)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		upstreamMu.Lock()
		postBodies = append(postBodies, append([]byte(nil), raw...))
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nestedDepthTwoFixtureTextSSE())
	})
	// A regular HTTP Messages request must use the strict CPA HTTP/SSE path. The
	// direct harness would otherwise select the preferred Responses WebSocket for
	// gpt-5.5, which is a different upstream transport from the bridge under test.
	h.app.cfg.CodexSessionMappingEnabled = true
	h.app.cfg.CodexCPAStrict = true
	const accountID = "nested-depth-two-fixture"
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: accountID, Label: accountID, GroupName: "cyber", Provider: "codex", Status: "active",
		UpstreamAccountID: "upstream-nested-depth-two-fixture",
	}, storage.AccountToken{AccountID: accountID, AccessToken: "access-nested-depth-two-fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(t.Context(), storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}
	setTestCapability(t, h, accountID, "gpt-5.5", 272000)
	h.app.scheduler.InvalidateAccountCache()

	const sessionID = "nested-depth-two-fixture-session"
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("bridge status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if !bytes.Contains(body, []byte("NESTED_DEPTH_TWO_RESUME_OK")) || !bytes.Contains(body, []byte("event: message_stop")) {
		upstreamMu.Lock()
		posts := append([][]byte(nil), postBodies...)
		upstreamMu.Unlock()
		t.Fatalf("Messages SSE did not complete nested resume: %s\nResponses POST bodies: %q", body, posts)
	}

	upstreamMu.Lock()
	posts := append([][]byte(nil), postBodies...)
	upstreamMu.Unlock()
	if len(posts) != 1 {
		t.Fatalf("converted Responses POST calls=%d, want one", len(posts))
	}
	var converted map[string]interface{}
	if err := json.Unmarshal(posts[0], &converted); err != nil {
		t.Fatal(err)
	}
	if converted["model"] != "gpt-5.5" {
		t.Fatalf("converted model=%v", converted["model"])
	}
	assertNestedDepthTwoConvertedPair(t, converted)
}

func assertNestedDepthTwoConvertedPair(t *testing.T, converted map[string]interface{}) {
	t.Helper()
	var functionCall, functionResult map[string]interface{}
	for _, rawInput := range converted["input"].([]interface{}) {
		input := rawInput.(map[string]interface{})
		switch input["type"] {
		case "function_call":
			if input["call_id"] == nestedDepthTwoFixtureCallID {
				functionCall = input
			}
		case "function_call_output":
			if input["call_id"] == nestedDepthTwoFixtureCallID {
				functionResult = input
			}
		}
	}
	if functionCall == nil || functionResult == nil {
		t.Fatalf("nested Agent call/result missing from converted input: %#v", converted["input"])
	}
	if functionCall["name"] != "Agent" || functionResult["call_id"] != functionCall["call_id"] {
		t.Fatalf("nested Agent identity diverged: call=%#v result=%#v", functionCall, functionResult)
	}
	var arguments map[string]interface{}
	if err := json.Unmarshal([]byte(functionCall["arguments"].(string)), &arguments); err != nil {
		t.Fatal(err)
	}
	if _, present := arguments["model"]; present {
		t.Fatalf("nested Agent call retained model override: %#v", arguments)
	}

	var agentTool map[string]interface{}
	for _, rawTool := range converted["tools"].([]interface{}) {
		tool := rawTool.(map[string]interface{})
		if tool["name"] == "Agent" {
			agentTool = tool
			break
		}
	}
	if agentTool == nil {
		t.Fatalf("Agent tool missing from converted tools: %#v", converted["tools"])
	}
	parameters := agentTool["parameters"].(map[string]interface{})
	properties := parameters["properties"].(map[string]interface{})
	if _, present := properties["model"]; present {
		t.Fatalf("nested Agent schema retained model override: %#v", parameters)
	}
}

func nestedDepthTwoFixtureTextSSE() string {
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_nested_depth_two","model":"gpt-5.5"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"NESTED_DEPTH_TWO_RESUME_OK"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_nested_depth_two","model":"gpt-5.5","status":"completed","usage":{"input_tokens":16,"output_tokens":4,"total_tokens":20}}}` + "\n\n"
}
