package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/clientidentity"
)

func TestFrozenRequestClientIdentityUsesOriginalBodyMeta(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("User-Agent", "some rewritten gateway user agent")
	identity := frozenRequestClientIdentity(req, bodysource.BodyMeta{
		ClientFamily: "claude_code", AgentClass: "subagent", Model: "claude-sonnet-4-6",
	})
	if identity.ClientFamily != clientidentity.ClientClaudeCode || identity.AgentClass != clientidentity.AgentSubagent || identity.InboundProtocol != "anthropic_messages" {
		t.Fatalf("identity=%+v", identity)
	}
	frozen := contextWithRequestClientIdentity(context.Background(), identity)
	if got := requestClientIdentityFromContext(frozen); got.ClientFamily != identity.ClientFamily || got.AgentClass != identity.AgentClass || got.InboundProtocol != identity.InboundProtocol {
		t.Fatalf("frozen identity=%+v want=%+v", got, identity.Normalize())
	}
}

func TestFrozenRequestClientIdentityTreatsWebSocketTurnBodyIndependently(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("User-Agent", "codex_cli_rs/0.1")
	identity := frozenRequestClientIdentity(req, bodysource.BodyMeta{ClientFamily: "openai_sdk", AgentClass: "root"})
	if identity.ClientFamily != clientidentity.ClientUnknown || !identity.Conflict || identity.InboundProtocol != "responses_websocket" {
		t.Fatalf("turn identity=%+v", identity)
	}
}

func TestFrozenRequestClientIdentityClassifiesNativeAgentLineage(t *testing.T) {
	const claudeBilling = "x-anthropic-billing-header: cc_version=2.1.241.abc; cc_entrypoint=cli;"
	claudeMeta := func(t *testing.T, text string) bodysource.BodyMeta {
		t.Helper()
		meta, err := bodysource.ScanJSON(context.Background(), bodysource.Bytes([]byte(`{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"`+text+`"}],"messages":[]}`)), nil)
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}
	for _, tc := range []struct {
		name       string
		path       string
		meta       bodysource.BodyMeta
		headers    map[string]string
		wantClient clientidentity.Client
		wantAgent  clientidentity.AgentClass
	}{
		{
			name:       "Claude Code root body signal",
			path:       "/v1/messages",
			meta:       claudeMeta(t, claudeBilling),
			wantClient: clientidentity.ClientClaudeCode,
			wantAgent:  clientidentity.AgentRoot,
		},
		{
			name:       "Claude Code subagent body signal",
			path:       "/v1/messages",
			meta:       claudeMeta(t, claudeBilling+" cc_is_subagent=true;"),
			wantClient: clientidentity.ClientClaudeCode,
			wantAgent:  clientidentity.AgentSubagent,
		},
		{
			name: "Codex CLI root thread header",
			path: "/v1/responses",
			meta: bodysource.BodyMeta{Model: "gpt-5.6-sol"},
			headers: map[string]string{
				"User-Agent": "codex_cli_rs/0.1",
				"Thread-Id":  "root-thread",
			},
			wantClient: clientidentity.ClientCodexCLI,
			wantAgent:  clientidentity.AgentRoot,
		},
		{
			name: "Codex CLI child thread header",
			path: "/v1/responses",
			meta: bodysource.BodyMeta{Model: "gpt-5.6-sol"},
			headers: map[string]string{
				"User-Agent":               "codex_cli_rs/0.1",
				"Thread-Id":                "child-thread",
				"X-Codex-Parent-Thread-Id": "root-thread",
			},
			wantClient: clientidentity.ClientCodexCLI,
			wantAgent:  clientidentity.AgentSubagent,
		},
		{
			name:       "proxy stripped signals remain unknown",
			path:       "/v1/messages",
			meta:       bodysource.BodyMeta{Model: "claude-sonnet-4-6"},
			wantClient: clientidentity.ClientUnknown,
			wantAgent:  clientidentity.AgentUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			identity := frozenRequestClientIdentity(req, tc.meta)
			if identity.ClientFamily != tc.wantClient || identity.AgentClass != tc.wantAgent {
				t.Fatalf("identity=%+v want client=%s agent=%s", identity, tc.wantClient, tc.wantAgent)
			}
		})
	}
}

func TestFrozenRequestClientIdentityPrefersExplicitHeaderLineage(t *testing.T) {
	meta, err := bodysource.ScanJSON(context.Background(), bodysource.Bytes([]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.241.abc; cc_entrypoint=cli;"}],"messages":[]}`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-OpenAI-Subagent", "opaque-codex-child-id")
	if got := frozenRequestClientIdentity(req, meta).AgentClass; got != clientidentity.AgentSubagent {
		t.Fatalf("explicit header lineage lost to body marker: %q", got)
	}
}
