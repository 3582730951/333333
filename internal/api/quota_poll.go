package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/cursorproxy"
	"codex-account-pool/internal/entitlement"
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
	quotaPollIntervalFloor = 120 // seconds — manual refresh reuse floor
	quotaPollMaxConcurrent = 5   // max concurrent fetches per tick
	quotaRefreshTimeout    = 10 * time.Minute

	// Healthy observations are refreshed before the scheduler's 15-minute trust
	// window expires. Exhausted observations are instead held until their own
	// reset deadline, with a small deterministic jitter so a shared window does
	// not wake every account in the same instant.
	quotaPollHealthyRefreshSeconds = int64(10 * 60)
	quotaPollResetJitterSeconds    = int64(5)
	quotaPollBatchWindowSeconds    = int64(30)
	quotaPollFailureBackoffBase    = int64(30)
	quotaPollFailureBackoffMax     = int64(15 * 60)
	quotaPollInitialStagger        = 30 * time.Second
)

var whamUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func codexWhamPrimaryWindowKind(primarySeconds, secondarySeconds int64) string {
	if primarySeconds <= 0 {
		return ""
	}
	if secondarySeconds <= 0 && primarySeconds >= 24*60*60 {
		return quotaWindowKind7d
	}
	return quotaWindowKind5h
}

// whamUsageResponse mirrors the JSON shape returned by /backend-api/wham/usage.
// Field names follow the codex-backend OpenAPI models (RateLimitStatusPayload):
// rate_limit, credits, spend_control and additional_rate_limits are siblings.
type whamUsageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		Allowed         bool       `json:"allowed"`
		LimitReached    bool       `json:"limit_reached"`
		PrimaryWindow   whamWindow `json:"primary_window"`
		SecondaryWindow whamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	// Extra paid balance beyond the plan's included windows. Distinct from
	// rate_limit_reset_credits, which counts discrete rate-limit resets.
	Credits      *whamCredits      `json:"credits"`
	SpendControl *whamSpendControl `json:"spend_control"`

	RateLimitResetCredits      map[string]interface{} `json:"rate_limit_reset_credits"`
	RateLimitResetCreditsCamel map[string]interface{} `json:"rateLimitResetCredits"`
}

// whamCredits mirrors CreditStatusDetails. `balance` is a preformatted string
// upstream (it may be hidden even when credits exist), so it is carried verbatim
// rather than coerced to a number.
type whamCredits struct {
	HasCredits            bool    `json:"has_credits"`
	Unlimited             bool    `json:"unlimited"`
	Balance               *string `json:"balance"`
	RemainingMilliCredits *int64  `json:"remaining_milli_credits,omitempty"`
}

// whamSpendControl mirrors SpendControlStatusDetails.
type whamSpendControl struct {
	Reached         bool                   `json:"reached"`
	IndividualLimit *whamSpendControlLimit `json:"individual_limit"`
}

// whamSpendControlLimit mirrors SpendControlLimitDetails.
type whamSpendControlLimit struct {
	Source            string  `json:"source"`
	Limit             string  `json:"limit"`
	Used              string  `json:"used"`
	Remaining         string  `json:"remaining"`
	UsedPercent       float64 `json:"used_percent"`
	RemainingPercent  float64 `json:"remaining_percent"`
	ResetAfterSeconds int64   `json:"reset_after_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

// UnmarshalJSON accepts the two wire variants observed in the wild: monetary
// fields are strings in the browser response but some deployments serialize
// them as JSON numbers.  A strict string unmarshal would discard the entire
// quota response (and therefore its usable windows) when that happens.
func (w *whamUsageResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Allowed         bool       `json:"allowed"`
			LimitReached    bool       `json:"limit_reached"`
			PrimaryWindow   whamWindow `json:"primary_window"`
			SecondaryWindow whamWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits                    json.RawMessage        `json:"credits"`
		SpendControl               json.RawMessage        `json:"spend_control"`
		RateLimitResetCredits      map[string]interface{} `json:"rate_limit_reset_credits"`
		RateLimitResetCreditsCamel map[string]interface{} `json:"rateLimitResetCredits"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*w = whamUsageResponse{PlanType: wire.PlanType}
	w.RateLimit.Allowed, w.RateLimit.LimitReached = wire.RateLimit.Allowed, wire.RateLimit.LimitReached
	w.RateLimit.PrimaryWindow, w.RateLimit.SecondaryWindow = wire.RateLimit.PrimaryWindow, wire.RateLimit.SecondaryWindow
	w.RateLimitResetCredits, w.RateLimitResetCreditsCamel = wire.RateLimitResetCredits, wire.RateLimitResetCreditsCamel
	if len(wire.Credits) > 0 && string(wire.Credits) != "null" {
		var credits struct {
			HasCredits            bool            `json:"has_credits"`
			Unlimited             bool            `json:"unlimited"`
			Balance               json.RawMessage `json:"balance"`
			RemainingMilliCredits json.RawMessage `json:"remaining_milli_credits"`
			CreditsRemainingMilli json.RawMessage `json:"credits_remaining_milli"`
			RemainingCredits      json.RawMessage `json:"remaining_credits"`
		}
		if err := json.Unmarshal(wire.Credits, &credits); err != nil {
			return err
		}
		w.Credits = &whamCredits{HasCredits: credits.HasCredits, Unlimited: credits.Unlimited}
		if value := flexibleJSONText(credits.Balance); value != "" {
			w.Credits.Balance = &value
		}
		for index, raw := range []json.RawMessage{credits.RemainingMilliCredits, credits.CreditsRemainingMilli, credits.RemainingCredits} {
			if value, ok := flexibleNonNegativeInt(raw); ok {
				// A field explicitly named *credits is interpreted as whole credits;
				// milli_credits is already in storage units.
				if index == 2 && len(credits.RemainingMilliCredits) == 0 && len(credits.CreditsRemainingMilli) == 0 {
					if value <= math.MaxInt64/1000 {
						value *= 1000
					}
				}
				w.Credits.RemainingMilliCredits = &value
				break
			}
		}
	}
	if len(wire.SpendControl) > 0 && string(wire.SpendControl) != "null" {
		var spend struct {
			Reached         bool            `json:"reached"`
			IndividualLimit json.RawMessage `json:"individual_limit"`
		}
		if err := json.Unmarshal(wire.SpendControl, &spend); err != nil {
			return err
		}
		w.SpendControl = &whamSpendControl{Reached: spend.Reached}
		if len(spend.IndividualLimit) > 0 && string(spend.IndividualLimit) != "null" {
			var limit struct {
				Source            json.RawMessage `json:"source"`
				Limit             json.RawMessage `json:"limit"`
				Used              json.RawMessage `json:"used"`
				Remaining         json.RawMessage `json:"remaining"`
				UsedPercent       float64         `json:"used_percent"`
				RemainingPercent  float64         `json:"remaining_percent"`
				ResetAfterSeconds int64           `json:"reset_after_seconds"`
				ResetAt           int64           `json:"reset_at"`
			}
			if err := json.Unmarshal(spend.IndividualLimit, &limit); err != nil {
				return err
			}
			w.SpendControl.IndividualLimit = &whamSpendControlLimit{
				Source: flexibleJSONText(limit.Source), Limit: flexibleJSONText(limit.Limit),
				Used: flexibleJSONText(limit.Used), Remaining: flexibleJSONText(limit.Remaining),
				UsedPercent: limit.UsedPercent, RemainingPercent: limit.RemainingPercent,
				ResetAfterSeconds: limit.ResetAfterSeconds, ResetAt: limit.ResetAt,
			}
		}
	}
	return nil
}

