package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const claudeRefreshWindowSeconds = 5 * 60

const (
	claudeOAuthUserAgent      = "axios/1.15.2"
	claudeOAuthAccept         = "application/json, text/plain, */*"
	claudeOAuthAcceptEncoding = "gzip, compress, deflate, br"
)

type claudeOAuthAuthorizationCodeGrant struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
}

type claudeOAuthRefreshGrant struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
}

func marshalClaudeOAuthJSON(value interface{}) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func claudeOAuthHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("Accept", claudeOAuthAccept)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", claudeOAuthUserAgent)
	headers.Set("Accept-Encoding", claudeOAuthAcceptEncoding)
	headers.Set("Connection", "keep-alive")
	return headers
}

func claudeOAuthRefreshScope(stored, configured, clientID string) string {
	if strings.TrimSpace(clientID) != config.DefaultClaudeOAuthClientID {
		if scope := strings.Join(strings.Fields(stored), " "); scope != "" {
			return scope
		}
		return strings.Join(strings.Fields(configured), " ")
	}
	ordered := strings.Fields(config.DefaultClaudeOAuthRefreshScope)
	seen := make(map[string]bool, len(ordered)+3)
	for _, scope := range ordered {
		seen[scope] = true
	}
	for _, scope := range strings.Fields(stored) {
		switch scope {
		case "user:projects:read", "user:projects:write", "user:plugins":
			if !seen[scope] {
				ordered = append(ordered, scope)
				seen[scope] = true
			}
		}
	}
	return strings.Join(ordered, " ")
}

type claudeRefreshGates struct {
	mu    sync.Mutex
	gates map[string]*claudeRefreshGate
}

type claudeRefreshGate struct {
	mu          sync.Mutex
	refreshing  bool
	done        chan struct{}
	err         error
	resumeAfter time.Time
	waiters     int
}

type claudeRefreshResult struct {
	Token               storage.AccountToken
	Refreshed           bool
	Reactivated         bool
	Method              string
	Reason              string
	TerminalAuthFailure bool
	StatusCode          int
	Header              http.Header
	Body                []byte
}

type claudeRefreshHeartbeat func() error

func newClaudeRefreshGates() *claudeRefreshGates {
	return &claudeRefreshGates{gates: map[string]*claudeRefreshGate{}}
}

func (g *claudeRefreshGates) gate(accountID string) *claudeRefreshGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing := g.gates[accountID]; existing != nil {
		return existing
	}
	created := &claudeRefreshGate{}
	g.gates[accountID] = created
	return created
}

func (g *claudeRefreshGates) wait(ctx context.Context, accountID string) error {
	return g.waitWithHeartbeat(ctx, accountID, nil)
}

func (g *claudeRefreshGates) waitWithHeartbeat(ctx context.Context, accountID string, heartbeat claudeRefreshHeartbeat) error {
	return g.gate(accountID).wait(ctx, heartbeat)
}

func (g *claudeRefreshGates) ensure(ctx context.Context, accountID string, fn func(context.Context) error) error {
	return g.ensureWithHeartbeat(ctx, accountID, nil, fn)
}

func (g *claudeRefreshGates) ensureWithHeartbeat(ctx context.Context, accountID string, heartbeat claudeRefreshHeartbeat, fn func(context.Context) error) error {
	return g.gate(accountID).ensure(ctx, heartbeat, fn)
}

func (g *claudeRefreshGate) wait(ctx context.Context, heartbeat claudeRefreshHeartbeat) error {
	g.mu.Lock()
	if !g.refreshing {
		resumeAfter := g.resumeAfter
		err := g.err
		g.mu.Unlock()
		if err != nil {
			return err
		}
		return waitUntilWithHeartbeat(ctx, resumeAfter, heartbeat)
	}
	done := g.done
	g.waiters++
	g.mu.Unlock()

	if err := waitForClaudeRefreshDone(ctx, done, heartbeat); err != nil {
		g.mu.Lock()
		g.waiters--
		g.mu.Unlock()
		return err
	}

	g.mu.Lock()
	g.waiters--
	resumeAfter := g.resumeAfter
	err := g.err
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return waitUntilWithHeartbeat(ctx, resumeAfter, heartbeat)
}

