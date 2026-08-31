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
