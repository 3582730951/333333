// Package proxyparse turns the operator-friendly proxy formats the admin UI
// accepts — four separate fields (host / port / user / pass) and the batch
// "host:port:username:password" line format — into a canonical proxy endpoint
// URL for an egress profile. It is pure (no I/O) so it is trivially testable.
package proxyparse

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Draft is a parsed proxy specification, ready to become an egress endpoint.
type Draft struct {
	Host     string
	Port     string
	Username string
	Password string
	// Scheme is populated only when the operator supplied one explicitly.  It
	// lets a pasted socks5:// URL override a stale HTTP selection in the form.
	Scheme string
}

// SchemeFor maps an egress profile type to a URL scheme.
func SchemeFor(egressType string) string {
	switch strings.ToLower(strings.TrimSpace(egressType)) {
	case "http_proxy":
		return "http"
	case "https_proxy":
		return "https"
	case "socks5_proxy":
		return "socks5"
	case "socks5h_proxy", "":
		return "socks5h"
	default:
		return "socks5h"
	}
}

// EgressTypeForScheme maps a proxy URL scheme back to the storage profile type.
func EgressTypeForScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		return "http_proxy"
	case "https":
		return "https_proxy"
	case "socks5":
		return "socks5_proxy"
	case "socks5h":
		return "socks5h_proxy"
	default:
		return ""
	}
}

// Endpoint renders the canonical scheme://[user:pass@]host:port endpoint URL for
// the given egress type. IPv6 hosts are bracketed.
func (d Draft) Endpoint(egressType string) string {
	hp := joinHostPort(d.Host, d.Port)
	scheme := SchemeFor(egressType)
	if EgressTypeForScheme(d.Scheme) != "" {
		scheme = strings.ToLower(strings.TrimSpace(d.Scheme))
	}
	if strings.TrimSpace(d.Username) != "" {
		return scheme + "://" + url.UserPassword(d.Username, d.Password).String() + "@" + hp
	}
	return scheme + "://" + hp
}

// EgressType returns the profile type implied by an explicit URL scheme, or
// fallback when the compact input did not include a scheme.
func (d Draft) EgressType(fallback string) string {
	if inferred := EgressTypeForScheme(d.Scheme); inferred != "" {
		return inferred
	}
	if strings.TrimSpace(fallback) == "" {
		return "socks5h_proxy"
	}
	return strings.ToLower(strings.TrimSpace(fallback))
}

func joinHostPort(host, port string) string {
	host = strings.TrimSpace(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

func (d Draft) validate() (Draft, error) {
	d.Host = strings.Trim(strings.TrimSpace(d.Host), "[]")
	d.Port = strings.TrimSpace(d.Port)
	d.Username = strings.TrimSpace(d.Username)
	if d.Host == "" || d.Port == "" {
		return Draft{}, fmt.Errorf("proxy needs host and port")
	}
	port, err := strconv.Atoi(d.Port)
	if err != nil || port < 1 || port > 65535 {
		return Draft{}, fmt.Errorf("proxy port must be an integer between 1 and 65535")
	}
	if EgressTypeForScheme(d.Scheme) == "" && strings.TrimSpace(d.Scheme) != "" {
		return Draft{}, fmt.Errorf("proxy scheme must be http, https, socks5, or socks5h")
	}
	return d, nil
}

// FromFields builds a Draft from the four-input-box form.
func FromFields(host, port, user, pass string) (Draft, error) {
	return Draft{
		Host:     strings.TrimSpace(host),
		Port:     strings.TrimSpace(port),
		Username: strings.TrimSpace(user),
		Password: pass,
	}.validate()
}

// ParseLine parses a single proxy line. Accepted forms:
//
//	host:port:username:password   (the batch format)
//	host:port:username
//	host:port
//	username:password@host:port
//	host:port@username:password
//	scheme://username:password@host:port
//
// A password may itself contain ':' (only the first three colons are split).
func ParseLine(line string) (Draft, error) {
	s := strings.TrimSpace(line)
	if s == "" {
		return Draft{}, fmt.Errorf("empty line")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return Draft{}, fmt.Errorf("proxy URL must contain only scheme, credentials, host, and port")
		}
		d := Draft{Host: u.Hostname(), Port: u.Port(), Scheme: strings.ToLower(u.Scheme)}
		if u.User != nil {
			d.Username = u.User.Username()
			d.Password, _ = u.User.Password()
		}
		return d.validate()
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		left, right := strings.TrimSpace(s[:at]), strings.TrimSpace(s[at+1:])
		// Standard form wins whenever the right side is a valid host:port.
		// Otherwise accept the provider-export form host:port@user:pass.
		if host, port, err := splitHostPort(right); err == nil {
			user, pass, ok := strings.Cut(left, ":")
			if !ok || strings.TrimSpace(user) == "" {
				return Draft{}, fmt.Errorf("proxy credentials must contain username:password")
			}
			return Draft{Host: host, Port: port, Username: user, Password: pass}.validate()
		}
		host, port, err := splitHostPort(left)
		if err != nil {
			return Draft{}, fmt.Errorf("proxy input must contain a valid host and numeric port")
		}
		user, pass, ok := strings.Cut(right, ":")
		if !ok || strings.TrimSpace(user) == "" {
			return Draft{}, fmt.Errorf("proxy credentials must contain username:password")
		}
		return Draft{Host: host, Port: port, Username: user, Password: pass}.validate()
	}
	parts := strings.SplitN(s, ":", 4)
	switch len(parts) {
	case 2:
		return Draft{Host: parts[0], Port: parts[1]}.validate()
	case 3:
		return Draft{Host: parts[0], Port: parts[1], Username: parts[2]}.validate()
	case 4:
		return Draft{Host: parts[0], Port: parts[1], Username: parts[2], Password: parts[3]}.validate()
	default:
		return Draft{}, fmt.Errorf("unrecognized proxy format (expected host:port:user:pass or user:pass@host:port)")
	}
}

// NormalizeEndpoint accepts every operator-facing compact form and returns a
// canonical, URL-escaped endpoint plus the matching egress type.
func NormalizeEndpoint(value, fallbackType string) (string, string, error) {
	draft, err := ParseLine(value)
	if err != nil {
		return "", "", err
	}
	typ := draft.EgressType(fallbackType)
	return draft.Endpoint(typ), typ, nil
}

// ParseLines parses a multi-line batch, skipping blank lines and #-comments.
// It returns the successfully parsed drafts and a slice of per-line errors.
func ParseLines(text string) ([]Draft, []error) {
	var drafts []Draft
	var errs []error
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d, err := ParseLine(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
			continue
		}
		drafts = append(drafts, d)
	}
	return drafts, errs
}

func splitHostPort(hp string) (string, string, error) {
	hp = strings.TrimSpace(hp)
	if host, port, err := net.SplitHostPort(hp); err == nil {
		if _, validateErr := (Draft{Host: host, Port: port}).validate(); validateErr == nil {
			return host, port, nil
		}
	}
	if i := strings.LastIndex(hp, ":"); i > 0 {
		host, port := hp[:i], hp[i+1:]
		if draft, err := (Draft{Host: host, Port: port}).validate(); err == nil {
			return draft.Host, draft.Port, nil
		}
	}
	return "", "", fmt.Errorf("host:port must use a numeric port")
}
