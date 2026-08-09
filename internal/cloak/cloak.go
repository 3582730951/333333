// Package cloak virtualizes a Claude (/v1/messages) request so that, combined
// with the account-bound headers in the upstream layer, the upstream sees a
// consistent first-party Claude Code client instead of the relay/host machine.
//
// It performs four jobs:
//   - removes top-level metadata before forwarding to Anthropic while keeping a
//     scrub rule for any downstream metadata.user_id;
//   - for OAuth (Claude Pro/Max) traffic, normalizes custom tool names to Claude
//     Code's TitleCase and guarantees the canonical Claude Code identity system
//     block is present (Anthropic fingerprints third-party clients by both);
//   - unifies system descriptors in the system prompt's environment block
//     (Platform / OS Version) to the account's virtual identity so the body
//     agrees with the request headers;
//   - compiles a streamrewrite.Matcher that replaces the real telemetry user id
//     and operator-configured sensitive strings across the whole data stream,
//     including the response SSE (exhaustive even across chunk boundaries).
//
// Per project policy it deliberately does NOT alter the downstream's working
// directory or file paths: the model must see real paths or its tool calls
// (executed on the downstream's real machine) would fail. Only environment /
// system descriptors are unified, never directories.
package cloak

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/streamrewrite"
)

// Result is the virtualized request body plus the scrubber to apply to the
// upstream response.
type Result struct {
	Body     []byte
	Scrubber *streamrewrite.Matcher
}

// claudeCodeIdentityLine is the exact first system block the official Claude
// Code CLI sends. OAuth requests are expected to carry it verbatim.
const (
	claudeCodeIdentityLine       = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	legacyClaudeCodeIdentityLine = "You are Claude Code, Anthropic's official CLI for Claude."
)

// claudeBillingHeaderPrefix marks the x-anthropic-billing-header system block that
// real Claude Code prepends to every request (carrying cc_version + the native
// build component). Captured 2026-08-09 ground truth for 2.1.226: it is the FIRST
// system block, type text, has NO cache_control and NO cch member, and uses the
// fixed .503 build component on every request.
const claudeBillingHeaderPrefix = "x-anthropic-billing-header:"

// claudeCodeToolNames maps OpenCode/lowercase tool names to Claude Code's
// official TitleCase names. Built-in typed tools (web_search, code_execution)
// are left untouched, and real Claude Code (already TitleCase) is a no-op.
var claudeCodeToolNames = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit",
	"glob": "Glob", "grep": "Grep", "task": "Task", "webfetch": "WebFetch",
	"todowrite": "TodoWrite", "todoread": "TodoRead", "ls": "LS",
	"notebookedit": "NotebookEdit", "question": "Question", "skill": "Skill",
}

// Virtualize rewrites the Claude request body and returns it with a response
// scrubber. oauth enables the Claude Code OAuth normalization (tool names +
// identity system block). It is VirtualizeClaudeCode with no billing-header pass.
func Virtualize(body []byte, id identity.Identity, sensitiveWords []string, oauth bool) Result {
	return VirtualizeClaudeCode(body, id, sensitiveWords, oauth, "")
}

type ClaudeCodeCacheOptions struct {
	NativeBreakpoints bool
	BreakpointPolicy  string
	TTL               string
}

// VirtualizeClaudeCode is Virtualize with the Claude Code x-anthropic-billing-header
// folded into the SAME single JSON parse/marshal pass. A non-empty billingVersion
// (meaningful only for OAuth/Claude-Code traffic) stamps the billing system block in
// place, so callers no longer need a second EnsureClaudeCodeBillingHeader round-trip —
// a full unmarshal+marshal — over what for Claude Code is often a very large request
// body. The billing block carries no cache_control and is placed at system[0] (ahead
// of any identity block), matching the captured real client shape, and is added AFTER
// capCacheControlBreakpoints so it neither consumes a breakpoint nor is affected by
// the cap. Its build component is the one captured from the shipping binary.
func VirtualizeClaudeCode(body []byte, id identity.Identity, sensitiveWords []string, oauth bool, billingVersion string) Result {
	return VirtualizeClaudeCodeWithCache(body, id, sensitiveWords, oauth, billingVersion, ClaudeCodeCacheOptions{})
}

