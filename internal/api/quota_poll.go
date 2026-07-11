package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

// quotaPoller periodically fetches 5h/7d rate-limit windows from the ChatGPT
// backend-api `/wham/usage` endpoint for every active Codex account, so the
// admin dashboard "账号额度 · 实时配额" gauge panel has data even when the
// account egresses through curl_cffi_sidecar (whose responses carry no
// x-ratelimit-* headers — the headers-only captureQuota path can never fill
// the account_rate_limits table for sidecar Codex accounts).
//
// Polling is the standard approach across the ecosystem:
//   - pi-multicodex / pi-dispatch: periodic freshness refresh
//   - codex-switch: background daemon with stale-only refresh
//   - codex-quota: dedicated CLI/Docker tool using this exact endpoint
//
// The poller is conservative: one in-flight poll per account max, concurrency-
// capped, with a floor interval so it never overwhelms the backend-api.

const (
	quotaPollIntervalFloor = 120 // seconds — never poll more often than every 2 min
	quotaPollInterval      = 300 // seconds — default
	quotaPollMaxConcurrent = 5   // max concurrent fetches per tick
)

var whamUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// whamUsageResponse mirrors the JSON shape returned by /backend-api/wham/usage.
type whamUsageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		LimitReached    bool       `json:"limit_reached"`
		PrimaryWindow   whamWindow `json:"primary_window"`
		SecondaryWindow whamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	RateLimitResetCredits      map[string]interface{} `json:"rate_limit_reset_credits"`
	RateLimitResetCreditsCamel map[string]interface{} `json:"rateLimitResetCredits"`
}

type whamWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	Status             string  `json:"status"`
}

type quotaPollTarget struct {
	Account storage.Account
	Token   storage.AccountToken
	Egress  storage.EgressProfile
}

type quotaPollError struct {
	reason     string
	statusCode int
	body       string
	err        error
}

func (e quotaPollError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	if e.statusCode > 0 {
		return fmt.Sprintf("http %d", e.statusCode)
	}
	if e.reason != "" {
		return e.reason
	}
	return "quota poll failed"
}

func (e quotaPollError) Unwrap() error {
	return e.err
}

func newQuotaPollError(reason string, statusCode int, body []byte, err error) quotaPollError {
	return quotaPollError{
		reason:     firstNonEmpty(strings.TrimSpace(reason), "poll_failed"),
		statusCode: statusCode,
		body:       bodySnippet(body, 300),
		err:        err,
	}
}

// StartQuotaPoller launches the background quota poller. Called once from main.
func (s *Server) StartQuotaPoller(ctx context.Context) {
	supervisor.Go(ctx, "quota-poller", func(ctx context.Context) {
		// Stagger startup so the server is fully ready before the first poll.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		log.Printf("[QUOTA-POLL] started (interval=%ds, max_concurrent=%d)", quotaPollInterval, quotaPollMaxConcurrent)
		ticker := time.NewTicker(time.Duration(quotaPollInterval) * time.Second)
		defer ticker.Stop()
		// First poll immediately after the stagger.
		s.pollAllCodexQuotas(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollAllCodexQuotas(ctx)
			}
		}
	})
}

// pollAllCodexQuotas fetches usage snapshots for every active Codex/Claude
// account, concurrency-capped. Best-effort: a single account failure never
// blocks the rest.
func (s *Server) pollAllCodexQuotas(ctx context.Context) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListAccounts error: %v", err)
		return
	}
	now := storage.Now()
	candidateIDs := quotaPollCandidateAccountIDs(accounts, now)
	if len(candidateIDs) == 0 {
		return
	}
	tokens, err := s.store.ListTokensByAccountIDs(ctx, candidateIDs)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListTokensByAccountIDs error: %v", err)
		return
	}
	codex, missingTokens := codexQuotaPollTargets(accounts, tokens, now)
	claude := claudeQuotaPollTargets(accounts, tokens, now)
	kiro := kiroQuotaPollTargets(accounts, tokens, now)
	if len(codex) == 0 && len(claude) == 0 && len(kiro) == 0 && missingTokens == 0 {
		return
	}
	s.attachQuotaPollEgresses(ctx, codex)
	s.attachQuotaPollEgresses(ctx, claude)
	s.attachQuotaPollEgresses(ctx, kiro)
	updated := 0
	failed := missingTokens
	// Concurrency-capped worker pool so we never open N connections at once.
	sem := make(chan struct{}, quotaPollMaxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range codex {
		target := codex[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					supervisor.LogPanic("quota-poller-worker", v)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("[QUOTA-POLL] account=%s skipped: %v", target.Account.ID, ctx.Err())
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneCodexQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "codex", err)
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("[QUOTA-POLL] account=%s poll failed: %v", target.Account.ID, err)
			} else {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}()
	}
	for i := range claude {
		target := claude[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					supervisor.LogPanic("quota-poller-worker", v)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("[QUOTA-POLL] account=%s skipped: %v", target.Account.ID, ctx.Err())
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneClaudeQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "claude", err)
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("[QUOTA-POLL] account=%s claude poll failed: %v", target.Account.ID, err)
			} else {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}()
	}
	for i := range kiro {
		target := kiro[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer supervisor.Recover("quota-poller-kiro-worker")
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneKiroQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "kiro", err)
				mu.Lock()
				failed++
				mu.Unlock()
			} else {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if updated > 0 && s.scheduler != nil {
		s.scheduler.NotifyStateChanged()
	}
	if updated > 0 || failed > 0 {
		log.Printf("[QUOTA-POLL] tick complete: updated=%d failed=%d total_codex=%d total_claude=%d", updated, failed, len(codex), len(claude))
	}
}

