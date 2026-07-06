package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"codex-account-pool/internal/routing"
)

func (s *Server) enterClaudeCacheSingleflight(ctx context.Context, enabled bool, body []byte, affinity routing.AffinityKey) (func(), bool) {
	if !enabled {
		return func() {}, false
	}
	key := claudeCacheSingleflightKey(body, affinity)
	if key == "" {
		return func() {}, false
	}
	s.claudeCacheFlightsMu.Lock()
	if done, ok := s.claudeCacheFlights[key]; ok {
		s.claudeCacheFlightsMu.Unlock()
		select {
		case <-done:
			return func() {}, true
		case <-ctx.Done():
			return func() {}, true
		}
	}
	done := make(chan struct{})
	s.claudeCacheFlights[key] = done
	s.claudeCacheFlightsMu.Unlock()
	release := func() {
		s.claudeCacheFlightsMu.Lock()
		if current, ok := s.claudeCacheFlights[key]; ok && current == done {
			delete(s.claudeCacheFlights, key)
			close(done)
		}
		s.claudeCacheFlightsMu.Unlock()
	}
	return release, false
}

func claudeCacheSingleflightKey(body []byte, affinity routing.AffinityKey) string {
	prefixHash := routing.AnthropicStablePromptPrefixHash(body)
	if prefixHash == "" {
		sum := sha256.Sum256(body)
		prefixHash = "body_" + hex.EncodeToString(sum[:])
	}
	parts := []string{"claude-cache", strings.TrimSpace(affinity.Source), strings.TrimSpace(affinity.Hash), prefixHash}
	return strings.Join(parts, ":")
}
