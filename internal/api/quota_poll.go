package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
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
	whamUsageURL           = "https://chatgpt.com/backend-api/wham/usage"
)

// whamUsageResponse mirrors the JSON shape returned by /backend-api/wham/usage.
type whamUsageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		LimitReached    bool       `json:"limit_reached"`
		PrimaryWindow   whamWindow `json:"primary_window"`
		SecondaryWindow whamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type whamWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
}

type quotaPollTarget struct {
	Account storage.Account
	Token   storage.AccountToken
	Egress  storage.EgressProfile
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

// pollAllCodexQuotas fetches /backend-api/wham/usage for every active Codex
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
	if len(codex) == 0 && missingTokens == 0 {
		return
	}
	s.attachQuotaPollEgresses(ctx, codex)
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
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("[QUOTA-POLL] tick complete: updated=%d failed=%d total_codex=%d", updated, failed, len(codex))
	}
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
	return provider == "" || provider == "codex" || provider == "chatgpt" || provider == "openai"
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
		return fmt.Errorf("no access token")
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
		return fmt.Errorf("build http client: %w", err)
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
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var wham whamUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&wham); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	now := storage.Now()

	// Primary (5h) window → the urgency gauge operators care about.
	// The ON CONFLICT(account_id) constraint means only one row per
	// account, so we store the 5h window as the primary snapshot and
	// embed the 7d window in the raw_json detail for the drawer.
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
		return fmt.Errorf("no primary window in wham response")
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
	return nil
}

func statusFromReached(reached bool) string {
	if reached {
		return "rejected"
	}
	return "allowed_warning"
}
