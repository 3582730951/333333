// Package cfsolve is a thin client for a FlareSolverr-compatible cf_clearance solver
// (FlareSolverr / Byparr / Solvearr all expose the same POST /v1 {cmd:"request.get"}
// contract). It is the last-resort rung of the CF ladder: when a WARP exit is itself
// Cloudflare-blocked, the server asks the solver to clear the challenge for the
// upstream host THROUGH that exit's proxy, then injects the returned cf_clearance via
// the existing injected-cookie plumbing. The solved cookie is only valid when replayed
// with the SAME User-Agent AND the SAME exit IP that solved it, so Solve returns the
// solver's UA for the caller to persist alongside the cookie + exit IP.
package cfsolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/config"
)

// Client talks to a FlareSolverr-compatible solver.
type Client struct {
	cfg   config.Config
	httpc *http.Client
}

func NewClient(cfg config.Config) *Client {
	return &Client{cfg: cfg, httpc: &http.Client{Timeout: 130 * time.Second}}
}

// Enabled reports whether the solver rung is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.CFSolverEnabled && strings.TrimSpace(c.cfg.CFSolverURL) != ""
}

// Solution is a solved Cloudflare challenge.
type Solution struct {
	CookieHeader string // "cf_clearance=...; other=..." for the cookie jar / sidecar store
	UserAgent    string // the browser UA the solver used (must be replayed)
	Cookies      map[string]string
}

// Solve asks the solver to clear Cloudflare for targetURL, optionally exiting through
// proxyURL (e.g. a WARP exit's local SOCKS5, so the clearance is bound to that IP).
func (c *Client) Solve(ctx context.Context, targetURL, proxyURL string) (Solution, error) {
	if !c.Enabled() {
		return Solution{}, errors.New("cf solver disabled")
	}
	reqBody := map[string]interface{}{
		"cmd":        "request.get",
		"url":        targetURL,
		"maxTimeout": 60000,
	}
	if p := strings.TrimSpace(proxyURL); p != "" {
		reqBody["proxy"] = map[string]interface{}{"url": p}
	}
	blob, err := json.Marshal(reqBody)
	if err != nil {
		return Solution{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, 125*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(strings.TrimSpace(c.cfg.CFSolverURL), "/") + "/v1"
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(blob))
	if err != nil {
		return Solution{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return Solution{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return Solution{}, fmt.Errorf("cf solver http %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			UserAgent string `json:"userAgent"`
			Cookies   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Solution{}, fmt.Errorf("cf solver parse: %w", err)
	}
	if !strings.EqualFold(out.Status, "ok") {
		return Solution{}, fmt.Errorf("cf solver status %q: %s", out.Status, out.Message)
	}
	cookies := map[string]string{}
	parts := make([]string, 0, len(out.Solution.Cookies))
	hasClearance := false
	for _, ck := range out.Solution.Cookies {
		if ck.Name == "" {
			continue
		}
		cookies[ck.Name] = ck.Value
		parts = append(parts, ck.Name+"="+ck.Value)
		if ck.Name == "cf_clearance" {
			hasClearance = true
		}
	}
	if !hasClearance {
		return Solution{}, errors.New("cf solver returned no cf_clearance cookie")
	}
	return Solution{
		CookieHeader: strings.Join(parts, "; "),
		UserAgent:    out.Solution.UserAgent,
		Cookies:      cookies,
	}, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
