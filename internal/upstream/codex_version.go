package upstream

import (
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
	codeModeToolNames       bool
	parentTurnID            bool
	promptCacheKeyBySession bool
}

func codexProtocolProfileForVersion(version string) codexProtocolProfile {
	version = strings.TrimSpace(version)
	return codexProtocolProfile{
		version:                 version,
		codeModeToolNames:       codexVersionAtLeast(version, "0.146.0"),
		parentTurnID:            codexVersionAtLeast(version, "0.147.0"),
		promptCacheKeyBySession: codexVersionAtLeast(version, "0.145.0"),
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
	if downstream := codexSupportedClientVersion(spec.Headers); downstream != "" {
		version = downstream
	}
	if override := strings.TrimSpace(spec.CodexClientVersion); override != "" {
		version = override
	}
	if strings.TrimSpace(version) == "" {
		return config.DefaultClientVersion
	}
	return strings.TrimSpace(version)
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
