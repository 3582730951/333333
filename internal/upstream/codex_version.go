package upstream

import (
	"net/url"
	"strconv"
	"strings"

	"codex-account-pool/internal/config"
)

// codexProtocolProfile is the request-shape contract for one Codex CLI release.
// The effective version is resolved once per request and then reused by headers,
// HTTP client_metadata, and WebSocket client_metadata so a hot config update cannot
// produce a mixed-version fingerprint inside one upstream request.
type codexProtocolProfile struct {
	version                 string
	fingerprintVersion      string
	requiredBetaFeatures    string
	codeModeToolNames       bool
	parentTurnID            bool
	promptCacheKeyBySession bool
	turnMetadataSchema      config.CodexTurnMetadataSchema
}

func codexProtocolProfileForVersion(version string) codexProtocolProfile {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if fingerprint, ok := config.CodexCLIFingerprintForVersion(version); ok {
		return codexProtocolProfile{
			version:                 fingerprint.Version,
			fingerprintVersion:      fingerprint.Version,
			requiredBetaFeatures:    fingerprint.RequiredBetaFeatures,
			codeModeToolNames:       fingerprint.CodeModeToolNames,
			parentTurnID:            fingerprint.ParentTurnID,
			promptCacheKeyBySession: fingerprint.PromptCacheKeyBySession,
			turnMetadataSchema:      fingerprint.TurnMetadataSchema,
		}
	}
	// Explicit operator/model pins outside the built-in window remain supported.
	// They retain their requested version string and use the conservative feature
	// thresholds that predated the exact library. Automatic downstream detection
	// never reaches this path because it accepts exact library entries only.
	return codexProtocolProfile{
		version:                 version,
		requiredBetaFeatures:    codexBetaFeaturesHeader,
		codeModeToolNames:       codexVersionAtLeast(version, "0.146.0"),
		parentTurnID:            codexVersionAtLeast(version, "0.147.0"),
		promptCacheKeyBySession: codexVersionAtLeast(version, "0.145.0"),
		turnMetadataSchema: func() config.CodexTurnMetadataSchema {
			if codexVersionAtLeast(version, "0.151.0") {
				return config.CodexTurnMetadataSchemaWindowNumber
			}
			return config.CodexTurnMetadataSchemaLegacy
		}(),
	}
}