func flexibleJSONText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return strings.TrimSpace(number.String())
	}
	return ""
}

func flexibleNonNegativeInt(raw json.RawMessage) (int64, bool) {
	text := flexibleJSONText(raw)
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value >= 0
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

type quotaRefreshResult struct {
	Updated           int      `json:"updated"`
	Failed            int      `json:"failed"`
	Candidates        int      `json:"candidates"`
	StartedAt         int64    `json:"started_at"`
	CompletedAt       int64    `json:"completed_at"`
	Coalesced         bool     `json:"coalesced,omitempty"`
	Reused            bool     `json:"reused,omitempty"`
	Scoped            bool     `json:"-"`
	UpdatedAccountIDs []string `json:"-"`
	FailedAccountIDs  []string `json:"-"`
}

type quotaRefreshFlight struct {
	done   chan struct{}
	result quotaRefreshResult
	scoped bool
}

func (s *Server) upsertLiveQuotaSnapshot(ctx context.Context, snapshot storage.AccountRateLimit) error {
	if s.scheduler != nil {
		s.scheduler.ApplyRateLimitSnapshot(snapshot)
		s.wakeRouteAvailability()
	}
	return s.store.UpsertAccountRateLimit(ctx, snapshot)
}

type quotaPollError struct {
	reason     string
	statusCode int
	body       string
	err        error
}

type quotaPollScopeContextKey struct{}

// withQuotaPollScope keeps the existing quota refresh flight/coalescing path
// while allowing the background scheduler to refresh only accounts whose reset
// or freshness deadline is due. A nil slice means the historical full scan.
func withQuotaPollScope(ctx context.Context, accountIDs []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	copyIDs := append([]string(nil), accountIDs...)
	return context.WithValue(ctx, quotaPollScopeContextKey{}, copyIDs)
}

func quotaPollScope(ctx context.Context) (map[string]struct{}, bool) {
	if ctx == nil {
		return nil, false
	}
	raw, ok := ctx.Value(quotaPollScopeContextKey{}).([]string)
	if !ok {
		return nil, false
	}
	scope := make(map[string]struct{}, len(raw))
	for _, id := range raw {
		if id = strings.TrimSpace(id); id != "" {
			scope[id] = struct{}{}
		}
	}
	return scope, true
}

type quotaPollFailureState struct {
	attempts int
	retryAt  int64
}

type quotaPollPlan struct {
	NextAt     int64
	AccountIDs []string
}

type quotaPollDueAccount struct {
	ID string
	At int64
}

// quotaPollJitter is stable per account, so a process restart does not create a
// new random burst while still spreading accounts sharing one upstream window.
func quotaPollJitter(accountID string) int64 {
	var hash uint32 = 2166136261
	for i := 0; i < len(accountID); i++ {
		hash ^= uint32(accountID[i])
		hash *= 16777619
	}
	return int64(hash % uint32(quotaPollResetJitterSeconds+1))
}

func quotaPollErrorSnapshot(row storage.AccountRateLimit) bool {
	return strings.TrimSpace(row.LimiterType) == "quota_poll_error" ||
		strings.TrimSpace(row.Source) == "quota_poll_error" ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(row.Status)), "error/")
}

// buildQuotaPollPlan chooses the next account-level refresh deadline. It is
// intentionally pure so reset timing, jitter, batching and failure backoff can
// be tested without sleeping or making an upstream request.
func buildQuotaPollPlan(accounts []storage.Account, rows []storage.AccountRateLimit, now int64, failures map[string]quotaPollFailureState) quotaPollPlan {
	byAccount := make(map[string][]storage.AccountRateLimit)
	for _, row := range rows {
		byAccount[row.AccountID] = append(byAccount[row.AccountID], row)
	}
	plan := quotaPollPlan{}
	dueAccounts := make([]quotaPollDueAccount, 0, len(accounts))
	for _, account := range accounts {
		if !isQuotaPollCandidate(account, now) && !isKiroQuotaPollCandidate(account, now) && !isCursorQuotaPollCandidate(account, now) {
			continue
		}
		accountID := strings.TrimSpace(account.ID)
		if accountID == "" {
			continue
		}
		latest := int64(0)
		exhaustedReset := int64(0)
		dueReset := false
		for _, row := range byAccount[accountID] {
			if quotaPollErrorSnapshot(row) {
				continue
			}
			if row.UpdatedAt > latest {
				latest = row.UpdatedAt
			}
			if row.ResetAt <= 0 {
				continue
			}
			if row.UpdatedAt > 0 && now > row.UpdatedAt && now-row.UpdatedAt > storage.AccountRateLimitSnapshotMaxAgeSeconds {
				continue
			}
			if row.ResetAt <= now {
				dueReset = true
				continue
			}
			if !storage.AccountRateLimitIsExhausted(row) {
				continue
			}
			if exhaustedReset == 0 || row.ResetAt < exhaustedReset {
				exhaustedReset = row.ResetAt
			}
		}
		baseAt := now
		switch {
		case dueReset:
			baseAt = now
		case exhaustedReset > now:
			baseAt = exhaustedReset
		case latest > 0 && now-latest <= storage.AccountRateLimitSnapshotMaxAgeSeconds:
			baseAt = latest + quotaPollHealthyRefreshSeconds
		case latest > 0 && now < latest:
			baseAt = now + quotaPollHealthyRefreshSeconds
		}
		if baseAt < now {
			baseAt = now
		}
		dueAt := baseAt + quotaPollJitter(accountID)
		if state, ok := failures[accountID]; ok && state.retryAt > dueAt {
			dueAt = state.retryAt
		}
		if plan.NextAt == 0 || dueAt < plan.NextAt {
			plan.NextAt = dueAt
		}
		dueAccounts = append(dueAccounts, quotaPollDueAccount{ID: accountID, At: dueAt})
	}
	// Include only accounts close to the earliest deadline. This batches a shared
	// reset window without polling a later account early merely because the current
	// wall clock happens to be within the broad batch window.
	if plan.NextAt > 0 {
		batchEnd := plan.NextAt + quotaPollBatchWindowSeconds
		for _, due := range dueAccounts {
			if due.At <= batchEnd {
				plan.AccountIDs = append(plan.AccountIDs, due.ID)
			}
		}
	}
	sort.Strings(plan.AccountIDs)
	return plan
}

