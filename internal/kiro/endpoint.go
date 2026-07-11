package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
)

var ErrEndpointNotAllowed = errors.New("kiro endpoint is not allowlisted")

// ValidateEndpoint authorizes the host before any bearer token is attached. Empty
// endpoints resolve to the official regional Amazon Q host. Custom/private test
// endpoints require an exact host (including port, when present) in the admin
// allowlist.
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

func officialKiroHost(host string) bool {
	if !strings.HasPrefix(host, "q.") || !strings.HasSuffix(host, ".amazonaws.com") {
		return false
	}
	region := strings.TrimSuffix(strings.TrimPrefix(host, "q."), ".amazonaws.com")
	if region == "" || strings.Contains(region, ".") {
		return false
	}
	for _, r := range region {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	// Reject numeric IP spellings that happen to pass textual suffix checks.
	return net.ParseIP(host) == nil
}