func VirtualizeClaudeCodeWithCache(body []byte, id identity.Identity, sensitiveWords []string, oauth bool, billingVersion string, cache ClaudeCodeCacheOptions) Result {
	rules := make([]streamrewrite.Rule, 0, len(sensitiveWords)+1)
	out := body

	var root map[string]interface{}
	if decodeClaudeJSONObject(body, &root) == nil {
		nativeClaudeCode := firstSystemIsClaudeCode(root)
		if md, ok := root["metadata"].(map[string]interface{}); ok {
			vid := claudeVirtualUserID(id, oauth || nativeClaudeCode)
			if orig, _ := md["user_id"].(string); orig != "" && orig != vid {
				rules = append(rules, streamrewrite.Rule{Pattern: orig, Replacement: vid})
			}
			md["user_id"] = vid
			stripMetadataTelemetry(md)
			root["metadata"] = md
		} else if oauth || nativeClaudeCode {
			root["metadata"] = map[string]interface{}{
				"user_id": claudeVirtualUserID(id, true),
			}
		}
		if oauth {
			// Only normalize tool names for requests that are GENUINELY Claude Code
			// (they already lead with the identity / billing system block). Real Claude
			// Code already sends TitleCase tools, so this stays a no-op for it — but it
			// stops us renaming a THIRD-PARTY OpenAI/Claude client's custom tool (e.g. one
			// literally named "read") to "Read", which would break that client's own
			// tool-call matching on the way back. ensureClaudeCodeSystem still runs for
			// all OAuth traffic (the identity block is required or Anthropic 400s).
			if firstSystemIsClaudeCode(root) {
				remapToolNames(root)
			}
			ensureClaudeCodeSystem(root, cache.TTL)
			normalizeClaudeCodeIdentityCacheTTL(root, cache.TTL)
		}
		normalizeSystemInfo(root, id)
		anthropicwire.SanitizeVolatileCacheControls(root)
		anthropicwire.NormalizeCacheControlTTLForPolicy(root, cache.TTL)
		if oauth && nativeClaudeCode && cache.NativeBreakpoints && normalizeClaudeCodeBreakpointPolicy(cache.BreakpointPolicy) != "coarse_safe" {
			injectClaudeNativeAutoContextBreakpoint(root, cache.TTL)
		}
		anthropicwire.CapCacheControlBreakpoints(root, 4)
		anthropicwire.NormalizeCacheControlTTLForPolicy(root, cache.TTL)
		if (oauth || nativeClaudeCode) && strings.TrimSpace(billingVersion) != "" {
			setClaudeBillingBlock(root, billingVersion)
		}
		if b, err := anthropicwire.MarshalPreservingOrder(body, root); err == nil {
			out = b
		}
	}

	for _, w := range sensitiveWords {
		if w = strings.TrimSpace(w); w != "" {
			rules = append(rules, streamrewrite.Rule{Pattern: w, Replacement: pseudonym(w)})
		}
	}

	m := streamrewrite.New(rules)
	out = m.ReplaceAll(out)
	return Result{Body: out, Scrubber: m}
}

func normalizeClaudeCodeBreakpointPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "coarse_safe":
		return "coarse_safe"
	default:
		return "balanced"
	}
}

// ScrubSensitive returns the body with operator sensitive words replaced and a
// scrubber to apply to the response stream. It does not parse or restructure the
// body and never touches paths, so it is safe on the Codex fast path; with no
// words configured it is a zero-cost pass-through.
func ScrubSensitive(body []byte, sensitiveWords []string) Result {
	rules := make([]streamrewrite.Rule, 0, len(sensitiveWords))
	for _, w := range sensitiveWords {
		if w = strings.TrimSpace(w); w != "" {
			rules = append(rules, streamrewrite.Rule{Pattern: w, Replacement: pseudonym(w)})
		}
	}
	m := streamrewrite.New(rules)
	return Result{Body: m.ReplaceAll(body), Scrubber: m}
}

