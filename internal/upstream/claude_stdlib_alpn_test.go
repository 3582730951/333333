package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// TestClaudeStdlibFallbackPinsHTTP11 covers the third transport engine.
//
// The in-process and sidecar engines both consult forceHTTP1ForClaudeImpersonation, because
// Node's bundled undici forces allowH2:false and a capture of the real client offers ALPN
// http/1.1 only — so a claude-cli User-Agent arriving over HTTP/2 is, by construction, never
// the real client. The stdlib fallback (ClaudeForceDirect, or an engine that resolved to
// stdlib because no TLS factory exists) built its client through httpClientForEgress, which
// hardcodes forceHTTP1=false, so net/http's ForceAttemptHTTP2 negotiated h2 against
// Anthropic's edge while applyClaudeHeaders had already stamped the full claude-cli
// identity onto the request. The operator's decision to skip TLS impersonation is not a
// decision to contradict the client being impersonated at the protocol layer.
//
// The transport cache key encodes the mode (transportKeyForMode prefixes "http1|"), so the
// key that appears after one request is direct evidence of which mode doClaude asked for.
func TestClaudeStdlibFallbackPinsHTTP11(t *testing.T) {
	client, _ := runClaudeStdlibRequest(t)

	client.tmu.Lock()
	keys := make([]string, 0, len(client.transports))
	for key := range client.transports {
		keys = append(keys, key)
	}
	transport := client.transports["http1|direct"]
	client.tmu.Unlock()
	sort.Strings(keys)

	if transport == nil {
		t.Fatalf("stdlib Claude fallback built transports %v; want the http1|direct transport, i.e. ALPN pinned to http/1.1 like the other two engines", keys)
	}
	if transport.ForceAttemptHTTP2 {
		t.Errorf("stdlib Claude transport has ForceAttemptHTTP2 set; a claude-cli User-Agent must not ride an h2 connection")
	}
	if transport.TLSNextProto == nil {
		t.Errorf("stdlib Claude transport leaves TLSNextProto nil; net/http will still auto-upgrade to h2 when the peer advertises it")
	} else if len(transport.TLSNextProto) != 0 {
		t.Errorf("stdlib Claude transport TLSNextProto has %d entries, want an empty (non-nil) map", len(transport.TLSNextProto))
	}
	if transport.TLSClientConfig == nil {
		t.Fatalf("stdlib Claude transport has no TLSClientConfig")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("stdlib Claude transport ALPN = %v, want exactly [http/1.1]", got)
	}
}

// TestClaudeStdlibFallbackStillSendsClaudeIdentity is the other half of the argument: the
// stdlib path is not a "plain" transport that has shed the impersonation. If it ever stops
// claiming to be claude-cli, the HTTP/1.1 pin above would no longer be required and this
// test says so out loud rather than leaving the pin unexplained.
func TestClaudeStdlibFallbackStillSendsClaudeIdentity(t *testing.T) {
	_, got := runClaudeStdlibRequest(t)

	if ua := got.Get("User-Agent"); len(ua) < len("claude-cli/") || ua[:len("claude-cli/")] != "claude-cli/" {
		t.Fatalf("stdlib fallback User-Agent = %q, want a claude-cli/... identity", ua)
	}
	if got.Get("X-App") != "cli" {
		t.Errorf("stdlib fallback x-app = %q, want cli", got.Get("X-App"))
	}
	if got.Get("X-Stainless-Lang") != "js" {
		t.Errorf("stdlib fallback x-stainless-lang = %q, want js", got.Get("X-Stainless-Lang"))
	}
}

// runClaudeStdlibRequest drives one Claude /v1/messages request through the stdlib engine
// and returns the client (for transport inspection) plus the headers the upstream saw.
func runClaudeStdlibRequest(t *testing.T) (*Client, http.Header) {
	t.Helper()
	var seen http.Header
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstreamSrv.Close()

	cfg := config.Default()
	cfg.ClaudeUpstreamBaseURL = upstreamSrv.URL
	// The escape hatch that selects the stdlib engine.
	cfg.ClaudeForceDirect = true
	client := NewClient(cfg)
	if engine := client.claudeFingerprintEngine(storage.EgressProfile{ID: "eg", Type: "direct"}); engine != claudeEngineStdlib {
		t.Fatalf("engine = %v, want claudeEngineStdlib; this test must exercise the stdlib fallback", engine)
	}

	resp, err := client.Do(t.Context(), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Model:          "claude-opus-4-6",
		Account:        storage.Account{ID: "acct-1", Provider: "claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Egress:         storage.EgressProfile{ID: "eg", Type: "direct", Health: "healthy"},
		Body:           testBody([]byte(`{"model":"claude-opus-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return client, seen
}
