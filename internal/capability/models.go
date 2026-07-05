package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// claudeModelWindows maps known Claude model ids to their native context window.
// Anthropic's GET /v1/models does not return context windows, so this table
// (mirrored from the reference relay's static model registry) both enriches a
// successful live probe and seeds the static fallback. Unknown ids default to
// the standard 200k Claude window in claudeWindow.
var claudeModelWindows = map[string]int64{
	"claude-haiku-4-5-20251001":  200000,
	"claude-sonnet-4-5-20250929": 200000,
	"claude-sonnet-4-6":          200000,
	"claude-opus-4-8":            1000000,
	"claude-opus-4-6":            1000000,
	"claude-opus-4-7":            1000000,
	"claude-opus-4-5-20251101":   200000,
	"claude-opus-4-1-20250805":   200000,
	"claude-opus-4-20250514":     200000,
	"claude-sonnet-4-20250514":   200000,
}

// claudeStaticModels is the current-generation Claude model set advertised when
// the per-account /v1/models probe is unavailable (e.g. an OAuth token that the
// models-listing endpoint rejects). It also acts as a FLOOR unioned onto a live
// probe (see MergeClaudeStatic) so the newest models surface even when Anthropic's
// /v1/models lags behind a freshly shipped release. Curated and ordered
// most-capable first; claude-opus-4-8 leads (1M context window by default).
var claudeStaticModels = []string{
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5-20250929",
	"claude-opus-4-5-20251101",
	"claude-haiku-4-5-20251001",
}

// claudeWindow returns the native context window for a Claude model id, falling
// back to the standard 200k window every current Claude model supports.
func claudeWindow(slug string) int64 {
	if w, ok := claudeModelWindows[slug]; ok {
		return w
	}
	return 200000
}

// ParseClaudeModels parses Anthropic's GET /v1/models response (data[].id) into
// capabilities, filling the context window from claudeModelWindows since the
// endpoint omits it. Source is tagged "claude_probe" to distinguish a live probe
// from the static fallback.
func ParseClaudeModels(accountID string, raw []byte, etag string) ([]storage.ModelCapability, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	models := extractModels(root)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(models))
	for _, model := range models {
		slug := firstString(model, "id", "slug", "model_slug", "name")
		if slug == "" {
			continue
		}
		window := firstInt(model, "context_window", "context_length", "max_context_tokens", "native_context_window")
		if window == 0 {
			window = claudeWindow(slug)
		}
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           window,
			NativeMaxContextWindow:        window,
			EffectiveContextWindowPercent: 100,
			Visibility:                    firstString(model, "visibility"),
			ETag:                          etag,
			RawModelJSONHash:              hash,
			Source:                        "claude_probe",
			LastProbeAt:                   now,
		})
	}
	return out, nil
}

// StaticClaudeModels returns the curated current-generation Claude capability set
// for an account, used when the live /v1/models probe is unavailable. Source is
// tagged "claude_static" so an operator can tell it apart from a live probe.
func StaticClaudeModels(accountID string) []storage.ModelCapability {
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(claudeStaticModels))
	for _, slug := range claudeStaticModels {
		window := claudeWindow(slug)
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           window,
			NativeMaxContextWindow:        window,
			EffectiveContextWindowPercent: 100,
			Source:                        "claude_static",
			LastProbeAt:                   now,
		})
	}
	return out
}