// firstSystemIsClaudeCode reports whether the request's first system block already
// marks it as genuine Claude Code (the identity line or the x-anthropic-billing-header
// block that precedes it). Used to gate tool-name normalization so third-party clients
// keep their own tool names.
func firstSystemIsClaudeCode(root map[string]interface{}) bool {
	switch sys := root["system"].(type) {
	case string:
		return isClaudeCodeFirstSystemText(sys)
	case []interface{}:
		if len(sys) > 0 {
			if first, ok := sys[0].(map[string]interface{}); ok {
				if t, _ := first["text"].(string); isClaudeCodeFirstSystemText(t) {
					return true
				}
			}
		}
	}
	return false
}

// remapToolNames renames top-level custom tool definitions to their Claude Code
// equivalents. Typed built-in tools (web_search, code_execution, ...) keep their
// names so server-side tools such as web search continue to work.
func remapToolNames(root map[string]interface{}) {
	tools, ok := root["tools"].([]interface{})
	if !ok {
		return
	}
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if _, hasType := tm["type"]; hasType {
			continue
		}
		if name, _ := tm["name"].(string); name != "" {
			if renamed, ok := claudeCodeToolNames[name]; ok {
				tm["name"] = renamed
			}
		}
	}
}

// ensureClaudeCodeSystem guarantees the request's first system block is the
// official Claude Code identity line, preserving any caller-supplied system
// content after it. It is idempotent: a request that already leads with the
// identity line (i.e. real Claude Code) is left unchanged.
func ensureClaudeCodeSystem(root map[string]interface{}, ttl string) {
	identityBlock := map[string]interface{}{
		"type":          "text",
		"text":          claudeCodeIdentityLine,
		"cache_control": claudeCacheControl(ttl),
	}
	switch sys := root["system"].(type) {
	case nil:
		root["system"] = []interface{}{identityBlock}
	case string:
		if strings.TrimSpace(sys) == "" || strings.HasPrefix(strings.TrimSpace(sys), claudeCodeIdentityLine) {
			root["system"] = []interface{}{identityBlock}
		} else {
			root["system"] = []interface{}{identityBlock, map[string]interface{}{"type": "text", "text": sys}}
		}
	case []interface{}:
		if len(sys) > 0 {
			if first, ok := sys[0].(map[string]interface{}); ok {
				// Real Claude Code leads with EITHER the identity line OR the
				// x-anthropic-billing-header block (which itself precedes the identity
				// line). Recognizing the billing prefix is essential: without it we fail
				// to detect a genuine Claude Code request and prepend a SECOND identity
				// block (double injection). Either marker means "already Claude Code": no-op.
				if t, _ := first["text"].(string); isClaudeCodeFirstSystemText(t) {
					return
				}
			}
		}
		root["system"] = append([]interface{}{identityBlock}, sys...)
	}
}

func normalizeClaudeCodeIdentityCacheTTL(root map[string]interface{}, ttl string) {
	sys, ok := root["system"].([]interface{})
	if !ok {
		return
	}
	for _, item := range sys {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := m["text"].(string)
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, legacyClaudeCodeIdentityLine) {
			// A downstream may still be an older Claude Code build. Keep any
			// environment suffix but align the leading identity with the captured
			// shipping client so the body agrees with the configured shipping UA.
			trimmed = claudeCodeIdentityLine + strings.TrimPrefix(trimmed, legacyClaudeCodeIdentityLine)
			m["text"] = trimmed
		}
		if !strings.HasPrefix(trimmed, claudeCodeIdentityLine) {
			continue
		}
		if strings.TrimSpace(ttl) == "1h" {
			if _, has := m["cache_control"]; has {
				m["cache_control"] = claudeCacheControl(ttl)
			}
		}
		return
	}
}

// isClaudeCodeFirstSystemText reports whether a first system block's text marks the
// request as already coming from Claude Code — either the identity line or the
// x-anthropic-billing-header attribution block that precedes it.
func isClaudeCodeFirstSystemText(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, claudeCodeIdentityLine) || strings.HasPrefix(t, legacyClaudeCodeIdentityLine) || strings.HasPrefix(t, claudeBillingHeaderPrefix)
}

