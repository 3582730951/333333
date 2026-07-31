package mailbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// mailboxNetworkGuard validates every provider request, including redirect
// destinations. Admin-configured mailbox endpoints are an outbound integration
// surface, so private, link-local, multicast, and metadata-network destinations
// are rejected. A loopback origin is admitted only when the initially configured
// endpoint is itself loopback over HTTP, which keeps deterministic fixture tests
// possible without widening production egress.
type mailboxNetworkGuard struct {
	base          http.RoundTripper
	loopbackHost  string
	allowLoopback bool
}

func newGuardedMailboxHTTPClient(base *http.Client, initialURL string) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	cloned := *base
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	guard := &mailboxNetworkGuard{base: transport}
	if parsed, err := url.Parse(strings.TrimSpace(initialURL)); err == nil &&
		strings.EqualFold(parsed.Scheme, "http") && isMailboxLoopbackHost(parsed.Hostname()) {
		guard.allowLoopback = true
		guard.loopbackHost = strings.ToLower(strings.Trim(strings.TrimSpace(parsed.Hostname()), "[]"))
	}
	cloned.Transport = guard
	return &cloned
}

func (g *mailboxNetworkGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("mailbox endpoint request is invalid")
	}
	if err := validateMailboxNetworkURL(req.Context(), req.URL, g.loopbackHost, g.allowLoopback); err != nil {
		return nil, err
	}
	return g.base.RoundTrip(req)
}

func validateMailboxNetworkURL(
	ctx context.Context,
	endpoint *url.URL,
	loopbackHost string,
	allowLoopback bool,
) error {
	if endpoint == nil || endpoint.Hostname() == "" || endpoint.User != nil {
		return errors.New("mailbox endpoint must be an absolute URL without credentials")
	}
	scheme := strings.ToLower(strings.TrimSpace(endpoint.Scheme))
	if scheme != "https" && scheme != "http" {
		return errors.New("mailbox endpoint uses an unsupported scheme")
	}
	host := strings.ToLower(strings.Trim(strings.TrimSpace(endpoint.Hostname()), "[]"))
	if scheme == "http" && !(allowLoopback && host == loopbackHost && isMailboxLoopbackHost(host)) {
		return errors.New("mailbox endpoint must use HTTPS")
	}
	if isMailboxLoopbackHost(host) {
		if allowLoopback && scheme == "http" && host == loopbackHost {
			return nil
		}
		return errors.New("mailbox endpoint resolved to a loopback address")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicMailboxIP(ip) {
			return errors.New("mailbox endpoint resolved to a non-public address")
		}
		return nil
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("mailbox endpoint resolved to a local hostname")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return errors.New("mailbox endpoint DNS lookup failed")
	}
	if len(addresses) == 0 {
		return errors.New("mailbox endpoint DNS returned no address")
	}
	for _, address := range addresses {
		if !isPublicMailboxIP(address.IP) {
			return errors.New("mailbox endpoint DNS includes a non-public address")
		}
	}
	return nil
}

func isMailboxLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isPublicMailboxIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() &&
		!ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsUnspecified() && !ip.IsMulticast()
}
