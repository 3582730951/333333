package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/sjson"

	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// downstreamPolicy is the resolved policy for an incoming gateway request: which
// account group to route to, and any forced model / reasoning-effort override the
// caller's api key (or its group default) imposes regardless of what the client
// requested.
type downstreamPolicy struct {
	Group        string
	ForceModel   string
	ForceEffort  string
	ProviderHint string
	KeyLabel     string
	// KeyHash / UserID identify the matched downstream api key and its owning portal
	// user (both empty for an unauthenticated/legacy request). They are attributed onto
	// each usage_records row so a user's own console can show their usage.
	KeyHash string
	UserID  string
	Authed  bool
	// UserGroupID, when set, means the key uses the two-layer group model.
	UserGroupID            string
	PinnedEgressNoFallback bool
	ModelOverrideSource    string
}

// downstreamIdent carries the resolved key/user identity through the request context
// so usage recording can attribute a row without threading it through every handler.
type downstreamIdent struct{ KeyHash, UserID string }

type ctxKey int

const (
	ctxKeyDownstream ctxKey = iota
	// ctxKeyInternal marks a relay-internal request (e.g. the moderation model call)
	// so it skips history moderation (no recursion) and the require-downstream-key gate.
	ctxKeyInternal
	// ctxKeyBillingHold carries the billing_hold id so usage recording can fall
	// back to estimated_tokens when the upstream response body lacks usage data.
	ctxKeyBillingHold
	ctxKeyUsageDiagnostics
	ctxKeyModelDiagnostics
	ctxKeyPublicChatPolicy
	// ctxKeyPinnedEgress marks a request resolved to a user group whose primary
	// account/egress is authoritative.  It is kept separate from the downstream
	// identity because custom and opaque passthrough adapters do not otherwise
	// receive the full policy value.
	ctxKeyPinnedEgress
)

type modelDiagnostics struct{ Requested, Resolved, Source string }

func withInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyInternal, true)
}

func isInternalCall(ctx context.Context) bool { v, _ := ctx.Value(ctxKeyInternal).(bool); return v }

func withPublicChatPolicy(ctx context.Context, pol downstreamPolicy) context.Context {
	return context.WithValue(ctx, ctxKeyPublicChatPolicy, pol)
}

func publicChatPolicyFromContext(ctx context.Context) (downstreamPolicy, bool) {
	pol, ok := ctx.Value(ctxKeyPublicChatPolicy).(downstreamPolicy)
	return pol, ok
}

func withDownstreamKey(ctx context.Context, pol downstreamPolicy) context.Context {
	if pol.PinnedEgressNoFallback {
		ctx = context.WithValue(ctx, ctxKeyPinnedEgress, true)
	}
	if pol.KeyHash == "" && pol.UserID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyDownstream, downstreamIdent{pol.KeyHash, pol.UserID})
}

func withPinnedEgressPolicy(ctx context.Context, pinned bool) context.Context {
	if !pinned {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyPinnedEgress, true)
}

func pinnedEgressNoFallbackFromContext(ctx context.Context) bool {
	pinned, _ := ctx.Value(ctxKeyPinnedEgress).(bool)
	return pinned
}

func downstreamFromCtx(ctx context.Context) (string, string) {
	if v, ok := ctx.Value(ctxKeyDownstream).(downstreamIdent); ok {
		return v.KeyHash, v.UserID
	}
	return "", ""
}

// withBillingHold stores the billing_hold id in the request context so the
// deferred usage-recording functions can fall back to estimated_tokens when
// the upstream response body does not contain countable usage data.
func withBillingHold(ctx context.Context, holdID string) context.Context {
	return context.WithValue(ctx, ctxKeyBillingHold, holdID)
}

// holdIDFromCtx returns the billing_hold id stored in the context, or "".
func holdIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyBillingHold).(string); ok {
		return v
	}
	return ""
}

func withUsageDiagnostics(ctx context.Context, diag storage.UsageDiagnostics) context.Context {
	return context.WithValue(ctx, ctxKeyUsageDiagnostics, diag)
}

func usageDiagnosticsFromCtx(ctx context.Context) storage.UsageDiagnostics {
	if v, ok := ctx.Value(ctxKeyUsageDiagnostics).(storage.UsageDiagnostics); ok {
		return v
	}
	return storage.UsageDiagnostics{}
}

func withModelDiagnostics(ctx context.Context, requested, resolved, source string) context.Context {
	if source == "" {
		source = "none"
	}
	return context.WithValue(ctx, ctxKeyModelDiagnostics, modelDiagnostics{Requested: requested, Resolved: resolved, Source: source})
}

