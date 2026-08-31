package api

import (
	"context"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

// forceCodex429ConfirmWindowSecs bounds the two-signal confirmation window.
// It tracks explicit upstream 429 responses only.
const forceCodex429ConfirmWindowSecs = 30

const forceCodex429StorageTimeout = 2 * time.Second

// forceCodex429State is transient confirmation state, not a retry controller.
// Durable confirmation storage is authoritative when available; this map is a
// process-local fallback for deployments without that migration.
type forceCodex429State struct {
	count       int
	windowStart int64
}

// confirmForceCodex429 records one explicit upstream 429. A second signal in
// the bounded window confirms the account. This helper performs no retry and
// never bypasses scheduler cooldown/quota checks.
func (s *Server) confirmForceCodex429(ctx context.Context, accountID string) bool {
	if s == nil || accountID == "" {
		return false
	}
	// SQLite/PostgreSQL is the authoritative cross-instance confirmation state.
	// A storage outage must never turn into a confirmed limit, so fall back only
	// to the process-local bounded streak and record a metadata-only degradation.
	if s.store != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forceCodex429StorageTimeout)
		confirmed, err := s.store.ObserveCodex429(stateCtx, accountID, "global", storage.Now(), forceCodex429ConfirmWindowSecs)
		cancel()
		if err == nil {
			return confirmed
		}
		s.enqueueAudit(storage.AuditLogRow{
			Action: "codex_429_confirmation_storage_degraded", State: "fallback",
			Reason: "storage_unavailable", Detail: "process_local_bounded_streak",
		})
	}
	now := storage.Now()
	s.forceCodex429Mu.Lock()
	defer s.forceCodex429Mu.Unlock()
	if s.forceCodex429Counts == nil {
		s.forceCodex429Counts = make(map[string]*forceCodex429State)
	}
	st := s.forceCodex429Counts[accountID]
	if st == nil || now < st.windowStart || now-st.windowStart > forceCodex429ConfirmWindowSecs {
		s.forceCodex429Counts[accountID] = &forceCodex429State{count: 1, windowStart: now}
		return false
	}
	if st.count < 2 {
		st.count++
	}
	return st.count >= 2
}

// clearForceCodex429Confirmation clears only the transient confirmation
// streak. It deliberately leaves any authoritative account cooldown intact.
func (s *Server) clearForceCodex429Confirmation(accountID string) {
	if s == nil || accountID == "" {
		return
	}
	s.forceCodex429Mu.Lock()
	delete(s.forceCodex429Counts, accountID)
	s.forceCodex429Mu.Unlock()
	if s.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), forceCodex429StorageTimeout)
		defer cancel()
		if err := s.store.ResetCodex429(ctx, accountID, "global"); err != nil {
			s.enqueueAudit(storage.AuditLogRow{
				Action: "codex_429_confirmation_storage_degraded", State: "clear_failed",
				Reason: "storage_unavailable", Detail: "admin_or_non_429_clear",
			})
		}
	}
}

// codex429GuardRuntimeEnabled is the global kill switch. Existing account flags
// do nothing until an operator enables this hot runtime setting.
func (s *Server) codex429GuardRuntimeEnabled(ctx context.Context) bool {
	return s != nil && s.flagEnabled(ctx, "codex_429_guard_runtime_enabled", s.cfg.Codex429GuardRuntimeEnabled)
}

func forceCodex429GuardProvider(account storage.Account, token storage.AccountToken) string {
	return strings.ToLower(strings.TrimSpace(accountprovider.EffectiveProvider(account.Provider, token, true)))
}

func forceCodex429GuardProviderEligible(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "openai", "chatgpt":
		return true
	default:
		return false
	}
}

// forceCodex429GuardCredentialEligible accepts OAuth and SetupToken-style
// access credentials but never a pay-as-you-go API key.
func forceCodex429GuardCredentialEligible(provider string, token storage.AccountToken) bool {
	if accountprovider.UsesAPIKey(provider, token) {
		return false
	}
	switch accountprovider.EffectiveAuthMethod(provider, token) {
	case accountprovider.AuthMethodOAuth:
		return accountprovider.IsAgentIdentity(token) || strings.TrimSpace(token.AccessToken) != "" ||
			strings.TrimSpace(token.RefreshToken) != "" || strings.TrimSpace(token.IDTokenRaw) != ""
	case accountprovider.AuthMethodAccessToken:
		return strings.TrimSpace(accountprovider.Credential(provider, token)) != ""
	default:
		return false
	}
}

// codex429ConfirmationAccountEligible is the baseline safety path. It is not
// gated by the account-local synthetic-pair setting: every eligible Codex/OpenAI
// OAuth or SetupToken account may require two-signal confirmation when the global
// runtime switch is enabled.
func codex429ConfirmationAccountEligible(account storage.Account, token storage.AccountToken) bool {
	provider := forceCodex429GuardProvider(account, token)
	return forceCodex429GuardProviderEligible(provider) &&
		forceCodex429GuardCredentialEligible(provider, token)
}

func (s *Server) codex429ConfirmationEnabled(ctx context.Context, account storage.Account, token storage.AccountToken) bool {
	return !account.IgnoreRateLimitControls && s.codex429GuardRuntimeEnabled(ctx) && codex429ConfirmationAccountEligible(account, token)
}

// codex429GuardEnabled controls only the account opt-in synthetic checkpoint.
// Confirmation/cooldown safety uses codex429ConfirmationEnabled above.
func (s *Server) codex429GuardEnabled(ctx context.Context, account storage.Account, token storage.AccountToken) bool {
	// IgnoreRateLimitControls is a separate, explicit high-risk override. When
	// both fields are true its legacy semantics remain authoritative; the 429
	// Guard does not add a second retry mechanism or synthetic checkpoint.
	return s.codex429ConfirmationEnabled(ctx, account, token) && account.ForceCodex429
}