func (g *claudeRefreshGate) ensure(ctx context.Context, heartbeat claudeRefreshHeartbeat, fn func(context.Context) error) error {
	for {
		g.mu.Lock()
		if !g.refreshing {
			g.refreshing = true
			g.done = make(chan struct{})
			g.err = nil
			g.mu.Unlock()

			err := fn(ctx)
			g.mu.Lock()
			g.err = err
			if err == nil {
				g.resumeAfter = time.Now().Add(claudeRefreshResumeDelay())
			} else {
				g.resumeAfter = time.Time{}
			}
			g.refreshing = false
			done := g.done
			close(done)
			resumeAfter := g.resumeAfter
			waiters := g.waiters
			g.mu.Unlock()

			if err != nil {
				return err
			}
			if waiters > 0 {
				log.Printf("[CLAUDE-REFRESH] release waiters=%d resume_after=%s", waiters, resumeAfter.Format(time.RFC3339Nano))
			}
			return waitUntilWithHeartbeat(ctx, resumeAfter, heartbeat)
		}
		done := g.done
		g.waiters++
		g.mu.Unlock()

		if err := waitForClaudeRefreshDone(ctx, done, heartbeat); err != nil {
			g.mu.Lock()
			g.waiters--
			g.mu.Unlock()
			return err
		}
		g.mu.Lock()
		g.waiters--
		err := g.err
		resumeAfter := g.resumeAfter
		g.mu.Unlock()
		if err != nil {
			return err
		}
		return waitUntilWithHeartbeat(ctx, resumeAfter, heartbeat)
	}
}

func waitUntil(ctx context.Context, t time.Time) error {
	return waitUntilWithHeartbeat(ctx, t, nil)
}

func waitForClaudeRefreshDone(ctx context.Context, done <-chan struct{}, heartbeat claudeRefreshHeartbeat) error {
	if heartbeat == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		}
	}
	if err := heartbeat(); err != nil {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		case <-ticker.C:
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
}

