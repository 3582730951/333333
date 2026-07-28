package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream/tlsclient"
)

// postInProcess is the in-process analogue of postViaSidecar. It performs the actual
// network call through the bogdanfinn/tls-client engine so the upstream sees a real
// browser TLS/JA3 + HTTP2 fingerprint (Chrome_120, matching the sidecar's chrome120
// default) instead of the Go standard library's — with no external Python process.
//
// It is provider-neutral: the caller supplies the target URL, the fully-built upstream
// header set, the overall timeout, and an optional explicit JA3/profile override
// (jaProfileOverride, mapped to a named tls-client profile). It shares the same idle-timeout
// guard semantics as every other upstream transport (bound to the body's Close, reset on
// each read) so a long streaming completion is never truncated. A sidecar egress's chain
// proxy is honored so traffic still leaves from the intended exit IP.
//
// jaProfileOverride "" keeps the default Chrome_120. The in-process engine works with
// hardened named profiles from profiles.MappedTLSClients rather than raw-string JA3 replay
// (the sidecar's curl_cffi feature); an unrecognized override degrades to Chrome_120 rather
// than failing the request.
func (c *Client) postInProcess(ctx context.Context, spec Request, target string, built http.Header, timeout time.Duration, jaProfileOverride, profileName string) (*Response, error) {
	// fhttp/uTLS negotiates and auto-decompresses content-encoding itself based on the
	// selected profile; a stray Accept-Encoding from the built header set would both
	// duplicate the profile's own value and risk an encoding the transport won't decode.
	headers := built.Clone()
	headers.Del("Accept-Encoding")

	ctx, guard := newRequestGuard(ctx, timeout)
	resp, err := c.tlsFactory.Do(ctx, tlsclient.Request{
		Method:       firstNonEmpty(spec.Method, http.MethodPost),
		URL:          target,
		Header:       headers,
		Body:         spec.Body,
		Profile:      profileName,
		JA3Override:  inProcessNamedProfile(jaProfileOverride),
		ProxyURL:     strings.TrimSpace(spec.Egress.ChainProxy),
		CookieJarKey: sidecarCookieKey(spec.Account.ID, spec.Egress.ID, target, spec.CookieJarKey),
		// Timeout intentionally left 0: the requestGuard context bounds the call with the
		// pool's idle-timeout semantics, not an absolute deadline.
	})
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       guard.Wrap(resp.Body),
	}, nil
}

// inProcessNamedProfile maps a resolved sidecar JA3 string onto a named tls-client
// profile key. The sidecar consumes a raw JA3 string; the in-process engine consumes a
// named profile from profiles.MappedTLSClients. Empty / Chrome sentinels keep the default
// (Chrome_120). A raw JA3 string that isn't a known named key returns "" so ResolveProfile
// degrades to Chrome_120 rather than presenting an incoherent partial fingerprint.
func inProcessNamedProfile(ja3 string) string {
	switch v := strings.ToLower(strings.TrimSpace(ja3)); v {
	case "", "off", "none", "disabled", "-", "chrome", "browser":
		return ""
	default:
		// Named profile passthrough (e.g. "chrome_133", "firefox_120"): ResolveProfile
		// looks these up in profiles.MappedTLSClients. A raw JA3 fingerprint string will
		// not match and safely degrades to Chrome_120.
		return v
	}
}

// doRawInProcess is the in-process analogue of DoViaSidecar for provider-neutral native
// adapters (Kiro). It routes an arbitrary request through the tls-client engine with the
// per-provider profile so the upstream sees the intended fingerprint. profileName selects
// ProfileNode / ProfileRustls / ProfileChrome; headerOrder, when non-nil, pins the wire
// order of the request headers to match the native client (e.g. aws-sdk-js).
func (c *Client) doRawInProcess(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey, profileName string, headerOrder []string) (*Response, error) {
	return c.doRawInProcessSource(ctx, egress, method, rawURL, headers, bodysource.Bytes(body), cookieJarKey, profileName, headerOrder)
}

func (c *Client) doRawInProcessSource(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body bodysource.BodySource, cookieJarKey, profileName string, headerOrder []string) (*Response, error) {
	built := headers.Clone()
	built.Del("Accept-Encoding")

	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	resp, err := c.tlsFactory.Do(ctx, tlsclient.Request{
		Method:       firstNonEmpty(method, http.MethodGet),
		URL:          rawURL,
		Header:       built,
		HeaderOrder:  headerOrder,
		Body:         body,
		Profile:      profileName,
		ProxyURL:     strings.TrimSpace(egress.ChainProxy),
		CookieJarKey: cookieJarKey,
	})
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       guard.Wrap(resp.Body),
	}, nil
}

