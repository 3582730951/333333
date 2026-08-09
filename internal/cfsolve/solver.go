// Package cfsolve is a client for a FlareSolverr-compatible cf_clearance solver.
//
// The recovery path deliberately keeps the browser solve separate from the live
// Codex/Claude request profile. Solve returns the browser UA together with the
// cookie; the API layer promotes that cookie only after an application-shaped
// request has replayed it through the same egress without meeting Cloudflare
// again. This prevents a successful browser solve from being mistaken for a
// usable CLI clearance.
package cfsolve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/config"
)

const (
	solverAttempts       = 2
	solverMaxTimeoutMS   = 90_000
	solverCommandTimeout = 105 * time.Second
	solverResponseLimit  = 8 << 20
)

// Client talks to a FlareSolverr-compatible solver.
type Client struct {
	cfg   config.Config
	httpc *http.Client
}

func NewClient(cfg config.Config) *Client {
	return &Client{cfg: cfg, httpc: &http.Client{Timeout: solverCommandTimeout}}
}

// Enabled reports whether the solver rung is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.CFSolverEnabled && strings.TrimSpace(c.cfg.CFSolverURL) != ""
}

// Solution is a solved Cloudflare challenge. CookieHeader contains only cookies
// that apply to the requested target at the time the solver returned them.
type Solution struct {
	CookieHeader  string
	UserAgent     string
	Cookies       map[string]string
	StatusCode    int
	FinalURL      string
	SolverVersion string
	Mode          string // "session" (FlareSolverr v3) or "stateless" compatibility
	Attempt       int
	ExpiresAt     int64
}

type solverCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
}

type solverResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Session  string `json:"session"`
	Version  string `json:"version"`
	Solution struct {
		URL       string          `json:"url"`
		Status    int             `json:"status"`
		Headers   json.RawMessage `json:"headers"`
		Response  string          `json:"response"`
		UserAgent string          `json:"userAgent"`
		Cookies   []solverCookie  `json:"cookies"`
	} `json:"solution"`
}

type solverProxy struct {
	session   map[string]interface{}
	stateless map[string]interface{}
	hasAuth   bool
}

// Solve asks the solver to clear Cloudflare for targetURL, optionally exiting
// through proxyURL. It first uses the FlareSolverr v3 session lifecycle so proxy
// authentication and browser/cookie continuity survive the challenge. A server
// without session commands falls back to the historical stateless request.get
// contract. Each failed solve gets one fresh-browser retry.
func (c *Client) Solve(ctx context.Context, targetURL, proxyURL string) (Solution, error) {
	if !c.Enabled() {
		return Solution{}, errors.New("cf solver disabled")
	}
	target, err := validateTargetURL(targetURL)
	if err != nil {
		return Solution{}, err
	}
	proxy, err := parseSolverProxy(proxyURL)
	if err != nil {
		return Solution{}, err
	}

	var failures []error
	sessionsSupported := true
	for attempt := 1; attempt <= solverAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Solution{}, err
		}
		if sessionsSupported {
			solution, created, solveErr := c.solveWithSession(ctx, target, proxy, attempt)
			if solveErr == nil {
				return solution, nil
			}
			failures = append(failures, fmt.Errorf("session attempt %d: %w", attempt, solveErr))
			if created {
				continue
			}
			sessionsSupported = false
		}
		if proxy.hasAuth {
			failures = append(failures, errors.New("stateless compatibility mode cannot carry proxy credentials"))
			break
		}
		solution, solveErr := c.solveStateless(ctx, target, proxy, attempt)
		if solveErr == nil {
			return solution, nil
		}
		failures = append(failures, fmt.Errorf("stateless attempt %d: %w", attempt, solveErr))
	}
	return Solution{}, fmt.Errorf("cf solver exhausted %d attempt(s): %w", solverAttempts, errors.Join(failures...))
}

