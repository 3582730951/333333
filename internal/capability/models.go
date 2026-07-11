package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
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

var kiroStaticModels = []string{
	"claude-sonnet-5", "claude-sonnet-4.6", "claude-sonnet-4.5",
	"claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6", "claude-opus-4.5",
	"claude-haiku-4.5", "claude-fable-5",
}

var kiroConcreteModelRE = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-([0-9]+)(?:[.-]([0-9]+))?(?:-([0-9]{8}))?$`)

// KiroModelAlias reports whether model needs account-specific capability
// resolution. Aliases are deliberately not resolved from the static catalog: those
// rows are only a discovery hint and do not prove that a particular account can use
// the model.
func KiroModelAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto", "default", "opus", "claude-opus", "sonnet", "claude-sonnet", "haiku", "claude-haiku", "fable", "claude-fable":
		return true
	default:
		return false
	}
}

// KiroCanonicalModel canonicalizes only a concrete Claude model identifier. Dot
// and hyphen spellings, and an optional dated suffix, map to the same numeric
// version. A different or previously unknown version is never attracted to a
// nearby static model (for example 4.9 can never become 4.8).
func KiroCanonicalModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	match := kiroConcreteModelRE.FindStringSubmatch(m)
	if match == nil {
		return "", false
	}
	version := match[2]
	if match[3] != "" {
		version += "." + match[3]
	}
	return "claude-" + match[1] + "-" + version, true
}

// ResolveKiroModel resolves aliases against models that were verified for the
// selected account and endpoint. Concrete requests do not require a prior probe;
// they are sent without changing their version and become verified only after a
// successful upstream response.
func ResolveKiroModel(model string, verified []string) (string, bool) {
	if concrete, ok := KiroCanonicalModel(model); ok {
		return concrete, true
	}
	if !KiroModelAlias(model) {
		return "", false
	}
	wantedFamily := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
	if wantedFamily == "auto" || wantedFamily == "default" {
		wantedFamily = ""
	}
	best := ""
	bestScore := int64(-1)
	for _, candidate := range verified {
		canonical, ok := KiroCanonicalModel(candidate)
		if !ok {
			continue
		}
		family, score := kiroModelRank(canonical)
		if wantedFamily != "" && family != wantedFamily {
			continue
		}
		if score > bestScore || (score == bestScore && canonical > best) {
			best, bestScore = canonical, score
		}
	}
	return best, best != ""
}

func kiroModelRank(model string) (string, int64) {
	match := kiroConcreteModelRE.FindStringSubmatch(model)
	if match == nil {
		return "", -1
	}
	familyRank := map[string]int64{"haiku": 1, "fable": 2, "sonnet": 3, "opus": 4}[match[1]]
	major, _ := strconv.ParseInt(match[2], 10, 64)
	minor, _ := strconv.ParseInt(match[3], 10, 64)
	return match[1], familyRank*1_000_000_000 + major*1_000_000 + minor*1_000
}

// KiroSupportsAdaptiveThinking is intentionally explicit. Unknown future models
// do not inherit reasoning support merely because their name contains Claude.
func KiroSupportsAdaptiveThinking(model string) bool {
	canonical, ok := KiroCanonicalModel(model)
	if !ok {
		return false
	}
	for _, prefix := range []string{"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4.6", "claude-sonnet-5"} {
		if canonical == prefix {
			return true
		}
	}
	return false
}

func StaticKiroModels(accountID string) []storage.ModelCapability {
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(kiroStaticModels))
	for _, slug := range kiroStaticModels {
		window := int64(1000000)
		if strings.Contains(slug, "4.5") || strings.Contains(slug, "haiku") {
			window = 200000
		}
		out = append(out, storage.ModelCapability{AccountID: accountID, ModelSlug: slug, NativeContextWindow: window,
			NativeMaxContextWindow: window, EffectiveContextWindowPercent: 100, Source: "kiro_static_unknown", LastProbeAt: now})
	}
	return out
}

// claudeWindow returns the native context window for a Claude model id, falling
// back to the standard 200k window every current Claude model supports.
func claudeWindow(slug string) int64 {
	if w, ok := claudeModelWindows[slug]; ok {
		return w
	}
	return 200000
}

// NormalizeClaudeModelAlias maps a Claude model alias that Anthropic's API would reject
// — bare "sonnet"/"opus"/"haiku", "auto"/"default", or a family name without a version —
// to a concrete current-generation model id of the SAME tier. It never crosses to a
// lower tier, so model quality is never downgraded; "auto"/"default" resolve to the
// strongest available model (opus). A value that is already a concrete claude-* id, or
// any non-Claude model, is returned unchanged. This is what makes Claude Code's default/
// auto model selection work against the pool without silently weakening the model.
func NormalizeClaudeModelAlias(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto", "default", "opus", "claude-opus":
		if v := newestStaticClaudeByTier("opus"); v != "" {
			return v
		}
	case "sonnet", "claude-sonnet":
		if v := newestStaticClaudeByTier("sonnet"); v != "" {
			return v
		}
	case "haiku", "claude-haiku":
		if v := newestStaticClaudeByTier("haiku"); v != "" {
			return v
		}
	}
	return model
}

// newestStaticClaudeByTier returns the most-capable current-generation Claude model id
// of a tier ("opus"/"sonnet"/"haiku"), or "" if none is known. claudeStaticModels is
// ordered most-capable first, so the first tier match is the newest/strongest.
func newestStaticClaudeByTier(tier string) string {
	for _, slug := range claudeStaticModels {
		if strings.Contains(slug, tier) {
			return slug
		}
	}
	return ""
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
	minimumClientVersion  string
	requiresCurrentClient bool
	preferWebSocket       bool
	responsesLite         bool
	reasoningLevels       []string
}

// codexStaticModels is the current-generation Codex model catalog advertised when
// the live ChatGPT /models probe is unavailable (network/auth failure). Mirrored
// from the official Codex CLI bundled model catalog; only client-visible models are
// listed (the hidden codex-auto-review preset is omitted). Ordered most-capable
// first. The live probe — when it works — is authoritative and supersedes this.
var codexStaticModels = []codexStaticModel{
	{slug: "gpt-5.6-sol", window: 372000, maxWindow: 372000, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{slug: "gpt-5.6-terra", window: 372000, maxWindow: 372000, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{slug: "gpt-5.6-luna", window: 372000, maxWindow: 372000, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}},
	{slug: "gpt-5.5", window: 272000, maxWindow: 272000, minimumClientVersion: "0.124.0", requiresCurrentClient: true, preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.4", window: 272000, maxWindow: 1000000, minimumClientVersion: "0.98.0", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.4-mini", window: 272000, maxWindow: 272000, minimumClientVersion: "0.98.0", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.2", window: 272000, maxWindow: 272000, minimumClientVersion: "0.0.1", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
}

func codexStaticModelForSlug(slug string) (codexStaticModel, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, m := range codexStaticModels {
		if strings.ToLower(m.slug) == slug {
			return m, true
		}
	}
	return codexStaticModel{}, false
}

func CodexRequiresCurrentClientVersion(slug string) bool {
	m, ok := codexStaticModelForSlug(slug)
	return ok && m.requiresCurrentClient
}

// CodexMinimumClientVersion returns the minimum official CLI version declared by
// the bundled Codex model catalog. An empty result means the model is not in the
// curated fallback catalog.
func CodexMinimumClientVersion(slug string) string {
	m, ok := codexStaticModelForSlug(slug)
	if !ok {
		return ""
	}
	return m.minimumClientVersion
}

// CodexPrefersWebSocket mirrors model_info.prefer_websockets from the official
// Codex catalog. It is deliberately independent of client-version gating: current
// Codex models can prefer the Responses WebSocket transport without requiring the
// newest possible CLI version.
func CodexPrefersWebSocket(slug string) bool {
	m, ok := codexStaticModelForSlug(slug)
	return ok && m.preferWebSocket
}

// CodexUsesResponsesLite mirrors model_info.use_responses_lite. The upstream
// transport uses it to attach the official HTTP header / WS client metadata.
func CodexUsesResponsesLite(slug string) bool {
	m, ok := codexStaticModelForSlug(slug)
	return ok && m.responsesLite
}

// CodexSupportsReasoningEffort reports the exact reasoning-level matrix from the
// official bundled catalog. Unknown models return false so callers never invent a
// capability; the relay itself still preserves downstream reasoning verbatim.
func CodexSupportsReasoningEffort(slug, effort string) bool {
	m, ok := codexStaticModelForSlug(slug)
	if !ok {
		return false
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, supported := range m.reasoningLevels {
		if supported == effort {
			return true
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
			EffectiveContextWindowPercent: 95,
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
			// Codex ModelInfo's serde default is 95, reserving headroom for the
			// model/tool prefix even when an older /models response omits the field.
			percent = 95
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
	case strings.HasPrefix(s, "kiro"):
		return "kiro"
	default:
		return "codex"
	}
}

func BuildModelsResponse(capabilities []storage.ModelCapability, cfg config.Config) ([]byte, string, error) {
	_ = cfg // kept for API compatibility; model responses now advertise only real native windows.
	// A freshly imported Codex account has a short interval before its first live
	// capability probe. Serve the same curated Codex floor used when that probe is
	// unavailable instead of leaking the obsolete gpt-5.4-codex/unknown placeholder.
	// Once stored probe data exists it remains authoritative and replaces this
	// cold-start catalog, including any larger context window advertised upstream.
	if len(capabilities) == 0 {
		capabilities = StaticCodexModels("codex-static-cold-start")
	}
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
	resp := map[string]interface{}{"object": "list", "data": data}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, `W/"` + hex.EncodeToString(sum[:])[:24] + `"`, nil
}

// BuildAnthropicModelsResponse renders the pool's Claude capabilities in Anthropic's
// native GET /v1/models schema — {data:[{type:"model",id,display_name,created_at}],
// has_more:false,first_id,last_id}. Anthropic clients (Claude Code) enumerate models
// from THIS shape and disable their model picker / "auto" selection when handed the
// OpenAI-shaped list BuildModelsResponse returns. Only claude-* models are listed; the
// curated current-generation set is unioned as a floor so a freshly shipped model (or an
// OAuth token whose /models endpoint is rejected) still surfaces.
func BuildAnthropicModelsResponse(capabilities []storage.ModelCapability) ([]byte, string, error) {
	claudeCaps := make([]storage.ModelCapability, 0, len(capabilities))
	kiroCaps := make([]storage.ModelCapability, 0, len(capabilities))
	for _, c := range capabilities {
		switch providerKeyForSource(c.Source) {
		case "claude":
			claudeCaps = append(claudeCaps, c)
		case "kiro":
			kiroCaps = append(kiroCaps, c)
		}
	}
	// Richest single Claude account's set, floored by the static current-gen list so the
	// full sonnet/haiku/opus family Claude Code's auto mode expects is always present.
	merged := append([]storage.ModelCapability(nil), richestAccountCaps(kiroCaps)...)
	if len(claudeCaps) > 0 {
		merged = append(merged, MergeClaudeStatic("", richestAccountCaps(claudeCaps))...)
	}
	seen := make(map[string]bool, len(merged))
	data := make([]interface{}, 0, len(merged))
	appendModel := func(slug string) {
		if slug == "" || seen[slug] {
			return
		}
		seen[slug] = true
		data = append(data, map[string]interface{}{
			"type":         "model",
			"id":           slug,
			"display_name": claudeDisplayName(slug),
			"created_at":   claudeCreatedAt(slug),
		})
	}
	for _, c := range merged {
		appendModel(c.ModelSlug)
	}
	if len(data) == 0 && len(kiroCaps) == 0 {
		for _, slug := range claudeStaticModels {
			appendModel(slug)
		}
	}
	resp := map[string]interface{}{"data": data, "has_more": false}
	if len(data) > 0 {
		resp["first_id"] = data[0].(map[string]interface{})["id"]
		resp["last_id"] = data[len(data)-1].(map[string]interface{})["id"]
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, `W/"` + hex.EncodeToString(sum[:])[:24] + `"`, nil
}

// claudeDisplayName derives a human label for a Claude model id (Claude Code shows it in
// the model picker). It strips a trailing -YYYYMMDD date and title-cases the remaining
// tokens, so "claude-sonnet-4-5-20250929" → "Claude Sonnet 4 5".
func claudeDisplayName(slug string) string {
	s := slug
	if d := trailingDate(s); d != "" {
		s = s[:len(s)-len(d)-1] // drop "-YYYYMMDD"
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	label := strings.Join(parts, " ")
	if label == "" {
		return slug
	}
	return label
}

// claudeCreatedAt returns an ISO-8601 created_at for a Claude model id, derived from a
// trailing -YYYYMMDD date when present, else a stable default. Deterministic so the
// /v1/models ETag stays stable across requests.
func claudeCreatedAt(slug string) string {
	if d := trailingDate(slug); d != "" {
		return d[0:4] + "-" + d[4:6] + "-" + d[6:8] + "T00:00:00Z"
	}
	return "2025-01-01T00:00:00Z"
}

// trailingDate returns the trailing 8-digit YYYYMMDD token of a "-YYYYMMDD"-suffixed
// slug, or "" when absent.
func trailingDate(slug string) string {
	if len(slug) < 9 || slug[len(slug)-9] != '-' {
		return ""
	}
	tail := slug[len(slug)-8:]
	for i := 0; i < 8; i++ {
		if tail[i] < '0' || tail[i] > '9' {
			return ""
		}
	}
	return tail
}

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