func kiroQuotaPollTargets(accounts []storage.Account, tokens map[string]storage.AccountToken, now int64) []quotaPollTarget {
	var out []quotaPollTarget
	for _, account := range accounts {
		if isQuotaPollCandidate(account, now) && strings.TrimSpace(account.Provider) == "kiro" {
			if token, ok := tokens[account.ID]; ok {
				out = append(out, quotaPollTarget{Account: account, Token: token})
			}
		}
	}
	return out
}

func (s *Server) pollOneKiroQuota(ctx context.Context, acc storage.Account, token storage.AccountToken, egress storage.EgressProfile) error {
	s.kiro.UpdateConfig(s.effectiveKiroConfig(ctx))
	cred, err := s.store.GetKiroCredentials(ctx, acc.ID)
	if err != nil {
		return err
	}
	bearer, token, cred, err := s.kiro.Prepare(ctx, acc, cred, token, egress, false)
	if err != nil {
		if errors.Is(err, kirowire.ErrInvalidGrant) {
			s.scheduler.InvalidateAccountCache()
		}
		return err
	}
	limits, err := s.kiro.UsageLimits(ctx, acc, cred, bearer, egress)
	if err != nil {
		return err
	}
	if plan := kiroPlan(limits); plan != "" {
		_ = s.store.SetAccountPlanType(ctx, acc.ID, plan)
	}
	list, _ := lookup(limits, "usageBreakdownList").([]interface{})
	if len(list) == 0 {
		return newQuotaPollError("partial", 0, nil, errors.New("kiro usage breakdown missing"))
	}
	base, _ := list[0].(map[string]interface{})
	current := kiroJSONFloat(base, "currentUsageWithPrecision", "currentUsage")
	limit := kiroJSONFloat(base, "usageLimitWithPrecision", "usageLimit")
	if trial, ok := lookup(base, "freeTrialInfo").(map[string]interface{}); ok && strings.EqualFold(fmt.Sprint(lookup(trial, "freeTrialStatus")), "ACTIVE") {
		current += kiroJSONFloat(trial, "currentUsageWithPrecision", "currentUsage")
		limit += kiroJSONFloat(trial, "usageLimitWithPrecision", "usageLimit")
	}
	if bonuses, ok := lookup(base, "bonuses").([]interface{}); ok {
		for _, v := range bonuses {
			if b, ok := v.(map[string]interface{}); ok && strings.EqualFold(fmt.Sprint(lookup(b, "status")), "ACTIVE") {
				current += kiroJSONFloat(b, "currentUsage")
				limit += kiroJSONFloat(b, "usageLimit")
			}
		}
	}
	used := -1.0
	if limit > 0 {
		used = current / limit * 100
	}
	reset := int64(kiroJSONFloat(base, "nextDateReset"))
	if reset == 0 {
		reset = int64(kiroJSONFloat(limits, "nextDateReset"))
	}
	raw, _ := json.Marshal(limits)
	return s.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{AccountID: acc.ID, Provider: "kiro", LimiterType: "kiro_usage", Source: "kiro_usage", UsedPercent: used, LimitTokens: int64(limit), RemainingTokens: int64(limit - current), LimitRequests: -1, RemainingRequests: -1, ResetAt: reset, Status: "allowed", Raw: string(raw), UpdatedAt: storage.Now()})
}