func codexVersionAtLeast(version, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		var out [3]int
		value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
		if cut := strings.IndexAny(value, "-+"); cut >= 0 {
			value = value[:cut]
		}
		parts := strings.Split(value, ".")
		if len(parts) != len(out) {
			return out, false
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	got, gotOK := parse(version)
	want, wantOK := parse(minimum)
	if !gotOK || !wantOK {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

func (c *Client) resolveCodexClientVersion(spec Request) string {
	identityVersion := ""
	if spec.CodexIdentity != nil {
		device := c.codexDevice(spec.Account.ID, spec.Egress.ID, spec.CodexIdentity.DeviceOSHint)
		identityVersion = device.CodexCLIVersion
	} else {
		identityVersion = c.identityForOS(spec.Account.ID, spec.OSHint).CodexCLIVersion
	}
	version := c.cfgSnapshot().CodexCLIVersionOrDefault(identityVersion)
	if downstream := analyzeCodexDownstreamVersion(spec); downstream.version != "" {
		version = downstream.version
	}
	if override := strings.TrimSpace(spec.CodexClientVersion); override != "" {
		version = override
	}
	if strings.TrimSpace(version) == "" {
		return config.DefaultClientVersion
	}
	return strings.TrimSpace(version)
}

type codexDownstreamVersionAnalysis struct {
	version            string
	fingerprintVersion string
	source             string
	conflict           bool
	unsupported        bool
}

type codexVersionEvidence struct {
	source string
	value  string
}

// analyzeCodexDownstreamVersion combines every bounded, version-bearing signal
// available before dispatch. Automatic selection is fail-closed: all non-empty
// signals must be valid and agree, and the result must have an exact entry in the
// current fingerprint library. This avoids emitting a 0.148 User-Agent with an
// older request body merely because one wrapper retained a stale query/header.
func analyzeCodexDownstreamVersion(spec Request) codexDownstreamVersionAnalysis {
	evidence := make([]codexVersionEvidence, 0, 8)
	appendHeaderEvidence := func(name, source string) {
		for key, values := range spec.Headers {
			if !strings.EqualFold(strings.TrimSpace(key), name) {
				continue
			}
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					evidence = append(evidence, codexVersionEvidence{source: source, value: value})
				}
			}
		}
	}
	appendHeaderEvidence("version", "header")
	appendHeaderEvidence("x-codex-client-version", "codex_header")
	for key, values := range spec.Headers {
		if !strings.EqualFold(strings.TrimSpace(key), "User-Agent") {
			continue
		}
		for _, value := range values {
			if !isRecognizedCodexOriginator(codexUserAgentOriginator(value)) {
				continue
			}
			if version := codexUserAgentVersion(value); version != "" {
				evidence = append(evidence, codexVersionEvidence{source: "user_agent", value: version})
			}
		}
	}
	if parsed, err := url.Parse(strings.TrimSpace(spec.DownstreamPath)); err == nil {
		if values, err := url.ParseQuery(parsed.RawQuery); err == nil {
			for _, value := range values["client_version"] {
				if strings.TrimSpace(value) != "" {
					evidence = append(evidence, codexVersionEvidence{source: "query", value: value})
				}
			}
		}
	}
	// BodyMeta is already a bounded single-pass scan owned by the caller. Consult
	// its scalar without forcing an otherwise streaming/spooled body into memory.
	if spec.BodyMeta != nil || spec.bodyLoaded {
		if version := requestCodexBodyString(spec, "client_version"); version != "" {
			evidence = append(evidence, codexVersionEvidence{source: "body", value: version})
		}
	}

	analysis := codexDownstreamVersionAnalysis{}
	sources := make([]string, 0, len(evidence))
	for _, candidate := range evidence {
		version, ok := canonicalCodexVersion(candidate.value)
		if !ok {
			analysis.conflict = true
			return analysis
		}
		if analysis.version != "" && analysis.version != version {
			analysis.version = ""
			analysis.conflict = true
			return analysis
		}
		analysis.version = version
		sources = append(sources, candidate.source)
	}
	if analysis.version == "" {
		return analysis
	}
	fingerprint, ok := config.CodexCLIFingerprintForVersion(analysis.version)
	if !ok {
		analysis.version = ""
		analysis.unsupported = true
		return analysis
	}
	analysis.fingerprintVersion = fingerprint.Version
	analysis.source = strings.Join(uniqueStrings(sources), "+")
	return analysis
}

// stripCodexResponsesClientVersionQuery removes the downstream-only profile hint
// after selection. The official client uses client_version on /models, so catalog
// requests retain it byte-for-byte; Responses requests must not acquire an extra
// upstream query parameter merely because a wrapper supplied version evidence.
func stripCodexResponsesClientVersionQuery(path string) string {
	question := strings.IndexByte(path, '?')
	if question < 0 || !strings.Contains(strings.ToLower(path[:question]), "/responses") {
		return path
	}
	base, rawQuery := path[:question], path[question+1:]
	fragment := ""
	if hash := strings.IndexByte(rawQuery, '#'); hash >= 0 {
		fragment, rawQuery = rawQuery[hash:], rawQuery[:hash]
	}
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]
	for _, part := range parts {
		key := part
		if equals := strings.IndexByte(key, '='); equals >= 0 {
			key = key[:equals]
		}
		decoded, err := url.QueryUnescape(key)
		if err == nil && decoded == "client_version" {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 || len(kept) == 1 && kept[0] == "" {
		return base + fragment
	}
	return base + "?" + strings.Join(kept, "&") + fragment
}

func canonicalCodexVersion(value string) (string, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || len(part) > 9 {
			return "", false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		if len(part) > 1 && part[0] == '0' {
			return "", false
		}
	}
	return value, true
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (c *Client) codexClientVersionForRequest(spec Request) string {
	if version := strings.TrimSpace(spec.codexResolvedClientVersion); version != "" {
		return version
	}
	return c.resolveCodexClientVersion(spec)
}

func (c *Client) codexProtocolProfileForRequest(spec Request) codexProtocolProfile {
	return codexProtocolProfileForVersion(c.codexClientVersionForRequest(spec))
}