func (c *Client) solveWithSession(ctx context.Context, target *url.URL, proxy solverProxy, attempt int) (Solution, bool, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return Solution{}, false, err
	}
	create := map[string]interface{}{"cmd": "sessions.create", "session": sessionID}
	if proxy.session != nil {
		create["proxy"] = proxy.session
	}
	created, err := c.command(ctx, create)
	if err != nil {
		return Solution{}, false, err
	}
	if !strings.EqualFold(created.Status, "ok") {
		return Solution{}, false, solverStatusError(created)
	}
	if returned := strings.TrimSpace(created.Session); returned != "" {
		sessionID = returned
	}
	defer c.destroySession(sessionID)

	out, err := c.command(ctx, map[string]interface{}{
		"cmd":                 "request.get",
		"url":                 target.String(),
		"session":             sessionID,
		"session_ttl_minutes": 2,
		"maxTimeout":          solverMaxTimeoutMS,
		"waitInSeconds":       2,
		"returnOnlyCookies":   false,
		"disableMedia":        false,
	})
	if err != nil {
		return Solution{}, true, err
	}
	solution, err := validateSolution(out, target)
	if err != nil {
		return Solution{}, true, err
	}
	solution.Mode = "session"
	solution.Attempt = attempt
	return solution, true, nil
}

func (c *Client) solveStateless(ctx context.Context, target *url.URL, proxy solverProxy, attempt int) (Solution, error) {
	payload := map[string]interface{}{
		"cmd":               "request.get",
		"url":               target.String(),
		"maxTimeout":        solverMaxTimeoutMS,
		"waitInSeconds":     2,
		"returnOnlyCookies": false,
		"disableMedia":      false,
	}
	if proxy.stateless != nil {
		payload["proxy"] = proxy.stateless
	}
	out, err := c.command(ctx, payload)
	if err != nil {
		return Solution{}, err
	}
	solution, err := validateSolution(out, target)
	if err != nil {
		return Solution{}, err
	}
	solution.Mode = "stateless"
	solution.Attempt = attempt
	return solution, nil
}

func (c *Client) command(ctx context.Context, payload map[string]interface{}) (solverResponse, error) {
	blob, err := json.Marshal(payload)
	if err != nil {
		return solverResponse{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, solverCommandTimeout)
	defer cancel()
	endpoint := strings.TrimRight(strings.TrimSpace(c.cfg.CFSolverURL), "/") + "/v1"
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(blob))
	if err != nil {
		return solverResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return solverResponse{}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, solverResponseLimit))
	if readErr != nil {
		return solverResponse{}, readErr
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return solverResponse{}, fmt.Errorf("cf solver http %d: %s", resp.StatusCode, snippet(raw))
	}
	var out solverResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return solverResponse{}, fmt.Errorf("cf solver parse: %w", err)
	}
	return out, nil
}

func (c *Client) destroySession(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.command(ctx, map[string]interface{}{"cmd": "sessions.destroy", "session": sessionID})
}

func validateSolution(out solverResponse, target *url.URL) (Solution, error) {
	if !strings.EqualFold(out.Status, "ok") {
		return Solution{}, solverStatusError(out)
	}
	if out.Solution.Status < http.StatusOK || out.Solution.Status >= http.StatusBadRequest {
		return Solution{}, fmt.Errorf("cf solver final http status %d", out.Solution.Status)
	}
	if strings.TrimSpace(out.Solution.UserAgent) == "" {
		return Solution{}, errors.New("cf solver returned an empty user agent")
	}
	finalURL := strings.TrimSpace(out.Solution.URL)
	if finalURL == "" {
		finalURL = target.String()
	}
	final, err := url.Parse(finalURL)
	if err != nil || final.Hostname() == "" || !sameHostname(target.Hostname(), final.Hostname()) {
		return Solution{}, fmt.Errorf("cf solver left target host: %q", finalURL)
	}
	if solverHeadersShowChallenge(out.Solution.Headers) || solverBodyShowsChallenge(out.Solution.Response) {
		return Solution{}, errors.New("cf solver final response is still a Cloudflare challenge")
	}

	cookies, order, expiresAt := applicableCookies(out.Solution.Cookies, target, time.Now())
	clearance, ok := cookies["cf_clearance"]
	if !ok || strings.TrimSpace(clearance) == "" {
		return Solution{}, errors.New("cf solver returned no applicable cf_clearance cookie")
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+cookies[name])
	}
	return Solution{
		CookieHeader:  strings.Join(parts, "; "),
		UserAgent:     strings.TrimSpace(out.Solution.UserAgent),
		Cookies:       cookies,
		StatusCode:    out.Solution.Status,
		FinalURL:      final.String(),
		SolverVersion: strings.TrimSpace(out.Version),
		ExpiresAt:     expiresAt,
	}, nil
}