func kiroJSONFloat(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v := lookup(m, k); v != nil {
			switch n := v.(type) {
			case float64:
				return n
			case json.Number:
				f, _ := n.Float64()
				return f
			}
		}
	}
	return 0
}

func quotaPollCandidateAccountIDs(accounts []storage.Account, now int64) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if !isQuotaPollCandidate(account, now) {
			continue
		}
		ids = append(ids, account.ID)
	}
	return ids
}

func codexQuotaPollTargets(accounts []storage.Account, tokens map[string]storage.AccountToken, now int64) ([]quotaPollTarget, int) {
	targets := make([]quotaPollTarget, 0, len(accounts))
	missingTokens := 0
	for _, account := range accounts {
		if !isQuotaPollCandidate(account, now) {
			continue
		}
		if p := strings.TrimSpace(account.Provider); p != "" && p != "codex" && p != "chatgpt" && p != "openai" {
			continue
		}
		token, ok := tokens[account.ID]
		if !ok {
			missingTokens++
			continue
		}
		if strings.TrimSpace(account.Provider) == "" && scheduler.ProviderFromToken(token) != "codex" {
			continue
		}
		targets = append(targets, quotaPollTarget{Account: account, Token: token})
	}
	return targets, missingTokens
}

func claudeQuotaPollTargets(accounts []storage.Account, tokens map[string]storage.AccountToken, now int64) []quotaPollTarget {
	targets := make([]quotaPollTarget, 0, len(accounts))
	for _, account := range accounts {
		if !isQuotaPollCandidate(account, now) {
			continue
		}
		token, ok := tokens[account.ID]
		if !ok {
			continue
		}
		provider := strings.TrimSpace(account.Provider)
		if provider == "" {
			provider = scheduler.ProviderFromToken(token)
		}
		if provider != "claude" || !claudeTokenCanRefresh(token) {
			continue
		}
		targets = append(targets, quotaPollTarget{Account: account, Token: token})
	}
	return targets
}

func (s *Server) attachQuotaPollEgresses(ctx context.Context, targets []quotaPollTarget) {
	if len(targets) == 0 {
		return
	}
	accountIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		accountIDs = append(accountIDs, target.Account.ID)
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListEgressBindingsByAccountIDs error: %v; using direct egress", err)
		bindings = map[string]storage.AccountEgressBinding{}
	}
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListEgressProfiles error: %v; using direct egress", err)
	}
	profilesByID := quotaPollEgressProfilesByID(profiles)
	for i := range targets {
		targets[i].Egress = quotaPollEgressForAccount(targets[i].Account.ID, bindings, profilesByID)
	}
}

func quotaPollEgressProfilesByID(profiles []storage.EgressProfile) map[string]storage.EgressProfile {
	out := make(map[string]storage.EgressProfile, len(profiles))
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) == "" {
			continue
		}
		out[profile.ID] = profile
	}
	return out
}

func quotaPollEgressForAccount(accountID string, bindings map[string]storage.AccountEgressBinding, profiles map[string]storage.EgressProfile) storage.EgressProfile {
	egressID := storage.DefaultDirectEgressID
	if binding, ok := bindings[accountID]; ok && strings.TrimSpace(binding.PrimaryEgressID) != "" {
		egressID = binding.PrimaryEgressID
	}
	if profile, ok := profiles[egressID]; ok {
		return profile
	}
	return storage.EgressProfile{ID: egressID, Type: "direct"}
}

func isQuotaPollCandidate(account storage.Account, now int64) bool {
	if account.Status != "active" {
		return false
	}
	if account.QuarantineUntil > now {
		return false
	}
	provider := strings.TrimSpace(account.Provider)
	return provider == "" || provider == "codex" || provider == "chatgpt" || provider == "openai" || provider == "claude"
}

