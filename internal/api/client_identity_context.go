package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/clientidentity"
	"codex-account-pool/internal/storage"
)

// requestClientIdentityContextKey holds the inbound classification frozen before
// any protocol adapter rewrites a body or headers. It is attribution only; no
// authorization or routing decision may depend on it.
type requestClientIdentityContextKey struct{}

func contextWithRequestClientIdentity(ctx context.Context, identity clientidentity.RequestClientIdentity) context.Context {
	return context.WithValue(ctx, requestClientIdentityContextKey{}, identity.Normalize())
}

func requestClientIdentityFromContext(ctx context.Context) clientidentity.RequestClientIdentity {
	if ctx != nil {
		if identity, ok := ctx.Value(requestClientIdentityContextKey{}).(clientidentity.RequestClientIdentity); ok {
			return identity.Normalize()
		}
	}
	return clientidentity.Unknown()
}

// usageDiagnosticsWithFrozenClientIdentity copies only fixed-enum classification
// fields into a durable usage write. It intentionally excludes evidence values,
// headers, raw models, and session/thread identifiers.
func usageDiagnosticsWithFrozenClientIdentity(ctx context.Context, diag storage.UsageDiagnostics) storage.UsageDiagnostics {
	identity := requestClientIdentityFromContext(ctx)
	diag.ClientFamily = storage.NormalizeClientFamily(string(identity.ClientFamily))
	diag.ClientConfidence = storage.NormalizeClientConfidence(string(identity.Confidence))
	diag.ClientConflict = identity.Conflict
	diag.ClassifierVersion = identity.ClassifierVersion
	diag.InboundProtocol = identity.InboundProtocol
	diag.RequestedModelFamily = identity.RequestedModelFamily
	diag.ResolvedProviderFamily = identity.ResolvedProviderFamily
	if diag.ResolvedProviderFamily == "" {
		diag.ResolvedProviderFamily = resolvedProviderFamily(diag.UsageProvider)
	}
	return diag
}

func resolvedProviderFamily(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "openai", "chatgpt":
		return "codex"
	case "claude", "anthropic":
		return "claude"
	case "kiro":
		return "kiro"
	case "antigravity", "gemini", "google":
		return "gemini"
	default:
		return "unknown"
	}
}

// frozenRequestClientIdentity reconstitutes a tiny body probe from BodyMeta.
// BodyMeta is captured from the original inbound bytes, before virtualizers and
// compatibility bridges rewrite them; this avoids retaining or rereading an
// unbounded user prompt merely for attribution.
func frozenRequestClientIdentity(r *http.Request, meta bodysource.BodyMeta) clientidentity.RequestClientIdentity {
	headers := http.Header(nil)
	if r != nil {
		headers = r.Header
	}
	probe := map[string]string{}
	if meta.Model != "" {
		probe["model"] = meta.Model
	}
	if meta.ClaudeCodeBilling {
		// The canonical Claude Code billing marker is inside system[0].text,
		// not an HTTP header.  BodyMeta validated its location and syntax while
		// the original body was captured, so retain only the fixed family enum.
		probe["client_family"] = "claude_code"
	} else if meta.ClientFamily != "" {
		probe["client_family"] = meta.ClientFamily
		// The scanner stores originator in ClientFamily for historical body
		// compatibility. Supplying it under both names preserves Codex's native
		// high-confidence originator marker.
		probe["originator"] = meta.ClientFamily
	}
	if meta.ClaudeCodeAgentClass != "" {
		probe["agent_class"] = meta.ClaudeCodeAgentClass
	} else if meta.AgentClass != "" {
		probe["agent_class"] = meta.AgentClass
	}
	body, _ := json.Marshal(probe)
	identity := clientidentity.FromHeadersAndBody(headers, body, requestInboundProtocol(r))
	if headerClass := clientidentity.NormalizeAgent(requestAccountAgentClass(r)); headerClass != clientidentity.AgentUnknown {
		// Header lineage remains authoritative over a body marker. The historical
		// helper owns the protocol order (explicit subagent marker, Anthropic
		// billing header, then Codex parent/fork/thread lineage); reconnecting it
		// here preserves that order for the frozen HTTP and WebSocket identity.
		identity.AgentClass = headerClass
	}
	return identity.Normalize()
}

func requestInboundProtocol(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	path := r.URL.Path
	if isResponsesWebSocketUpgrade(r) {
		return "responses_websocket"
	}
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic_messages"
	case strings.HasPrefix(path, "/v1/responses"):
		return "responses"
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return "chat_completions"
	default:
		return ""
	}
}