// MergeClaudeStatic unions the curated current-generation static set onto a live
// /v1/models probe result, treating the static list as a FLOOR. A live probe is
// authoritative for what an account exposes, but Anthropic's /v1/models both omits
// context windows and can lag a freshly shipped model by days (claude-opus-4-8 is
// the immediate example), while an OAuth token may be rejected by the endpoint
// entirely. Without a floor, a probe that "succeeds" but is missing the latest
// model would hide it. probeCaps win on conflict (their slug already present is
// kept as-is); any static slug the probe did not return is appended so the newest
// known models always appear. Order is preserved (probe first, then new static).
func MergeClaudeStatic(accountID string, probeCaps []storage.ModelCapability) []storage.ModelCapability {
	have := make(map[string]struct{}, len(probeCaps))
	out := make([]storage.ModelCapability, 0, len(probeCaps)+len(claudeStaticModels))
	for _, c := range probeCaps {
		have[c.ModelSlug] = struct{}{}
		out = append(out, c)
	}
	for _, c := range StaticClaudeModels(accountID) {
		if _, ok := have[c.ModelSlug]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// codexStaticModel describes one current-generation Codex (ChatGPT) model for the
// static fallback catalog. Mirrors the fields the live ChatGPT /models probe would
// return for the model.
type codexStaticModel struct {
	slug                  string
	window                int64
	maxWindow             int64
	requiresCurrentClient bool
}

// codexStaticModels is the current-generation Codex model catalog advertised when
// the live ChatGPT /models probe is unavailable (network/auth failure). Mirrored
// from the official Codex CLI bundled model catalog; only client-visible models are
// listed (the hidden codex-auto-review preset is omitted). Ordered most-capable
// first. The live probe — when it works — is authoritative and supersedes this.
var codexStaticModels = []codexStaticModel{
	{slug: "gpt-5.5", window: 272000, maxWindow: 272000, requiresCurrentClient: true},
	{slug: "gpt-5.4", window: 272000, maxWindow: 1000000},
	{slug: "gpt-5.4-mini", window: 272000, maxWindow: 272000},
	{slug: "gpt-5.3-codex", window: 272000, maxWindow: 272000},
	{slug: "gpt-5.2", window: 272000, maxWindow: 272000},
}

func CodexRequiresCurrentClientVersion(slug string) bool {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, m := range codexStaticModels {
		if strings.ToLower(m.slug) == slug {
			return m.requiresCurrentClient
		}
	}
	return false
}

// StaticCodexModels returns the curated current-generation Codex capability set for
// an account, used as a fallback when the live ChatGPT /models probe fails. Source
// is tagged "codex_static" so an operator can distinguish it from a live probe.
func StaticCodexModels(accountID string) []storage.ModelCapability {
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(codexStaticModels))
	for _, m := range codexStaticModels {
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     m.slug,
			NativeContextWindow:           m.window,
			NativeMaxContextWindow:        m.maxWindow,
			EffectiveContextWindowPercent: 100,
			Visibility:                    "list",
			Source:                        "codex_static",
			LastProbeAt:                   now,
		})
	}
	return out
}

// MergeCodexStatic adds only the static Codex models that are NEWER than the
// newest known static model returned by a live probe. ChatGPT /models is version-
// gated and can legitimately return "up to gpt-5.4" when the probe version/scope
// is stale; adding the static prefix surfaces the current flagship without turning
// every successful probe into the full fallback catalog. Probe entries still win on
// conflicts, and older static entries are not re-added so a re-probe can evict stale
// models the account no longer exposes.
func MergeCodexStatic(accountID string, probeCaps []storage.ModelCapability) []storage.ModelCapability {
	if len(probeCaps) == 0 {
		return StaticCodexModels(accountID)
	}
	staticIndex := make(map[string]int, len(codexStaticModels))
	for i, m := range codexStaticModels {
		staticIndex[m.slug] = i
	}
	have := make(map[string]struct{}, len(probeCaps))
	firstKnown := len(codexStaticModels)
	for _, c := range probeCaps {
		have[c.ModelSlug] = struct{}{}
		if idx, ok := staticIndex[c.ModelSlug]; ok && idx < firstKnown {
			firstKnown = idx
		}
	}
	out := make([]storage.ModelCapability, 0, len(probeCaps)+firstKnown)
	for _, c := range probeCaps {
		out = append(out, c)
	}
	if firstKnown == len(codexStaticModels) {
		return out
	}
	for _, c := range StaticCodexModels(accountID)[:firstKnown] {
		if _, ok := have[c.ModelSlug]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

func Parse(accountID string, raw []byte, etag string) ([]storage.ModelCapability, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	models := extractModels(root)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(models))
	for _, model := range models {
		slug := firstString(model, "id", "slug", "model_slug", "name")
		if slug == "" {
			continue
		}
		visibility := firstString(model, "visibility")
		// The ChatGPT /models backend returns internal, non-selectable presets
		// (e.g. codex-auto-review, visibility "hide") alongside the user-facing
		// ones. The real Codex CLI never shows these in its picker, so the relay
		// must not advertise them either — skip anything not explicitly listable.
		if isHiddenVisibility(visibility) {
			continue
		}
		native := firstInt(model, "context_window", "context_length", "max_context_tokens", "native_context_window")
		maxNative := firstInt(model, "max_context_window", "native_max_context_window", "max_context_tokens", "context_window")
		if maxNative == 0 {
			maxNative = native
		}
		percent := firstInt(model, "effective_context_window_percent")
		if percent == 0 {
			percent = 100
		}
		autoCompact := firstInt(model, "auto_compact_token_limit", "auto_compact_context_window", "compact_token_limit")
		rawModel := ""
		if b, err := json.Marshal(model); err == nil {
			rawModel = string(b)
		}
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           native,
			NativeMaxContextWindow:        maxNative,
			EffectiveContextWindowPercent: percent,
			AutoCompactTokenLimit:         autoCompact,
			Visibility:                    visibility,
			ETag:                          etag,
			RawModelJSONHash:              hash,
			RawModelJSON:                  rawModel,
			Source:                        "probe",
			LastProbeAt:                   now,
		})
	}
	return out, nil
}

