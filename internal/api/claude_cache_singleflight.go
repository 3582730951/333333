package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"codex-account-pool/internal/routing"
)

const (
	cacheSingleflightMaxFlights = 4096
	cacheSingleflightMaxWait    = 30 * time.Second
)

func (s *Server) enterClaudeCacheSingleflight(ctx context.Context, enabled bool, accountID, model string, body []byte, affinity routing.AffinityKey) (func(), bool) {
	if !enabled {
		return func() {}, false
	}
	key := claudeCacheSingleflightKey(accountID, model, body, affinity)
	if key == "" {
		return func() {}, false
	}
	s.claudeCacheFlightsMu.Lock()
	if done, ok := s.claudeCacheFlights[key]; ok {
		s.claudeCacheFlightsMu.Unlock()
		timer := time.NewTimer(cacheSingleflightMaxWait)
		defer timer.Stop()
		select {
		case <-done:
			return func() {}, true
		case <-timer.C:
			return func() {}, true
		case <-ctx.Done():
			return func() {}, true
		}
	}
	if len(s.claudeCacheFlights) >= cacheSingleflightMaxFlights {
		s.claudeCacheFlightsMu.Unlock()
		return func() {}, false
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
	time.AfterFunc(cacheSingleflightMaxWait, release)
	return release, false
}

func claudeCacheSingleflightKey(accountID, model string, body []byte, affinity routing.AffinityKey) string {
	prefixHash := routing.AnthropicStablePromptPrefixHash(body)
	if prefixHash == "" {
		prefixHash = claudeCacheSingleflightBodyHash(body)
	}
	parts := []string{"claude-cache", strings.TrimSpace(accountID), strings.TrimSpace(model), strings.TrimSpace(affinity.Source), strings.TrimSpace(affinity.Hash), prefixHash}
	return strings.Join(parts, ":")
}

// claudeCacheSingleflightBodyHash is the exact-request fallback for short prompts
// that are below the reusable-prefix threshold. Billing attribution is transport
// identity rather than prompt content, so remove only those blocks before hashing;
// all actual system/messages/tools/metadata content remains part of the key. This
// remains correct for the fixed .503 component emitted by Claude Code 2.1.226 and
// also keeps the cache key stable across a future client build bump.
func claudeCacheSingleflightBodyHash(body []byte) string {
	canonical := body
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) == nil {
		if rawSystem, ok := root["system"]; ok {
			changed := false
			var blocks []json.RawMessage
			if json.Unmarshal(rawSystem, &blocks) == nil {
				filtered := blocks[:0]
				for _, block := range blocks {
					var textBlock struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(block, &textBlock) == nil && strings.HasPrefix(strings.TrimSpace(textBlock.Text), "x-anthropic-billing-header:") {
						changed = true
						continue
					}
					filtered = append(filtered, block)
				}
				if changed {
					if encoded, err := json.Marshal(filtered); err == nil {
						root["system"] = encoded
					}
				}
			} else {
				var text string
				if json.Unmarshal(rawSystem, &text) == nil && strings.HasPrefix(strings.TrimSpace(text), "x-anthropic-billing-header:") {
					delete(root, "system")
					changed = true
				}
			}
			if changed {
				if encoded, err := json.Marshal(root); err == nil {
					canonical = encoded
				}
			}
		}
	}
	sum := sha256.Sum256(canonical)
	return "body_" + hex.EncodeToString(sum[:])
}
