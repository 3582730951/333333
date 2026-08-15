package api

import (
	"context"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
)

func (s *Server) enterCodexCacheSingleflight(ctx context.Context, enabled bool, accountID, model string, body []byte, affinity routing.AffinityKey, metadata ...*bodysource.BodyMeta) (func(), bool) {
	if !enabled {
		return func() {}, false
	}
	var meta *bodysource.BodyMeta
	if len(metadata) > 0 {
		meta = metadata[0]
	}
	promptCacheKey := strings.TrimSpace(promptCacheKeyWithMeta(body, meta))
	if promptCacheKey == "" {
		return func() {}, false
	}
	key := strings.Join([]string{strings.TrimSpace(accountID), strings.TrimSpace(model), promptCacheKey, strings.TrimSpace(affinity.Hash)}, "\x00")
	s.codexCacheFlightsMu.Lock()
	if existing := s.codexCacheFlights[key]; existing != nil {
		s.codexCacheFlightsMu.Unlock()
		timer := time.NewTimer(cacheSingleflightMaxWait)
		defer timer.Stop()
		select {
		case <-existing:
		case <-timer.C:
		case <-ctx.Done():
		}
		return func() {}, true
	}
	if len(s.codexCacheFlights) >= cacheSingleflightMaxFlights {
		s.codexCacheFlightsMu.Unlock()
		return func() {}, false
	}
	done := make(chan struct{})
	s.codexCacheFlights[key] = done
	s.codexCacheFlightsMu.Unlock()
	release := func() {
		s.codexCacheFlightsMu.Lock()
		if current := s.codexCacheFlights[key]; current == done {
			delete(s.codexCacheFlights, key)
			close(done)
		}
		s.codexCacheFlightsMu.Unlock()
	}
	time.AfterFunc(cacheSingleflightMaxWait, release)
	return release, false
}