func waitUntilWithHeartbeat(ctx context.Context, t time.Time, heartbeat claudeRefreshHeartbeat) error {
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	if heartbeat == nil {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	deadline := time.NewTimer(d)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	if err := heartbeat(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
}

func claudeRefreshResumeDelay() time.Duration {
	return time.Second + time.Duration(time.Now().UnixNano()%int64(time.Second))
}

func claudeTokenCanRefresh(token storage.AccountToken) bool {
	if accountprovider.UsesAPIKey("claude", token) {
		return false
	}
	cred := accountprovider.Credential("claude", token)
	return cred != "" && !strings.HasPrefix(cred, "sk-ant-api") && strings.TrimSpace(token.RefreshToken) != ""
}

func claudeTokenNeedsRefresh(token storage.AccountToken, now int64) bool {
	if !claudeTokenCanRefresh(token) || token.ExpiresAt <= 0 {
		return false
	}
	return token.ExpiresAt <= now+claudeRefreshWindowSeconds
}

func (s *Server) prepareClaudeToken(ctx context.Context, account storage.Account, token storage.AccountToken, source string) (storage.AccountToken, error) {
	return s.prepareClaudeTokenWithHeartbeat(ctx, account, token, source, nil)
}

func (s *Server) prepareClaudeTokenWithHeartbeat(ctx context.Context, account storage.Account, token storage.AccountToken, source string, heartbeat claudeRefreshHeartbeat) (storage.AccountToken, error) {
	if s.claudeRefresh != nil {
		if err := s.claudeRefresh.waitWithHeartbeat(ctx, account.ID, heartbeat); err != nil {
			return token, err
		}
		if fresh, err := s.store.GetToken(ctx, account.ID); err == nil {
			token = fresh
		}
	}
	if !claudeTokenNeedsRefresh(token, storage.Now()) {
		return token, nil
	}
	return s.forceRefreshClaudeTokenWithHeartbeat(ctx, account, source, heartbeat)
}

func (s *Server) forceRefreshClaudeToken(ctx context.Context, account storage.Account, source string) (storage.AccountToken, error) {
	return s.forceRefreshClaudeTokenWithHeartbeat(ctx, account, source, nil)
}

func (s *Server) forceRefreshClaudeTokenWithHeartbeat(ctx context.Context, account storage.Account, source string, heartbeat claudeRefreshHeartbeat) (storage.AccountToken, error) {
	if s.claudeRefresh == nil {
		s.claudeRefresh = newClaudeRefreshGates()
	}
	var out storage.AccountToken
	err := s.claudeRefresh.ensureWithHeartbeat(ctx, account.ID, heartbeat, func(refreshCtx context.Context) error {
		current, err := s.store.GetToken(refreshCtx, account.ID)
		if err != nil {
			return err
		}
		if source != "auth_error" && source != "admin_refresh" && !claudeTokenNeedsRefresh(current, storage.Now()) {
			out = current
			return nil
		}
		refreshed, err := s.refreshClaudeTokenWithHeartbeat(refreshCtx, account, current, heartbeat)
		out = refreshed.Token
		if err != nil {
			s.handleClaudeRefreshFailure(refreshCtx, account, refreshed, err, source)
			return err
		}
		if refreshed.Reactivated {
			_ = s.store.InsertAuditLog(refreshCtx, storage.AuditLogRow{
				AccountID:    account.ID,
				AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
				Action:       "auth_recovered",
				State:        "active",
				Reason:       "credential_refresh",
				Detail:       fmt.Sprintf("provider=claude source=%s method=%s", source, refreshed.Method),
			})
			if s.scheduler != nil {
				s.scheduler.InvalidateAccountCache()
				s.scheduler.NotifyStateChanged()
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	fresh, err := s.store.GetToken(ctx, account.ID)
	if err == nil {
		out = fresh
	}
	return out, err
}

func (s *Server) refreshClaudeTokenWithHeartbeat(ctx context.Context, account storage.Account, token storage.AccountToken, heartbeat claudeRefreshHeartbeat) (claudeRefreshResult, error) {
	if heartbeat == nil {
		return s.refreshClaudeToken(ctx, account, token)
	}
	type refreshOut struct {
		result claudeRefreshResult
		err    error
	}
	done := make(chan refreshOut, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				supervisor.LogPanic("claude-refresh-heartbeat", v)
				done <- refreshOut{result: claudeRefreshResult{Token: token}, err: fmt.Errorf("claude refresh panic: %v", v)}
			}
		}()
		result, err := s.refreshClaudeToken(ctx, account, token)
		done <- refreshOut{result: result, err: err}
	}()
	if err := heartbeat(); err != nil {
		out := <-done
		if out.err != nil {
			return out.result, out.err
		}
		return out.result, err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.result, out.err
		case <-ctx.Done():
			out := <-done
			if out.err != nil {
				return out.result, out.err
			}
			return out.result, ctx.Err()
		case <-ticker.C:
			if err := heartbeat(); err != nil {
				out := <-done
				if out.err != nil {
					return out.result, out.err
				}
				return out.result, err
			}
		}
	}
}

type claudeRefreshSSEHeartbeat struct {
	w         http.ResponseWriter
	openAI    bool
	committed bool
}

func newClaudeRefreshSSEHeartbeat(w http.ResponseWriter, openAI bool) *claudeRefreshSSEHeartbeat {
	if w == nil {
		return nil
	}
	return &claudeRefreshSSEHeartbeat{w: w, openAI: openAI}
}

func (h *claudeRefreshSSEHeartbeat) beat() error {
	if h == nil {
		return nil
	}
	if !h.committed {
		header := h.w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no")
		h.w.WriteHeader(http.StatusOK)
		h.committed = true
	}
	if _, err := io.WriteString(h.w, ": claude-refresh-wait\n\n"); err != nil {
		return err
	}
	if flusher, ok := h.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (h *claudeRefreshSSEHeartbeat) Committed() bool {
	return h != nil && h.committed
}

func (h *claudeRefreshSSEHeartbeat) writeError(err error) bool {
	if !h.Committed() {
		return false
	}
	_ = err
	if h.openAI {
		payload, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": publicRetryMessage,
				"type":    "server_error",
			},
		})
		_, _ = h.w.Write([]byte("data: "))
		_, _ = h.w.Write(payload)
		_, _ = h.w.Write([]byte("\n\n"))
		_, _ = h.w.Write([]byte("data: [DONE]\n\n"))
	} else {
		payload, _ := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"message": publicRetryMessage,
				"type":    "api_error",
			},
		})
		_, _ = h.w.Write([]byte("event: error\n"))
		_, _ = h.w.Write([]byte("data: "))
		_, _ = h.w.Write(payload)
		_, _ = h.w.Write([]byte("\n\n"))
	}
	if flusher, ok := h.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// claudeOAuthEgress resolves the egress an account's Anthropic OAuth calls must use. It
