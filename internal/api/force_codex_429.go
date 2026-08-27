package api

import (
	"context"

	"codex-account-pool/internal/storage"
)

// forceCodex429ConfirmWindowSecs is the trailing window (in storage.Now() unix
// seconds) within which two explicit 429s on one account confirm "强制卡429"
// same-account retention. Mirrors sub2api-plus openAIOAuth429ConfirmationWindow.
const forceCodex429ConfirmWindowSecs = 30

// forceCodex429State tracks the trailing-window 429 count for one account. Pure
// in-memory state: an opt-in, niche override, lost on restart without harm.
type forceCodex429State struct {
	count       int
	windowStart int64
}

// confirmForceCodex429 advances the per-account trailing-window 429 counter and
// reports whether the account has now produced two explicit 429s within
// forceCodex429ConfirmWindowSecs. The first 429 returns false so the ordinary
// failover/cooldown path handles it; the second inside the window returns true so
// the caller switches to same-account retention (the analog of sub2api-plus
// pinning the connection after confirming the 429 guard). The window resets
// naturally: a 429 arriving after the window elapsed restarts the count at one.
func (s *Server) confirmForceCodex429(ctx context.Context, accountID string) bool {
	now := storage.Now()
	s.forceCodex429Mu.Lock()
	defer s.forceCodex429Mu.Unlock()
	if s.forceCodex429Counts == nil {
		s.forceCodex429Counts = make(map[string]*forceCodex429State)
	}
	st := s.forceCodex429Counts[accountID]
	if st == nil || now-st.windowStart > forceCodex429ConfirmWindowSecs {
		s.forceCodex429Counts[accountID] = &forceCodex429State{count: 1, windowStart: now}
		return false
	}
	st.count++
	return st.count >= 2
}
