package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/plantier"
	"codex-account-pool/internal/storage"
)

const (
	AvailabilityVerified    = "verified"
	AvailabilityUnverified  = "unverified"
	AvailabilityUnsupported = "unsupported"

	Context1MSupported   = "supported"
	Context1MUnsupported = "unsupported"
	Context1MUnknown     = "unknown"

	// GPT-5.6 has one client-visible contract across the pool. The effective
	// percentage is 100 so Codex's resolved hard window is exactly 372K; automatic
	// compaction starts at 90%, leaving 37.2K tokens of headroom.
	GPT56ContextWindow         = int64(372000)
	GPT56AutoCompactTokenLimit = int64(334800)
	GPT56EffectivePercent      = int64(100)
)

// RequestedClaudeModel separates the downstream model spelling from the model
// sent upstream. [1m] is a context request and never participates in version or
// tier selection.
type RequestedClaudeModel struct {
	RequestedModel string
	BaseModel      string
	ContextMode    string
}

var claudeContextSuffixRE = regexp.MustCompile(`(?i)^(.*)\[([^\[\]]+)\]$`)

func ParseRequestedClaudeModel(model string) (RequestedClaudeModel, error) {
	requested := strings.TrimSpace(model)
	parsed := RequestedClaudeModel{RequestedModel: requested, BaseModel: requested}
	if !strings.ContainsAny(requested, "[]") {
		parsed.BaseModel = normalizeRequestedClaudeBase(parsed.BaseModel)
		return parsed, nil
	}
	match := claudeContextSuffixRE.FindStringSubmatch(requested)
	if match == nil || strings.TrimSpace(match[1]) == "" {
		return RequestedClaudeModel{}, fmt.Errorf("invalid Claude model context suffix in %q", requested)
	}
	if !strings.EqualFold(strings.TrimSpace(match[2]), "1m") {
		return RequestedClaudeModel{}, fmt.Errorf("unsupported Claude model context suffix [%s] in %q; supported suffix: [1m]", strings.TrimSpace(match[2]), requested)
	}
	parsed.BaseModel = normalizeRequestedClaudeBase(match[1])
	parsed.ContextMode = "1m"
	return parsed, nil
}

func normalizeRequestedClaudeBase(model string) string {
	trimmed := strings.TrimSpace(model)
	match := kiroConcreteModelRE.FindStringSubmatch(strings.ToLower(trimmed))
	if match == nil {
		return trimmed
	}
	base := "claude-" + match[1] + "-" + match[2]
	if match[3] != "" {
		base += "." + match[3]
	}
	if match[4] != "" {
		base += "-" + match[4]
	}
	return base
}

// KiroEffectiveContextWindow applies the requested context mode to the exact
// selected model/account. Normal requests never acquire a synthetic 1M window.
func KiroEffectiveContextWindow(model, contextMode string, measured int64) int64 {
	limit := KiroContextWindow(model)
	requestLimit := int64(200000)
	// GPT-5.6 is a native Kiro model family with a 372K standard context
	// window. This is not the paid Claude-only 1M extension, so a normal GPT
	// request must retain the model's documented window rather than being
	// artificially reduced to the generic 200K default.
	if KiroSupportsGPTModel(model) {
		requestLimit = limit
	}
	if strings.EqualFold(strings.TrimSpace(contextMode), "1m") {
		requestLimit = 1000000
	}
	if requestLimit < limit {
		limit = requestLimit
	}
	if measured > 0 && measured < limit {
		limit = measured
	}
	return limit
}

