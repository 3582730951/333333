package api

import "context"

// sumQuotaWindowCost returns the versioned API-list-price equivalent that is
// paired with an upstream used_percent sample. It is not an upstream-reported
// dollar balance and is never used to fabricate one for a subscription window.
//
// The amount comes only from usage_components + usage_valuations. Those rows
// persist the requested, forwarded, observed, and billed tiers, and the
// versioned valuation has already applied the billed tier before this estimator
// receives it. In particular, do not reapply a Fast multiplier here.
//
// Legacy usage_records predate that audit trail and have neither a catalog ID
// nor a durable billed tier. They are deliberately excluded instead of being
// priced at a guessed default tier: a zero-valued sample makes the empirical
// estimator wait for auditable traffic rather than understate Fast consumption.
func (s *Server) sumQuotaWindowCost(ctx context.Context, accountID string, windowStart, windowEnd int64) (float64, float64) {
	if s == nil || s.store == nil || windowEnd <= windowStart {
		return 0, 0
	}
	fixed, err := s.store.AccountUsageValuationWindowSummary(ctx, accountID, windowStart, windowEnd)
	if err != nil || fixed.TotalEvents == 0 {
		return 0, 0
	}
	total := float64(fixed.TotalMicroUSD) / 1_000_000
	unsettledShare := 0.0
	if fixed.TotalMicroUSD > 0 {
		unsettledShare = float64(fixed.ProvisionalMicroUSD) / float64(fixed.TotalMicroUSD)
	}
	// Missing rates/components have no amount that can be included in the
	// numerator. Their event share is therefore a lower-bound quality penalty,
	// never a fabricated zero-dollar contribution.
	unavailableShare := float64(fixed.UnavailableEvents) / float64(fixed.TotalEvents)
	if unavailableShare > unsettledShare {
		unsettledShare = unavailableShare
	}
	return total, unsettledShare
}
