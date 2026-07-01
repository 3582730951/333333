package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// downstreamPolicy is the resolved policy for an incoming gateway request: which
// account group to route to, and any forced model / reasoning-effort override the
// caller's api key (or its group default) imposes regardless of what the client
// requested.
type downstreamPolicy struct {
	Group       string
	ForceModel  string
	ForceEffort string
	KeyLabel    string
	// KeyHash / UserID identify the matched downstream api key and its owning portal
	// user (both empty for an unauthenticated/legacy request). They are attributed onto
	// each usage_records row so a user's own console can show their usage.
	KeyHash string
	UserID  string
	Authed  bool
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
)

func withInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyInternal, true)
}

func isInternalCall(ctx context.Context) bool { v, _ := ctx.Value(ctxKeyInternal).(bool); return v }

func withDownstreamKey(ctx context.Context, pol downstreamPolicy) context.Context {
	if pol.KeyHash == "" && pol.UserID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyDownstream, downstreamIdent{pol.KeyHash, pol.UserID})
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

// resolveDownstreamPolicy authenticates the request's api key (if any) and
// resolves the effective routing group + forced model/effort, folding in the
// group-level defaults when the key sets none. When RequireDownstreamKey is on
// and the key is missing/invalid it writes a 401 and returns ok=false.
func (s *Server) resolveDownstreamPolicy(w http.ResponseWriter, r *http.Request) (downstreamPolicy, bool) {
	ctx := r.Context()
	// Relay-internal calls (the moderation model call) bypass downstream-key enforcement.
	if isInternalCall(ctx) {
		return downstreamPolicy{Group: s.cfg.DefaultGroup, Authed: true}, true
	}
	pol := downstreamPolicy{Group: s.cfg.DefaultGroup}
	plain := downstreamBearer(r)
	if plain != "" {
		if key, found, _ := s.store.LookupAPIKey(ctx, hashAPIKey(plain)); found {
			if !key.Enabled {
				if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
					writeError(w, http.StatusUnauthorized, errors.New("api key disabled"))
					return downstreamPolicy{}, false
				}
			} else {
				pol.Authed = true
				pol.KeyLabel = key.Label
				pol.KeyHash = key.KeyHash
				pol.UserID = key.UserID
				if strings.TrimSpace(key.GroupName) != "" {
					pol.Group = key.GroupName
				}
				pol.ForceModel = strings.TrimSpace(key.ForceModel)
				pol.ForceEffort = strings.TrimSpace(key.ForceEffort)
			}
		} else if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
			writeError(w, http.StatusUnauthorized, errors.New("unknown api key"))
			return downstreamPolicy{}, false
		}
	} else if s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey) {
		writeError(w, http.StatusUnauthorized, errors.New("api key required"))
		return downstreamPolicy{}, false
	}
	// Group-level fallback for any force value the key did not set.
	if pol.ForceModel == "" || pol.ForceEffort == "" {
		if g, err := s.store.GetGroup(ctx, pol.Group); err == nil {
			if pol.ForceModel == "" {
				pol.ForceModel = strings.TrimSpace(g.ForceModel)
			}
			if pol.ForceEffort == "" {
				pol.ForceEffort = strings.TrimSpace(g.ForceEffort)
			}
		}
	}
	return pol, true
}

// setForcedModel rewrites the top-level "model" field of a request body. It is
// only invoked on the override path, so the full unmarshal/marshal round-trip
// (which may reorder keys) is acceptable; the normal fast path leaves the raw
// body untouched.
func setForcedModel(raw []byte, model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" {
		return raw
	}
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	root["model"] = model
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return raw
}

// normalizeEffort canonicalizes a reasoning-effort string. Empty stays empty
// (no override). Recognized tiers: minimal, low, medium, high, xhigh.
func normalizeEffort(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	switch e {
	case "":
		return ""
	case "x-high", "extra-high", "extra_high", "veryhigh", "very-high", "very_high", "max", "maximum":
		return "xhigh"
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
	if effort == "" {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	reasoning, _ := root["reasoning"].(map[string]interface{})
	if reasoning == nil {
		reasoning = map[string]interface{}{}
	}
	reasoning["effort"] = effort
	root["reasoning"] = reasoning
	if out, err := json.Marshal(root); err == nil {
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
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return body
}