func quotaPollFailureBackoff(attempts int) int64 {
	if attempts <= 0 {
		return quotaPollFailureBackoffBase
	}
	backoff := quotaPollFailureBackoffBase
	for i := 1; i < attempts; i++ {
		if backoff >= quotaPollFailureBackoffMax/2 {
			return quotaPollFailureBackoffMax
		}
		backoff *= 2
	}
	if backoff > quotaPollFailureBackoffMax {
		return quotaPollFailureBackoffMax
	}
	return backoff
}

func observeQuotaPollResult(failures map[string]quotaPollFailureState, result quotaRefreshResult, now int64) {
	for _, accountID := range result.UpdatedAccountIDs {
		delete(failures, accountID)
	}
	for _, accountID := range result.FailedAccountIDs {
		state := failures[accountID]
		state.attempts++
		state.retryAt = now + quotaPollFailureBackoff(state.attempts)
		failures[accountID] = state
	}
}

func waitQuotaPollUntil(ctx context.Context, unixAt int64) bool {
	if ctx == nil {
		return false
	}
	seconds := unixAt - storage.Now()
	if seconds <= 0 {
		return true
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
		s.runQuotaPoller(ctx, quotaPollInitialStagger, nil)
	})
}

// runQuotaPoller is split out of StartQuotaPoller so its cancellation behavior can
// be tested after it has entered a real timer wait. ready is test-only plumbing;
// production passes nil.
func (s *Server) runQuotaPoller(ctx context.Context, initialStagger time.Duration, ready chan<- struct{}) {
	// Stagger startup so the server is fully ready before the first poll.
	stagger := time.NewTimer(initialStagger)
	defer stagger.Stop()
	select {
	case <-ctx.Done():
		return
	case <-stagger.C:
	}
	log.Printf("[QUOTA-POLL] started (healthy_refresh=%ds, max_concurrent=%d, reset_jitter=%ds)", quotaPollHealthyRefreshSeconds, quotaPollMaxConcurrent, quotaPollResetJitterSeconds)
	failures := make(map[string]quotaPollFailureState)
	// First poll is intentionally full so accounts without a snapshot enter the
	// reset-driven plan. Subsequent scans are scoped to due accounts.
	if result, err := s.waitForQuotaRefreshScoped(ctx, nil, true); err == nil {
		observeQuotaPollResult(failures, result, storage.Now())
	}
	if ready != nil {
		close(ready)
	}
	for {
		accounts, err := s.store.ListAccounts(ctx)
		if err != nil {
			if !waitQuotaPollUntil(ctx, storage.Now()+quotaPollFailureBackoffBase) {
				return
			}
			continue
		}
		rows, err := s.store.ListAccountRateLimits(ctx)
		if err != nil {
			if !waitQuotaPollUntil(ctx, storage.Now()+quotaPollFailureBackoffBase) {
				return
			}
			continue
		}
		plan := buildQuotaPollPlan(accounts, rows, storage.Now(), failures)
		if plan.NextAt == 0 {
			if !waitQuotaPollUntil(ctx, storage.Now()+quotaPollHealthyRefreshSeconds) {
				return
			}
			continue
		}
		if plan.NextAt > storage.Now() {
			if !waitQuotaPollUntil(ctx, plan.NextAt) {
				return
			}
			continue
		}
		if len(plan.AccountIDs) == 0 {
			// A due account can be just outside the batch window after a clock
			// tick; recalculate rather than spinning.
			if !waitQuotaPollUntil(ctx, storage.Now()+1) {
				return
			}
			continue
		}
		result, err := s.waitForQuotaRefreshScoped(ctx, plan.AccountIDs, true)
		if err != nil {
			for _, accountID := range plan.AccountIDs {
				state := failures[accountID]
				state.attempts++
				state.retryAt = storage.Now() + quotaPollFailureBackoff(state.attempts)
				failures[accountID] = state
			}
			continue
		}
		observeQuotaPollResult(failures, result, storage.Now())
	}
}

