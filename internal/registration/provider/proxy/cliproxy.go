// Package proxy provides rotating-residential proxy support for the registration
// pipeline. It is a Go port of other_project/backend/providers/proxy/cliproxy.py:
// cliproxy.io issues a fresh exit IP for every distinct "sid-" session token embedded
// in the proxy username, so generating a new sid per registration rotates the residential
// IP without any extra API call.
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Cliproxy holds the static credentials for a cliproxy.io residential gateway. URL()
// returns a fresh, IP-rotating proxy URL each call.
type Cliproxy struct {
	Host     string
	Port     int
	Account  string
	Password string
	Region   string
	TTL      int // session lifetime in minutes
}

// sidRe matches the "-sid-<token>" segment inside a cliproxy username so it can be
// swapped for a fresh session token (rotating the exit IP).
var sidRe = regexp.MustCompile(`-sid-[^-:@/]+`)

// regionRe matches the "-region-<code>" segment so the exit country can be pinned.
var regionRe = regexp.MustCompile(`-region-[^-:@/]+`)

// freshSID returns a random 8-hex-char session token.
func freshSID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// URL builds a proxy URL with a freshly generated session id (rotating residential IP).
func (c *Cliproxy) URL() string {
	region := c.Region
	if region == "" {
		region = "US"
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 15
	}
	user := fmt.Sprintf("%s-region-%s-sid-%s-t-%d", c.Account, region, freshSID(), ttl)
	return fmt.Sprintf("http://%s:%s@%s:%d", user, c.Password, c.Host, c.Port)
}

// ParseSpec parses the operator-facing "host:port:username:password" cliproxy spec, e.g.
//
//	proxy.example:3010:account-region-US-sid-example-t-15:password
//
// The username already encodes region / sid / ttl; ParseSpec extracts the static account,
// region and ttl so URL() can regenerate the sid. The literal sid in the spec is ignored
// (it is only a sample session token).
func ParseSpec(spec string) (*Cliproxy, error) {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	if len(parts) < 4 {
		return nil, fmt.Errorf("cliproxy spec must be host:port:user:pass, got %q", spec)
	}
	host := parts[0]
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("cliproxy port: %w", err)
	}
	// The password may itself contain ':' in theory; everything after the username field
	// is the password. The username is parts[2].
	user := parts[2]
	pass := strings.Join(parts[3:], ":")

	c := &Cliproxy{Host: host, Port: port, Password: pass, Region: "US", TTL: 15}
	// Account is the username up to the first "-region-" (fallback: whole username).
	c.Account = user
	if i := strings.Index(user, "-region-"); i >= 0 {
		c.Account = user[:i]
	}
	if m := regexp.MustCompile(`-region-([^-]+)`).FindStringSubmatch(user); len(m) == 2 {
		c.Region = m[1]
	}
	if m := regexp.MustCompile(`-t-(\d+)`).FindStringSubmatch(user); len(m) == 2 {
		if t, e := strconv.Atoi(m[1]); e == nil {
			c.TTL = t
		}
	}
	return c, nil
}

// RotateSID returns proxyURL with its "-sid-<token>" segment replaced by a fresh session
// token, so the next request through it gets a new residential exit IP. If the URL has no
// sid segment it is returned unchanged. This lets the pipeline rotate a cliproxy egress
// stored as a plain proxy URL in an egress profile, without re-parsing credentials.
func RotateSID(proxyURL string) string {
	if !strings.Contains(proxyURL, "-sid-") {
		return proxyURL
	}
	return sidRe.ReplaceAllString(proxyURL, "-sid-"+freshSID())
}

// IsCliproxy reports whether a proxy URL looks like a cliproxy rotating-session endpoint.
func IsCliproxy(proxyURL string) bool {
	return strings.Contains(proxyURL, "cliproxy") || strings.Contains(proxyURL, "-sid-")
}

// WithRegion pins the cliproxy exit country by replacing the "-region-<code>" segment with
// region and rotating the sid (so the new exit IP is in that country). This is used to keep
// the residential IP country aligned with the SMS number's country during add-phone
// verification — a random region ("-region-Rand") raises OpenAI's phone-gate and ban rates.
// If proxyURL has no region segment it is returned with only the sid rotated.
func WithRegion(proxyURL, region string) string {
	if region == "" {
		return RotateSID(proxyURL)
	}
	out := proxyURL
	if regionRe.MatchString(out) {
		out = regionRe.ReplaceAllString(out, "-region-"+region)
	}
	return RotateSID(out)
}
