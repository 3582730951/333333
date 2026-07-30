package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var ErrEndpointNotAllowed = errors.New("kiro endpoint is not allowlisted")

// ValidateEndpoint authorizes the configured host before any bearer token is
// attached. Empty endpoints retain the legacy q.<region>.amazonaws.com
// compatibility value; the operation helpers below translate official values to
// the Kiro CLI runtime/management service plane. Custom/private test endpoints
// require an exact host (including port, when present) in the admin allowlist.
func ValidateEndpoint(raw, region string, allowlist []string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		region = normalizeRegion(region)
		if !validRegion(region) {
			return "", ErrEndpointNotAllowed
		}
		return "https://q." + region + ".amazonaws.com", nil
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", ErrEndpointNotAllowed
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "https" && officialKiroHost(host) {
		return strings.TrimRight(parsed.String(), "/"), nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrEndpointNotAllowed
	}
	wantedHostPort := strings.ToLower(parsed.Host)
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		allowedHostPort := strings.ToLower(entry)
		if allowed, parseErr := url.Parse(entry); parseErr == nil && allowed.Host != "" {
			allowedHostPort = strings.ToLower(allowed.Host)
		}
		if wantedHostPort == allowedHostPort {
			return strings.TrimRight(parsed.String(), "/"), nil
		}
	}
	return "", ErrEndpointNotAllowed
}

func normalizeRegion(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "us-east-1"
	}
	return region
}

// validRegion deliberately accepts only the DNS-label form used by AWS regions.
// Region values come from imported credential files; validating them before URL
// interpolation prevents values containing '@', '/', dots, or control characters
// from changing the authority of an otherwise official-looking endpoint.
func validRegion(region string) bool {
	if region == "" || len(region) > 63 || region[0] == '-' || region[len(region)-1] == '-' {
		return false
	}
	for _, r := range region {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func EndpointHash(raw, region string, allowlist []string) (string, error) {
	validated, err := ValidateEndpoint(raw, region, allowlist)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(validated)
	if err != nil {
		return "", err
	}
	normalized := strings.ToLower(parsed.Scheme+"://"+parsed.Host) + strings.TrimRight(parsed.EscapedPath(), "/")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:]), nil
}

// GenerateAssistantResponseEndpoint, ListAvailableModelsEndpoint, and
// GetUsageLimitsEndpoint keep the service plane separate while retaining the
// legacy operation paths of an explicitly allowlisted compatibility endpoint.
// The official Kiro CLI uses the runtime host for generation and the management
// host for catalog/profile/usage operations, all at the root path.
func GenerateAssistantResponseEndpoint(raw, region string, allowlist []string) (string, error) {
	return operationEndpoint(raw, region, allowlist, "runtime", "/generateAssistantResponse")
}

func ListAvailableModelsEndpoint(raw, region string, allowlist []string) (string, error) {
	return operationEndpoint(raw, region, allowlist, "management", "/listAvailableModels")
}

func GetUsageLimitsEndpoint(raw, region string, allowlist []string) (string, error) {
	return operationEndpoint(raw, region, allowlist, "management", "/getUsageLimits")
}

func operationEndpoint(raw, region string, allowlist []string, plane, compatibilityPath string) (string, error) {
	validated, err := ValidateEndpoint(raw, region, allowlist)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(validated)
	if err != nil {
		return "", ErrEndpointNotAllowed
	}
	if endpointRegion, official := officialKiroRegion(strings.ToLower(parsed.Hostname())); official {
		return fmt.Sprintf("https://%s.%s.kiro.dev/", plane, endpointRegion), nil
	}

	// An administrator may explicitly point at an allowlisted compatibility
	// service. Preserve that authority and any base path instead of rewriting it
	// to a public Kiro host.
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	for _, suffix := range []string{"/generateAssistantResponse", "/getUsageLimits", "/listAvailableModels"} {
		parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), suffix)
	}
	parsed.RawPath = ""
	base := strings.TrimRight(parsed.String(), "/")
	if base == "" {
		return "", ErrEndpointNotAllowed
	}
	return base + compatibilityPath, nil
}

func officialKiroHost(host string) bool {
	_, ok := officialKiroRegion(strings.ToLower(strings.TrimSpace(host)))
	return ok
}

func officialKiroRegion(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	var region string
	switch {
	case strings.HasPrefix(host, "q.") && strings.HasSuffix(host, ".amazonaws.com"):
		region = strings.TrimSuffix(strings.TrimPrefix(host, "q."), ".amazonaws.com")
	case strings.HasPrefix(host, "runtime.") && strings.HasSuffix(host, ".kiro.dev"):
		region = strings.TrimSuffix(strings.TrimPrefix(host, "runtime."), ".kiro.dev")
	case strings.HasPrefix(host, "management.") && strings.HasSuffix(host, ".kiro.dev"):
		region = strings.TrimSuffix(strings.TrimPrefix(host, "management."), ".kiro.dev")
	default:
		return "", false
	}
	if region == "" || strings.Contains(region, ".") || !validRegion(region) {
		return "", false
	}
	// Reject numeric IP spellings that happen to pass textual suffix checks.
	return region, net.ParseIP(host) == nil
}