// isHiddenVisibility reports whether a model's `visibility` marks it as a hidden /
// internal preset that the official Codex CLI omits from its model picker (e.g.
// codex-auto-review is "hide"). Empty visibility is treated as visible — many
// backends omit the field for normal models.
func isHiddenVisibility(visibility string) bool {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "hide", "hidden", "none", "internal", "deprecated":
		return true
	}
	return false
}

// richestAccountCaps narrows a pool-wide capability list down to the single
// account that supports the MOST distinct models. This is what the user wants
// advertised on /v1/models: not the union across the whole pool (which could
// promise a model that only one odd account has), but the coherent model set of
// the richest real account. Ties break toward the larger total native window.
// The input order is preserved for determinism (capabilities arrive sorted by
// account_id from storage.ListCapabilities).
func richestAccountCaps(capabilities []storage.ModelCapability) []storage.ModelCapability {
	if len(capabilities) == 0 {
		return capabilities
	}
	type acct struct {
		caps   []storage.ModelCapability
		slugs  map[string]struct{}
		window int64
	}
	byAccount := map[string]*acct{}
	order := make([]string, 0)
	for _, c := range capabilities {
		a := byAccount[c.AccountID]
		if a == nil {
			a = &acct{slugs: map[string]struct{}{}}
			byAccount[c.AccountID] = a
			order = append(order, c.AccountID)
		}
		a.caps = append(a.caps, c)
		a.slugs[c.ModelSlug] = struct{}{}
		a.window += c.NativeMaxContextWindow
	}
	var best *acct
	for _, id := range order {
		a := byAccount[id]
		if best == nil || len(a.slugs) > len(best.slugs) ||
			(len(a.slugs) == len(best.slugs) && a.window > best.window) {
			best = a
		}
	}
	return best.caps
}

// providerKeyForSource maps a capability's Source tag to a provider bucket key:
// "claude" for the Anthropic sets, "custom:<id>" for a custom OpenAI-compatible
// provider (tagged "custom:<id>" by the probe), and "codex" otherwise.
func providerKeyForSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch {
	case strings.HasPrefix(s, "custom:"):
		return source
	case strings.HasPrefix(s, "claude"):
		return "claude"
	default:
		return "codex"
	}
}