// claudeModelWindows records the model's technical maximum. It is deliberately
// separate from NativeContextWindow: older extended-context models may still
// require account evidence, while generation-5 Fable/Opus/Sonnet have 1M as both
// the default and maximum window.
var claudeModelWindows = map[string]int64{
	"claude-fable-5":             1000000,
	"claude-opus-5":              1000000,
	"claude-sonnet-5":            1000000,
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

// claudeStaticModels is a discovery hint for credentials that cannot enumerate
// models (notably some OAuth tokens). Static rows are never advertised as verified
// and are never unioned into a successful live model response.
var claudeStaticModels = []string{
	"claude-fable-5",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5-20250929",
	"claude-opus-4-5-20251101",
	"claude-haiku-4-5-20251001",
}

var claudeProbeModels = append(append([]string(nil), claudeStaticModels...),
	// Pre-4.6 Claude API aliases are accepted by many relay stations even when
	// their catalog exposes only the pinned dated IDs.
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"claude-haiku-4-5",
)

// ClaudeProbeModelTable returns the maintained candidate table used by
// Anthropic-compatible third-party relays that do not implement GET /v1/models.
// Callers must verify candidates against that relay before advertising them as
// authoritative; this table is a discovery input, not an entitlement claim.
func ClaudeProbeModelTable() []string {
	return append([]string(nil), claudeProbeModels...)
}

// IsClaudeProbeModel reports whether a model is one of the maintained
// third-party discovery candidates.
func IsClaudeProbeModel(model string) bool {
	model = strings.TrimSpace(model)
	for _, candidate := range claudeProbeModels {
		if strings.EqualFold(candidate, model) {
			return true
		}
	}
	return false
}

var kiroStaticModels = []string{
	"claude-opus-5",
	"claude-sonnet-5", "claude-sonnet-4.6", "claude-sonnet-4.5",
	"claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6", "claude-opus-4.5",
	"claude-haiku-4.5",
	// Kiro also exposes this GPT-5.6 family through generateAssistantResponse.
	// Keep this list intentionally exact: an unknown GPT slug must not be routed
	// to Kiro merely because it shares a prefix with a Codex model.
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
}

var kiroConcreteModelRE = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-([0-9]+)(?:[.-]([0-9]+))?(?:-([0-9]{8}))?$`)
var kiroGPTModelRE = regexp.MustCompile(`^gpt-5\.6-(sol|terra|luna)$`)

func isGPT56Model(model string) bool {
	canonical, ok := KiroCanonicalModel(NormalizeCodexModelAlias(model))
	return ok && strings.HasPrefix(canonical, "gpt-5.6-")
}

// ApplyGPT56ContextContract normalizes Codex/ChatGPT discovery metadata before it
// is persisted or rendered. It must not be applied to Kiro live-catalog rows:
// Kiro is a different transport and its account-scoped limit is authoritative.
func ApplyGPT56ContextContract(cap storage.ModelCapability) storage.ModelCapability {
	if !isGPT56Model(cap.ModelSlug) {
		return cap
	}
	cap.NativeContextWindow = GPT56ContextWindow
	cap.NativeMaxContextWindow = GPT56ContextWindow
	cap.EffectiveContextWindowPercent = GPT56EffectivePercent
	cap.AutoCompactTokenLimit = GPT56AutoCompactTokenLimit
	return cap
}

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

// KiroCanonicalModel canonicalizes concrete Kiro model identifiers. Claude dot
// and hyphen spellings, an optional dated suffix, and the explicit CLI
// "-thinking" presentation alias map to the same numeric version. Matching stays
// anchored, so Opus 5 can never attract Opus 4.5 by substring. The GPT family is
// deliberately exact: only the GPT-5.6 ids exposed by Kiro are accepted.
func KiroCanonicalModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if match := kiroGPTModelRE.FindStringSubmatch(m); match != nil {
		return "gpt-5.6-" + match[1], true
	}
	m = strings.TrimSuffix(m, "-thinking")
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

// KiroSupportsGPTModel reports whether a concrete GPT request can be served by
// Kiro's generateAssistantResponse backend. It is used only for auto spillover;
// explicit Codex traffic remains on Codex.
func KiroSupportsGPTModel(model string) bool {
	canonical, ok := KiroCanonicalModel(model)
	return ok && strings.HasPrefix(canonical, "gpt-")
}

// ResolveKiroModel resolves aliases against models that were verified for the
// selected account and endpoint. Concrete requests do not require a prior probe;
// they are sent without changing their version and become verified only after a
// successful upstream response.
func ResolveKiroModel(model string, verified []string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "auto" {
		return "auto", true
	}
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

// ResolveKiroCatalogModel resolves a downstream model against one complete,
// account-scoped live Kiro catalog. It returns the exact upstream ID rather than
// synthesizing an ID from a family name. "auto" is an independent upstream model;
// "default" selects only the catalog-designated default.
func ResolveKiroCatalogModel(model string, catalog []storage.KiroModelDescriptor) (storage.KiroModelDescriptor, bool) {
	requested := strings.ToLower(strings.TrimSpace(model))
	if requested == "" {
		return storage.KiroModelDescriptor{}, false
	}
	if requested == "default" {
		for _, candidate := range catalog {
			if candidate.Complete && candidate.Default {
				return candidate, true
			}
		}
		return storage.KiroModelDescriptor{}, false
	}
	if requested == "auto" {
		for _, candidate := range catalog {
			if !candidate.Complete {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(candidate.PublicID), "auto") ||
				strings.EqualFold(strings.TrimSpace(candidate.UpstreamID), "auto") ||
				stringSliceContainsFold(candidate.Aliases, "auto") {
				return candidate, true
			}
		}
		return storage.KiroModelDescriptor{}, false
	}
	if canonical, ok := KiroCanonicalModel(model); ok {
		for _, candidate := range catalog {
			if candidate.Complete && kiroDescriptorMatches(candidate, canonical) {
				return candidate, true
			}
		}
		return storage.KiroModelDescriptor{}, false
	}
	if !KiroModelAlias(model) {
		return storage.KiroModelDescriptor{}, false
	}
	family := strings.TrimPrefix(requested, "claude-")
	bestIndex, bestScore := -1, int64(-1)
	for index, candidate := range catalog {
		if !candidate.Complete {
			continue
		}
		canonical := ""
		for _, identifier := range append([]string{candidate.PublicID, candidate.UpstreamID}, candidate.Aliases...) {
			if parsed, ok := KiroCanonicalModel(identifier); ok {
				canonical = parsed
				break
			}
		}
		candidateFamily, score := kiroModelRank(canonical)
		if candidateFamily != family {
			continue
		}
		if score > bestScore || (score == bestScore && candidate.UpstreamID > catalog[bestIndex].UpstreamID) {
			bestIndex, bestScore = index, score
		}
	}
	if bestIndex < 0 {
		return storage.KiroModelDescriptor{}, false
	}
	return catalog[bestIndex], true
}

func kiroDescriptorMatches(candidate storage.KiroModelDescriptor, canonical string) bool {
	for _, identifier := range append([]string{candidate.PublicID, candidate.UpstreamID}, candidate.Aliases...) {
		if parsed, ok := KiroCanonicalModel(identifier); ok && parsed == canonical {
			return true
		}
	}
	return false
}

func stringSliceContainsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
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
	for _, prefix := range []string{"claude-opus-5", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4.6", "claude-sonnet-5"} {
		if canonical == prefix {
			return true
		}
	}
	return false
}

// KiroContextWindow returns the conservative context window used by the Kiro
// request planner when the account capability table has no exact value. Keep
// this aligned with StaticKiroModels: the generateAssistantResponse catalogue
// exposes one-million-token windows for the newer families, while 4.5/Haiku
// models retain the 200k window. Unknown future versions use 200k until a live
// capability proves otherwise.
//
// GPT-5.6 uses the pool-wide 372K window contract.
func KiroContextWindow(model string) int64 {
	canonical, ok := KiroCanonicalModel(model)
	if !ok {
		return 200000
	}
	if strings.HasPrefix(canonical, "gpt-") {
		return GPT56ContextWindow
	}
	switch canonical {
	case "claude-sonnet-5", "claude-sonnet-4.6",
		"claude-opus-5", "claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6":
		return 1000000
	default:
		return 200000
	}
}

// KiroPlanAllowsBootstrap reports whether an unverified concrete model may be
// attempted for the account's current subscription. Runtime-verified models are
// handled separately by the scheduler; this guard stops a stale static capability
// row from granting a model that the current plan cannot use. KIRO FREE is known
// not to include Opus.
//
// The plan is matched on its normalized tier rather than by searching for "FREE" in
// the raw name. A substring search also fires on any paid plan whose name happens
// to contain the word — `business_free`, `free_trial_enterprise` — and the result is
// a silent Opus denial for a paying account with nothing logged to explain it. An
// unrecognized plan is treated as permitting bootstrap: the scheduler still has
// runtime verification behind this, so a vocabulary gap should not deny a model.
func KiroPlanAllowsBootstrap(plan, model string) bool {
	canonical, ok := KiroCanonicalModel(model)
	if !ok {
		return false
	}
	if plantier.Normalize(plan) == plantier.Free && strings.Contains(canonical, "-opus-") {
		return false
	}
	return true
}

// KiroPlanAllows1M is intentionally independent of a human-readable plan name.
// Account-scoped live catalog evidence (Context1MState) is the entitlement gate;
// this helper only checks the selected model's technical fallback limit.
func KiroPlanAllows1M(plan, model string) bool {
	_ = plan
	return KiroContextWindow(model) >= 1000000
}

func minKiroNativeWindow(slug string, maximum int64) int64 {
	if strings.HasPrefix(slug, "gpt-") {
		return maximum
	}
	return 200000
}

func StaticKiroModels(accountID string) []storage.ModelCapability {
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(kiroStaticModels))
	for _, slug := range kiroStaticModels {
		window := int64(1000000)
		if strings.HasPrefix(slug, "gpt-") {
			window = GPT56ContextWindow
		} else if strings.Contains(slug, "4.5") || strings.Contains(slug, "haiku") {
			window = 200000
		}
		out = append(out, storage.ModelCapability{AccountID: accountID, ModelSlug: slug, NativeContextWindow: minKiroNativeWindow(slug, window),
			NativeMaxContextWindow: window, EffectiveContextWindowPercent: 100, AvailabilityState: AvailabilityUnverified,
			Context1MState: Context1MUnknown, Source: "kiro_static_unknown", LastProbeAt: now})
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

// Claude Fable 5, Opus 5, and Sonnet 5 ship with a one-million-token context as
// the standard and maximum window. Unlike older extended-context models, they
// have no 200K variant, so every paid subscription gets the 1M window without
// proving a context-1m beta entitlement. The official client still emits the
// context-1m beta on these models for entitled accounts (captured 2.1.241 wire),
// which the pool mirrors via ClaudeModelHas1MContext.
func claudeDefault1M(slug string) bool {
	slug = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(slug)), ".", "-")
	return slug == "claude-fable-5" || slug == "claude-opus-5" || slug == "claude-sonnet-5"
}

// ClaudeModelHas1MContext reports whether Claude Code's context-1m beta rides a
// model string: it is emitted for the explicit [1m] opt-in alias on any model,
// and unconditionally for the generation-5 native-one-million models
// (Fable/Opus/Sonnet 5), whose 1M window is the model default on every paid
// subscription. The extended-context Opus 4.6–4.8 line carries the beta ONLY
// via the [1m] alias — a plain claude-opus-4-8 request is a 200K turn and must
// never synthesize it (a non-entitled account would reject the 1M beta).
//
// Captured 2.1.241 wire: an entitled account sends context-1m for
// claude-opus-5 (plain and [1m]); a non-entitled (fake) token omits it. The
// account's entitlement is enforced by the pool at route time (Context1MState,
// plus the virtual-1M fallback strips the beta for accounts that cannot prove
// it); this gate is only the model-string half of the policy.
func ClaudeModelHas1MContext(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.ReplaceAll(m, ".", "-")
	if strings.Contains(m, "[1m]") {
		return true
	}
	return claudeDefault1M(m)
}

// NormalizeClaudeModelAlias maps a Claude model alias that Anthropic's API would reject
// — bare "fable"/"sonnet"/"opus"/"haiku", "auto"/"default", or a family name without a version —
// to a concrete current-generation model id of the SAME tier. It never crosses to a
// lower tier, so model quality is never downgraded; "auto"/"default" resolve to the
// strongest broadly compatible model (opus). A value that is already a concrete claude-* id, or
// any non-Claude model, is returned unchanged. This is what makes Claude Code's default/
// auto model selection work against the pool without silently weakening the model.
func NormalizeClaudeModelAlias(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto", "default", "opus", "claude-opus":
		if v := newestStaticClaudeByTier("opus"); v != "" {
			return v
		}
	case "fable", "claude-fable":
		if v := newestStaticClaudeByTier("fable"); v != "" {
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

// ClaudeModelAlias reports aliases whose concrete model must be selected from
// the candidate account's capabilities rather than from the bundled catalog.
func ClaudeModelAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "auto", "default", "fable", "claude-fable", "opus", "claude-opus", "sonnet", "claude-sonnet", "haiku", "claude-haiku":
		return true
	default:
		return false
	}
}

// ResolveClaudeModel resolves an alias only against usable capability rows for
// one candidate account. Static discovery hints cannot make an alias routable.
func ResolveClaudeModel(model string, caps []storage.ModelCapability, context1M bool) (string, bool) {
	if !ClaudeModelAlias(model) {
		return normalizeRequestedClaudeBase(model), strings.TrimSpace(model) != ""
	}
	wanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
	if wanted == "auto" || wanted == "default" {
		wanted = ""
	}
	best := ""
	bestScore := int64(-1)
	for _, c := range caps {
		// Aliases cannot safely bootstrap from a bundled discovery hint: there is
		// no exact downstream model to retry or reject. Resolve them only from
		// account-scoped live/runtime evidence.
		if !capabilityIsVerified(c) {
			continue
		}
		if context1M && c.Context1MState != Context1MSupported {
			continue
		}
		family, score := claudeModelRank(c.ModelSlug)
		if family == "" || (wanted != "" && family != wanted) {
			continue
		}
		if score > bestScore || (score == bestScore && c.ModelSlug > best) {
			best, bestScore = c.ModelSlug, score
		}
	}
	return best, best != ""
}

func capabilityIsVerified(c storage.ModelCapability) bool {
	switch c.AvailabilityState {
	case AvailabilityVerified:
		return true
	case AvailabilityUnverified, AvailabilityUnsupported:
		return false
	}
	source := strings.ToLower(strings.TrimSpace(c.Source))
	return source != "" && !strings.Contains(source, "static") && !strings.Contains(source, "unknown")
}

func claudeModelRank(model string) (string, int64) {
	canonical := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), ".", "-")
	match := kiroConcreteModelRE.FindStringSubmatch(canonical)
	if match == nil {
		return "", -1
	}
	familyRank := map[string]int64{"haiku": 1, "sonnet": 2, "opus": 3, "fable": 4}[match[1]]
	major, _ := strconv.ParseInt(match[2], 10, 64)
	minor, _ := strconv.ParseInt(match[3], 10, 64)
	date, _ := strconv.ParseInt(match[4], 10, 64)
	return match[1], familyRank*1_000_000_000_000_000 + major*1_000_000_000_000 + minor*1_000_000_000 + date
}

// ClaudeModelFamily returns fable, opus, sonnet or haiku for a concrete Claude id or
// family alias. It is used to keep fallback advice inside the requested tier.
func ClaudeModelFamily(model string) string {
	trimmed := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
	for _, family := range []string{"fable", "opus", "sonnet", "haiku"} {
		if trimmed == family || strings.HasPrefix(trimmed, family+"-") || strings.HasPrefix(trimmed, family+".") {
			return family
		}
	}
	return ""
}

// SuggestedClaudeFallback selects the highest verified lower-version model in
// the same family. It never crosses Opus/Sonnet/Haiku boundaries.
func SuggestedClaudeFallback(requested string, caps []storage.ModelCapability) string {
	family, requestedScore := claudeModelRank(requested)
	if family == "" {
		family = ClaudeModelFamily(requested)
		requestedScore = int64(^uint64(0) >> 1)
	}
	best, bestScore := "", int64(-1)
	for _, c := range caps {
		if !capabilityIsVerified(c) {
			continue
		}
		candidateFamily, score := claudeModelRank(c.ModelSlug)
		if candidateFamily != family || score >= requestedScore {
			continue
		}
		if score > bestScore {
			best, bestScore = c.ModelSlug, score
		}
	}
	return best
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
// capabilities. Generation-5 Fable/Opus/Sonnet use 1M as their standard window; older
// models keep 200K unless the live payload reports an extended-context grant.
// The curated table remains discovery metadata, not account availability proof.
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
		reportedMax := firstInt(model, "max_input_tokens", "max_context_window", "native_max_context_window", "max_context_tokens", "context_window", "context_length")
		technicalMax := reportedMax
		if technicalMax == 0 {
			technicalMax = claudeWindow(slug)
		}
		contextState, contextSource := Context1MUnknown, ""
		if reportedMax >= 1000000 || modelDeclaresContext1M(model) {
			contextState, contextSource = Context1MSupported, "live_models"
		}
		nativeWindow := int64(200000)
		if claudeDefault1M(slug) {
			nativeWindow = 1000000
			technicalMax = 1000000
			contextState, contextSource = Context1MSupported, "model_default"
		}
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           nativeWindow,
			NativeMaxContextWindow:        technicalMax,
			EffectiveContextWindowPercent: 100,
			AvailabilityState:             AvailabilityVerified,
			Context1MState:                contextState,
			Context1MSource:               contextSource,
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
		technicalMax := claudeWindow(slug)
		nativeWindow := int64(200000)
		if claudeDefault1M(slug) {
			nativeWindow = technicalMax
		}
		out = append(out, storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           nativeWindow,
			NativeMaxContextWindow:        technicalMax,
			EffectiveContextWindowPercent: 100,
			AvailabilityState:             AvailabilityUnverified,
			Context1MState:                Context1MUnknown,
			Source:                        "claude_static_unverified",
			LastProbeAt:                   now,
		})
	}
	return out
}

// MergeClaudeStatic remains for callers compiled against the previous API. A
// successful live list is authoritative, so the result is now an exact copy.
func MergeClaudeStatic(accountID string, probeCaps []storage.ModelCapability) []storage.ModelCapability {
	_ = accountID
	return append([]storage.ModelCapability(nil), probeCaps...)
}

// ApplyClaudeAccountPolicy converts technical/live metadata into the context
// entitlement of this credential. Fable/Opus/Sonnet 5 always use their model-default
// 1M window. For older extended-context models, API keys still require live
// evidence and OAuth credentials still use plan-specific entitlement.
func ApplyClaudeAccountPolicy(caps []storage.ModelCapability, account storage.Account, token storage.AccountToken) []storage.ModelCapability {
	method := accountprovider.EffectiveAuthMethod("claude", token)
	// Authorize on the normalized tier, but keep the raw plan for the source string
	// an operator reads back.
	plan := strings.ToLower(strings.TrimSpace(account.PlanType))
	tier := plantier.Normalize(account.PlanType)
	for i := range caps {
		c := &caps[i]
		if claudeDefault1M(c.ModelSlug) {
			c.NativeContextWindow = 1000000
			c.NativeMaxContextWindow = 1000000
			if c.AvailabilityState == AvailabilityVerified {
				c.Context1MState, c.Context1MSource = Context1MSupported, "model_default"
			} else {
				c.Context1MState, c.Context1MSource = Context1MUnknown, ""
			}
			continue
		}
		c.NativeContextWindow = 200000
		if c.NativeMaxContextWindow == 0 {
			c.NativeMaxContextWindow = claudeWindow(c.ModelSlug)
		}
		if method == accountprovider.AuthMethodAPIKey {
			if c.AvailabilityState != AvailabilityVerified && c.Context1MState == Context1MSupported {
				c.Context1MState, c.Context1MSource = Context1MUnknown, ""
			}
			continue
		}
		switch {
		case tier == plantier.Pro:
			c.Context1MState, c.Context1MSource = Context1MUnsupported, "plan:pro"
		case claudeOAuthPlanAllows1M(tier) && claudeOpusSupports1M(c.ModelSlug, c.NativeMaxContextWindow):
			c.Context1MState, c.Context1MSource = Context1MSupported, "plan:"+plan
		case claudeOAuthPlanAllows1M(tier):
			c.Context1MState, c.Context1MSource = Context1MUnsupported, "model_not_eligible"
		default:
			c.Context1MState, c.Context1MSource = Context1MUnknown, "plan:unknown"
		}
	}
	return caps
}

// claudeOAuthPlanAllows1M keys off the normalized tier so that a plan name merely
// containing a tier word cannot flip the entitlement: `Contains(plan, "pro")` also
// matched `provisional`, and because the pro branch is evaluated first it beat an
// explicit `max` appearing later in the same string. Business is deliberately
// absent, matching the previous behaviour for `self_serve_business_usage_based`.
func claudeOAuthPlanAllows1M(tier plantier.Tier) bool {
	switch tier {
	case plantier.Max, plantier.Team, plantier.Enterprise:
		return true
	default:
		return false
	}
}

func claudeOpusSupports1M(model string, technicalMax int64) bool {
	family, score := claudeModelRank(model)
	_, baseline := claudeModelRank("claude-opus-4-6")
	return family == "opus" && score >= baseline && technicalMax >= 1000000
}

func modelDeclaresContext1M(model map[string]interface{}) bool {
	for key, value := range model {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "1m") || (strings.Contains(lower, "context") && strings.Contains(lower, "million")) {
			if b, ok := value.(bool); ok && b {
				return true
			}
		}
		switch nested := value.(type) {
		case map[string]interface{}:
			if modelDeclaresContext1M(nested) {
				return true
			}
		case []interface{}:
			for _, item := range nested {
				if child, ok := item.(map[string]interface{}); ok && modelDeclaresContext1M(child) {
					return true
				}
			}
		}
	}
	return false
}

// codexStaticModel describes one current-generation Codex (ChatGPT) model for the
// static fallback catalog. Mirrors the fields the live ChatGPT /models probe would
// return for the model.
type codexStaticModel struct {
	slug                  string
	window                int64
	maxWindow             int64
	autoCompactTokenLimit int64
	overrideClientContext bool
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
	{slug: "gpt-5.6-sol", window: GPT56ContextWindow, maxWindow: GPT56ContextWindow, autoCompactTokenLimit: GPT56AutoCompactTokenLimit, overrideClientContext: true, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{slug: "gpt-5.6-terra", window: GPT56ContextWindow, maxWindow: GPT56ContextWindow, autoCompactTokenLimit: GPT56AutoCompactTokenLimit, overrideClientContext: true, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{slug: "gpt-5.6-luna", window: GPT56ContextWindow, maxWindow: GPT56ContextWindow, autoCompactTokenLimit: GPT56AutoCompactTokenLimit, overrideClientContext: true, minimumClientVersion: "0.144.0", requiresCurrentClient: true, preferWebSocket: true, responsesLite: true, reasoningLevels: []string{"low", "medium", "high", "xhigh", "max"}},
	{slug: "gpt-5.5", window: 272000, maxWindow: 272000, minimumClientVersion: "0.124.0", requiresCurrentClient: true, preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.4", window: 272000, maxWindow: 1000000, minimumClientVersion: "0.98.0", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.4-mini", window: 272000, maxWindow: 272000, minimumClientVersion: "0.98.0", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
	{slug: "gpt-5.2", window: 272000, maxWindow: 272000, minimumClientVersion: "0.0.1", preferWebSocket: true, reasoningLevels: []string{"low", "medium", "high", "xhigh"}},
}

// RemoteCodexModel is the validated compatibility-manifest projection used to
// refresh conservative fallback metadata without rebuilding the binary. It never
// grants routability: account-scoped live capability evidence remains authoritative.
type RemoteCodexModel struct {
	Slug                  string
	ContextWindow         int64
	MaxContextWindow      int64
	AutoCompactTokenLimit int64
	MinimumClientVersion  string
	RequiresCurrentClient bool
	PreferWebSocket       bool
	ResponsesLite         bool
	ReasoningLevels       []string
}

var remoteCodexCatalog atomic.Value // stores []RemoteCodexModel

// SetRemoteCodexModels atomically publishes a previously signature/schema-
// validated fallback catalog. Passing nil or an empty slice restores the bundled
// catalog. The input is defensively copied because request paths read lock-free.
func SetRemoteCodexModels(models []RemoteCodexModel) {
	copyModels := make([]RemoteCodexModel, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model.Slug = strings.ToLower(strings.TrimSpace(model.Slug))
		if model.Slug == "" {
			continue
		}
		if _, duplicate := seen[model.Slug]; duplicate {
			continue
		}
		seen[model.Slug] = struct{}{}
		model.ReasoningLevels = append([]string(nil), model.ReasoningLevels...)
		copyModels = append(copyModels, model)
	}
	remoteCodexCatalog.Store(copyModels)
}

func remoteCodexModels() []RemoteCodexModel {
	value := remoteCodexCatalog.Load()
	if value == nil {
		return nil
	}
	models, _ := value.([]RemoteCodexModel)
	return models
}

func effectiveCodexStaticModels() ([]codexStaticModel, map[string]struct{}) {
	remote := remoteCodexModels()
	if len(remote) == 0 {
		return codexStaticModels, nil
	}
	// A signed manifest may be intentionally partial. Remote entries override a
	// bundled slug and new entries are appended; omission never erases a safe
	// bundled fallback model.
	overrides := make(map[string]codexStaticModel, len(remote))
	remoteOrder := make([]string, 0, len(remote))
	for _, model := range remote {
		converted := codexStaticModel{
			slug: model.Slug, window: model.ContextWindow, maxWindow: model.MaxContextWindow,
			autoCompactTokenLimit: model.AutoCompactTokenLimit,
			overrideClientContext: model.ContextWindow > 0 && model.AutoCompactTokenLimit > 0,
			minimumClientVersion:  model.MinimumClientVersion,
			requiresCurrentClient: model.RequiresCurrentClient, preferWebSocket: model.PreferWebSocket,
			responsesLite: model.ResponsesLite, reasoningLevels: append([]string(nil), model.ReasoningLevels...),
		}
		overrides[model.Slug] = converted
		remoteOrder = append(remoteOrder, model.Slug)
	}
	out := make([]codexStaticModel, 0, len(codexStaticModels)+len(remote))
	consumed := make(map[string]struct{}, len(remote))
	for _, bundled := range codexStaticModels {
		if replacement, ok := overrides[bundled.slug]; ok {
			// A remote fallback may add capabilities but cannot silently shrink a
			// bundled model's context, transports, or reasoning choices. Live
			// account-scoped discovery remains authoritative in either direction.
			if replacement.window < bundled.window {
				replacement.window = bundled.window
			}
			if replacement.maxWindow < bundled.maxWindow {
				replacement.maxWindow = bundled.maxWindow
			}
			if replacement.autoCompactTokenLimit < bundled.autoCompactTokenLimit {
				replacement.autoCompactTokenLimit = bundled.autoCompactTokenLimit
			}
			replacement.overrideClientContext = replacement.overrideClientContext || bundled.overrideClientContext
			replacement.requiresCurrentClient = replacement.requiresCurrentClient || bundled.requiresCurrentClient
			replacement.preferWebSocket = replacement.preferWebSocket || bundled.preferWebSocket
			replacement.responsesLite = replacement.responsesLite || bundled.responsesLite
			replacement.reasoningLevels = mergeReasoningLevels(bundled.reasoningLevels, replacement.reasoningLevels)
			out = append(out, replacement)
			consumed[bundled.slug] = struct{}{}
		} else {
			out = append(out, bundled)
		}
	}
	for _, slug := range remoteOrder {
		if _, ok := consumed[slug]; !ok {
			out = append(out, overrides[slug])
			consumed[slug] = struct{}{}
		}
	}
	return out, consumed
}

func mergeReasoningLevels(base, extra []string) []string {
	out := append([]string(nil), base...)
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, value := range base {
		seen[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	for _, value := range extra {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func codexStaticModelForSlug(slug string) (codexStaticModel, bool) {
	slug = NormalizeCodexModelAlias(slug)
	models, _ := effectiveCodexStaticModels()
	for _, m := range models {
		if strings.ToLower(m.slug) == slug {
			return m, true
		}
	}
	return codexStaticModel{}, false
}

// IsCatalogCodexModel reports whether a slug is present in the effective Codex model
// catalog (bundled entries plus any published remote fallback manifest). It is the
// authoritative answer to "does the Codex channel own this model", as opposed to
// guessing from the slug's spelling.
//
// Request dispatch needs this because SetRemoteCodexModels normalizes and dedupes
// slugs but does not reserve any prefix. A signed manifest may therefore publish a
// slug that a name-based Claude/Codex heuristic would classify the other way, and
// routing must follow the catalog rather than the spelling.
func IsCatalogCodexModel(slug string) bool {
	_, ok := codexStaticModelForSlug(slug)
	return ok
}

// NormalizeCodexModelAlias resolves only aliases documented by the direct
// OpenAI/Codex surface. Kiro keeps its own exact model namespace and deliberately
// does not call this helper.
func NormalizeCodexModelAlias(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "gpt-5.6" {
		return "gpt-5.6-sol"
	}
	return slug
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

// CodexClientContextOverrides returns the fixed GPT-5.6 context contract for
// callers that cannot consume the native /models catalog. The generated official
// client config deliberately does not use these values: command authentication
// makes Codex fetch the server-rendered model contract from /v1/models instead.
func CodexClientContextOverrides(slug string) (contextWindow, autoCompactTokenLimit int64, ok bool) {
	m, found := codexStaticModelForSlug(slug)
	if !found || !m.overrideClientContext || m.window <= 0 {
		return 0, 0, false
	}
	return m.window, m.autoCompactTokenLimit, true
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
	models, remoteSlugs := effectiveCodexStaticModels()
	out := make([]storage.ModelCapability, 0, len(models))
	for _, m := range models {
		source := "codex_static"
		if _, remote := remoteSlugs[m.slug]; remote {
			source = "codex_compatibility_manifest"
		}
		cap := storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     m.slug,
			NativeContextWindow:           m.window,
			NativeMaxContextWindow:        m.maxWindow,
			EffectiveContextWindowPercent: 95,
			AutoCompactTokenLimit:         m.autoCompactTokenLimit,
			AvailabilityState:             AvailabilityUnverified,
			Context1MState:                Context1MUnknown,
			Visibility:                    "list",
			Source:                        source,
			LastProbeAt:                   now,
		}
		out = append(out, ApplyGPT56ContextContract(cap))
	}
	return out
}

// MergeCodexStatic remains for compatibility. Live /models data is authoritative.
func MergeCodexStatic(accountID string, probeCaps []storage.ModelCapability) []storage.ModelCapability {
	_ = accountID
	return append([]storage.ModelCapability(nil), probeCaps...)
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
		cap := storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     slug,
			NativeContextWindow:           native,
			NativeMaxContextWindow:        maxNative,
			EffectiveContextWindowPercent: percent,
			AvailabilityState:             AvailabilityVerified,
			Context1MState:                Context1MUnknown,
			AutoCompactTokenLimit:         autoCompact,
			Visibility:                    visibility,
			ETag:                          etag,
			RawModelJSONHash:              hash,
			RawModelJSON:                  rawModel,
			Source:                        "probe",
			LastProbeAt:                   now,
		}
		out = append(out, ApplyGPT56ContextContract(cap))
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
// "claude" for the Anthropic sets, "antigravity" for the native Antigravity
// probe, "custom:<id>" for a custom OpenAI-compatible provider (tagged
// "custom:<id>" by the probe), and "codex" otherwise.
func providerKeyForSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch {
	case strings.HasPrefix(s, "custom:"):
		return source
	case strings.HasPrefix(s, "claude"):
		return "claude"
	case strings.HasPrefix(s, "kiro"):
		return "kiro"
	case strings.HasPrefix(s, "antigravity"):
		return "antigravity"
	default:
		return "codex"
	}
}

// scopedCatalogCapabilities preserves the coherent richest-account policy inside
// each independently routable target, then unions those target catalogs. The
// synthetic account id is internal to catalog construction: it makes the existing
// response builders treat the selected sets from multiple targets as one provider
// catalog without exposing or mutating a stored account identity.
func scopedCatalogCapabilities(scopes [][]storage.ModelCapability) []storage.ModelCapability {
	selected := make([]storage.ModelCapability, 0)
	for _, scope := range scopes {
		buckets := map[string][]storage.ModelCapability{}
		order := make([]string, 0)
		for _, c := range scope {
			if !capabilityIsVerified(c) {
				continue
			}
			key := providerKeyForSource(c.Source)
			if _, exists := buckets[key]; !exists {
				order = append(order, key)
			}
			buckets[key] = append(buckets[key], c)
		}
		for _, key := range order {
			for _, c := range richestAccountCaps(buckets[key]) {
				c.AccountID = "\x00scoped-catalog\x00" + key
				selected = append(selected, c)
			}
		}
	}
	return selected
}

// BuildModelsResponseForScopes advertises the union of independently routable
// user-group target catalogs while retaining richest-account coherence per target.
func BuildModelsResponseForScopes(scopes [][]storage.ModelCapability, cfg config.Config) ([]byte, string, error) {
	return BuildModelsResponse(scopedCatalogCapabilities(scopes), cfg)
}

// BuildCodexModelsResponseForScopes is the native Codex schema counterpart of
// BuildModelsResponseForScopes.
func BuildCodexModelsResponseForScopes(scopes [][]storage.ModelCapability) ([]byte, string, error) {
	selected := scopedCatalogCapabilities(scopes)
	seen56 := make(map[string]bool)
	for _, cap := range selected {
		if isGPT56Model(cap.ModelSlug) {
			seen56[strings.ToLower(strings.TrimSpace(cap.ModelSlug))] = true
		}
	}
	// A live probe can be temporarily unavailable on a newly imported account.
	// The scoped list already contains only active/routable accounts, so promote a
	// cloned GPT-5.6 static hint for this native client response only. The stored
	// row remains unverified and normal routing still bootstraps/records evidence.
	for scopeIndex, scope := range scopes {
		for _, cap := range scope {
			slug := strings.ToLower(strings.TrimSpace(cap.ModelSlug))
			if seen56[slug] || !isGPT56Model(slug) || capabilityIsVerified(cap) {
				continue
			}
			source := strings.ToLower(strings.TrimSpace(cap.Source))
			if !strings.Contains(source, "static") && !strings.Contains(source, "unknown") {
				continue
			}
			cap = ApplyGPT56ContextContract(cap)
			cap.AccountID = fmt.Sprintf("\x00scoped-codex-bootstrap\x00%d", scopeIndex)
			cap.AvailabilityState = AvailabilityVerified
			selected = append(selected, cap)
			seen56[slug] = true
		}
	}
	return BuildCodexModelsResponse(selected)
}

// BuildAnthropicModelsResponseForScopes is the native Anthropic schema
// counterpart of BuildModelsResponseForScopes.
func BuildAnthropicModelsResponseForScopes(scopes [][]storage.ModelCapability) ([]byte, string, error) {
	return BuildAnthropicModelsResponse(scopedCatalogCapabilities(scopes))
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
		if !capabilityIsVerified(c) {
			continue
		}
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
		window := cap.NativeContextWindow
		if window == 0 {
			window = cap.NativeMaxContextWindow
		}
		native := cap.NativeContextWindow
		if native == 0 {
			native = cap.NativeMaxContextWindow
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
		if cap.Context1MState == Context1MSupported && cap.NativeMaxContextWindow >= 1_000_000 &&
			!strings.HasSuffix(strings.ToLower(strings.TrimSpace(cap.ModelSlug)), "[1m]") {
			alias := make(map[string]interface{}, len(item))
			for field, value := range item {
				alias[field] = value
			}
			alias["id"] = cap.ModelSlug + "[1m]"
			alias["context_window"] = int64(1_000_000)
			alias["native_context_window"] = int64(1_000_000)
			alias["native_max_context_window"] = int64(1_000_000)
			alias["window_mode"] = "context_1m"
			data = append(data, alias)
		}
	}
	resp := map[string]interface{}{"object": "list", "data": data}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, `W/"` + hex.EncodeToString(sum[:])[:24] + `"`, nil
}

// BuildCodexModelsResponse renders the native model catalog consumed by the
// official Codex client when it calls GET /v1/models?client_version=.... Unlike
// the OpenAI compatibility endpoint, Codex deserializes the whole response as
// {"models":[ModelInfo,...]}; returning {"object":"list","data":[...]} makes
// the refresh fail and leaves the client on stale or fallback context limits.
//
// A request can be scheduled to any routable account that advertises the selected
// slug, so window metadata is the minimum supported by those accounts. This keeps
// client-side automatic compaction ahead of the smallest real upstream window.
func BuildCodexModelsResponse(capabilities []storage.ModelCapability) ([]byte, string, error) {
	best := advertisedCapabilitiesBySlug(capabilities)
	keys := make([]string, 0, len(best))
	for key := range best {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	models := make([]interface{}, 0, len(keys))
	for priority, key := range keys {
		cap := best[key]
		item := codexModelInfoItem(cap, capabilities, priority+1)
		models = append(models, item)
	}
	resp := map[string]interface{}{"models": models}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, `W/"` + hex.EncodeToString(sum[:])[:24] + `"`, nil
}

// advertisedCapabilitiesBySlug preserves the existing public catalog policy:
// each provider contributes the coherent model set of its richest routable
// account, and duplicate slugs retain the metadata row with the largest maximum.
func advertisedCapabilitiesBySlug(capabilities []storage.ModelCapability) map[string]storage.ModelCapability {
	buckets := map[string][]storage.ModelCapability{}
	order := make([]string, 0)
	for _, c := range capabilities {
		if !capabilityIsVerified(c) {
			continue
		}
		key := providerKeyForSource(c.Source)
		if _, ok := buckets[key]; !ok {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], c)
	}
	best := map[string]storage.ModelCapability{}
	for _, key := range order {
		for _, cap := range richestAccountCaps(buckets[key]) {
			current, exists := best[cap.ModelSlug]
			if !exists || cap.NativeMaxContextWindow > current.NativeMaxContextWindow {
				best[cap.ModelSlug] = cap
			}
			if cap.Context1MState == Context1MSupported && cap.NativeMaxContextWindow >= 1_000_000 &&
				!strings.HasSuffix(strings.ToLower(strings.TrimSpace(cap.ModelSlug)), "[1m]") {
				alias := cap
				alias.ModelSlug += "[1m]"
				alias.NativeContextWindow = 1_000_000
				alias.NativeMaxContextWindow = 1_000_000
				best[alias.ModelSlug] = alias
			}
		}
	}
	return best
}

func codexModelInfoItem(selected storage.ModelCapability, capabilities []storage.ModelCapability, priority int) map[string]interface{} {
	item := rawOfficialModelItem(selected)
	if len(item) == 0 {
		// Runtime inference can verify a model before that account has completed a
		// fresh /models probe. Reuse another routable account's official metadata
		// for the same slug when available.
		for _, candidate := range capabilities {
			if !capabilityIsVerified(candidate) || !strings.EqualFold(candidate.ModelSlug, selected.ModelSlug) {
				continue
			}
			if raw := rawOfficialModelItem(candidate); len(raw) != 0 {
				item = raw
				break
			}
		}
	}
	if item == nil {
		item = map[string]interface{}{}
	}

	window, maxWindow, percent, autoCompact := conservativeCodexLimits(selected.ModelSlug, capabilities)
	if window == 0 {
		window = firstPositive(selected.NativeContextWindow, selected.NativeMaxContextWindow)
	}
	if maxWindow == 0 {
		maxWindow = firstPositive(selected.NativeMaxContextWindow, window)
	}
	if maxWindow > 0 && (window == 0 || window > maxWindow) {
		window = maxWindow
	}
	if percent == 0 {
		percent = 95
	}
	static, knownStatic := codexStaticModelForSlug(selected.ModelSlug)
	// GPT-5.6 has one pool-wide client contract: 372K with compaction at 90%.
	// Live account probes can arrive with stale bundled-client metadata, so the
	// curated override wins for this family. Other models retain route-scoped
	// conservative limits.
	if knownStatic && static.overrideClientContext {
		window = static.window
		maxWindow = static.maxWindow
		autoCompact = static.autoCompactTokenLimit
		percent = GPT56EffectivePercent
	} else {
		if window == 0 && knownStatic {
			window = static.window
		}
		if maxWindow == 0 && knownStatic {
			maxWindow = static.maxWindow
		}
	}
	if maxWindow > 0 && (window == 0 || window > maxWindow) {
		window = maxWindow
	}
	if window > 0 {
		derivedLimit := (window * 9) / 10
		if autoCompact == 0 || autoCompact > derivedLimit {
			autoCompact = derivedLimit
		}
	}

	setModelInfoDefault(item, "slug", selected.ModelSlug)
	// A raw endpoint may use id rather than slug. The route's canonical persisted
	// slug always wins so Codex can match the returned metadata to its request.
	item["slug"] = selected.ModelSlug
	setModelInfoDefault(item, "display_name", selected.ModelSlug)
	setModelInfoDefault(item, "description", selected.ModelSlug+" coding model")
	setModelInfoDefault(item, "default_reasoning_level", "medium")
	if _, ok := item["supported_reasoning_levels"]; !ok {
		levels := []string{"low", "medium", "high"}
		if knownStatic && len(static.reasoningLevels) > 0 {
			levels = static.reasoningLevels
		}
		presets := make([]interface{}, 0, len(levels))
		for _, level := range levels {
			presets = append(presets, map[string]interface{}{
				"effort": level, "description": codexReasoningDescription(level),
			})
		}
		item["supported_reasoning_levels"] = presets
	}
	setModelInfoDefault(item, "shell_type", "shell_command")
	visibility := strings.TrimSpace(selected.Visibility)
	if visibility == "" {
		visibility = "list"
	}
	setModelInfoDefault(item, "visibility", visibility)
	setModelInfoDefault(item, "supported_in_api", true)
	setModelInfoDefault(item, "priority", priority)
	setModelInfoDefault(item, "availability_nux", nil)
	setModelInfoDefault(item, "upgrade", nil)
	setModelInfoDefault(item, "base_instructions", "You are Codex, a coding agent. Work with the user in the current workspace until the task is complete.")
	setModelInfoDefault(item, "support_verbosity", true)
	setModelInfoDefault(item, "default_verbosity", "low")
	setModelInfoDefault(item, "apply_patch_tool_type", "freeform")
	setModelInfoDefault(item, "truncation_policy", map[string]interface{}{"mode": "tokens", "limit": 10000})
	setModelInfoDefault(item, "supports_parallel_tool_calls", true)
	setModelInfoDefault(item, "experimental_supported_tools", []interface{}{})
	setModelInfoDefault(item, "input_modalities", []interface{}{"text", "image"})
	item["context_window"] = window
	item["max_context_window"] = maxWindow
	if autoCompact > 0 {
		item["auto_compact_token_limit"] = autoCompact
	} else {
		delete(item, "auto_compact_token_limit")
	}
	item["effective_context_window_percent"] = percent
	if knownStatic {
		setModelInfoDefault(item, "minimal_client_version", static.minimumClientVersion)
		setModelInfoDefault(item, "prefer_websockets", static.preferWebSocket)
		setModelInfoDefault(item, "use_responses_lite", static.responsesLite)
	} else {
		setModelInfoDefault(item, "minimal_client_version", "0.0.1")
		setModelInfoDefault(item, "prefer_websockets", false)
		setModelInfoDefault(item, "use_responses_lite", false)
	}
	return item
}

func conservativeCodexLimits(slug string, capabilities []storage.ModelCapability) (window, maxWindow, percent, autoCompact int64) {
	for _, cap := range capabilities {
		if !capabilityIsVerified(cap) || !strings.EqualFold(strings.TrimSpace(cap.ModelSlug), strings.TrimSpace(slug)) {
			continue
		}
		candidateWindow := firstPositive(cap.NativeContextWindow, cap.NativeMaxContextWindow)
		candidateMax := firstPositive(cap.NativeMaxContextWindow, candidateWindow)
		window = minPositive(window, candidateWindow)
		maxWindow = minPositive(maxWindow, candidateMax)
		candidatePercent := cap.EffectiveContextWindowPercent
		if candidatePercent == 0 {
			candidatePercent = 95
		}
		percent = minPositive(percent, candidatePercent)
		if cap.AutoCompactTokenLimit > 0 {
			autoCompact = minPositive(autoCompact, cap.AutoCompactTokenLimit)
		}
	}
	return window, maxWindow, percent, autoCompact
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func minPositive(current, candidate int64) int64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func setModelInfoDefault(item map[string]interface{}, key string, value interface{}) {
	current, exists := item[key]
	if !exists || current == nil || (isEmptyString(current) && value != nil) {
		item[key] = value
	}
}

func isEmptyString(value interface{}) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

func codexReasoningDescription(level string) string {
	switch level {
	case "low":
		return "Fast responses with lighter reasoning"
	case "medium":
		return "Balances speed and reasoning depth"
	case "high":
		return "Greater reasoning depth for complex problems"
	case "xhigh":
		return "Extra high reasoning depth for complex problems"
	case "max", "ultra":
		return "Maximum reasoning depth for the hardest problems"
	default:
		return level
	}
}

// BuildAnthropicModelsResponse renders the pool's native Messages capabilities in Anthropic's
// native GET /v1/models schema — {data:[{type:"model",id,display_name,created_at}],
// has_more:false,first_id,last_id}. Anthropic clients (Claude Code) enumerate models
// from THIS shape and disable their model picker / "auto" selection when handed the
// OpenAI-shaped list BuildModelsResponse returns. Verified Claude, Kiro, and
// Antigravity models are listed; unverified static discovery hints never leak.
func BuildAnthropicModelsResponse(capabilities []storage.ModelCapability) ([]byte, string, error) {
	claudeCaps := make([]storage.ModelCapability, 0, len(capabilities))
	kiroCaps := make([]storage.ModelCapability, 0, len(capabilities))
	antigravityCaps := make([]storage.ModelCapability, 0, len(capabilities))
	for _, c := range capabilities {
		switch providerKeyForSource(c.Source) {
		case "claude":
			claudeCaps = append(claudeCaps, c)
		case "kiro":
			kiroCaps = append(kiroCaps, c)
		case "antigravity":
			antigravityCaps = append(antigravityCaps, c)
		}
	}
	merged := append([]storage.ModelCapability(nil), richestAccountCaps(kiroCaps)...)
	if len(claudeCaps) > 0 {
		merged = append(merged, richestAccountCaps(claudeCaps)...)
	}
	if len(antigravityCaps) > 0 {
		merged = append(merged, richestAccountCaps(antigravityCaps)...)
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
		if !capabilityIsVerified(c) {
			continue
		}
		slug := c.ModelSlug
		if providerKeyForSource(c.Source) == "kiro" {
			slug = ClaudeFacingKiroModelID(slug)
		}
		appendModel(slug)
		// Claude Code recognizes [1m] locally and keeps a 1M client transcript.
		// Native 1M accounts remain preferred by the scheduler; a verified Claude
		// model with a smaller native window can still expose the alias because the
		// Messages router falls back to its real window and triggers native client
		// compaction before crossing it.
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(slug)), "claude") &&
			c.NativeContextWindow > 0 &&
			!strings.HasSuffix(strings.ToLower(strings.TrimSpace(slug)), "[1m]") {
			appendModel(slug + "[1m]")
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

// ClaudeFacingKiroModelID converts Kiro's native dotted version spelling
// (claude-opus-4.8) to the public Anthropic/Claude Code spelling
// (claude-opus-4-8). Request routing accepts both forms and canonicalizes back to
// Kiro's native ID; this conversion is used for all client-facing model metadata
// and fallback instructions.
func ClaudeFacingKiroModelID(slug string) string {
	trimmed := strings.ToLower(strings.TrimSpace(slug))
	match := kiroConcreteModelRE.FindStringSubmatch(trimmed)
	if match == nil {
		return strings.TrimSpace(slug)
	}
	out := "claude-" + match[1] + "-" + match[2]
	if match[3] != "" {
		out += "-" + match[3]
	}
	if match[4] != "" {
		out += "-" + match[4]
	}
	return out
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