// stripMetadataTelemetry removes known telemetry keys from the request metadata
// that are not required for the API to function but could leak the downstream's
// identity. The real Claude Code embeds device_id / account_uuid / session_id
// inside the user_id JSON string (not as separate metadata keys), so separate
// metadata keys for these fields are a third-party tell. We preserve user_id
// (already set to the virtual value above) and remove the rest.
func stripMetadataTelemetry(md map[string]interface{}) {
	keysToRemove := []string{
		"device_id", "account_uuid", "session_id",
		"user_email", "email", "account_id",
		"machine_id", "installation_id",
		"client_id", "client_version", "browser_id",
	}
	for _, k := range keysToRemove {
		delete(md, k)
	}
}

// claudeVirtualUserID returns the metadata.user_id to present. Real Claude Code 2.1.x
// sends a JSON-STRING value (captured ground truth, capture/out_mock) on both
// OAuth and API-key Claude Code paths:
//
//	{"device_id":"<64hex>","account_uuid":"<uuid|empty>","session_id":"<uuid>"}
//
// (NOT the older user_<hex>_account_<uuid>_session_<uuid> form some references still use).
// account_uuid is empty for the captured third-party relay shape;
// device_id and session_id are account-bound deterministic values. The key ORDER is
// preserved to match the real client byte-shape. API-key/SDK callers keep the plain id.
func claudeVirtualUserID(id identity.Identity, claudeCode bool) string {
	if !claudeCode {
		return id.UserID
	}
	return fmt.Sprintf(`{"device_id":%q,"account_uuid":%q,"session_id":%q}`, id.UserID, "", id.ClaudeSessionID)
}

// normalizeSystemInfo unifies the "Platform:" and "OS Version:" markers in the
// system prompt's environment block to the account's virtual identity, so the
// body's reported OS matches the request headers. Also replaces the real
// username@hostname string (the primary identity leak in the <env> block) and
// the real home directory prefix with the virtual equivalents.
//
// Project paths and working directories are deliberately NOT rewritten — they
// are the model's tool-call input and rewriting them would break tool use.
// Only the identity markers (username@hostname, /home/<realuser> or
// /Users/<realuser> prefix) are replaced.
func normalizeSystemInfo(root map[string]interface{}, id identity.Identity) {
	// Build the virtual home dir for the identity so we can replace the real one.
	// We do NOT use it to rewrite home directory prefixes in paths — policy is
	// never to rewrite project paths. The virtualHome is kept for documentation
	// and as a future extension point.
	_ = id.HomeDir

	apply := func(s string) string {
		s = replaceMarkerValue(s, "Platform:", nodePlatform(id.OSName))
		s = replaceMarkerValue(s, "OS Version:", id.OSRelease)
		// Replace the real username@hostname string wherever it appears as a
		// standalone identity token (e.g. "alice@alice-dev" → "v_usr_01@v_usr_01-dev").
		// We scan for common shell-prompt patterns: "username@hostname" preceded by
		// a line start or whitespace. The replacement is anchored to the "@" delimiter
		// so it never matches a project directory name that happens to contain a dash.
		s = replaceRealUserHost(s, id.Username+"@"+id.Hostname)
		// NOTE: we deliberately do NOT rewrite the home directory PREFIX inside
		// paths. Per project policy, working directories and file paths are NEVER
		// rewritten (the model's tool calls execute on the downstream's real
		// filesystem, so rewriting /home/<realuser>/proj would break tool use).
		// The home directory prefix is part of the project path here. Only the
		// standalone username@hostname identity token above is replaced.
		return s
	}
	switch sys := root["system"].(type) {
	case string:
		root["system"] = apply(sys)
	case []interface{}:
		for _, blk := range sys {
			if bm, ok := blk.(map[string]interface{}); ok {
				if t, ok := bm["text"].(string); ok {
					bm["text"] = apply(t)
				}
			}
		}
	}
}