// is the account's own bound primary egress (with its sidecar wrapper applied), i.e. the
// identical exit the account's /v1/messages traffic uses.
//
// The fallback ladder is deliberate, and the ordering is the security-relevant part:
//
//  1. binding + primary + sidecar wrapper all resolve  -> the account's real exit. Normal.
//  2. primary resolves but its sidecar wrapper does not -> use the primary WITHOUT the
//     wrapper. The exit IP is still the account's own; only the impersonation backend
//     changes, and claudeFingerprintEngine picks the in-process engine for any egress
//     type, so the fingerprint stays coherent. Degrading straight to host-direct here
//     (the previous behavior) threw away a perfectly good, correctly-bound exit IP.
//  3. nothing about the account's exit is knowable -> fail closed. Sending a refresh from
//     the relay host would preserve availability by exposing the relay on a fixed schedule.
func (s *Server) claudeOAuthEgress(ctx context.Context, accountID string) (storage.EgressProfile, string, error) {
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		s.auditClaudeOAuthEgressDegraded(ctx, "binding_lookup_failed", err)
		return storage.EgressProfile{}, "", fmt.Errorf("claude oauth egress binding unavailable: %w", err)
	}
	binding, err = s.store.EffectiveEgressBinding(ctx, binding)
	if err != nil {
		s.auditClaudeOAuthEgressDegraded(ctx, "binding_resolution_failed", err)
		return storage.EgressProfile{}, "", fmt.Errorf("claude oauth egress binding unavailable: %w", err)
	}
	primary, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil {
		s.auditClaudeOAuthEgressDegraded(ctx, "primary_egress_lookup_failed", err)
		return storage.EgressProfile{}, binding.CookieJarKey, fmt.Errorf("claude oauth primary egress unavailable: %w", err)
	}
	wrapped, err := s.store.ApplySidecarEgressBinding(ctx, binding, primary)
	if err != nil {
		// Keep the account's exit IP; only the sidecar transport is unavailable.
		s.auditClaudeOAuthEgressDegraded(ctx, "sidecar_wrapper_unavailable", err)
		return primary, binding.CookieJarKey, nil
	}
	return wrapped, binding.CookieJarKey, nil
}

const claudeOAuthEgressAuditWindow = 10 * time.Minute

type claudeOAuthEgressAuditBucket struct {
	mu         sync.Mutex
	last       time.Time
	suppressed uint64
}

var claudeOAuthEgressAuditBuckets sync.Map

// auditClaudeOAuthEgressDegraded records that a Claude OAuth token call could not use the
// account's bound exit. Rate-limited per reason so a persistently broken binding cannot
// flood the audit log, but never fully silenced. No account identifier and no credential
// value enters the key or the payload.
func (s *Server) auditClaudeOAuthEgressDegraded(ctx context.Context, reason string, cause error) {
	if s == nil || s.store == nil {
		return
	}
	key := fmt.Sprintf("%p\x00%s", s.store, reason)
	value, _ := claudeOAuthEgressAuditBuckets.LoadOrStore(key, &claudeOAuthEgressAuditBucket{})
	bucket := value.(*claudeOAuthEgressAuditBucket)
	now := time.Now()
	bucket.mu.Lock()
	if !bucket.last.IsZero() && now.Sub(bucket.last) < claudeOAuthEgressAuditWindow {
		bucket.suppressed++
		bucket.mu.Unlock()
		return
	}
	suppressed := bucket.suppressed
	bucket.last = now
	bucket.suppressed = 0
	bucket.mu.Unlock()

	state := "blocked"
	if reason == "sidecar_wrapper_unavailable" {
		state = "bound_exit_kept"
	}
	detail := "reason=" + reason
	if suppressed > 0 {
		detail += fmt.Sprintf(" suppressed=%d", suppressed)
	}
	log.Printf("[CLAUDE-OAUTH] egress degraded reason=%s state=%s: %v", reason, state, cause)
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		Action: "claude_oauth_egress_degraded", State: state, Reason: reason,
		Detail: detail,
	})
}