// pollAllCodexQuotas fetches usage snapshots for every active Codex/Claude
// account, concurrency-capped. Best-effort: a single account failure never
// blocks the rest.
func (s *Server) pollAllCodexQuotas(ctx context.Context) (result quotaRefreshResult) {
	result.StartedAt = storage.Now()
	defer func() { result.CompletedAt = storage.Now() }()
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListAccounts error: %v", err)
		result.Failed = 1
		return result
	}
	now := storage.Now()
	candidateIDs := quotaPollCandidateAccountIDs(accounts, now)
	pollAccounts := accounts
	if scope, scoped := quotaPollScope(ctx); scoped {
		filtered := candidateIDs[:0]
		for _, accountID := range candidateIDs {
			if _, ok := scope[accountID]; ok {
				filtered = append(filtered, accountID)
			}
		}
		candidateIDs = filtered
		filteredAccounts := make([]storage.Account, 0, len(candidateIDs))
		for _, account := range accounts {
			if _, ok := scope[account.ID]; ok {
				filteredAccounts = append(filteredAccounts, account)
			}
		}
		pollAccounts = filteredAccounts
	}
	if len(candidateIDs) == 0 {
		return result
	}
	result.Candidates = len(candidateIDs)
	tokens, err := s.store.ListTokensByAccountIDs(ctx, candidateIDs)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListTokensByAccountIDs error: %v", err)
		result.Failed = len(candidateIDs)
		result.FailedAccountIDs = uniqueSortedStrings(candidateIDs)
		return result
	}
	codex, _ := codexQuotaPollTargets(pollAccounts, tokens, now)
	claude := claudeQuotaPollTargets(pollAccounts, tokens, now)
	kiro := kiroQuotaPollTargets(pollAccounts, tokens, now)
	cursor := cursorQuotaPollTargets(pollAccounts, tokens, now)
	unattemptedIDs := quotaPollUnattemptedAccountIDs(candidateIDs, codex, claude, kiro, cursor)
	if len(codex) == 0 && len(claude) == 0 && len(kiro) == 0 && len(cursor) == 0 && len(unattemptedIDs) == 0 {
		return result
	}
	if len(codex) == 0 && len(claude) == 0 && len(kiro) == 0 && len(cursor) == 0 {
		result.Failed = len(unattemptedIDs)
		result.FailedAccountIDs = unattemptedIDs
		return result
	}
	s.attachQuotaPollEgresses(ctx, codex)
	s.attachQuotaPollEgresses(ctx, claude)
	s.attachQuotaPollEgresses(ctx, kiro)
	s.attachQuotaPollEgresses(ctx, cursor)
	updated := 0
	failed := len(unattemptedIDs)
	failedIDs := unattemptedIDs
	updatedIDs := make([]string, 0, len(codex)+len(claude)+len(kiro)+len(cursor))
	// Concurrency-capped worker pool so we never open N connections at once.
	sem := make(chan struct{}, quotaPollMaxConcurrent)
	var mu sync.Mutex
	markFailed := func(accountID string) {
		mu.Lock()
		failed++
		if strings.TrimSpace(accountID) != "" {
			failedIDs = append(failedIDs, accountID)
		}
		mu.Unlock()
	}
	markUpdated := func(accountID string) {
		mu.Lock()
		updated++
		if strings.TrimSpace(accountID) != "" {
			updatedIDs = append(updatedIDs, accountID)
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := range codex {
		target := codex[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					markFailed(target.Account.ID)
					supervisor.LogPanic("quota-poller-worker", v)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				markFailed(target.Account.ID)
				log.Printf("[QUOTA-POLL] account=%s skipped: %v", target.Account.ID, ctx.Err())
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneCodexQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "codex", err)
				markFailed(target.Account.ID)
				log.Printf("[QUOTA-POLL] account=%s poll failed: %v", target.Account.ID, err)
			} else {
				markUpdated(target.Account.ID)
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
					markFailed(target.Account.ID)
					supervisor.LogPanic("quota-poller-worker", v)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				markFailed(target.Account.ID)
				log.Printf("[QUOTA-POLL] account=%s skipped: %v", target.Account.ID, ctx.Err())
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneClaudeQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "claude", err)
				markFailed(target.Account.ID)
				log.Printf("[QUOTA-POLL] account=%s claude poll failed: %v", target.Account.ID, err)
			} else {
				markUpdated(target.Account.ID)
			}
		}()
	}
	for i := range kiro {
		target := kiro[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					markFailed(target.Account.ID)
					supervisor.LogPanic("quota-poller-kiro-worker", panicValue)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				markFailed(target.Account.ID)
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneKiroQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, "kiro", err)
				markFailed(target.Account.ID)
			} else {
				markUpdated(target.Account.ID)
			}
		}()
	}
	for i := range cursor {
		target := cursor[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					markFailed(target.Account.ID)
					supervisor.LogPanic("quota-poller-cursor-worker", panicValue)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				markFailed(target.Account.ID)
				return
			}
			defer func() { <-sem }()
			if err := s.pollOneCursorQuota(ctx, target.Account, target.Token, target.Egress); err != nil {
				s.recordQuotaPollError(ctx, target.Account, cursorproxy.ProviderID, err)
				markFailed(target.Account.ID)
			} else {
				markUpdated(target.Account.ID)
			}
		}()
	}
	wg.Wait()
	if updated > 0 && s.scheduler != nil {
		s.scheduler.NotifyStateChanged()
	}
	if updated > 0 {
		s.wakeRouteAvailability()
	}
	if updated > 0 || failed > 0 {
		log.Printf("[QUOTA-POLL] tick complete: updated=%d failed=%d total_codex=%d total_claude=%d total_kiro=%d total_cursor=%d", updated, failed, len(codex), len(claude), len(kiro), len(cursor))
	}
	result.Updated = updated
	result.Failed = failed
	result.UpdatedAccountIDs = uniqueSortedStrings(updatedIDs)
	result.FailedAccountIDs = uniqueSortedStrings(failedIDs)
	return result
}

// beginQuotaRefresh coalesces both the background ticker and every manual UI
// refresh into one global flight. This prevents repeated clicks, multiple admin
// tabs, and a coincident ticker from multiplying upstream requests.
func (s *Server) beginQuotaRefresh(base context.Context) (*quotaRefreshFlight, bool) {
	return s.beginQuotaRefreshWithScope(base, nil, false)
}