func (s *Server) recordQuotaPollError(ctx context.Context, acc storage.Account, provider string, err error) {
	if strings.TrimSpace(acc.ID) == "" {
		return
	}
	var qerr quotaPollError
	if !errors.As(err, &qerr) {
		qerr = quotaPollError{reason: "poll_failed", err: err}
	}
	reason := firstNonEmpty(strings.TrimSpace(qerr.reason), "poll_failed")
	syncReason := "error/" + reason
	detail := map[string]interface{}{
		"sync_reason": syncReason,
		"error":       "",
	}
	if qerr.err != nil {
		detail["error"] = qerr.err.Error()
	} else if err != nil {
		detail["error"] = err.Error()
	}
	if qerr.statusCode > 0 {
		detail["http_status"] = qerr.statusCode
	}
	if strings.TrimSpace(qerr.body) != "" {
		detail["body_snippet"] = qerr.body
	}
	raw, _ := json.Marshal(detail)
	now := storage.Now()
	_ = s.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         acc.ID,
		Provider:          strings.TrimSpace(provider),
		LimiterType:       "quota_poll_error",
		Source:            "quota_poll_error",
		UsedPercent:       -1,
		LimitTokens:       -1,
		RemainingTokens:   -1,
		LimitRequests:     -1,
		RemainingRequests: -1,
		Status:            syncReason,
		Raw:               string(raw),
		UpdatedAt:         now,
	})
}