// doClaudeOAuthRequest issues one Anthropic OAuth token call through the account's egress
// and fingerprint engine. The application headers are the Axios wire shape captured from
// Claude Code 2.1.226's native authorization-code and refresh-token requests.
func (s *Server) doClaudeOAuthRequest(ctx context.Context, egress storage.EgressProfile, account storage.Account, cookieJarKey, tokenURL string, payload []byte) (*http.Response, error) {
	headers := claudeOAuthHeaders()
	if s.upstream != nil {
		resp, err := s.upstream.DoAnthropicOAuth(ctx, egress, account, tokenURL, headers, payload, cookieJarKey)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
	}
	parsed, err := url.Parse(tokenURL)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return nil, errors.New("claude oauth fingerprint transport is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return oauthHTTPClient().Do(req)
}

func (s *Server) refreshClaudeToken(ctx context.Context, account storage.Account, token storage.AccountToken) (claudeRefreshResult, error) {
	result := claudeRefreshResult{Token: token}
	cred := accountprovider.Credential("claude", token)
	if strings.HasPrefix(cred, "sk-ant-api") || strings.TrimSpace(token.RefreshToken) == "" {
		token.LastRefresh = storage.Now()
		_ = s.store.UpdateToken(ctx, token)
		result.Token = token
		result.Reason = "api key or missing refresh_token; nothing to refresh"
		return result, nil
	}
	tokenURL := strings.TrimSpace(s.cfg.ClaudeOAuthTokenURL)
	if tokenURL == "" {
		result.Reason = "missing claude oauth token url"
		return result, errors.New(result.Reason)
	}

	// The token endpoint and inference endpoint are in the same Anthropic account perimeter.
	// Keep the account on one exit and one TLS family across both domains.
	oauthEgress, oauthCookieKey, err := s.claudeOAuthEgress(ctx, account.ID)
	if err != nil {
		result.Reason = "claude oauth egress unavailable"
		return result, err
	}

	backoff := 500 * time.Millisecond
	for {
		payload, marshalErr := marshalClaudeOAuthJSON(claudeOAuthRefreshGrant{
			GrantType:    "refresh_token",
			RefreshToken: token.RefreshToken,
			ClientID:     s.cfg.ClaudeOAuthClientID,
			Scope:        claudeOAuthRefreshScope(token.Scopes, s.cfg.ClaudeOAuthScope, s.cfg.ClaudeOAuthClientID),
		})
		if marshalErr != nil {
			return result, marshalErr
		}
		resp, err := s.doClaudeOAuthRequest(ctx, oauthEgress, account, oauthCookieKey, tokenURL, payload)
		if err != nil {
			if waitErr := waitRefreshBackoff(ctx, nil, backoff); waitErr != nil {
				return result, err
			}
			backoff = nextRefreshBackoff(backoff)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			result.StatusCode = resp.StatusCode
			result.Header = resp.Header.Clone()
			result.Body = raw
			result.Reason, result.TerminalAuthFailure = claudeRefreshFailureReason(resp.StatusCode, raw)
			err := fmt.Errorf("anthropic token refresh failed (%d): %s", resp.StatusCode, result.Reason)
			if result.TerminalAuthFailure {
				// A rotating refresh token can be consumed by the previous worker
				// during a release handoff. Re-read the committed credential before
				// quarantining the account: if another worker persisted a newer
				// access/refresh pair, this invalid_grant belongs to the stale copy.
				if fresh, recovered := s.recoverClaudeRefreshRace(ctx, token); recovered {
					result.Token = fresh
					result.Refreshed = true
					result.TerminalAuthFailure = false
					result.Method = "database_race_recovery"
					result.Reason = "rotated_credential_recovered"
					result.StatusCode = 0
					result.Header = nil
					result.Body = nil
					return result, nil
				}
				return result, err
			}
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return result, err
			}
			if waitErr := waitRefreshBackoff(ctx, resp.Header, backoff); waitErr != nil {
				return result, err
			}
			backoff = nextRefreshBackoff(backoff)
			continue
		}
		var refreshed struct {
			AccessToken      string   `json:"access_token"`
			RefreshToken     string   `json:"refresh_token"`
			ExpiresIn        int64    `json:"expires_in"`
			ExpiresAt        int64    `json:"expires_at"`
			Scope            string   `json:"scope"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscription_type"`
			RateLimitTier    string   `json:"rate_limit_tier"`
		}
		if err := json.Unmarshal(raw, &refreshed); err != nil {
			return result, err
		}
		if strings.TrimSpace(refreshed.AccessToken) == "" {
			return result, errors.New("anthropic oauth response had no access_token")
		}
		token.AccessToken = strings.TrimSpace(refreshed.AccessToken)
		if strings.TrimSpace(refreshed.RefreshToken) != "" {
			token.RefreshToken = strings.TrimSpace(refreshed.RefreshToken)
		}
		if refreshed.ExpiresAt > 1_000_000_000_000 {
			refreshed.ExpiresAt /= 1000
		}
		switch {
		case refreshed.ExpiresAt > 0:
			token.ExpiresAt = refreshed.ExpiresAt
		case refreshed.ExpiresIn > 0:
			token.ExpiresAt = storage.Now() + refreshed.ExpiresIn
		}
		scopes := refreshed.Scopes
		if len(scopes) == 0 && strings.TrimSpace(refreshed.Scope) != "" {
			scopes = strings.Fields(refreshed.Scope)
		}
		if len(scopes) > 0 {
			token.Scopes = strings.Join(scopes, " ")
		}
		if strings.TrimSpace(refreshed.RateLimitTier) != "" {
			token.OAuthRateLimitTier = strings.TrimSpace(refreshed.RateLimitTier)
		}
		token.LastRefresh = storage.Now()
		reactivated, err := s.store.UpdateTokenAfterCredentialRefresh(ctx, token)
		if err != nil {
			return result, err
		}
		if strings.TrimSpace(refreshed.SubscriptionType) != "" {
			_ = s.store.SetAccountPlanType(ctx, account.ID, refreshed.SubscriptionType)
		}
		result.Token = token
		result.Refreshed = true
		result.Reactivated = reactivated
		result.Method = "anthropic_oauth"
		return result, nil
	}
}