func modelDiagnosticsFromCtx(ctx context.Context) modelDiagnostics {
	if value, ok := ctx.Value(ctxKeyModelDiagnostics).(modelDiagnostics); ok {
		return value
	}
	return modelDiagnostics{Source: "none"}
}

// hashAPIKey is the canonical sha256 hex of a downstream api key; both key
// creation (server-side) and request authentication use it so only the hash is
// ever persisted.
func hashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// downstreamBearer extracts the client-presented credential from either the
// Anthropic-style x-api-key header or an Authorization: Bearer header.
func downstreamBearer(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return auth
}

// isAnthropicClient reports whether the request comes from an Anthropic-native client
// (Claude Code, the Anthropic SDKs). It keys off headers only those clients send —
// anthropic-version / anthropic-beta, or the Anthropic-style x-api-key credential — so
// OpenAI/Codex clients (which authenticate via Authorization: Bearer and omit these) are
// never misclassified. Used to serve the correct /v1/models schema per client family.
func isAnthropicClient(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("anthropic-version")) != "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("anthropic-beta")) != "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("x-api-key")) != "" {
		return true
	}
	return false
}

// resolveDownstreamPolicy authenticates the request's api key (if any) and
// resolves the effective routing group + forced model/effort, folding in the
// group-level defaults when the key sets none. When RequireDownstreamKey is on
// and the key is missing/invalid it writes a 401 and returns ok=false.
func (s *Server) resolveDownstreamPolicy(w http.ResponseWriter, r *http.Request) (downstreamPolicy, bool) {
	ctx := r.Context()
	// Relay-internal calls (the moderation model call) bypass downstream-key enforcement.
	if isInternalCall(ctx) {
		return downstreamPolicy{Group: s.cfg.DefaultGroup, ProviderHint: "auto", Authed: true}, true
	}
	if pol, ok := publicChatPolicyFromContext(ctx); ok {
		if strings.TrimSpace(pol.Group) == "" {
			pol.Group = s.cfg.DefaultGroup
		}
		if strings.TrimSpace(pol.ProviderHint) == "" {
			pol.ProviderHint = "auto"
		}
		if strings.TrimSpace(pol.UserGroupID) != "" {
			if ug, ok, ugErr := s.store.GetUserGroup(ctx, pol.UserGroupID); ugErr == nil && ok {
				pol.PinnedEgressNoFallback = ug.PinnedEgressNoFallback
				if strings.TrimSpace(pol.ForceModel) == "" {
					pol.ForceModel = strings.TrimSpace(ug.ForceModel)
					if pol.ForceModel != "" {
						pol.ModelOverrideSource = "user_group"
					}
				}
				if strings.TrimSpace(pol.ForceEffort) == "" {
					pol.ForceEffort = strings.TrimSpace(ug.ForceEffort)
				}
			}
		}
		pol.Authed = true
		return pol, true
	}
	pol := downstreamPolicy{Group: s.cfg.DefaultGroup, ProviderHint: "auto"}
	plain := downstreamBearer(r)
	if plain != "" {
		if key, found, _ := s.store.LookupAPIKey(ctx, hashAPIKey(plain)); found {
			if normalizeAPIKeyType(key.KeyType) == "pool_import" || isPoolImportKeyPlain(plain) {
				writeError(w, http.StatusForbidden, errors.New("pool import key cannot be used for inference"))
				return downstreamPolicy{}, false
			}
			expired := key.ExpiresAt > 0 && key.ExpiresAt <= storage.Now()
			if !key.Enabled || expired {
				if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
					if expired {
						writeError(w, http.StatusUnauthorized, errors.New("api key expired"))
					} else {
						writeError(w, http.StatusUnauthorized, errors.New("api key disabled"))
					}
					return downstreamPolicy{}, false
				}
			} else {
				// Authentication has succeeded. Usage attribution must not depend on this
				// best-effort observability write, so a transient DB error cannot reject an
				// otherwise valid inference request.
				keyHash, usedAt := key.KeyHash, storage.Now()
				s.enqueueAPIKeyUsed(keyHash, usedAt)
				pol.Authed = true
				pol.KeyLabel = key.Label
				pol.KeyHash = key.KeyHash
				pol.UserID = key.UserID
				if strings.TrimSpace(key.GroupName) != "" {
					pol.Group = key.GroupName
				}
				pol.ForceModel = strings.TrimSpace(key.ForceModel)
				if pol.ForceModel != "" {
					pol.ModelOverrideSource = "api_key"
				}
				pol.ForceEffort = strings.TrimSpace(key.ForceEffort)
				pol.ProviderHint = normalizeProviderHintLoose(key.ProviderHint)
				if strings.TrimSpace(key.UserGroupID) != "" {
					pol.UserGroupID = key.UserGroupID
				}
			}
		} else if isPoolImportKeyPlain(plain) {
			writeError(w, http.StatusForbidden, errors.New("pool import key cannot be used for inference"))
			return downstreamPolicy{}, false
		} else if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
			writeError(w, http.StatusUnauthorized, errors.New("unknown api key"))
			return downstreamPolicy{}, false
		}
	} else if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
		writeError(w, http.StatusUnauthorized, errors.New("api key required"))
		return downstreamPolicy{}, false
	}
	// User-facing policy lives exclusively on user groups. Account-pool groups are
	// routing inventory and must never override model or effort.
	if pol.UserGroupID != "" {
		if ug, ok, ugErr := s.store.GetUserGroup(ctx, pol.UserGroupID); ugErr == nil && ok {
			pol.PinnedEgressNoFallback = ug.PinnedEgressNoFallback
			if pol.ForceModel == "" {
				pol.ForceModel = strings.TrimSpace(ug.ForceModel)
				if pol.ForceModel != "" {
					pol.ModelOverrideSource = "user_group"
				}
			}
			if pol.ForceEffort == "" {
				pol.ForceEffort = strings.TrimSpace(ug.ForceEffort)
			}
		}
	}
	// A server-selected traffic fallback is applied after ordinary API-key and
	// source-group policy resolution. It is therefore authoritative for the
	// destination user group and rewritten model, while identity / attribution
	// remain tied to the caller's original key.
	if fallback, ok := trafficFallbackExecutionFromContext(ctx); ok {
		pol.UserGroupID = strings.TrimSpace(fallback.TargetUserGroupID)
		pol.ForceModel = strings.TrimSpace(fallback.TargetModel)
		pol.ProviderHint = "auto"
		pol.ModelOverrideSource = "traffic_fallback"
		if ug, found, ugErr := s.store.GetUserGroup(ctx, pol.UserGroupID); ugErr == nil && found {
			pol.PinnedEgressNoFallback = ug.PinnedEgressNoFallback
		}
	}
	return pol, true
}