// pollOneCodexQuota fetches the wham usage snapshot for one account and persists
// two AccountRateLimit rows: one for the 5h (primary) window and one for the 7d
// (secondary) window.
func (s *Server) pollOneCodexQuota(ctx context.Context, acc storage.Account, token storage.AccountToken, egress storage.EgressProfile) error {
	accessToken := strings.TrimSpace(token.AccessToken)
	if accessToken == "" {
		accessToken = strings.TrimSpace(token.OpenAIAPIKey)
	}
	if accessToken == "" {
		return newQuotaPollError("token_missing", 0, nil, fmt.Errorf("no access token"))
	}
	chatgptUserID := acc.ChatGPTUserID
	if chatgptUserID == "" {
		chatgptUserID = acc.UpstreamAccountID
	}
	if chatgptUserID == "" {
		// Try the account_id embedded in the token struct
		chatgptUserID = acc.ID
	}

	hc, err := s.upstream.EgressHTTPClient(egress)
	if err != nil {
		return newQuotaPollError("egress_error", 0, nil, fmt.Errorf("build http client: %w", err))
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, whamUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-Id", chatgptUserID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")

	resp, err := hc.Do(req)
	if err != nil {
		return newQuotaPollError("network_error", 0, nil, fmt.Errorf("http: %w", err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return newQuotaPollError("http_error", resp.StatusCode, body, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var wham whamUsageResponse
	if err := json.Unmarshal(body, &wham); err != nil {
		return newQuotaPollError("decode_error", 0, nil, fmt.Errorf("decode: %w", err))
	}

	now := storage.Now()

	var rawDetail []byte
	if wham.RateLimit.SecondaryWindow.LimitWindowSeconds > 0 {
		raw, _ := json.Marshal(map[string]interface{}{
			"primary":   wham.RateLimit.PrimaryWindow,
			"secondary": wham.RateLimit.SecondaryWindow,
			"plan_type": wham.PlanType,
		})
		rawDetail = raw
	}

	pw := wham.RateLimit.PrimaryWindow
	if pw.LimitWindowSeconds <= 0 {
		return newQuotaPollError("partial", 0, rawDetail, fmt.Errorf("no primary window in wham response"))
	}

	snap := storage.AccountRateLimit{
		AccountID:       acc.ID,
		Provider:        "codex",
		LimiterType:     "5h_polled",
		Source:          "5h_polled",
		UsedPercent:     pw.UsedPercent,
		RemainingTokens: -1,
		LimitTokens:     -1,
		ResetAt:         now + pw.ResetAfterSeconds,
		Status:          statusFromReached(wham.RateLimit.LimitReached),
		Raw:             string(rawDetail),
		UpdatedAt:       now,
	}
	_ = s.store.UpsertAccountRateLimit(ctx, snap)
	sw := wham.RateLimit.SecondaryWindow
	if sw.LimitWindowSeconds > 0 {
		_ = s.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
			AccountID:         acc.ID,
			Provider:          "codex",
			LimiterType:       "7d_polled",
			Source:            "7d_polled",
			UsedPercent:       sw.UsedPercent,
			RemainingTokens:   -1,
			LimitTokens:       -1,
			LimitRequests:     -1,
			RemainingRequests: -1,
			ResetAt:           now + sw.ResetAfterSeconds,
			Status:            statusFromCodexWhamWindow(sw),
			Raw:               string(rawDetail),
			UpdatedAt:         now,
		})
	}
	if credits := parseCodexResetCredits(body, "usage_fallback"); credits.Known {
		credits.UpdatedAt = now
		s.upsertCodexResetCreditsSnapshot(ctx, acc.ID, credits)
	}
	return nil
}

type claudeOAuthUsageParsed struct {
	PlanType      string
	RateLimitTier string
	Windows       []claudeOAuthUsageWindow
}

type claudeOAuthUsageWindow struct {
	LimiterType        string
	Model              string
	Source             string
	UsedPercent        float64
	LimitTokens        int64
	RemainingTokens    int64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
	ResetAt            int64
	Status             string
	Raw                string
}

func (s *Server) pollOneClaudeQuota(ctx context.Context, acc storage.Account, token storage.AccountToken, egress storage.EgressProfile) error {
	var err error
	token, err = s.prepareClaudeToken(ctx, acc, token, "quota_preflight")
	if err != nil {
		return newQuotaPollError("auth_error", 0, nil, err)
	}
	if !claudeTokenCanRefresh(token) {
		return newQuotaPollError("unsupported_claude_non_oauth", 0, nil, fmt.Errorf("claude oauth usage requires oauth access_token and refresh_token"))
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Anthropic-Beta", "oauth-2025-04-20")
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         http.MethodGet,
			Provider:       "claude",
			PassThrough:    true,
			DownstreamPath: "/api/oauth/usage",
			Headers:        headers,
			Account:        acc,
			Token:          t,
			Egress:         egress,
		}
	}
	resp, err := s.upstream.Do(ctx, requestForToken(token))
	if err != nil {
		return newQuotaPollError("network_error", 0, nil, fmt.Errorf("http: %w", err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if refreshed, rerr := s.forceRefreshClaudeToken(ctx, acc, "auth_error"); rerr == nil {
			token = refreshed
			resp.Body.Close()
			resp, err = s.upstream.Do(ctx, requestForToken(token))
			if err != nil {
				return newQuotaPollError("network_error", 0, nil, fmt.Errorf("http after refresh: %w", err))
			}
			defer resp.Body.Close()
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		}
	}
	if resp.StatusCode != http.StatusOK {
		return newQuotaPollError("http_error", resp.StatusCode, body, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(bodySnippet(body, 300))))
	}
	parsed, err := parseClaudeOAuthUsage(body, storage.Now())
	if err != nil {
		return newQuotaPollError("decode_error", 0, body, err)
	}
	if parsed.PlanType != "" {
		_ = s.store.SetAccountPlanType(ctx, acc.ID, parsed.PlanType)
	}
	if parsed.RateLimitTier != "" && parsed.RateLimitTier != token.OAuthRateLimitTier {
		token.OAuthRateLimitTier = parsed.RateLimitTier
		_ = s.store.UpdateToken(ctx, token)
	}
	if len(parsed.Windows) == 0 {
		return newQuotaPollError("partial", 0, body, fmt.Errorf("no usage windows"))
	}
	rawByLimiter := map[string]string{}
	for _, win := range parsed.Windows {
		rawByLimiter[win.LimiterType] = win.Raw
	}
	for _, win := range parsed.Windows {
		raw := win.Raw
		if win.LimiterType == "5h_oauth_usage" {
			if secondary := rawByLimiter["7d_oauth_usage"]; secondary != "" {
				combined, _ := json.Marshal(map[string]interface{}{
					"plan_type":             parsed.PlanType,
					"oauth_rate_limit_tier": parsed.RateLimitTier,
					"primary":               json.RawMessage([]byte(win.Raw)),
					"secondary":             json.RawMessage([]byte(secondary)),
				})
				raw = string(combined)
			}
		}
		snap := storage.AccountRateLimit{
			AccountID:         acc.ID,
			Provider:          "claude",
			Model:             win.Model,
			LimiterType:       win.LimiterType,
			Source:            win.Source,
			UsedPercent:       win.UsedPercent,
			LimitTokens:       win.LimitTokens,
			RemainingTokens:   win.RemainingTokens,
			LimitRequests:     -1,
			RemainingRequests: -1,
			ResetAt:           win.ResetAt,
			Status:            firstNonEmpty(win.Status, "allowed"),
			Raw:               raw,
			UpdatedAt:         storage.Now(),
		}
		if err := s.store.UpsertAccountRateLimit(ctx, snap); err != nil {
			return err
		}
	}
	return nil
}

func parseClaudeOAuthUsage(body []byte, now int64) (claudeOAuthUsageParsed, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return claudeOAuthUsageParsed{}, fmt.Errorf("decode: %w", err)
	}
	out := claudeOAuthUsageParsed{
		PlanType:      jsonStringAny(root, "subscription_type", "subscriptionType", "plan_type"),
		RateLimitTier: jsonStringAny(root, "rate_limit_tier", "rateLimitTier", "oauth_rate_limit_tier"),
	}
	add := func(path string, m map[string]interface{}) {
		win, ok := claudeUsageWindowFromMap(path, m, now)
		if !ok {
			return
		}
		raw, _ := json.Marshal(m)
		win.Raw = string(raw)
		out.Windows = append(out.Windows, win)
	}
	var walk func(string, interface{})
	walk = func(path string, v interface{}) {
		switch typed := v.(type) {
		case map[string]interface{}:
			if path != "" {
				add(path, typed)
			}
			for name, child := range typed {
				childPath := name
				if path != "" {
					childPath = path + "." + name
				}
				walk(childPath, child)
			}
		case []interface{}:
			for i, child := range typed {
				childPath := fmt.Sprintf("%s.%d", path, i)
				walk(childPath, child)
			}
		}
	}
	walk("", root)
	return out, nil
}

func claudeUsageWindowFromMap(name string, m map[string]interface{}, now int64) (claudeOAuthUsageWindow, bool) {
	used, ok := jsonFloatAny(m, "used_percent", "usedPercent", "usage_percent")
	limitWindow := jsonIntAny(m, "limit_window_seconds", "limitWindowSeconds", "window_seconds", "windowSeconds")
	resetAfter := jsonIntAny(m, "reset_after_seconds", "resetAfterSeconds")
	resetAt := jsonIntAny(m, "reset_at", "resetAt", "resets_at", "resetsAt")
	if !ok && limitWindow == 0 && resetAfter == 0 && resetAt == 0 {
		return claudeOAuthUsageWindow{}, false
	}
	windowName := firstNonEmpty(jsonStringAny(m, "limiter_type", "limiterType", "name", "type"), name)
	limiter, model := normalizeClaudeUsageLimiter(windowName, limitWindow)
	if limiter == "" {
		return claudeOAuthUsageWindow{}, false
	}
	model = firstNonEmpty(model, jsonStringAny(m, "model"))
	if resetAt == 0 && resetAfter > 0 {
		resetAt = now + resetAfter
	}
	return claudeOAuthUsageWindow{
		LimiterType:        limiter,
		Model:              model,
		Source:             limiter,
		UsedPercent:        used,
		LimitTokens:        jsonIntDefault(m, -1, "limit_tokens", "limitTokens"),
		RemainingTokens:    jsonIntDefault(m, -1, "remaining_tokens", "remainingTokens"),
		LimitWindowSeconds: limitWindow,
		ResetAfterSeconds:  resetAfter,
		ResetAt:            resetAt,
		Status:             jsonStringAny(m, "status", "state"),
	}, true
}

func normalizeClaudeUsageLimiter(name string, windowSeconds int64) (limiter, model string) {
	l := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(l, "oauth") && strings.Contains(l, "app"):
		return "oauth_app", ""
	case strings.Contains(l, "claude") || strings.Contains(l, "opus") || strings.Contains(l, "sonnet") || strings.Contains(l, "haiku"):
		return "", ""
	case strings.Contains(l, "5h") || strings.Contains(l, "five") || (windowSeconds > 0 && windowSeconds <= 6*3600):
		return "5h_oauth_usage", ""
	case strings.Contains(l, "7d") || strings.Contains(l, "week") || windowSeconds >= 6*24*3600:
		return "7d_oauth_usage", ""
	default:
		return "", ""
	}
}

func jsonStringAny(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func jsonFloatAny(m map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case string:
			var f float64
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%f", &f); err == nil {
				return f, true
			}
		}
	}
	return -1, false
}

func jsonIntAny(m map[string]interface{}, keys ...string) int64 {
	n, _ := jsonIntAnyOK(m, keys...)
	return n
}

func jsonIntAnyOK(m map[string]interface{}, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return int64(v), true
		case int:
			return int64(v), true
		case int64:
			return v, true
		case string:
			var n int64
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func jsonIntDefault(m map[string]interface{}, def int64, keys ...string) int64 {
	if n, ok := jsonIntAnyOK(m, keys...); ok {
		return n
	}
	return def
}

func statusFromReached(reached bool) string {
	if reached {
		return "rejected"
	}
	return "allowed_warning"
}

func statusFromCodexWhamWindow(win whamWindow) string {
	if status := strings.TrimSpace(win.Status); status != "" {
		return status
	}
	if win.UsedPercent >= 100 {
		return "rejected"
	}
	return "allowed_warning"
}