func (s *Server) recoverClaudeRefreshRace(ctx context.Context, used storage.AccountToken) (storage.AccountToken, bool) {
	// A successful peer refresh normally commits before the losing invalid_grant
	// response arrives. Short bounded retries cover the remaining commit race
	// without turning a genuinely revoked credential into an unbounded retry loop.
	delays := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return storage.AccountToken{}, false
			case <-timer.C:
			}
		}
		fresh, err := s.store.GetTokenFresh(ctx, used.AccountID)
		if err != nil {
			continue
		}
		if claudeCredentialAdvanced(used, fresh) {
			return fresh, true
		}
	}
	return storage.AccountToken{}, false
}

func claudeCredentialAdvanced(used, fresh storage.AccountToken) bool {
	if strings.TrimSpace(fresh.AccessToken) == "" {
		return false
	}
	rotated := fresh.RefreshToken != used.RefreshToken || fresh.AccessToken != used.AccessToken
	if !rotated {
		return false
	}
	// UpdatedAt is the durable row version. LastRefresh is retained as a fallback
	// for imported legacy rows that predate reliable updated_at timestamps.
	return fresh.UpdatedAt > used.UpdatedAt || fresh.LastRefresh > used.LastRefresh || fresh.ExpiresAt > used.ExpiresAt
}

func waitRefreshBackoff(ctx context.Context, header http.Header, fallback time.Duration) error {
	delay := fallback
	if header != nil {
		if ra := retryAfterSeconds(header, storage.Now()); ra > 0 {
			delay = time.Duration(ra) * time.Second
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextRefreshBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}

func claudeRefreshFailureReason(status int, body []byte) (string, bool) {
	hay := strings.ToLower(string(body))
	for _, sig := range []string{
		"invalid_grant",
		"refresh_token_expired",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"invalid refresh",
		"revoked",
	} {
		if strings.Contains(hay, sig) {
			return sig, true
		}
	}
	return fmt.Sprintf("http_%d", status), false
}

func (s *Server) handleClaudeRefreshFailure(ctx context.Context, account storage.Account, result claudeRefreshResult, err error, source string) {
	_ = err
	reason := firstNonEmpty(result.Reason, "refresh_failed")
	if result.TerminalAuthFailure {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			AccountID:    account.ID,
			AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
			Action:       "auth_expired",
			State:        string(ban.AuthExpired),
			Reason:       reason,
			Detail:       fmt.Sprintf("source=%s http=%d class=%s", source, result.StatusCode, reason),
		})
		_ = s.store.SetAccountStatus(ctx, account.ID, "auth_expired")
		return
	}
	_ = s.store.BenchBindingForRecheck(ctx, account.ID, storage.Now()+int64((5*time.Minute)/time.Second))
}

func claudeAuthError(status int, header http.Header, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	v := ban.Classify(false, status, header, body)
	return v.State == ban.AuthExpired
}