func normalizeProviderHintLoose(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "auto"
	}
	return v
}

// withIntelligentRoutingFallbacks decorates a request's context with same-group
// fallback route choices when intelligent routing is enabled and the resolved
// group declares a fallback chain. The scheduler's primary-first selection then
// serves the primary group instantly whenever it holds a schedulable account and
// steps to the next fallback group the moment the primary has none — the
// "没有账号就立刻切换" behavior. User-group routing is excluded: it already runs its
// own cross-group dispatch with persistent bindings.
func (s *Server) withIntelligentRoutingFallbacks(r *http.Request, pol downstreamPolicy) *http.Request {
	if !s.cfg.IntelligentRoutingEnabled {
		return r
	}
	if strings.TrimSpace(pol.UserGroupID) != "" {
		return r
	}
	primary := strings.TrimSpace(pol.Group)
	fallbacks := s.cfg.GroupFallbacks[primary]
	if len(fallbacks) == 0 {
		return r
	}
	choices := make([]scheduler.RouteChoice, 0, 1+len(fallbacks))
	choices = append(choices, scheduler.RouteChoice{ChoiceKey: primary, Route: scheduler.Route{Group: primary}})
	for _, group := range fallbacks {
		choices = append(choices, scheduler.RouteChoice{ChoiceKey: group, Route: scheduler.Route{Group: group}})
	}
	ctx, _ := scheduler.WithPrimaryRouteChoices(r.Context(), primary, choices, nil)
	return r.WithContext(ctx)
}

func (s *Server) attachUserGroupPolicy(w http.ResponseWriter, r *http.Request, pol downstreamPolicy) (*http.Request, bool) {
	if strings.TrimSpace(pol.UserGroupID) == "" {
		return r.WithContext(withRequestAccountGroupPolicy(r.Context(), storage.Group{})), true
	}
	group, ok, err := s.store.GetUserGroup(r.Context(), pol.UserGroupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return r, false
	}
	if !ok {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_not_found", "configured user group was not found")
		return r, false
	}
	pol.PinnedEgressNoFallback = group.PinnedEgressNoFallback
	requestGroup := superInstructPolicyForClient(userGroupPolicyAsAccountGroup(group), r)
	return r.WithContext(withRequestAccountGroupPolicy(r.Context(), requestGroup)), true
}

