package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// captureClaudeUpstreamBody runs one Claude request against a fake upstream on the stdlib
// engine (ClaudeForceDirect) and returns the JSON body that actually left this process.
func captureClaudeUpstreamBody(t *testing.T, cfgMutate func(*config.Config), body string) map[string]interface{} {
	t.Helper()
	var sent []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstreamSrv.Close()

	cfg := config.Default()
	cfg.ClaudeUpstreamBaseURL = upstreamSrv.URL
	// The stdlib engine is the simplest way to observe the body; the thinking injection
	// under test runs before any transport choice.
	cfg.ClaudeForceDirect = true
	if cfgMutate != nil {
		cfgMutate(&cfg)
	}
	client := NewClient(cfg)

	// spec.Model is what thinking validation keys off, so it has to agree with the body.
	var requested struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal([]byte(body), &requested)

	resp, err := client.Do(t.Context(), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Model:          requested.Model,
		Account:        storage.Account{ID: "acct-1", Provider: "claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Egress:         storage.EgressProfile{ID: "eg", Type: "direct", Health: "healthy"},
		Body:           testBody([]byte(body)),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	var root map[string]interface{}
	if err := json.Unmarshal(sent, &root); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, sent)
	}
	return root
}

// TestThinkingInjectionLeavesZeroMaxTokensPrewarmAlone is the regression test for this
// relay's own thinking configuration being injected into a max_tokens:0 cache pre-warm.
//
// Anthropic documents max_tokens:0 as the cache pre-warm: the API writes the cache at each
// cache_control breakpoint and returns immediately, empty content, zero output tokens
// billed. It also documents that the pre-warm is rejected with invalid_request_error when
// extended thinking is enabled on the request. Thinking injection did not know about that
// and stamped its thinking block onto the pre-warm body anyway.
//
// Two things are wrong with that independent of how Anthropic grades the combination. The
// pre-warm is the one request shape whose whole purpose is to carry the client's own
// parameters unchanged so the cache entry it writes matches the real turn that follows;
// a thinking block the downstream client never sent makes the pre-warm and the real turn
// disagree. And a pre-warm is emitted per warmed turn, so any rejection is a repeating,
// self-similar 400 pattern against the account.
func TestThinkingInjectionLeavesZeroMaxTokensPrewarmAlone(t *testing.T) {
	prewarm := `{"model":"claude-opus-4-6","max_tokens":0,"messages":[{"role":"user","content":"warmup"}]}`
	root := captureClaudeUpstreamBody(t, func(cfg *config.Config) {
		cfg.ThinkingEnabled = true
		cfg.ThinkingDefaultMode = "budget"
		cfg.ThinkingDefaultBudget = 4096
	}, prewarm)

	got, isNumber := root["max_tokens"].(float64)
	if !isNumber || got != 0 {
		t.Errorf("upstream max_tokens = %v, want the pre-warm's own 0", root["max_tokens"])
	}
	if thinkingBlock, hasThinking := root["thinking"]; hasThinking {
		t.Errorf("upstream body carries thinking %v on a max_tokens:0 pre-warm; Anthropic rejects max_tokens:0 when extended thinking is enabled, so this request could only 400", thinkingBlock)
	}
}

// TestThinkingInjectionStillAppliesToRealTurns: the guard above must be scoped to
// max_tokens:0 only. A normal turn must keep the operator's thinking configuration, or the
// fix would silently disable the feature.
func TestThinkingInjectionStillAppliesToRealTurns(t *testing.T) {
	real := `{"model":"claude-opus-4-6","max_tokens":8192,"messages":[{"role":"user","content":"hi"}]}`
	root := captureClaudeUpstreamBody(t, func(cfg *config.Config) {
		cfg.ThinkingEnabled = true
		cfg.ThinkingDefaultMode = "budget"
		cfg.ThinkingDefaultBudget = 4096
	}, real)
	if _, hasThinking := root["thinking"]; !hasThinking {
		t.Fatalf("thinking injection did not apply to a normal turn (body=%v); the max_tokens:0 guard is too broad", root)
	}
}

// TestZeroMaxTokensPrewarmDetection pins the predicate itself: only an explicit numeric zero
// counts. A missing max_tokens is a normal turn, and a non-zero value must never match.
func TestZeroMaxTokensPrewarmDetection(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"max_tokens":0}`, true},
		{`{"max_tokens":0.0}`, true},
		{`{"max_tokens":1}`, false},
		{`{"max_tokens":1024}`, false},
		{`{}`, false},
		{`{"max_tokens":null}`, false},
		{`{"max_tokens":"0"}`, false},
		{`not json`, false},
	} {
		if got := claudeZeroMaxTokensPrewarm([]byte(tc.body)); got != tc.want {
			t.Errorf("claudeZeroMaxTokensPrewarm(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