func BuildModelsResponse(capabilities []storage.ModelCapability, cfg config.Config) ([]byte, string, error) {
	_ = cfg // kept for API compatibility; model responses now advertise only real native windows.
	// Group by provider so a mixed pool advertises EVERY provider's models. The old
	// single pool-wide "richest account" pick hid all but one provider's models when
	// codex/claude/custom accounts coexisted. Within each provider we still advertise
	// the model set of the account that supports the most models (the pool's
	// "按账号池内支持最多的模型的账号展示" policy).
	buckets := map[string][]storage.ModelCapability{}
	order := make([]string, 0)
	for _, c := range capabilities {
		key := providerKeyForSource(c.Source)
		if _, ok := buckets[key]; !ok {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], c)
	}
	best := map[string]storage.ModelCapability{}
	customSlug := map[string]bool{}
	for _, key := range order {
		isCustom := strings.HasPrefix(key, "custom:")
		for _, cap := range richestAccountCaps(buckets[key]) {
			if current, ok := best[cap.ModelSlug]; !ok || cap.NativeMaxContextWindow > current.NativeMaxContextWindow {
				best[cap.ModelSlug] = cap
			}
			if isCustom {
				customSlug[cap.ModelSlug] = true
			}
		}
	}
	keys := make([]string, 0, len(best))
	for key := range best {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	data := make([]interface{}, 0, len(best)+1)
	for _, key := range keys {
		cap := best[key]
		window := cap.NativeMaxContextWindow
		if window == 0 {
			window = cap.NativeContextWindow
		}
		native := cap.NativeMaxContextWindow
		if native == 0 {
			native = cap.NativeContextWindow
		}
		item := rawOfficialModelItem(cap)
		item["id"] = cap.ModelSlug
		item["object"] = "model"
		item["owned_by"] = "codex-pool"
		item["context_window"] = window
		item["native_context_window"] = native
		item["window_mode"] = "native"
		item["visibility"] = cap.Visibility
		if cap.NativeContextWindow > 0 {
			item["native_base_context_window"] = cap.NativeContextWindow
		}
		if cap.NativeMaxContextWindow > 0 {
			item["native_max_context_window"] = cap.NativeMaxContextWindow
		}
		if cap.AutoCompactTokenLimit > 0 {
			item["auto_compact_token_limit"] = cap.AutoCompactTokenLimit
		}
		if cap.EffectiveContextWindowPercent > 0 {
			item["effective_context_window_percent"] = cap.EffectiveContextWindowPercent
		}
		if customSlug[cap.ModelSlug] {
			item["provider_window_mode"] = "custom_native"
		}
		data = append(data, item)
	}
	if len(data) == 0 {
		data = append(data, map[string]interface{}{
			"id":                    "gpt-5.4-codex",
			"object":                "model",
			"owned_by":              "codex-pool",
			"context_window":        0,
			"native_context_window": 0,
			"window_mode":           "unknown",
		})
	}
	resp := map[string]interface{}{"object": "list", "data": data}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, `W/"` + hex.EncodeToString(sum[:])[:24] + `"`, nil
}

// rawOfficialModelItem returns the probed model JSON for official Codex accounts,
// if present. Starting from the raw object preserves forward-compatible capability
// metadata (future tools/features/capabilities) that the gateway does not yet
// understand; BuildModelsResponse then overlays only the relay-required fields.
func rawOfficialModelItem(cap storage.ModelCapability) map[string]interface{} {
	if providerKeyForSource(cap.Source) != "codex" || strings.TrimSpace(cap.RawModelJSON) == "" {
		return map[string]interface{}{}
	}
	var item map[string]interface{}
	if err := json.Unmarshal([]byte(cap.RawModelJSON), &item); err != nil || item == nil {
		return map[string]interface{}{}
	}
	return item
}

func ProbePath(clientVersion string) string {
	if clientVersion == "" {
		return "/v1/models"
	}
	return "/v1/models?client_version=" + urlQueryEscape(clientVersion)
}

func ETagFromHeader(h http.Header) string {
	return h.Get("ETag")
}

func extractModels(root interface{}) []map[string]interface{} {
	switch t := root.(type) {
	case map[string]interface{}:
		for _, key := range []string{"data", "models", "items"} {
			if arr, ok := t[key].([]interface{}); ok {
				return maps(arr)
			}
		}
		if modelMap, ok := t["models"].(map[string]interface{}); ok {
			out := make([]map[string]interface{}, 0, len(modelMap))
			for slug, item := range modelMap {
				if m, ok := item.(map[string]interface{}); ok {
					if _, exists := m["id"]; !exists {
						m["id"] = slug
					}
					out = append(out, m)
				}
			}
			return out
		}
	case []interface{}:
		return maps(t)
	}
	return nil
}

func maps(arr []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func firstInt(m map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case int:
				return int64(t)
			case string:
				parsed, _ := strconv.ParseInt(t, 10, 64)
				return parsed
			}
		}
	}
	return 0
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}