func normalizeProviderHint(v string) (string, bool) {
	v = normalizeProviderHintLoose(v)
	switch v {
	case "auto", "codex", "claude", "kiro", "antigravity":
		return v, true
	}
	if strings.HasPrefix(v, "custom:") && strings.TrimPrefix(v, "custom:") != "" {
		return v, true
	}
	return "", false
}

func claudeAllowedProviders(r *http.Request, pol downstreamPolicy) ([]string, error) {
	hint := normalizeProviderHintLoose(r.Header.Get("X-Pool-Provider"))
	if strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		hint = normalizeProviderHintLoose(pol.ProviderHint)
	}
	switch hint {
	case "auto", "":
		return []string{"claude", "kiro"}, nil
	case "claude":
		return []string{"claude"}, nil
	case "kiro":
		return []string{"kiro"}, nil
	case "antigravity":
		return []string{"antigravity"}, nil
	default:
		return nil, errors.New("Claude-family inference provider must be auto, claude, kiro, or antigravity")
	}
}

// setForcedModel rewrites the top-level "model" field of a request body. It fires on
// most Claude requests (every alias/auto normalization, plus forced-model keys), so it
// uses a single-pass sjson edit rather than materializing the whole (often multi-MB
// 1M-context) body as a map[string]interface{} and re-marshaling it — same result,
// far less CPU/GC on the hot path. Returns raw unchanged on empty model or invalid JSON.
func setForcedModel(raw []byte, model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" {
		return raw
	}
	if out, err := sjson.SetBytes(raw, "model", model); err == nil {
		return out
	}
	return raw
}

// normalizeEffort canonicalizes a reasoning-effort string. Empty stays empty
// (no override). Recognized tiers: minimal, low, medium, high, xhigh, max, ultra.
func normalizeEffort(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	switch e {
	case "":
		return ""
	case "x-high", "extra-high", "extra_high", "veryhigh", "very-high", "very_high":
		return "xhigh"
	case "maximum":
		return "max"
	case "min", "none":
		return "minimal"
	}
	return e
}

// applyForcedReasoningResponses sets reasoning.effort on a Codex /v1/responses
// body. The OpenAI-compat chat body is converted to the responses shape before
// this runs, so it covers both entry paths.
func applyForcedReasoningResponses(body []byte, effort string) []byte {
	effort = normalizeEffort(effort)
	if effort == "" || !json.Valid(body) {
		return body
	}
	// This is a policy override of one leaf, not a request rewrite.  A targeted
	// edit keeps input/instructions/tool payloads (including >2^53 integer IDs)
	// byte-identical.
	if out, err := sjson.SetBytes(body, "reasoning.effort", effort); err == nil {
		return out
	}
	return body
}

// claudeThinkingBudget maps a reasoning-effort tier to an Anthropic extended-
// thinking token budget. ok=false means "no recognized tier, leave the request
// unchanged"; budget<=0 means "disable thinking".
func claudeThinkingBudget(effort string) (int, bool) {
	switch normalizeEffort(effort) {
	case "minimal":
		return 0, true
	case "low":
		return 4096, true
	case "medium":
		return 12000, true
	case "high":
		return 24000, true
	case "xhigh":
		return 48000, true
	case "max":
		return 128000, true
	}
	return 0, false
}

// applyForcedThinkingClaude maps a forced reasoning effort onto an Anthropic
// Messages body's extended-thinking budget. Enabling thinking also satisfies the
// API's coupled constraints (max_tokens must exceed the budget; temperature must
// be 1 with no top_p/top_k). minimal disables thinking.
func applyForcedThinkingClaude(body []byte, effort string) []byte {
	budget, ok := claudeThinkingBudget(effort)
	if !ok {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	if budget <= 0 {
		delete(root, "thinking")
	} else {
		root["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
		maxTokens := 0
		if v, ok := root["max_tokens"].(float64); ok {
			maxTokens = int(v)
		}
		if maxTokens <= budget {
			root["max_tokens"] = budget + 8192
		}
		root["temperature"] = 1
		delete(root, "top_p")
		delete(root, "top_k")
	}
	if out, err := anthropicwire.MarshalPreservingOrder(body, root); err == nil {
		return out
	}
	return body
}