func applicableCookies(input []solverCookie, target *url.URL, now time.Time) (map[string]string, []string, int64) {
	values := make(map[string]string, len(input))
	seen := make(map[string]bool, len(input))
	order := make([]string, 0, len(input))
	var clearanceExpiry int64
	for _, cookie := range input {
		name := strings.TrimSpace(cookie.Name)
		if !validCookiePair(name, cookie.Value) || !cookieApplies(cookie, target, now) {
			continue
		}
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
		values[name] = cookie.Value
		if name == "cf_clearance" && cookie.Expires > 0 {
			clearanceExpiry = int64(cookie.Expires)
		}
	}
	// FlareSolverr normally returns browser order. Sorting makes alternative
	// solver output deterministic while keeping cf_clearance first for diagnosis.
	sort.SliceStable(order, func(i, j int) bool {
		if order[i] == "cf_clearance" {
			return true
		}
		if order[j] == "cf_clearance" {
			return false
		}
		return order[i] < order[j]
	})
	return values, order, clearanceExpiry
}

func validCookiePair(name, value string) bool {
	if name == "" || strings.ContainsAny(name, "()<>@,;:\\\"/[]?={} \t\r\n") {
		return false
	}
	return !strings.ContainsAny(value, ";\r\n")
}

func cookieApplies(cookie solverCookie, target *url.URL, now time.Time) bool {
	if cookie.Expires > 0 && int64(cookie.Expires) <= now.Unix() {
		return false
	}
	if cookie.Secure && !strings.EqualFold(target.Scheme, "https") {
		return false
	}
	domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
	host := strings.ToLower(target.Hostname())
	if domain != "" && host != domain && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	path := strings.TrimSpace(cookie.Path)
	if path != "" && !strings.HasPrefix(target.EscapedPath(), path) {
		return false
	}
	return true
}

func solverHeadersShowChallenge(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var object map[string]interface{}
	if json.Unmarshal(raw, &object) == nil {
		for name, value := range object {
			if strings.EqualFold(strings.TrimSpace(name), "cf-mitigated") && strings.EqualFold(strings.TrimSpace(fmt.Sprint(value)), "challenge") {
				return true
			}
		}
	}
	return false
}

func solverBodyShowsChallenge(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"<title>just a moment",
		"/cdn-cgi/challenge-platform/",
		"id=\"cf-challenge-running\"",
		"name=\"cf-turnstile-response\"",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validateTargetURL(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("cf solver target must be an absolute HTTP(S) URL")
	}
	if target.User != nil {
		return nil, errors.New("cf solver target must not contain credentials")
	}
	return target, nil
}

func parseSolverProxy(raw string) (solverProxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return solverProxy{}, nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil || proxyURL.Host == "" {
		return solverProxy{}, errors.New("cf solver proxy must be an absolute proxy URL")
	}
	scheme := strings.ToLower(proxyURL.Scheme)
	if scheme == "socks5h" {
		// Chromium's proxy configuration calls this SOCKS5; DNS still travels
		// through the SOCKS connection instead of being resolved by this process.
		proxyURL.Scheme = "socks5"
		scheme = "socks5"
	}
	if scheme != "http" && scheme != "https" && scheme != "socks4" && scheme != "socks5" {
		return solverProxy{}, fmt.Errorf("cf solver proxy scheme %q is unsupported", proxyURL.Scheme)
	}
	username := ""
	password := ""
	if proxyURL.User != nil {
		username = proxyURL.User.Username()
		password, _ = proxyURL.User.Password()
		proxyURL.User = nil
	}
	base := map[string]interface{}{"url": proxyURL.String()}
	session := map[string]interface{}{"url": proxyURL.String()}
	if username != "" {
		session["username"] = username
		session["password"] = password
	}
	return solverProxy{session: session, stateless: base, hasAuth: username != ""}, nil
}

func newSessionID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "codex-pool-" + hex.EncodeToString(raw), nil
}

func sameHostname(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(a), "."), strings.TrimSuffix(strings.TrimSpace(b), "."))
}

func solverStatusError(out solverResponse) error {
	message := strings.TrimSpace(out.Message)
	if message == "" {
		message = "no message"
	}
	return fmt.Errorf("cf solver status %q: %s", out.Status, message)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
