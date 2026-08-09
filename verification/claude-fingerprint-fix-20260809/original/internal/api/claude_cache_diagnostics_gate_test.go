package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
)

// TestCacheDiagnosticsHeaderStrippedWhenDisabled: claudeCacheDiagnosticsHeader is an
// INTERNAL control header. The upstream builder reads it off the downstream header set to
// decide whether to append the cache-diagnosis beta, so if the flag is off and we leave a
// client-supplied value in place, a downstream caller can drive the account's
// anthropic-beta set from outside the operator's control. The active beta set is a
// prompt-affecting request parameter upstream, so an untrusted caller must not toggle it.
func TestCacheDiagnosticsHeaderStrippedWhenDisabled(t *testing.T) {
	s := &Server{cfg: config.Default(), store: apiTestStore(t), claudeCacheDiagPrev: map[string]string{}}
	if s.claudeCacheDiagnosticsEnabled(context.Background()) {
		t.Fatal("cache diagnostics must default to off; this test covers the disabled path")
	}

	headers := http.Header{}
	headers.Set(claudeCacheDiagnosticsHeader, "1")
	body := []byte(`{"model":"claude-opus-5"}`)

	out := s.applyClaudeCacheDiagnostics(context.Background(), headers, body, routing.AffinityKey{Hash: "abc"})

	if got := headers.Get(claudeCacheDiagnosticsHeader); got != "" {
		t.Errorf("%s = %q with the feature disabled; a client can force the cache-diagnosis beta upstream", claudeCacheDiagnosticsHeader, got)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated while disabled: %s", out)
	}
}

// TestCacheDiagnosticsDefaultsOff records the default. The beta and the body field are both
// genuine Anthropic API surface (cache-diagnosis-2026-04-07), but they are not part of a
// stock Claude Code turn, so they stay opt-in.
func TestCacheDiagnosticsDefaultsOff(t *testing.T) {
	s := &Server{cfg: config.Default(), store: apiTestStore(t), claudeCacheDiagPrev: map[string]string{}}
	if s.claudeCacheDiagnosticsEnabled(context.Background()) {
		t.Error("claude cache diagnostics is enabled by default; it adds a beta and a body field a stock Claude Code turn does not send")
	}
}
