package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codex-account-pool/internal/storage"
)

// Sample-side wiring of the calculate_money quota estimation scheme. Every quota
// poll that sees a live used_percent window records one sample: the upstream
// used_percent plus the relay's recorded USD cost accumulated inside that window
// cycle. Expired cycles are finalized (best prefix estimate persisted, raw samples
// pruned) exactly like the reference implementation, which keeps only the
// best-quality summary per cycle.
const (
	quotaWindowKind5h = "5h"
	quotaWindowKind7d = "7d"

	// Bucketing absorbs the poll-to-poll drift of computed reset_at (the wham
	// response reports reset_after_seconds from the poll moment) so one cycle
	// keeps a single cycle_start key. The quota poll floor is 120s; 300s is safe.
	quotaWindowBucketSeconds = 300

	// A cycle is finalized once its reset has been passed by this grace period.
	quotaWindowFinalizeGraceSeconds = int64(10 * 60)
)

func quotaWindowBucket(ts int64) int64 {
	return (ts / quotaWindowBucketSeconds) * quotaWindowBucketSeconds
}

// quotaWindowMinutesForCodex derives the window duration from the wham window
// fields. Zero means "no window of this kind" (absent), matching the reference
// implementation's zero-duration handling.
func quotaWindowMinutesForCodex(windowSeconds int64) int64 {
	if windowSeconds <= 0 {
		return 0
	}
	return windowSeconds / 60
}

// quotaWindowMinutesForClaude derives the window duration from the oauth usage
// limiter type (5h_oauth_usage / 7d_oauth_usage).
func quotaWindowMinutesForClaude(limiterType string) int64 {
	switch {
	case strings.HasPrefix(limiterType, "7d"):
		return 7 * 24 * 60
	case strings.HasPrefix(limiterType, "5h"):
		return 5 * 60
	}
	return 0
}

// recordQuotaWindowSample snapshots one poll into the current cycle of the
// account's window kind and finalizes any cycle that just expired. Windows with
// no percentage signal (used_percent <= 0) or zero duration are skipped, matching
// the reference collector's zero_percent / zero_duration branches.
func (s *Server) recordQuotaWindowSample(ctx context.Context, accountID, windowKind string, windowMinutes, resetAt, now int64, usedPercent float64) {
	if s == nil || s.store == nil || accountID == "" {
		return
	}
	if windowMinutes <= 0 || usedPercent <= 0 || resetAt <= 0 {
		return
	}
	windowSeconds := windowMinutes * 60
	cycleStart := quotaWindowBucket(resetAt - windowSeconds)
	if cycleStart <= 0 || cycleStart+windowSeconds <= now {
		// The window already ended; its samples were finalized by the poll that
		// saw it expire. A late duplicate of an ended cycle adds nothing.
		return
	}
	if err := s.store.UpsertQuotaWindowCycle(ctx, storage.QuotaWindowCycle{
		AccountID: accountID, WindowKind: windowKind, CycleStart: cycleStart, WindowMinutes: windowMinutes,
	}); err != nil {
		return
	}
	cost, unsettledShare := s.sumQuotaWindowCost(ctx, accountID, cycleStart, now)
	if err := s.store.UpsertQuotaWindowSample(ctx, storage.QuotaWindowSample{
		AccountID: accountID, WindowKind: windowKind, CycleStart: cycleStart, SampleAt: now,
		UsedPercent: usedPercent, CostUSD: cost, UnsettledShare: unsettledShare,
	}); err != nil {
		return
	}
	s.finalizeExpiredQuotaWindows(ctx, accountID, windowKind, cycleStart, now)
}

