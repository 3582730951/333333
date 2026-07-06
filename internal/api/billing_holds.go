package api

import (
	"context"
	"log"
	"time"

	"codex-account-pool/internal/supervisor"
)

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
