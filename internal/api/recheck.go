package api

import (
	"context"
	"log"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// startRecheckLoop launches the cooldown→health-recheck background sweep. An account
// benched after an upstream error (BenchBindingForRecheck) is held out of the
// candidate pool until this loop probes it — after its cooldown elapses — and
// confirms it is healthy again ("通过测活没问题后才可以继续放入候选账号池"). Returns
// immediately; the loop runs until ctx is cancelled. No-op when disabled in config.
func (s *Server) startRecheckLoop(ctx context.Context) {
	if !s.cfg.AccountRecheckEnabled {
		log.Printf("[RECHECK] disabled (account_recheck_enabled=false): benched accounts re-enter the pool on cooldown expiry without a liveness probe")
		return
	}
	interval := time.Duration(s.cfg.AccountRecheckIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultRecheckIntervalSeconds * time.Second
	}
	supervisor.Go(ctx, "account-recheck", func(ctx context.Context) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.recheckPendingAccounts(ctx)
			}
		}
	})
}

// defaultRecheckIntervalSeconds mirrors config.DefaultAccountRecheckIntervalSeconds
// for the in-loop fallback; normalize() already floors the configured value, so this
// is belt-and-braces for directly-constructed configs.
const defaultRecheckIntervalSeconds = 20

// recheckPendingAccounts probes every account that is recheck-pending and whose
// cooldown has elapsed, then either restores it to the pool (clean probe), re-benches
// it with a short backoff (still unhealthy / rate-limited / unreachable), or removes
// it (confirmed ban). Probes are staggered to avoid a thundering herd on the shared
// egress, and every blocking point selects on ctx.Done() so a shutdown is prompt.
func (s *Server) recheckPendingAccounts(ctx context.Context) {
	bindings, err := s.store.ListBindingsNeedingRecheck(ctx, storage.Now())
	if err != nil {
		log.Printf("[RECHECK] list pending: %v", err)
		return
	}
	if len(bindings) == 0 {
		return
	}
	accountIDs := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		accountIDs = append(accountIDs, binding.AccountID)
	}
	accounts, err := s.store.ListAccountsByIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("[RECHECK] load accounts: %v", err)
		return
	}
	tokens, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("[RECHECK] load tokens: %v", err)
		return
	}
	backoff := int64(s.cfg.AccountRecheckBackoffSeconds)
	if backoff <= 0 {
		backoff = 120
	}
	const stagger = 2 * time.Second
	restored, benched, removed := 0, 0, 0
	for _, b := range bindings {
		if ctx.Err() != nil {
			return
		}
		account, ok := accounts[b.AccountID]
		if !ok {
			// Account gone — drop the flag so the loop stops revisiting it.
			_ = s.store.SetBindingRecheckPending(ctx, b.AccountID, false)
			continue
		}
		// A quarantined or inactive account is already excluded from selection by other
		// means and has its own (admin-driven) recovery path; clear the recheck flag so
		// the loop doesn't keep probing it.
		if account.Status != "active" || account.QuarantineUntil > storage.Now() {
			_ = s.store.SetBindingRecheckPending(ctx, b.AccountID, false)
			continue
		}
		token, ok := tokens[b.AccountID]
		if !ok {
			_ = s.store.SetBindingRecheckPending(ctx, b.AccountID, false)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout())
		res := s.probeAccountLiveness(cctx, account, token)
		cancel()
		switch {
		case res.Verdict.IsBanned() && s.flagEnabled(ctx, "ban_detection_enabled", s.cfg.BanDetectionEnabled):
			// Confirmed ban surfaced by the probe → delete/quarantine via the standard
			// path; clear the flag (the account is no longer poolable).
			s.handleBannedAccount(ctx, account, res.Verdict, res.Status, res.Body, "recheck")
			_ = s.store.SetBindingRecheckPending(ctx, b.AccountID, false)
			removed++
		case res.Ready:
			// Clean 2xx → the account recovered. Clear both the cooldown and the
			// recheck flag so it immediately rejoins the candidate pool.
			_ = s.store.ClearBindingRecheck(ctx, b.AccountID)
			restored++
			log.Printf("[RECHECK] restored account=%s (probe http=%d)", b.AccountID, res.Status)
		default:
			// Still not ready (rate-limited, transport error, or other non-2xx). Keep it
			// out of the pool and retry after a short backoff. Note: a rate-limited probe
			// classifies as Alive-but-not-Ready, so a still-exhausted account is NOT
			// re-admitted here — it waits until a probe returns a clean success.
			_ = s.store.BenchBindingForRecheck(ctx, b.AccountID, storage.Now()+backoff)
			benched++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stagger):
		}
	}
	if restored+benched+removed > 0 {
		log.Printf("[RECHECK] sweep: restored=%d re-benched=%d removed=%d", restored, benched, removed)
	}
}