// CloseIdleFingerprintConnections releases idle keep-alive connections held by the
// in-process TLS engine. Called alongside the sidecar/ transport idle sweeps.
func (c *Client) CloseIdleFingerprintConnections() {
	if c.tlsFactory != nil {
		c.tlsFactory.CloseIdle()
	}
}

// FingerprintReflection is the parsed subset of a JA3/JA4/Akamai reflector response
// (tls.peet.ws/api/all shape) plus the raw body, for the admin fidelity-diff diagnostic.
type FingerprintReflection struct {
	Engine        string          `json:"engine"` // "inprocess" or "sidecar"
	StatusCode    int             `json:"status_code"`
	JA3           string          `json:"ja3,omitempty"`
	JA3Hash       string          `json:"ja3_hash,omitempty"`
	JA4           string          `json:"ja4,omitempty"`
	AkamaiHash    string          `json:"akamai_hash,omitempty"`
	Akamai        string          `json:"akamai,omitempty"`
	UserAgentSeen string          `json:"user_agent_seen,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// ReflectFingerprint performs a GET against a public TLS/HTTP2 reflector (default
// https://tls.peet.ws/api/all) through the requested fingerprint engine and returns the
// observed JA3/JA4/Akamai fingerprint. It is the diagnostic that lets an operator verify,
// post-deploy on the live VPS, that the in-process engine presents the SAME fingerprint the
// sidecar did before flipping (the non-negotiable validation gate).
//
// SECURITY NOTE: this makes an outbound request that reveals the chosen egress IP to the
// third-party reflector. It is an explicit, admin-triggered diagnostic only; never called
// on the request hot path. Operators can point `reflectorURL` at a self-hosted reflector.
//
// engine: "inprocess" routes through the tls-client engine; "sidecar" routes through the
// curl_cffi sidecar (only valid for a curl_cffi_sidecar egress). profileName selects the
// in-process profile (ProfileChrome for Codex/Claude, ProfileNode/ProfileRustls for Kiro).
func (c *Client) ReflectFingerprint(ctx context.Context, egress storage.EgressProfile, engine, profileName, reflectorURL string) FingerprintReflection {
	if strings.TrimSpace(reflectorURL) == "" {
		reflectorURL = "https://tls.peet.ws/api/all"
	}
	out := FingerprintReflection{Engine: engine}
	headers := http.Header{}
	var resp *Response
	var err error
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "sidecar":
		resp, err = c.DoViaSidecar(ctx, egress, http.MethodGet, reflectorURL, headers, nil, "fingerprint-check")
	default: // inprocess
		resp, err = c.doRawInProcess(ctx, egress, http.MethodGet, reflectorURL, headers, nil, "fingerprint-check", profileName, nil)
	}
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer resp.Body.Close()
	out.StatusCode = resp.StatusCode
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if readErr != nil {
		out.Error = readErr.Error()
		return out
	}
	out.Raw = json.RawMessage(body)
	parseReflectorBody(body, &out)
	return out
}

// parseReflectorBody extracts the tls.peet.ws /api/all fingerprint fields into out. It is
// tolerant of a body that does not match the expected shape (leaves fields empty).
func parseReflectorBody(body []byte, out *FingerprintReflection) {
	var parsed struct {
		TLS struct {
			JA3     string `json:"ja3"`
			JA3Hash string `json:"ja3_hash"`
			JA4     string `json:"ja4"`
		} `json:"tls"`
		HTTP2 struct {
			AkamaiFingerprint     string `json:"akamai_fingerprint"`
			AkamaiFingerprintHash string `json:"akamai_fingerprint_hash"`
		} `json:"http2"`
		UserAgent string `json:"user_agent"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return
	}
	out.JA3 = parsed.TLS.JA3
	out.JA3Hash = parsed.TLS.JA3Hash
	out.JA4 = parsed.TLS.JA4
	out.Akamai = parsed.HTTP2.AkamaiFingerprint
	out.AkamaiHash = parsed.HTTP2.AkamaiFingerprintHash
	out.UserAgentSeen = parsed.UserAgent
}