func (s *Server) beginQuotaRefreshWithScope(base context.Context, accountIDs []string, bypassFloor bool) (*quotaRefreshFlight, bool) {
	s.quotaRefreshMu.Lock()
	if current := s.quotaRefreshFlight; current != nil {
		s.quotaRefreshMu.Unlock()
		return current, false
	}
	if !bypassFloor {
		if last := s.quotaRefreshLast; last.CompletedAt > 0 && storage.Now()-last.CompletedAt < quotaPollIntervalFloor {
			if !last.Scoped {
				last.Reused = true
				flight := &quotaRefreshFlight{done: make(chan struct{}), result: last}
				close(flight.done)
				s.quotaRefreshMu.Unlock()
				return flight, false
			}
		}
	}
	flight := &quotaRefreshFlight{done: make(chan struct{}), scoped: accountIDs != nil}
	s.quotaRefreshFlight = flight
	s.quotaRefreshMu.Unlock()
	if base == nil {
		base = context.Background()
	}
	pollBase := base
	if accountIDs != nil {
		pollBase = withQuotaPollScope(pollBase, accountIDs)
	}
	go func() {
		defer func() {
			if panicValue := recover(); panicValue != nil {
				supervisor.LogPanic("quota-refresh-flight", panicValue)
				flight.result.Failed++
			}
			if flight.result.StartedAt == 0 {
				flight.result.StartedAt = storage.Now()
			}
			flight.result.Scoped = flight.scoped
			flight.result.CompletedAt = storage.Now()
			s.quotaRefreshMu.Lock()
			if s.quotaRefreshFlight == flight {
				s.quotaRefreshFlight = nil
			}
			s.quotaRefreshLast = flight.result
			close(flight.done)
			s.quotaRefreshMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(pollBase, quotaRefreshTimeout)
		defer cancel()
		flight.result = s.pollAllCodexQuotas(ctx)
	}()
	return flight, true
}

func (s *Server) waitForQuotaRefresh(ctx context.Context) (quotaRefreshResult, error) {
	base := s.runtimeTaskCtx
	if base == nil {
		base = ctx
	}
	flight, leader := s.beginQuotaRefresh(base)
	select {
	case <-flight.done:
		result := flight.result
		if result.Scoped {
			// A manual refresh that arrived during (or immediately after) a targeted
			// background scan must still mean "refresh all", not merely return the
			// targeted subset. This second full flight coalesces concurrent manual
			// callers and bypasses the manual reuse floor for the scoped predecessor.
			fullFlight, fullLeader := s.beginQuotaRefreshWithScope(base, nil, true)
			select {
			case <-fullFlight.done:
				result = fullFlight.result
				result.Coalesced = !leader || !fullLeader
				return result, nil
			case <-ctx.Done():
				return quotaRefreshResult{Coalesced: !leader || !fullLeader}, ctx.Err()
			}
		}
		result.Coalesced = !leader
		return result, nil
	case <-ctx.Done():
		return quotaRefreshResult{Coalesced: !leader}, ctx.Err()
	}
}

func (s *Server) waitForQuotaRefreshScoped(ctx context.Context, accountIDs []string, bypassFloor bool) (quotaRefreshResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	flight, leader := s.beginQuotaRefreshWithScope(ctx, accountIDs, bypassFloor)
	select {
	case <-flight.done:
		result := flight.result
		result.Coalesced = !leader
		return result, nil
	case <-ctx.Done():
		return quotaRefreshResult{Coalesced: !leader}, ctx.Err()
	}
}

func (s *Server) adminQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	result, err := s.waitForQuotaRefresh(r.Context())
	if err != nil {
		writeError(w, http.StatusRequestTimeout, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func kiroQuotaPollTargets(accounts []storage.Account, tokens map[string]storage.AccountToken, now int64) []quotaPollTarget {
	var out []quotaPollTarget
	for _, account := range accounts {
		if isKiroQuotaPollCandidate(account, now) {
			if token, ok := tokens[account.ID]; ok {
				out = append(out, quotaPollTarget{Account: account, Token: token})
			}
		}
	}
	return out
}

func cursorQuotaPollTargets(accounts []storage.Account, tokens map[string]storage.AccountToken, now int64) []quotaPollTarget {
	var out []quotaPollTarget
	for _, account := range accounts {
		if !isCursorQuotaPollCandidate(account, now) {
			continue
		}
		token, ok := tokens[account.ID]
		if !ok || !strings.EqualFold(strings.TrimSpace(token.CredentialMode), cursorproxy.CredentialBrowser) {
			continue
		}
		out = append(out, quotaPollTarget{Account: account, Token: token})
	}
	return out
}

func (s *Server) pollOneCursorQuota(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile) error {
	if !strings.EqualFold(strings.TrimSpace(token.CredentialMode), cursorproxy.CredentialBrowser) {
		return newQuotaPollError("unsupported_api_key_billing", 0, nil, errors.New("Cursor User API keys do not expose individual-plan usage through the official API"))
	}
	credential := cursorproxy.Credential{BridgeKey: strings.TrimSpace(token.AccessToken), ConfigDir: strings.TrimSpace(token.AgentRuntimeID)}
	client, err := s.upstream.EgressHTTPClient(egress)
	if err != nil {
		return newQuotaPollError("cursor_egress", 0, nil, err)
	}
	usage, err := s.cursorProxy.FetchUsageWithClient(ctx, credential, client)
	if err != nil {
		return newQuotaPollError("cursor_usage", 0, nil, err)
	}
	if len(usage.Models) == 0 {
		return newQuotaPollError("partial", 0, nil, errors.New("Cursor usage response contained no model pools"))
	}
	for _, snapshot := range cursorUsageSnapshots(account.ID, usage, storage.Now()) {
		if err := s.upsertLiveQuotaSnapshot(ctx, snapshot); err != nil {
			return err
		}
	}
	return nil
}

// cursorUsageSnapshots keeps per-model diagnostics while emitting one stable
// aggregate row for the quota page. The aggregate follows the most-consumed
// bounded pool; it cannot randomly switch with Go map iteration order.
func cursorUsageSnapshots(accountID string, usage cursorproxy.Usage, updatedAt int64) []storage.AccountRateLimit {
	resetAt := int64(0)
	if start, parseErr := time.Parse(time.RFC3339, usage.StartOfMonth); parseErr == nil {
		resetAt = start.AddDate(0, 1, 0).Unix()
	}
	models := make([]string, 0, len(usage.Models))
	for model := range usage.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	individual := make([]storage.AccountRateLimit, 0, len(models))
	aggregate := storage.AccountRateLimit{
		AccountID: accountID, Provider: cursorproxy.ProviderID,
		LimiterType: "cursor_monthly", Source: "cursor_usage", UsedPercent: -1,
		LimitTokens: -1, RemainingTokens: -1, LimitRequests: -1, RemainingRequests: -1,
		ResetAt: resetAt, Status: "partial", UpdatedAt: updatedAt,
	}
	limitingModel := ""
	bestSet := false
	for _, model := range models {
		value := usage.Models[model]
		usedPercent := float64(-1)
		limitRequests, remainingRequests := int64(-1), int64(-1)
		limitTokens, remainingTokens := int64(-1), int64(-1)
		if value.MaxRequestUsage != nil && *value.MaxRequestUsage > 0 {
			limitRequests = *value.MaxRequestUsage
			remainingRequests = max(int64(0), limitRequests-value.NumRequests)
			usedPercent = float64(value.NumRequests) * 100 / float64(limitRequests)
		} else if value.MaxTokenUsage != nil && *value.MaxTokenUsage > 0 {
			limitTokens = *value.MaxTokenUsage
			remainingTokens = max(int64(0), limitTokens-value.NumTokens)
			usedPercent = float64(value.NumTokens) * 100 / float64(limitTokens)
		}
		if usedPercent > 100 {
			usedPercent = 100
		}
		status := "allowed"
		if usedPercent < 0 {
			status = "partial"
		} else if usedPercent >= 100 {
			status = "exhausted"
		}
		raw, _ := json.Marshal(value)
		snapshot := storage.AccountRateLimit{
			AccountID: accountID, Provider: cursorproxy.ProviderID, Model: model,
			LimiterType: "cursor_model_monthly", Source: "cursor_usage", UsedPercent: usedPercent,
			LimitTokens: limitTokens, RemainingTokens: remainingTokens,
			LimitRequests: limitRequests, RemainingRequests: remainingRequests,
			ResetAt: resetAt, Status: status, Raw: string(raw), UpdatedAt: updatedAt,
		}
		individual = append(individual, snapshot)
		if !bestSet || usedPercent > aggregate.UsedPercent {
			bestSet = true
			limitingModel = model
			aggregate.UsedPercent = usedPercent
			aggregate.LimitTokens = limitTokens
			aggregate.RemainingTokens = remainingTokens
			aggregate.LimitRequests = limitRequests
			aggregate.RemainingRequests = remainingRequests
			aggregate.Status = status
		}
	}
	aggregateRaw, _ := json.Marshal(map[string]any{"limiting_model": limitingModel, "usage": usage})
	aggregate.Raw = string(aggregateRaw)
	return append([]storage.AccountRateLimit{aggregate}, individual...)
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
			s.scheduler.RefreshAccountCache()
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
	usage, err := parseKiroUsageLimits(limits)
	if err != nil {
		return newQuotaPollError("partial", 0, nil, err)
	}
	raw, _ := json.Marshal(limits)
	return s.upsertLiveQuotaSnapshot(ctx, storage.AccountRateLimit{
		AccountID: acc.ID, Provider: "kiro", LimiterType: "kiro_usage", Source: "kiro_usage",
		UsedPercent: usage.UsedPercent, LimitTokens: int64(usage.Limit),
		RemainingTokens: int64(usage.Remaining), LimitRequests: -1, RemainingRequests: -1,
		ResetAt: usage.ResetAt, Status: usage.Status, Raw: string(raw), UpdatedAt: storage.Now(),
	})
}

func quotaPollCandidateAccountIDs(accounts []storage.Account, now int64) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if !isQuotaPollCandidate(account, now) && !isKiroQuotaPollCandidate(account, now) && !isCursorQuotaPollCandidate(account, now) {
			continue
		}
		ids = append(ids, account.ID)
	}
	return ids
}

func quotaPollUnattemptedAccountIDs(candidateIDs []string, targetSets ...[]quotaPollTarget) []string {
	attempted := make(map[string]struct{}, len(candidateIDs))
	for _, targets := range targetSets {
		for _, target := range targets {
			if accountID := strings.TrimSpace(target.Account.ID); accountID != "" {
				attempted[accountID] = struct{}{}
			}
		}
	}
	unattempted := make([]string, 0)
	for _, accountID := range candidateIDs {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			continue
		}
		if _, ok := attempted[accountID]; !ok {
			unattempted = append(unattempted, accountID)
		}
	}
	return uniqueSortedStrings(unattempted)
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
		if accountprovider.UsesAPIKey("codex", token) {
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
		log.Printf("[QUOTA-POLL] ListEgressBindingsByAccountIDs error: %v; skipping egress-dependent polls", err)
		for i := range targets {
			targets[i].Egress = storage.EgressProfile{Type: "unavailable_egress_binding"}
		}
		return
	}
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		log.Printf("[QUOTA-POLL] ListEgressProfiles error: %v; skipping egress-dependent polls", err)
		for i := range targets {
			targets[i].Egress = storage.EgressProfile{Type: "unavailable_egress_binding"}
		}
		return
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
	binding, hasBinding := bindings[accountID]
	if hasBinding && strings.TrimSpace(binding.PrimaryEgressID) != "" {
		egressID = binding.PrimaryEgressID
	}
	if profile, ok := profiles[egressID]; ok {
		if hasBinding && strings.TrimSpace(binding.SidecarEgressID) != "" {
			sidecar, sidecarOK := profiles[binding.SidecarEgressID]
			if !sidecarOK || !scheduler.EgressHealthy(sidecar, storage.Now()) {
				return storage.EgressProfile{ID: egressID, Type: "invalid_sidecar_binding"}
			}
			wrapped, err := storage.WrapEgressWithSidecar(profile, sidecar)
			if err != nil {
				return storage.EgressProfile{ID: egressID, Type: "invalid_sidecar_binding"}
			}
			return wrapped
		}
		return profile
	}
	if hasBinding && strings.TrimSpace(binding.SidecarEgressID) != "" {
		return storage.EgressProfile{ID: egressID, Type: "invalid_sidecar_binding"}
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

func isKiroQuotaPollCandidate(account storage.Account, now int64) bool {
	return account.Status == "active" &&
		account.QuarantineUntil <= now &&
		strings.EqualFold(strings.TrimSpace(account.Provider), "kiro")
}

func isCursorQuotaPollCandidate(account storage.Account, now int64) bool {
	return account.Status == "active" && account.QuarantineUntil <= now &&
		strings.EqualFold(strings.TrimSpace(account.Provider), cursorproxy.ProviderID)
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
	if lastSuccess, lastErr := s.store.LastSuccessfulQuotaAt(ctx, acc.ID); lastErr == nil && lastSuccess > 0 {
		detail["last_success_at"] = lastSuccess
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
	authorization := ""
	if isAgentIdentityToken(token) {
		var err error
		token, err = s.ensureAgentIdentityTask(ctx, acc, token, egress, "quota:"+acc.ID, "")
		if err != nil {
			return newQuotaPollError("agent_task_error", 0, nil, err)
		}
		authorization, err = agentIdentityAuthorization(token)
		if err != nil {
			return newQuotaPollError("agent_assertion_error", 0, nil, err)
		}
	} else {
		accessToken := accountprovider.Credential("codex", token)
		if accessToken == "" {
			return newQuotaPollError("token_missing", 0, nil, fmt.Errorf("no access token"))
		}
		authorization = "Bearer " + accessToken
	}
	chatgptUserID := acc.ChatGPTUserID
	if chatgptUserID == "" {
		chatgptUserID = acc.UpstreamAccountID
	}

	headers := http.Header{}
	headers.Set("Authorization", authorization)
	if chatgptUserID != "" {
		headers.Set("ChatGPT-Account-Id", chatgptUserID)
	}
	if acc.IsFedramp {
		headers.Set("X-OpenAI-Fedramp", "true")
	}
	headers.Set("Accept", "application/json")
	headers.Set("Origin", "https://chatgpt.com")
	headers.Set("Referer", "https://chatgpt.com/")
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")

	// DoRaw is provider-neutral and, unlike EgressHTTPClient, can execute this GET
	// through curl_cffi's /proxy protocol. This keeps background quota polling on
	// the same TLS/HTTP2 fingerprint-safe path as inference.
	resp, err := s.upstream.DoRaw(ctx, egress, http.MethodGet, whamUsageURL, headers, nil, "quota:"+acc.ID)
	if err != nil {
		return newQuotaPollError("network_error", 0, nil, fmt.Errorf("http: %w", err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body = redactAgentIdentityError(token, body)
	if isInvalidAgentIdentityTask(resp.StatusCode, body, token) {
		recovered, recoverErr := s.ensureAgentIdentityTask(ctx, acc, token, egress, "quota:"+acc.ID, token.AgentTaskID)
		if recoverErr != nil {
			return newQuotaPollError("agent_task_error", resp.StatusCode, body, recoverErr)
		}
		token = recovered
		authorization, recoverErr = agentIdentityAuthorization(token)
		if recoverErr != nil {
			return newQuotaPollError("agent_assertion_error", 0, nil, recoverErr)
		}
		headers.Set("Authorization", authorization)
		resp.Body.Close()
		resp, err = s.upstream.DoRaw(ctx, egress, http.MethodGet, whamUsageURL, headers, nil, "quota:"+acc.ID)
		if err != nil {
			return newQuotaPollError("network_error", 0, nil, fmt.Errorf("http after task recovery: %w", err))
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		body = redactAgentIdentityError(token, body)
	}
	if resp.StatusCode != http.StatusOK {
		return newQuotaPollError("http_error", resp.StatusCode, body, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var wham whamUsageResponse
	if err := json.Unmarshal(body, &wham); err != nil {
		return newQuotaPollError("decode_error", 0, nil, fmt.Errorf("decode: %w", err))
	}
	var whamEvidence map[string]interface{}
	if json.Unmarshal(body, &whamEvidence) == nil {
		s.captureEntitlementEvidence(ctx, acc.ID, "quota_metadata", whamEvidence)
	}

	now := storage.Now()

	// The wham response is the authoritative current plan (registration-time
	// detection drifts); persist it so the USD estimate keys off the live plan.
	if plan := strings.TrimSpace(wham.PlanType); plan != "" {
		_ = s.store.SetAccountPlanType(ctx, acc.ID, plan)
	}

	var rawDetail []byte
	if wham.RateLimit.PrimaryWindow.LimitWindowSeconds > 0 || wham.RateLimit.SecondaryWindow.LimitWindowSeconds > 0 {
		detail := map[string]interface{}{"plan_type": wham.PlanType}
		if wham.RateLimit.PrimaryWindow.LimitWindowSeconds > 0 {
			detail["primary"] = wham.RateLimit.PrimaryWindow
		}
		if wham.RateLimit.SecondaryWindow.LimitWindowSeconds > 0 {
			detail["secondary"] = wham.RateLimit.SecondaryWindow
		}
		raw, _ := json.Marshal(detail)
		rawDetail = raw
	}

	pw := wham.RateLimit.PrimaryWindow
	sw := wham.RateLimit.SecondaryWindow
	// The 7d window and the credits/spend-control blocks are siblings of the
	// primary window in the same response; persist whatever is present even when
	// the primary window is missing, so a malformed primary never discards the
	// rest of the payload. The partial error marker still flags the hole.
	if sw.LimitWindowSeconds > 0 {
		snap := storage.AccountRateLimit{
			AccountID:         acc.ID,
			Provider:          "codex",
			LimiterType:       "7d_polled",
			Source:            "7d_polled",
			UsedPercent:       sw.UsedPercent,
			RemainingTokens:   -1,
			LimitTokens:       -1,
			LimitRequests:     -1,
			RemainingRequests: -1,
			ResetAt:           codexWhamResetAt(now, sw.ResetAfterSeconds),
			Status:            statusFromCodexWhamWindow(sw),
			Raw:               string(rawDetail),
			UpdatedAt:         now,
		}
		_ = s.upsertLiveQuotaSnapshot(ctx, snap)
		// Empirical window estimation: snapshot used_percent together with the
		// recorded cost of the current 7d cycle.
		s.recordQuotaWindowSample(ctx, acc.ID, quotaWindowKind7d,
			quotaWindowMinutesForCodex(sw.LimitWindowSeconds), now+sw.ResetAfterSeconds, now, sw.UsedPercent)
	}
	if credits := parseCodexResetCredits(body, "usage_fallback"); credits.Known {
		credits.UpdatedAt = now
		s.upsertCodexResetCreditsSnapshot(ctx, acc.ID, credits)
	}
	s.upsertCodexCreditsSnapshot(ctx, acc.ID, wham, now)
	if pw.LimitWindowSeconds <= 0 {
		return newQuotaPollError("partial", 0, rawDetail, fmt.Errorf("no primary window in wham response"))
	}

	// Premium/5x Business responses have no five-hour window: their sole
	// primary window is the seven-day window. The old path unconditionally
	// labeled every primary as 5h, which created a fake 5h card, polluted the
	// empirical capacity baseline, and made Premium detection contradict itself.
	// Duration chooses the storage bucket; entitlement cleanup additionally
	// requires the reviewed 5x subtype so an ordinary Team missing a window due
	// to an upstream bug is never promoted to Premium.
	if codexWhamPrimaryWindowKind(pw.LimitWindowSeconds, sw.LimitWindowSeconds) == quotaWindowKind7d {
		snap := storage.AccountRateLimit{
			AccountID: acc.ID, Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled",
			UsedPercent: pw.UsedPercent, RemainingTokens: -1, LimitTokens: -1,
			LimitRequests: -1, RemainingRequests: -1, ResetAt: codexWhamResetAt(now, pw.ResetAfterSeconds),
			Status: statusFromReached(wham.RateLimit.LimitReached), Raw: string(rawDetail), UpdatedAt: now,
		}
		_ = s.upsertLiveQuotaSnapshot(ctx, snap)
		s.recordQuotaWindowSample(ctx, acc.ID, quotaWindowKind7d,
			quotaWindowMinutesForCodex(pw.LimitWindowSeconds), now+pw.ResetAfterSeconds, now, pw.UsedPercent)
		if _, reviewedPremium := entitlement.ReviewedBusinessPremiumQuotaMapping(whamEvidence, "quota_metadata"); reviewedPremium {
			if err := s.store.DeleteMisclassifiedLongFiveHourQuotaState(ctx, acc.ID); err == nil && s.scheduler != nil {
				s.scheduler.RefreshAccountCache()
			}
		}
		return nil
	}

	snap := storage.AccountRateLimit{
		AccountID:       acc.ID,
		Provider:        "codex",
		LimiterType:     "5h_polled",
		Source:          "5h_polled",
		UsedPercent:     pw.UsedPercent,
		RemainingTokens: -1,
		LimitTokens:     -1,
		ResetAt:         codexWhamResetAt(now, pw.ResetAfterSeconds),
		Status:          statusFromReached(wham.RateLimit.LimitReached),
		Raw:             string(rawDetail),
		UpdatedAt:       now,
	}
	_ = s.upsertLiveQuotaSnapshot(ctx, snap)
	// Empirical window estimation: snapshot used_percent together with the
	// recorded cost of the current 5h cycle.
	s.recordQuotaWindowSample(ctx, acc.ID, quotaWindowKind5h,
		quotaWindowMinutesForCodex(pw.LimitWindowSeconds), now+pw.ResetAfterSeconds, now, pw.UsedPercent)
	return nil
}

// upsertCodexCreditsSnapshot persists the account's extra paid balance (the
// `credits` / `spend_control` blocks of /wham/usage) as its own limiter row so the
// admin quota surface can show it next to the 5h/7d windows. Accounts on plans that
// do not expose credits simply never get a row, which is what lets the UI hide the
// panel instead of rendering a permanently empty card.
func (s *Server) upsertCodexCreditsSnapshot(ctx context.Context, accountID string, wham whamUsageResponse, now int64) {
	if wham.Credits == nil && wham.SpendControl == nil {
		return
	}
	detail := map[string]interface{}{}
	usedPercent := float64(-1)
	resetAt := int64(0)
	status := "ok"

	if wham.Credits != nil {
		detail["has_credits"] = wham.Credits.HasCredits
		detail["unlimited"] = wham.Credits.Unlimited
		if wham.Credits.Balance != nil {
			detail["balance"] = strings.TrimSpace(*wham.Credits.Balance)
		}
		if wham.Credits.RemainingMilliCredits != nil {
			detail["credits_remaining_milli"] = *wham.Credits.RemainingMilliCredits
		}
		if wham.Credits.Unlimited {
			status = "unlimited"
		} else if !wham.Credits.HasCredits {
			status = "depleted"
		}
	}
	if wham.SpendControl != nil {
		// Distinguish a spend-control-only row (no extra credits block) from a
		// genuinely depleted balance: the console must not render the former as
		// "已耗尽".
		detail["spend_control"] = true
		detail["spend_control_reached"] = wham.SpendControl.Reached
		if wham.SpendControl.Reached {
			status = "spend_limit_reached"
		}
		if limit := wham.SpendControl.IndividualLimit; limit != nil {
			detail["source"] = strings.TrimSpace(limit.Source)
			detail["limit"] = strings.TrimSpace(limit.Limit)
			detail["used"] = strings.TrimSpace(limit.Used)
			detail["remaining"] = strings.TrimSpace(limit.Remaining)
			detail["used_percent"] = limit.UsedPercent
			detail["remaining_percent"] = limit.RemainingPercent
			usedPercent = limit.UsedPercent
			if limit.ResetAt > 0 {
				resetAt = limit.ResetAt
			} else if limit.ResetAfterSeconds > 0 {
				resetAt = now + limit.ResetAfterSeconds
			}
		}
	}

	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = s.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         accountID,
		Provider:          "codex",
		LimiterType:       codexCreditsLimiterType,
		Source:            "wham_usage_credits",
		UsedPercent:       usedPercent,
		LimitTokens:       -1,
		RemainingTokens:   -1,
		LimitRequests:     -1,
		RemainingRequests: -1,
		ResetAt:           resetAt,
		Status:            status,
		Raw:               string(raw),
		UpdatedAt:         now,
	})
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
		if err := s.upsertLiveQuotaSnapshot(ctx, snap); err != nil {
			return err
		}
		// Empirical window estimation: snapshot used_percent together with the
		// recorded cost of the current oauth usage cycle.
		if kind := quotaWindowKindForClaudeLimiter(win.LimiterType); kind != "" {
			s.recordQuotaWindowSample(ctx, acc.ID, kind,
				quotaWindowMinutesForClaude(win.LimiterType), win.ResetAt, storage.Now(), win.UsedPercent)
		}
	}
	return nil
}

func quotaWindowKindForClaudeLimiter(limiterType string) string {
	switch {
	case strings.HasPrefix(limiterType, "7d"):
		return quotaWindowKind7d
	case strings.HasPrefix(limiterType, "5h"):
		return quotaWindowKind5h
	}
	return ""
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
	// The live Claude usage endpoint reports used-percent as "utilization" and reset as an
	// RFC3339 "resets_at" string; older shapes used "used_percent"/"reset_after_seconds".
	// Accept both so quota is correct against the current API (matches the reference
	// implementations' five_hour/seven_day {utilization, resets_at} shape).
	used, ok := jsonFloatAny(m, "used_percent", "usedPercent", "usage_percent", "utilization")
	limitWindow := jsonIntAny(m, "limit_window_seconds", "limitWindowSeconds", "window_seconds", "windowSeconds")
	resetAfter := jsonIntAny(m, "reset_after_seconds", "resetAfterSeconds")
	// resets_at is an RFC3339 string in the current API; parse it as such FIRST so a
	// timestamp is never misread as a bare integer (jsonIntAny would grab the leading
	// "2030" of "2030-01-01T..."). Fall back to an integer epoch for older shapes.
	resetAt := int64(0)
	for _, k := range []string{"resets_at", "resetsAt", "reset_at", "resetAt"} {
		if ts := strings.TrimSpace(jsonStringAny(m, k)); ts != "" {
			if off := parseResetTimestamp(ts, now); off > 0 {
				resetAt = now + off
				break
			}
		}
	}
	if resetAt == 0 {
		if n := jsonIntAny(m, "reset_at", "resetAt", "resets_at", "resetsAt"); n > 946684800 {
			resetAt = n // a real absolute epoch (> year 2000), not a stringified timestamp's prefix
		}
	}
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
	case strings.Contains(l, "opus") || strings.Contains(l, "sonnet") || strings.Contains(l, "haiku") || strings.Contains(l, "cowork"):
		// Per-model / cowork windows are not the account's primary/secondary gauge.
		return "", ""
	case strings.Contains(l, "5h") || strings.Contains(l, "five") || (windowSeconds > 0 && windowSeconds <= 6*3600):
		return "5h_oauth_usage", ""
	case strings.Contains(l, "7d") || strings.Contains(l, "seven") || strings.Contains(l, "week") || windowSeconds >= 6*24*3600:
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

func codexWhamResetAt(now, resetAfterSeconds int64) int64 {
	if resetAfterSeconds <= 0 || now > math.MaxInt64-resetAfterSeconds {
		return 0
	}
	return now + resetAfterSeconds
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
