// Package proxyparse turns the operator-friendly proxy formats the admin UI
// accepts — four separate fields (host / port / user / pass) and the batch
// "host:port:username:password" line format — into a canonical proxy endpoint
// URL for an egress profile. It is pure (no I/O) so it is trivially testable.
package proxyparse

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Draft is a parsed proxy specification, ready to become an egress endpoint.
type Draft struct {
	Host     string
	Port     string
	Username string
	Password string
}

// SchemeFor maps an egress profile type to a URL scheme.
func SchemeFor(egressType string) string {
	switch egressType {
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

// Endpoint renders the canonical scheme://[user:pass@]host:port endpoint URL for
// the given egress type. IPv6 hosts are bracketed.
func (d Draft) Endpoint(egressType string) string {
	hp := joinHostPort(d.Host, d.Port)
	scheme := SchemeFor(egressType)
	if strings.TrimSpace(d.Username) != "" {
		return scheme + "://" + url.UserPassword(d.Username, d.Password).String() + "@" + hp
	}
	return scheme + "://" + hp
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
	if strings.TrimSpace(d.Host) == "" || strings.TrimSpace(d.Port) == "" {
		return Draft{}, fmt.Errorf("proxy needs host and port")
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
		if err != nil || u.Host == "" {
			return Draft{}, fmt.Errorf("bad proxy url %q", line)
		}
		d := Draft{Host: u.Hostname(), Port: u.Port()}
		if u.User != nil {
			d.Username = u.User.Username()
			d.Password, _ = u.User.Password()
		}
		return d.validate()
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		cred, hp := s[:at], s[at+1:]
		host, port, err := splitHostPort(hp)
		if err != nil {
			return Draft{}, err
		}
		user, pass, _ := strings.Cut(cred, ":")
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
		return Draft{}, fmt.Errorf("unrecognized proxy format %q (expect host:port:username:password)", line)
	}
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
		return host, port, nil
	}
	if i := strings.LastIndex(hp, ":"); i > 0 {
		return hp[:i], hp[i+1:], nil
	}
	return "", "", fmt.Errorf("bad host:port %q", hp)
}
