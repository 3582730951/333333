package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"codex-account-pool/internal/storage"
)

const poolClientInstanceHeader = "X-Pool-Client-ID"

type downstreamClientScopeKey struct{}

// downstreamClientIdentity returns an identity already emitted by the downstream
// protocol. Official Codex and Claude Code clients therefore need no custom
// headers or local identifier file: configuring the pool URL and API key is
// sufficient. X-Pool-Client-ID remains first because the optional local Claude
// gateway creates and persists it internally without user configuration.
func downstreamClientIdentity(r *http.Request) (kind, value string) {
	if r == nil {
		return "", ""
	}
	for _, candidate := range []struct {
		kind   string
		header string
	}{
		{"pool_instance", poolClientInstanceHeader},
		{"explicit_session", "X-Session-ID"},
		{"claude_session", "X-Claude-Code-Session-Id"},
		{"codex_session", "Session-Id"},
		{"codex_conversation", "Conversation-Id"},
		{"codex_thread", "Thread-Id"},
	} {
		if value := strings.TrimSpace(r.Header.Get(candidate.header)); value != "" {
			return candidate.kind, value
		}
	}
	return "", ""
}

// downstreamClientScope is an opaque, domain-separated digest of the downstream
// API-key namespace and an automatically available client/session identity. Raw
// identifiers are never persisted, logged, or included in diagnostics.
func downstreamClientScope(keyHash string, r *http.Request) string {
	kind, clientID := downstreamClientIdentity(r)
	if clientID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("pool-client-scope-v2\x00" +
		strings.TrimSpace(keyHash) + "\x00" + kind + "\x00" + clientID))
	return hex.EncodeToString(sum[:])
}

// withDownstreamClientScope snapshots the protocol-native client identity before
// any Codex recovery path sanitizes Session-Id/Conversation-Id headers. This is
// especially important for a downstream WebSocket: turns admitted after its
// upstream socket has switched to HTTPS rebuild Goal history before CPA mapping
// runs, and that rebuild may not have an *http.Request available.
func withDownstreamClientScope(ctx context.Context, keyHash string, r *http.Request) context.Context {
	scope := downstreamClientScope(keyHash, r)
	if scope == "" {
		return ctx
	}
	return context.WithValue(ctx, downstreamClientScopeKey{}, scope)
}

func downstreamClientScopeFromContext(ctx context.Context) string {
	scope, _ := ctx.Value(downstreamClientScopeKey{}).(string)
	return strings.TrimSpace(scope)
}

func goalDownstreamClientScope(ctx context.Context, keyHash string, r *http.Request) string {
	if scope := downstreamClientScopeFromContext(ctx); scope != "" {
		return scope
	}
	if mapping := codexSessionMappingFromContext(ctx); mapping != nil {
		if scope := strings.TrimSpace(mapping.clientScope); scope != "" {
			return scope
		}
	}
	return downstreamClientScope(keyHash, r)
}

func namespacedGoalAliases(aliases []storage.GoalAlias, namespace string) []storage.GoalAlias {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return append([]storage.GoalAlias(nil), aliases...)
	}
	out := make([]storage.GoalAlias, len(aliases))
	copy(out, aliases)
	for index := range out {
		out[index].Namespace = namespace
	}
	return out
}

// familyGoalAliases stamps the wire-history family onto a copy of the alias list.
// The namespace alone does not separate a Claude Code session that reached Codex
// through the Anthropic→Responses bridge from the same session speaking native
// Messages: both emit identical client identifiers, so without the family the two
// goals would rebind one globally unique alias row away from each other on
// every turn.
func familyGoalAliases(aliases []storage.GoalAlias, family string) []storage.GoalAlias {
	family = strings.TrimSpace(strings.ToLower(family))
	out := make([]storage.GoalAlias, len(aliases))
	copy(out, aliases)
	if family == "" {
		return out
	}
	for index := range out {
		out[index].Family = family
	}
	return out
}

func scopedGoalDownstreamKeyHash(keyHash, namespace string) string {
	keyHash = strings.TrimSpace(keyHash)
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return keyHash
	}
	sum := sha256.Sum256([]byte("goal-downstream-client-v1\x00" + keyHash + "\x00" + namespace))
	return hex.EncodeToString(sum[:])
}

// goalResolutionSetsForClient resolves the client-scoped identities first. For
// upgrades, an exact response/turn-state alias may make one final lookup in the
// legacy unscoped namespace and is then rebound under the client scope by the
// successful commit. Weak root/session aliases never cross that migration bridge.
func goalResolutionSetsForClient(raw []storage.GoalAlias, namespace string, allowResumeFallback bool) [][]storage.GoalAlias {
	return goalResolutionSetsForClientFamily(raw, namespace, "", allowResumeFallback)
}

// goalResolutionSetsForClientFamily adds the wire-history family to every scoped set
// and, for the families that moved to the v3 alias digest, appends the same sets once
// more without a family. That second pass reads the still-valid v2 rows of a goal
// persisted before this upgrade; family-filtered resolution in storage keeps the
// bridge safe, because a v2 row owned by another family is discarded rather than
// resumed. The first commit after the upgrade writes the v3 rows, so the bridge
// naturally stops mattering for that goal.
func goalResolutionSetsForClientFamily(raw []storage.GoalAlias, namespace, family string, allowResumeFallback bool) [][]storage.GoalAlias {
	scoped := familyGoalAliases(namespacedGoalAliases(raw, namespace), family)
	sets := goalResolutionAliasSets(scoped, allowResumeFallback)
	if strings.TrimSpace(namespace) == "" {
		return sets
	}
	if family = strings.TrimSpace(strings.ToLower(family)); family != "" && family != storage.GoalFamilyResponses {
		// Same namespace, same alias values, previous digest — not a widening of the
		// identity, only the older hash of the very same client-scoped identifiers.
		legacyDigest := goalResolutionAliasSets(namespacedGoalAliases(raw, namespace), allowResumeFallback)
		sets = append(sets, legacyDigest...)
	}
	if !allowResumeFallback {
		return sets
	}
	for _, legacy := range goalAliasPrioritySets(raw) {
		if len(legacy) == 0 {
			continue
		}
		switch legacy[0].Type {
		case "response_id", "codex_turn_state":
			sets = append(sets, legacy)
		}
	}
	return sets
}
