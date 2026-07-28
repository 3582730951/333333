package api

import (
	"context"
	"log"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"

	"github.com/google/uuid"
)

func (s *Server) createBillingHold(ctx context.Context, routeKeyHash, accountID string, routeEpoch, estimatedTokens int64) string {
	now := storage.Now()
	id := "hold_" + uuid.NewString()
	eventID := usageEventIDFromContext(ctx)
	if eventID == "" {
		eventID = requestIDFromContext(ctx)
	}
	if eventID == "" {
		eventID = "usage_" + id
	}
	s.enqueueBillingHold(storage.BillingHoldWrite{ID: id, EventID: eventID, RouteKeyHash: routeKeyHash, AccountID: accountID, EstimatedTokens: estimatedTokens, RouteEpoch: routeEpoch, CreatedAt: now, Create: true})
	s.billingEstimates.Store(id, estimatedTokens)
	return id
}

func (s *Server) settleBillingHold(_ context.Context, id, status string) error {
	if id != "" {
		s.enqueueBillingHold(storage.BillingHoldWrite{ID: id, Status: status, CreatedAt: storage.Now()})
		s.billingEstimates.Delete(id)
	}
	return nil
}

func (s *Server) billingHoldEstimate(id string) int64 {
	if value, ok := s.billingEstimates.Load(id); ok {
		return value.(int64)
	}
	return 0
}

func (s *Server) settleBillingHoldIfHeld(_ context.Context, id, status string) error {
	if id != "" {
		s.enqueueBillingHold(storage.BillingHoldWrite{ID: id, Status: status, CreatedAt: storage.Now(), IfHeld: true})
		s.billingEstimates.Delete(id)
	}
	return nil
}

func (s *Server) startBillingHoldExpiryLoop(ctx context.Context) {
	expire := func() {
		n, err := s.store.ExpireStaleBillingHolds(ctx, s.billingHoldExpiryAge())
		if err != nil {
			log.Printf("[BILLING-HOLDS] expire stale holds: %v", err)
			return
		}
		if n > 0 {
			log.Printf("[BILLING-HOLDS] expired_unsettled=%d", n)
		}
	}
	expire()
	supervisor.Go(ctx, "billing-hold-expiry", func(ctx context.Context) {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				expire()
			}
		}
	})
}

func (s *Server) billingHoldExpiryAge() time.Duration {
	age := s.cfg.RequestTimeout() * 2
	if age < time.Hour {
		return time.Hour
	}
	return age
}