// finalizeExpiredQuotaWindows runs the best-prefix estimator over every fully
// expired cycle of the account's window kind, persists the summary, and prunes the
// raw samples — the reference implementation's "raw sample data is deleted,
// keeping only the best-quality summary result".
func (s *Server) finalizeExpiredQuotaWindows(ctx context.Context, accountID, windowKind string, currentCycleStart, now int64) {
	if s == nil || s.store == nil {
		return
	}
	cycles, err := s.store.ExpiredQuotaWindowCycles(ctx, accountID, windowKind, currentCycleStart)
	if err != nil {
		return
	}
	for _, cycle := range cycles {
		windowMinutes := cycle.WindowMinutes
		if windowMinutes <= 0 {
			windowMinutes = quotaWindowDefaultMinutes(windowKind)
		}
		if cycle.CycleStart+windowMinutes*60+quotaWindowFinalizeGraceSeconds > now {
			// Still inside the grace window after its reset; keep sampling.
			continue
		}
		samples, err := s.store.QuotaWindowSamples(ctx, accountID, windowKind, cycle.CycleStart)
		if err != nil || len(samples) == 0 {
			continue
		}
		lastSampleAt := samples[len(samples)-1].SampleAt
		estimate := quotaWindowSelectBest(quotaWindowSamplesFromStorage(samples), lastSampleAt, int64(^uint64(0)>>1))
		if estimate.State != "estimated" {
			// Nothing worth keeping: prune the samples regardless so an
			// empty-looking cycle never re-enters this path.
			_ = s.store.DeleteQuotaWindowSamples(ctx, accountID, windowKind, cycle.CycleStart)
			continue
		}
		raw, err := json.Marshal(estimate)
		if err != nil {
			continue
		}
		if err := s.store.FinalizeQuotaWindowEstimate(ctx, storage.QuotaWindowEstimateSummary{
			AccountID: accountID, WindowKind: windowKind, CycleStart: cycle.CycleStart,
			EstimateJSON: string(raw), QualityScore: estimate.QualityScore, Confidence: estimate.Confidence,
			FinalizedAt: now,
		}); err != nil {
			continue
		}
		_ = s.store.DeleteQuotaWindowSamples(ctx, accountID, windowKind, cycle.CycleStart)
	}
}

func quotaWindowSamplesFromStorage(rows []storage.QuotaWindowSample) []quotaWindowSample {
	out := make([]quotaWindowSample, 0, len(rows))
	for _, row := range rows {
		out = append(out, quotaWindowSample{
			AccountID: row.AccountID, WindowKind: row.WindowKind, CycleStart: row.CycleStart,
			SampleAt: row.SampleAt, UsedPercent: row.UsedPercent, CostUSD: row.CostUSD,
			UnsettledShare: row.UnsettledShare,
		})
	}
	return out
}

func quotaWindowDefaultMinutes(windowKind string) int64 {
	if windowKind == quotaWindowKind7d {
		return 7 * 24 * 60
	}
	return 5 * 60
}

func quotaWindowStaleAfter(windowKind string) int64 {
	if windowKind == quotaWindowKind7d {
		return quotaWindowStaleAfterSeconds7d
	}
	return quotaWindowStaleAfterSeconds5h
}

// quotaWindowEstimateFor computes the live estimate of the account's current
// cycle. It returns nil when the account has no samples or the evidence is still
// too thin (state "waiting").
func (s *Server) quotaWindowEstimateFor(ctx context.Context, accountID, windowKind string, now int64) *quotaWindowEstimate {
	if s == nil || s.store == nil || accountID == "" {
		return nil
	}
	cycle, ok, err := s.store.QuotaWindowCurrentCycle(ctx, accountID, windowKind)
	if err != nil || !ok {
		return nil
	}
	samples, err := s.store.QuotaWindowSamples(ctx, accountID, windowKind, cycle.CycleStart)
	if err != nil || len(samples) == 0 {
		return nil
	}
	estimate := quotaWindowSelectBest(quotaWindowSamplesFromStorage(samples), now, quotaWindowStaleAfter(windowKind))
	if estimate.State != "estimated" {
		return nil
	}
	return &estimate
}

// attachQuotaWindowEstimate replaces the window_based placeholder in the quota
// summary with the empirical dollar estimate once the current cycle has enough
// evidence. The upstream-reported credit balance, when present, is preserved as
// the extra-budget figure.
func (s *Server) attachQuotaWindowEstimate(ctx context.Context, account storage.Account, summary *QuotaSummary, now int64) {
	if s == nil || summary == nil || summary.Estimate == nil {
		return
	}
	kind := quotaWindowKindForProvider(summary.Provider)
	estimate := s.quotaWindowEstimateFor(ctx, account.ID, kind, now)
	if estimate == nil {
		return
	}
	existing := summary.Estimate
	existing.Estimated = true
	existing.Method = "window_cost_estimate"
	existing.Confidence = estimate.Confidence
	existing.LimitUSD = round2(estimate.Cost.Center)
	existing.LimitUSDMin = round2(estimate.Cost.Lower)
	existing.LimitUSDMax = round2(estimate.Cost.Upper)
	existing.UsedUSD = round2(estimate.UsedCost.Center)
	existing.RemainingUSD = round2(estimate.RemainingCost.Center)
	existing.UsedPercent = estimate.UsedPercent
	if estimate.Evidence.CandidateCount > 0 {
		existing.Note = fmt.Sprintf("empirical window estimate: %d candidates over %d samples, %s confidence (%s)", estimate.Evidence.DeltaCandidateCount, estimate.Evidence.SampleCount, estimate.Confidence, estimate.Method)
	}
}

func quotaWindowKindForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "chatgpt", "openai", "claude":
		return quotaWindowKind5h
	}
	return ""
}