// replaceRealUserHost replaces real "username@hostname" tokens in the system
// prompt text with the virtual identity's username@hostname. The real username
// is detected by scanning for "@hostname" patterns (a real hostname is usually
// short and alphanumeric-dash); we then replace the full "realuser@hostname"
// with the virtual one. This is deliberately conservative: it only replaces
// tokens that match the common shell-prompt shape and does NOT touch project
// paths or directory names.
func replaceRealUserHost(s, virtualUserHost string) string {
	// Strategy: find "@" followed by a plausible hostname suffix (alphanumeric +
	// dash + dot), then walk backwards to find the username preceding it. Replace
	// the whole "user@host" token.
	// We use a simple scan: find "@", then determine the token boundaries.
	i := 0
	for i < len(s) {
		at := strings.Index(s[i:], "@")
		if at < 0 {
			break
		}
		at += i
		// Walk backwards from "@" to find the username start.
		userStart := at
		for userStart > 0 {
			c := s[userStart-1]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' {
				userStart--
			} else {
				break
			}
		}
		// Walk forward from "@" to find the hostname end.
		hostEnd := at + 1
		for hostEnd < len(s) {
			c := s[hostEnd]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
				hostEnd++
			} else {
				break
			}
		}
		realToken := s[userStart:hostEnd]
		// Only replace if the real token is a plausible user@host shape:
		// the username must be at least 2 chars, the hostname at least 2 chars,
		// and the token is NOT the virtual one (avoid no-op loop).
		if at-userStart >= 2 && hostEnd-at-1 >= 2 && realToken != virtualUserHost {
			s = s[:userStart] + virtualUserHost + s[hostEnd:]
			// Advance past the replacement so we don't re-scan it.
			i = userStart + len(virtualUserHost)
		} else {
			i = hostEnd
		}
	}
	return s
}

// replaceRealHomePrefix replaces known real home directory prefixes (e.g.
// "/home/alice", "/Users/alice") with the virtual home directory. It matches the
// pattern "/home/<word>" or "/Users/<word>" and replaces the prefix with the
// virtual home dir. The trailing boundary (slash, whitespace, or EOL) ensures
// we replace only the home prefix, NEVER the project subdirectory name.
func replaceRealHomePrefix(s, virtualHome string) string {
	for _, prefix := range []string{"/home/", "/Users/"} {
		idx := 0
		for {
			pos := strings.Index(s[idx:], prefix)
			if pos < 0 {
				break
			}
			pos += idx
			start := pos + len(prefix)
			end := start
			for end < len(s) {
				c := s[end]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
					end++
				} else {
					break
				}
			}
			realHome := s[pos:end]
			// Only replace if the real home is different from the virtual one.
			if realHome != virtualHome && end > start {
				s = s[:pos] + virtualHome + s[end:]
				idx = pos + len(virtualHome)
			} else {
				idx = end
			}
		}
	}
	return s
}

// replaceMarkerValue replaces the value following the first occurrence of marker
// (up to end-of-line) with value, preserving the marker and leading whitespace.
// It operates on decoded text (real newlines). A no-op when the marker is absent
// or value is empty.
func replaceMarkerValue(s, marker, value string) string {
	if value == "" {
		return s
	}
	idx := strings.Index(s, marker)
	if idx < 0 {
		return s
	}
	start := idx + len(marker)
	lead := len(s[start:]) - len(strings.TrimLeft(s[start:], " \t"))
	valStart := start + lead
	rest := s[valStart:]
	end := len(rest)
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		end = nl
	}
	return s[:valStart] + value + rest[end:]
}

func injectClaudeNativeAutoContextBreakpoint(root map[string]interface{}, ttl string) bool {
	existing := countCacheControls(root)
	if existing <= 0 || existing >= 4 {
		return false
	}
	msgs, ok := root["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return false
	}
	msg, ok := msgs[0].(map[string]interface{})
	if !ok || msg["role"] != "user" {
		return false
	}
	blocks, ok := msg["content"].([]interface{})
	if !ok || len(blocks) < 2 {
		return false
	}
	auto, ok := blocks[0].(map[string]interface{})
	if !ok {
		return false
	}
	if _, has := auto["cache_control"]; has {
		return false
	}
	if !anthropicwire.IsClaudeCodeAutoContextBlock(blocks[0], blocks[1]) {
		return false
	}
	auto["cache_control"] = claudeCacheControl(ttl)
	return true
}

func claudeCacheControl(ttl string) map[string]interface{} {
	cc := map[string]interface{}{"type": "ephemeral"}
	if strings.TrimSpace(ttl) == "1h" {
		cc["ttl"] = "1h"
	}
	return cc
}

func countCacheControls(root map[string]interface{}) int {
	n := 0
	countBlocks := func(v interface{}) {
		arr, ok := v.([]interface{})
		if !ok {
			return
		}
		for _, item := range arr {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := m["cache_control"]; has {
				n++
			}
		}
	}
	countBlocks(root["system"])
	countBlocks(root["tools"])
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			if m, ok := msg.(map[string]interface{}); ok {
				countBlocks(m["content"])
			}
		}
	}
	return n
}

