package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// These tests exercise the gateway reliability layer END TO END through the real
// handler (handleGatewayPost → codexAttempt), proving the wiring — not just the
// internal/reliability unit logic. They capture the body that actually reaches the
// upstream and the body the downstream actually receives.

func enableReliability(t *testing.T, h *testHarness, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "gateway_reliability", "true"); err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if err := h.store.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReliabilityInjectsRulesEffortAndEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	})
	enableReliability(t, h, nil)
	h.importAccount(t, "rel-a", "upstream-a", "access-a")

	// A critical (auth) coding goal: must floor reasoning effort to xhigh and carry the
	// developer rules + envelope upstream.
	body := `{"model":"gpt","input":[{"role":"user","content":"add authentication to the login flow"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	rb := reqs[0].Body
	// Decode the wire body: json.Marshal escapes <,>,& as \uXXXX, but the model sees the
	// DECODED content, so assert on the decoded instructions + input items.
	var root struct {
		Instructions string `json:"instructions"`
		Input        []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(rb), &root); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, rb)
	}
	if !strings.Contains(root.Instructions, "Do not invent facts") {
		t.Errorf("developer preamble not injected into instructions:\n%s", root.Instructions)
	}
	var envelope, userGoal string
	for _, it := range root.Input {
		if it.Role == "developer" {
			envelope = it.Content
		}
		if it.Role == "user" {
			userGoal = it.Content
		}
	}
	if !strings.Contains(envelope, "<gateway_request>") || !strings.Contains(envelope, "<risk_level>critical</risk_level>") {
		t.Errorf("gateway_request envelope / critical risk missing from developer item:\n%s", envelope)
	}
	if root.Reasoning.Effort != "xhigh" {
		t.Errorf("critical risk did not floor reasoning effort to xhigh: %q", root.Reasoning.Effort)
	}
	// The original user goal must still be present (we never drop user content).
	if !strings.Contains(userGoal, "add authentication to the login flow") {
		t.Errorf("original user goal lost: %q", userGoal)
	}
}

func TestReliabilityLowRiskFloorsToLow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
	})
	enableReliability(t, h, nil)
	h.importAccount(t, "rel-b", "upstream-b", "access-b")
	// A low-risk explanation must floor to LOW effort (the floor maps risk→effort and,
	// per MaxEffort, only ever raises — never lowers — an operator-forced effort).
	body := `{"model":"gpt","input":[{"role":"user","content":"explain what this file does"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	rb := h.requests()[0].Body
	if !strings.Contains(rb, `"effort":"low"`) {
		t.Errorf("low-risk explanation should floor to low effort:\n%s", rb)
	}
}

func TestReliabilityGuardDowngradesFabrication(t *testing.T) {
	// Upstream claims tests pass, but the request carried no tool calls/results.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"All tests pass and the change is complete.","output":[{"type":"message","content":[{"type":"output_text","text":"All tests pass and the change is complete."}]}],"usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}`))
	})
	enableReliability(t, h, nil)
	h.importAccount(t, "rel-c", "upstream-c", "access-c")

	body := `{"model":"gpt","input":[{"role":"user","content":"implement the parser"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(got), "NOT VERIFIED") {
		t.Fatalf("guard did not downgrade a fabricated test claim:\n%s", got)
	}
	if !strings.Contains(string(got), "fabricated_test_result") {
		t.Fatalf("downgrade notice missing the finding code:\n%s", got)
	}
}

func TestReliabilityOffByDefaultNoInjection(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"All tests pass.","output":[{"type":"message","content":[{"type":"output_text","text":"All tests pass."}]}]}`))
	})
	// Do NOT enable reliability — default off.
	h.importAccount(t, "rel-d", "upstream-d", "access-d")
	body := `{"model":"gpt","input":[{"role":"user","content":"add authentication to the login flow"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Upstream body must be untouched by the reliability layer.
	rb := h.requests()[0].Body
	if strings.Contains(rb, "Do not invent facts") || strings.Contains(rb, "<gateway_request>") {
		t.Errorf("reliability injected while flag was OFF:\n%s", rb)
	}
	// Downstream response must NOT be downgraded when the layer is off.
	if strings.Contains(string(got), "NOT VERIFIED") {
		t.Errorf("guard ran while flag was OFF:\n%s", got)
	}
}
