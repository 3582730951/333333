package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

// promptCacheDriftGuard observes per-conversation prompt-cache key stability across
// turns and records the drift as an audit trail. Sessions A (sticky pinning) and B
// (conversation-anchor keys) make cross-account moves and dead-zone key gaps
// recoverable by design; this guard is the observability half — when the key still
// changes between two turns of the same conversation (upstream stripping it, a
// client rotating an explicit key, a system-prompt hot-reload, a compaction),
// the reason is written down instead of being silently charged to "cache miss".
type promptCacheDriftGuard struct {
	mu      sync.Mutex
	entries map[string]promptCacheDriftEntry
}

type promptCacheDriftEntry struct {
	keySource  string
	key        string
	systemHash string
	at         time.Time
	lastAudit  time.Time
}

const (
	promptDriftMaxEntries = 4096
	promptDriftTTL        = 30 * time.Minute
	promptDriftAuditQuiet = 60 * time.Second
)

func newPromptCacheDriftGuard() *promptCacheDriftGuard {
	return &promptCacheDriftGuard{entries: make(map[string]promptCacheDriftEntry, 128)}
}

func promptSystemHash(systemPrompt string) string {
	if systemPrompt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemPrompt))
	return hex.EncodeToString(sum[:])[:16]
}

// note records one turn's key facts for a conversation. It returns whether a drift
// was observed so the caller can also log it at the request level (diagnostics).
func (g *promptCacheDriftGuard) note(ctx context.Context, store *storage.Store, affinityHash, keySource, key, systemPrompt string) bool {
	if g == nil || affinityHash == "" || key == "" {
		return false
	}
	systemHash := promptSystemHash(systemPrompt)
	now := time.Now()
	g.mu.Lock()
	prev, ok := g.entries[affinityHash]
	if !ok {
		g.entries[affinityHash] = promptCacheDriftEntry{keySource: keySource, key: key, systemHash: systemHash, at: now}
		if len(g.entries) > promptDriftMaxEntries {
			for k, e := range g.entries {
				if now.Sub(e.at) > promptDriftTTL {
					delete(g.entries, k)
				}
			}
			if len(g.entries) > promptDriftMaxEntries {
				g.entries = make(map[string]promptCacheDriftEntry, 128)
			}
		}
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()

	var drifts []string
	if prev.key != key {
		drifts = append(drifts, fmt.Sprintf("key %q -> %q", truncateCacheKey(prev.key), truncateCacheKey(key)))
	}
	if prev.keySource != "" && prev.keySource != keySource {
		drifts = append(drifts, fmt.Sprintf("source %s -> %s", prev.keySource, keySource))
	}
	if prev.systemHash != "" && systemHash != "" && prev.systemHash != systemHash {
		drifts = append(drifts, fmt.Sprintf("system_prompt %s -> %s", prev.systemHash, systemHash))
	}
	if len(drifts) == 0 {
		g.mu.Lock()
		g.entries[affinityHash] = promptCacheDriftEntry{keySource: keySource, key: key, systemHash: systemHash, at: now}
		g.mu.Unlock()
		return false
	}

	// Throttled audit: an agentic conversation that drifts every turn would
	// otherwise spam one row per request.
	audit := now.Sub(prev.lastAudit) > promptDriftAuditQuiet
	g.mu.Lock()
	entry := promptCacheDriftEntry{keySource: keySource, key: key, systemHash: systemHash, at: now}
	if audit {
		entry.lastAudit = now
	} else {
		entry.lastAudit = prev.lastAudit
	}
	g.entries[affinityHash] = entry
	g.mu.Unlock()
	if audit {
		_ = store.InsertAuditLog(context.WithoutCancel(ctx), storage.AuditLogRow{
			Action: "prompt_cache_prefix_drift", State: "observed", Reason: "prefix_changed",
			Detail: fmt.Sprintf("affinity=%s %s", affinityHash, strings.Join(drifts, "; ")),
		})
	}
	return true
}

func truncateCacheKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 48 {
		return key
	}
	return key[:24] + "…" + key[len(key)-24:]
}