// capCacheControlBreakpoints enforces Anthropic's limit of at most max
// cache_control breakpoints across system + messages, keeping the first max (the
// largest, earliest, most valuable prefixes) and stripping any excess. Without
// this, prepending the cached Claude Code identity block to a request that already
// used its full 4-breakpoint budget would yield 5 breakpoints and a hard 400.
func capCacheControlBreakpoints(root map[string]interface{}, max int) {
	count := 0
	strip := func(blocks interface{}) {
		arr, ok := blocks.([]interface{})
		if !ok {
			return
		}
		for _, b := range arr {
			m, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := m["cache_control"]; has {
				count++
				if count > max {
					delete(m, "cache_control")
				}
			}
		}
	}
	strip(root["system"])
	strip(root["tools"])
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			if mm, ok := msg.(map[string]interface{}); ok {
				strip(mm["content"])
			}
		}
	}
}

// EnsureClaudeCodeBillingHeader makes the request carry a well-formed Claude Code
// x-anthropic-billing-header system block as system[0], matching captured ground truth
// (capture/out_v2): a text block, NO cache_control, of the form
//
//	x-anthropic-billing-header: cc_version=<version>.503; cc_entrypoint=cli;
//
// It runs after other virtualization/cache-control injection so the attribution
// attribution block is the final request shape.
// Behavior:
//   - real Claude Code (block already present): the block is REPLACED so cc_version is
//     realigned to our account-bound claude-cli version (the downstream's real version
//     would otherwise contradict our User-Agent).
//   - other clients (OpenAI-compat→Claude, etc.): the block is PREPENDED, so the request
//     is not missing the header every genuine Claude Code request carries.
//
// version is the SAME claude-cli version emitted in the User-Agent, so cc_version and the
// UA agree. Apply only to OAuth/Claude-Code traffic.
func EnsureClaudeCodeBillingHeader(body []byte, version string) []byte {
	var root map[string]interface{}
	if decodeClaudeJSONObject(body, &root) != nil {
		return body
	}
	if !setClaudeBillingBlock(root, version) {
		return body
	}
	if out, err := anthropicwire.MarshalPreservingOrder(body, root); err == nil {
		return out
	}
	return body
}

