package api

import (
	"context"
	"strings"
	"time"

	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func (s *Server) enterKiroCacheSingleflight(ctx context.Context, raw []byte, affinity routing.AffinityKey, lease scheduler.Lease, model string, cachePointCount int) (func(), bool) {
	cfg := s.effectiveKiroConfig(ctx)
	if strings.ToLower(strings.TrimSpace(cfg.KiroCacheMode)) != "auto" || cachePointCount <= 0 {
		return func() {}, false
	}
	credentials, err := s.store.GetKiroCredentials(ctx, lease.Account.ID)
	if err != nil {
		return func() {}, false
	}
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, firstNonEmpty(credentials.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1"), cfg.KiroEndpointAllowlist)
	if err != nil {
		return func() {}, false
	}
	capability, err := s.store.GetKiroRuntimeCapability(ctx, lease.Account.ID, endpointHash, model)
	if err != nil && !storage.NotFound(err) {
		return func() {}, false
	}
	if err == nil && capability.CachePointState == "unsupported" {
		return func() {}, false
	}
	prefix := routing.AnthropicStablePromptPrefixHash(raw)
	if prefix == "" {
		return func() {}, false
	}
	key := lease.Account.ID + "\x00" + endpointHash + "\x00" + model + "\x00" + prefix
	waited := false
	s.kiroCacheFlightsMu.Lock()
	if existing := s.kiroCacheFlights[key]; existing != nil {
		s.kiroCacheFlightsMu.Unlock()
		waited = true
		timer := time.NewTimer(cacheSingleflightMaxWait)
		defer timer.Stop()
		select {
		case <-existing:
		case <-timer.C:
		case <-ctx.Done():
		}
		return func() {}, waited
	}
	if len(s.kiroCacheFlights) >= cacheSingleflightMaxFlights {
		s.kiroCacheFlightsMu.Unlock()
		return func() {}, false
	}
	done := make(chan struct{})
	s.kiroCacheFlights[key] = done
	s.kiroCacheFlightsMu.Unlock()
	release := func() {
		s.kiroCacheFlightsMu.Lock()
		if current := s.kiroCacheFlights[key]; current == done {
			delete(s.kiroCacheFlights, key)
			close(done)
		}
		s.kiroCacheFlightsMu.Unlock()
	}
	time.AfterFunc(cacheSingleflightMaxWait, release)
	return release, waited
}
