package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

// Per-account consecutive-upstream-failure breaker.
//
// benchOnLimit only ever benched an account on a rate/quota signal or a literal 429
// (usageLimitCooldown returns 0 for everything else), so an upstream that failed
// *every* request was never removed from rotation. Production diagnostics record the
// resulting behavior precisely: one custom-provider account returned 1512 upstream
// 503s over 14 unbroken hours while remaining a first-choice candidate, and 1457 of
// those requests each additionally consumed a fallback attempt against a second
// account group. Nothing in the pool noticed, because no single response looked like
// a limit.
//
// The breaker closes that gap with the cheapest possible signal: count consecutive
// 5xx responses per account, and once the streak crosses the threshold, bench the
// account for recheck. Bench-for-recheck (not a plain cooldown) is deliberate — a
// dead relay must prove it recovered via the liveness probe rather than silently
// resurfacing the moment a timer elapses.
//
// Any non-5xx response resets the streak, so this cannot accumulate across an
// account's normal lifetime: a 4xx, a success, or a rate-limit response all clear it.
// Rate/quota handling is untouched and still owns 429 and quota-signal benching.

const (
	// failureStreakEntryTTL drops idle streak state. A streak that has not advanced in
	// this long is not a live outage, and keeping it would let two unrelated failures
	// hours apart accumulate toward a bench.
	failureStreakEntryTTL = 10 * time.Minute
	// failureStreakWindow bounds how long one streak may take to reach the threshold.
	// Without it, a slow trickle of failures spaced just under the idle TTL would still
	// accumulate to a bench over many hours. A dead upstream reaches the threshold in
	// seconds, so a short window loses nothing real.
	failureStreakWindow = 15 * time.Minute
	// failureStreakMaxEntries bounds the table. Accounts are a small, operator-managed
	// set, so this is a safety valve against unbounded growth, not a working limit.
	failureStreakMaxEntries = 4096
)

type failureStreakEntry struct {
	count     int
	firstAt   time.Time
	updatedAt time.Time
}

type failureStreakTable struct {
	mu      sync.Mutex
	entries map[string]failureStreakEntry
}

// observe records one upstream outcome for accountID and returns the resulting
// consecutive-failure count. failed=false clears the streak and returns 0.
func (t *failureStreakTable) observe(accountID string, failed bool, now time.Time) int {
	if accountID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]failureStreakEntry)
	}
	if !failed {
		delete(t.entries, accountID)
		return 0
	}
	entry := t.entries[accountID]
	idle := entry.count > 0 && now.Sub(entry.updatedAt) > failureStreakEntryTTL
	stale := entry.count > 0 && now.Sub(entry.firstAt) > failureStreakWindow
	if idle || stale {
		entry.count = 0
	}
	if entry.count == 0 {
		entry.firstAt = now
	}
	entry.count++
	entry.updatedAt = now
	t.entries[accountID] = entry
	t.sweepLocked(now)
	return entry.count
}

// reset clears the streak for one account.
func (t *failureStreakTable) reset(accountID string) {
	if accountID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, accountID)
}

// sweepLocked evicts expired entries, and — only if the table is still over its
// bound — the least recently updated ones.
func (t *failureStreakTable) sweepLocked(now time.Time) {
	if len(t.entries) <= failureStreakMaxEntries {
		if len(t.entries) < failureStreakMaxEntries/2 {
			return
		}
		for id, entry := range t.entries {
			if now.Sub(entry.updatedAt) > failureStreakEntryTTL {
				delete(t.entries, id)
			}
		}
		return
	}
	for id, entry := range t.entries {
		if now.Sub(entry.updatedAt) > failureStreakEntryTTL {
			delete(t.entries, id)
		}
	}
	for len(t.entries) > failureStreakMaxEntries {
		oldestID := ""
		var oldestAt time.Time
		for id, entry := range t.entries {
			if oldestID == "" || entry.updatedAt.Before(oldestAt) {
				oldestID, oldestAt = id, entry.updatedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(t.entries, oldestID)
	}
}

// failureStreakBreakerSettings resolves the runtime-overridable breaker knobs.
// threshold 0 means the breaker is off.
func (s *Server) failureStreakBreakerSettings(ctx context.Context) (threshold int, cooldown int64) {
	threshold = s.settingInt(ctx, "account_failure_streak_threshold", s.cfg.AccountFailureStreakThreshold)
	if threshold < 0 {
		threshold = 0
	}
	cooldown = int64(s.settingInt(ctx, "account_failure_streak_cooldown_seconds", s.cfg.AccountFailureStreakCooldownSeconds))
	if cooldown <= 0 {
		cooldown = 300
	}
	return threshold, cooldown
}

// noteUpstreamSuccess clears an account's failure streak. Called from the success
// path so a single good response ends the streak immediately.
func (s *Server) noteUpstreamSuccess(accountID string) {
	s.failureStreaks.reset(accountID)
}

// benchOnFailureStreak counts one upstream 5xx for the account and benches it for
// recheck once the consecutive-failure threshold is crossed. Non-5xx statuses reset
// the streak. It returns true when this call benched the account.
//
// This runs after the ban classifier and the rate/quota path, so a status that
// already has a specific remediation keeps it; the breaker only catches the plain
// server-error case those paths deliberately ignore.
func (s *Server) benchOnFailureStreak(ctx context.Context, account storage.Account, status int) bool {
	// An explicit per-account operator override opts out of automatic benching.
	if account.IgnoreRateLimitControls {
		return false
	}
	threshold, cooldown := s.failureStreakBreakerSettings(ctx)
	if threshold <= 0 {
		return false
	}
	if status < http.StatusInternalServerError {
		s.failureStreaks.reset(account.ID)
		return false
	}
	streak := s.failureStreaks.observe(account.ID, true, time.Now())
	if streak < threshold {
		return false
	}
	// The upstream verdict is authoritative even if the downstream request is being
	// cancelled, so persist the bench on a context that outlives it.
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.BenchBindingForRecheck(stateCtx, account.ID, storage.Now()+cooldown); err != nil {
		log.Printf("[FAILURE-STREAK] bench persistence failed: account=%s streak=%d: %v", account.ID, streak, err)
		return false
	}
	log.Printf("[FAILURE-STREAK] BENCH: account=%s consecutive_5xx=%d threshold=%d cooldown=%ds last_status=%d",
		account.ID, streak, threshold, cooldown, status)
	// Record the bench as a diagnostic event so an exported package explains *why* an
	// account left the pool. The 14-hour outage above produced 1709 identical
	// `http_error` rows and no record of any pool-side decision at all.
	detail, _ := json.Marshal(map[string]interface{}{
		"error_class":            "consecutive_upstream_5xx",
		"component":              "scheduler",
		"operation":              "bench_on_failure_streak",
		"provider":               account.Provider,
		"group":                  account.GroupName,
		"consecutive_failures":   streak,
		"threshold":              threshold,
		"cooldown_seconds":       cooldown,
		"last_status":            status,
		"recheck_probe_required": true,
	})
	eventCtx, eventCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer eventCancel()
	_ = s.store.AddDiagnosticEvent(eventCtx, storage.DiagnosticEvent{
		ID:          newRequestID(),
		EventType:   "account_failure_streak_bench",
		Severity:    "warning",
		EntityType:  "account",
		EntityAlias: account.ID,
		DetailJSON:  string(detail),
	})
	// The streak has been acted on; start a fresh count so a still-dead upstream
	// re-benches after the next full threshold rather than on its very next response.
	s.failureStreaks.reset(account.ID)
	if s.scheduler != nil {
		s.scheduler.NotifyStateChanged()
	}
	return true
}