// decodeClaudeJSONObject keeps JSON numbers as json.Number so a request that is
// normalized and re-marshaled cannot silently round integer tool inputs through
// float64. The second decode preserves json.Unmarshal's single-value contract.
func decodeClaudeJSONObject(body []byte, root *map[string]interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(root); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// setClaudeBillingBlock injects/replaces the Claude Code x-anthropic-billing-header
// system block on an already-parsed request root, returning whether root["system"] is
// now a usable shape (false only for an unexpected non-string/array/nil system type, so
// the byte-level caller can leave the body untouched). Factored out of
// EnsureClaudeCodeBillingHeader so VirtualizeClaudeCode can fold the stamp into its
// single parse/marshal pass without a second JSON round-trip. See
// EnsureClaudeCodeBillingHeader for the fingerprint rationale.
func setClaudeBillingBlock(root map[string]interface{}, version string) bool {
	attrs := claudeBillingAttrs{entrypoint: "cli"}
	switch sys := root["system"].(type) {
	case []interface{}:
		idx := -1
		for i, blk := range sys {
			if bm, ok := blk.(map[string]interface{}); ok {
				if t, _ := bm["text"].(string); strings.HasPrefix(strings.TrimSpace(t), claudeBillingHeaderPrefix) {
					attrs = parseClaudeBillingAttrs(t)
					idx = i
					break
				}
			}
		}
		billing := map[string]interface{}{"type": "text", "text": claudeBillingHeaderTextForAttrs(version, attrs)}
		if idx >= 0 {
			sys[idx] = billing
			root["system"] = sys
		} else {
			root["system"] = append([]interface{}{billing}, sys...)
		}
	case string:
		billing := map[string]interface{}{"type": "text", "text": claudeBillingHeaderTextForAttrs(version, attrs)}
		root["system"] = []interface{}{billing, map[string]interface{}{"type": "text", "text": sys}}
	case nil:
		billing := map[string]interface{}{"type": "text", "text": claudeBillingHeaderTextForAttrs(version, attrs)}
		root["system"] = []interface{}{billing}
	default:
		return false
	}
	return true
}

// claudeBillingAttrs holds the fields of a downstream x-anthropic-billing-header block that
// we PROJECT onto the block we emit instead of overwriting. cc_version is deliberately NOT
// in here: it is realigned to our account-bound claude-cli version so it agrees with our
// User-Agent.
type claudeBillingAttrs struct {
	entrypoint string
	// subagent is the verbatim cc_is_subagent value ("true"/"false"), or "" when the
	// downstream block carried no such field.
	subagent string
}

// parseClaudeBillingAttrs pulls the projectable fields out of a downstream billing block.
//
// cc_is_subagent is emitted by genuine Claude Code whenever the request comes from a
// subagent (a Task-tool invocation) rather than the main conversation; an observed real
// block reads:
//
//	x-anthropic-billing-header: cc_version=2.1.226.503; cc_entrypoint=cli; cc_is_subagent=true;
//
// Dropping it was a coherence leak in the same class the cc_entrypoint projection already
// guards against. A subagent request is independently recognizable from its body (subagent
// system prompt, no human-authored user turn), so stripping the marker left the body saying
// "subagent" while the attribution block said "main agent" — a contradiction the real
// client never produces. Fleet-wide it also pinned every account at a 0% subagent rate,
// which for an account doing heavy agentic work is itself anomalous.
//
// We only ever PASS THROUGH what the downstream sent and never synthesize the field, so
// this cannot invent a marker the client did not claim. Unrecognized values are dropped
// rather than forwarded, so a downstream cannot inject arbitrary text into the block.
func parseClaudeBillingAttrs(text string) claudeBillingAttrs {
	attrs := claudeBillingAttrs{entrypoint: "cli"}
	fields := strings.TrimSpace(text)
	fields = strings.TrimPrefix(fields, claudeBillingHeaderPrefix)
	for _, field := range strings.Split(fields, ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "cc_entrypoint":
			if strings.EqualFold(strings.TrimSpace(value), "sdk-cli") {
				attrs.entrypoint = "sdk-cli"
			}
		case "cc_is_subagent":
			switch v := strings.ToLower(strings.TrimSpace(value)); v {
			case "true", "false":
				attrs.subagent = v
			}
		}
	}
	return attrs
}

// claudeBillingHeaderTextForAttrs renders the block, keeping the real client's field order
// (cc_version, cc_entrypoint, then cc_is_subagent when present).
func claudeBillingHeaderTextForAttrs(version string, attrs claudeBillingAttrs) string {
	out := claudeBillingHeaderTextForEntrypoint(version, attrs.entrypoint)
	if attrs.subagent != "" {
		out += fmt.Sprintf(" cc_is_subagent=%s;", attrs.subagent)
	}
	return out
}

// claudeBillingHeaderText builds the billing-header block for a claude-cli version.
// Shipping 2.1.226 emits the fixed .503 build component and no cch field.
// The launch entrypoint is projected from an existing native billing block.
func claudeBillingHeaderText(version string) string {
	return claudeBillingHeaderTextForEntrypoint(version, "cli")
}

func claudeBillingHeaderTextForEntrypoint(version, entrypoint string) string {
	if version == "" {
		version = identity.ClaudeCLIVersion
	}
	if entrypoint != "sdk-cli" {
		entrypoint = "cli"
	}
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s;",
		version, identity.ClaudeCodeBuild, entrypoint)
}

// nodePlatform maps a virtual OS name to the Node.js process.platform value
// Claude Code reports in its environment block.
func nodePlatform(osName string) string {
	switch osName {
	case "Mac OS":
		return "darwin"
	case "Linux":
		return "linux"
	case "Windows":
		return "win32"
	default:
		return ""
	}
}

// pseudonym is a stable, value-erasing replacement for a sensitive string. It is
// deterministic so the same secret maps consistently within and across requests.
func pseudonym(word string) string {
	sum := sha256.Sum256([]byte(word))
	return "x" + hex.EncodeToString(sum[:])[:8]
}
