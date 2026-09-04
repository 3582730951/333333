package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

type codexCacheKeyMetric struct {
	minute int64
	rpm    int64
	active int64
	peak   int64
	// shards is the ratcheted fan-out for a prefix-load entry (see
	// codexPromptCachePrefixShards). It is unused on per-key observation entries.
	shards int
}

// codexPromptCacheShardRPMPerShard is the request rate one prompt_cache_key is expected
// to carry before it is worth splitting. OpenAI pins a cache key to a single backend, so
// a key only needs a second shard once it approaches what one backend serves; below that
// every extra shard is a separate cache that must be written from cold.
const codexPromptCacheShardRPMPerShard = 15

// codexPromptCachePrefixShards sizes the shard fan-out for one shared prefix from its
// MEASURED load, with the configured value as a ceiling rather than a constant.
//
// The fan-out exists so that many sibling Codex agents sharing an identical 7-8K
// system/tool preamble do not pile onto one upstream backend. Applying it unconditionally
// inverted the trade for everyone below that rate: a low-volume deployment wrote the same
// large prefix into 4 distinct caches and read back from whichever shard a conversation
// happened to land on, so most requests paid a cold write instead of taking a hit.
//
// The count ratchets up immediately when load justifies it and steps down by at most one
// shard per quiet minute, so a burst can subside without abandoning the keys it warmed.
func (s *Server) codexPromptCachePrefixShards(accountID, model, base string, maxShards int, now time.Time) int {
	if maxShards < 1 {
		maxShards = 1
	}
	base = strings.TrimSpace(base)
	if base == "" || maxShards == 1 {
		return maxShards
	}
	mac := hmac.New(sha256.New, s.identitySecret())
	_, _ = mac.Write([]byte("codex-prompt-cache-prefix\x00" + strings.TrimSpace(accountID) + "\x00" + strings.TrimSpace(model) + "\x00" + base))
	metricKey := hex.EncodeToString(mac.Sum(nil))
	minute := now.Unix() / 60

	s.codexCacheMetricsMu.Lock()
	defer s.codexCacheMetricsMu.Unlock()
	if s.codexCacheMetrics == nil {
		s.codexCacheMetrics = make(map[string]*codexCacheKeyMetric)
	}
	metric := s.codexCacheMetrics[metricKey]
	if metric == nil {
		metric = &codexCacheKeyMetric{minute: minute, shards: 1}
		s.codexCacheMetrics[metricKey] = metric
	}
	if metric.minute != minute {
		// Step down one shard when the minute that just closed no longer justifies the
		// current width. A hard reset would re-cold-start the prefix after any 60s pause.
		if metric.shards > 1 && metric.rpm <= int64(codexPromptCacheShardRPMPerShard)*int64(metric.shards-1) {
			metric.shards--
		}
		metric.minute = minute
		metric.rpm = 0
	}
	metric.rpm++
	if metric.shards < 1 {
		metric.shards = 1
	}
	if want := int((metric.rpm + codexPromptCacheShardRPMPerShard - 1) / codexPromptCacheShardRPMPerShard); want > metric.shards {
		metric.shards = want
	}
	if metric.shards > maxShards {
		metric.shards = maxShards
	}
	return metric.shards
}

type codexCacheKeyObservation struct {
	Hash            string
	Shard           int
	MinuteRPM       int64
	ConcurrencyPeak int64
	done            func()
	doneOnce        *sync.Once
}

func (o *codexCacheKeyObservation) Done() {
	// The observation is commonly deferred and may also be closed by a transport
	// fallback; make the decrement idempotent so a retry cannot undercount active
	// concurrency for a different request.
	if o.doneOnce == nil {
		if o.done != nil {
			o.done()
		}
		return
	}
	o.doneOnce.Do(func() {
		if o.done != nil {
			o.done()
		}
	})
}

func (s *Server) observeCodexPromptCacheKey(accountID, model, key string, shardCount int, now time.Time) codexCacheKeyObservation {
	key = strings.TrimSpace(key)
	if key == "" {
		return codexCacheKeyObservation{Shard: -1}
	}
	mac := hmac.New(sha256.New, s.identitySecret())
	_, _ = mac.Write([]byte("codex-prompt-cache-key\x00" + strings.TrimSpace(accountID) + "\x00" + strings.TrimSpace(model) + "\x00" + key))
	metricKey := hex.EncodeToString(mac.Sum(nil))
	minute := now.Unix() / 60
	s.codexCacheMetricsMu.Lock()
	if s.codexCacheMetrics == nil {
		s.codexCacheMetrics = make(map[string]*codexCacheKeyMetric)
	}
	metric := s.codexCacheMetrics[metricKey]
	if metric == nil {
		metric = &codexCacheKeyMetric{minute: minute}
		s.codexCacheMetrics[metricKey] = metric
	}
	if metric.minute != minute {
		metric.minute = minute
		metric.rpm = 0
		metric.peak = metric.active
	}
	metric.rpm++
	metric.active++
	if metric.active > metric.peak {
		metric.peak = metric.active
	}
	rpm, peak := metric.rpm, metric.peak
	if len(s.codexCacheMetrics) > 8192 {
		for hash, candidate := range s.codexCacheMetrics {
			if candidate.active == 0 && candidate.minute < minute-1 {
				delete(s.codexCacheMetrics, hash)
			}
		}
	}
	s.codexCacheMetricsMu.Unlock()

	return codexCacheKeyObservation{
		Hash:            metricKey[:16],
		Shard:           codexPromptCacheKeyShardFromKey(key, shardCount),
		MinuteRPM:       rpm,
		ConcurrencyPeak: peak,
		doneOnce:        &sync.Once{},
		done: func() {
			s.codexCacheMetricsMu.Lock()
			if current := s.codexCacheMetrics[metricKey]; current != nil && current.active > 0 {
				current.active--
			}
			s.codexCacheMetricsMu.Unlock()
		},
	}
}

// observeCodexPromptCacheCoordinationKey is the internal counterpart to the
// wire-key observer. Official Codex CLI prompt_cache_key values stay untouched
// upstream; their stable, account-scoped coordination key is used only here.
func (s *Server) observeCodexPromptCacheCoordinationKey(accountID, model, key string, shardCount int, now time.Time) codexCacheKeyObservation {
	return s.observeCodexPromptCacheKey(accountID, model, key, shardCount, now)
}

func codexPromptCacheKeyShardFromKey(key string, shardCount int) int {
	// Only keys generated by the relay carry the `_sNN` suffix. An operator's
	// explicit key is opaque—even if it happens to contain the same characters.
	if !codexGeneratedPromptCacheKey(key) {
		return -1
	}
	if marker := strings.LastIndex(key, "_s"); marker >= 0 && marker+2 < len(key) {
		if marker+4 == len(key) {
			if shard, err := strconv.Atoi(key[marker+2:]); err == nil && shard >= 0 && shard < 16 {
				return shard
			}
		}
	}
	if shardCount == 1 && strings.HasPrefix(key, "auto_") {
		return 0
	}
	return -1
}

func codexGeneratedPromptCacheKey(key string) bool {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "auto_") {
		return false
	}
	body := strings.TrimPrefix(key, "auto_")
	if len(body) == 24 {
		return isLowerHex(body)
	}
	if len(body) != 28 || body[24:26] != "_s" {
		return false
	}
	return isLowerHex(body[:24]) && body[26] >= '0' && body[26] <= '9' && body[27] >= '0' && body[27] <= '9'
}

func isLowerHex(value string) bool {
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
