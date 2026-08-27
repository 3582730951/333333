package api

import (
	"testing"
	"time"
)

func TestCodexPromptCacheKeyMetricsAreRedactedAndTrackMinutePeak(t *testing.T) {
	s := &Server{codexCacheMetrics: map[string]*codexCacheKeyMetric{}, identitySecretCached: []byte("deployment-a")}
	now := time.Unix(1_700_000_000, 0)
	key := "auto_0123456789abcdef01234567_s03"
	one := s.observeCodexPromptCacheKey("account", "gpt-5.6-sol", key, 4, now)
	two := s.observeCodexPromptCacheKey("account", "gpt-5.6-sol", key, 4, now)
	if one.Hash == "" || one.Hash == "auto_secret_s03" || len(one.Hash) != 16 {
		t.Fatalf("key hash was not redacted: %q", one.Hash)
	}
	if one.Shard != 3 || two.MinuteRPM != 2 || two.ConcurrencyPeak != 2 {
		t.Fatalf("metric snapshots one=%+v two=%+v", one, two)
	}
	otherAccount := s.observeCodexPromptCacheKey("account-2", "gpt-5.6-sol", key, 4, now)
	defer otherAccount.Done()
	if otherAccount.MinuteRPM != 1 || otherAccount.Hash == one.Hash {
		t.Fatalf("account namespace metrics leaked: first=%+v other=%+v", one, otherAccount)
	}
	otherDeployment := &Server{codexCacheMetrics: map[string]*codexCacheKeyMetric{}, identitySecretCached: []byte("deployment-b")}
	otherKey := otherDeployment.observeCodexPromptCacheKey("account", "gpt-5.6-sol", key, 4, now)
	defer otherKey.Done()
	if otherKey.Hash == one.Hash {
		t.Fatalf("deployment namespace metrics leaked: first=%+v other=%+v", one, otherKey)
	}
	one.Done()
	two.Done()
	next := s.observeCodexPromptCacheKey("account", "gpt-5.6-sol", key, 4, now.Add(time.Minute))
	defer next.Done()
	if next.MinuteRPM != 1 || next.ConcurrencyPeak != 1 {
		t.Fatalf("minute reset = %+v", next)
	}
}

func TestCodexPromptCachePrefixShardsScaleWithMeasuredLoad(t *testing.T) {
	s := &Server{codexCacheMetrics: map[string]*codexCacheKeyMetric{}, identitySecretCached: []byte("deployment-a")}
	now := time.Unix(1_700_000_000, 0)
	base := "auto_0123456789abcdef01234567"
	shards := func(at time.Time) int {
		return s.codexPromptCachePrefixShards("account", "gpt-5.6-sol", base, 4, at)
	}

	// A quiet prefix stays on ONE key. Splitting it would write the same large
	// system/tool preamble into several upstream caches and then read back from only
	// whichever shard a conversation landed on.
	for i := 1; i <= codexPromptCacheShardRPMPerShard; i++ {
		if got := shards(now); got != 1 {
			t.Fatalf("request %d of a quiet minute took %d shards, want 1", i, got)
		}
	}
	// Passing one backend's worth of traffic is what earns a second shard.
	if got := shards(now); got != 2 {
		t.Fatalf("shards just past the per-shard rate = %d, want 2", got)
	}
	for i := 0; i < codexPromptCacheShardRPMPerShard; i++ {
		shards(now)
	}
	if got := shards(now); got != 3 {
		t.Fatalf("shards at triple the per-shard rate = %d, want 3", got)
	}

	// The configured value is a ceiling, not a target.
	for i := 0; i < 4*codexPromptCacheShardRPMPerShard; i++ {
		shards(now)
	}
	if got := shards(now); got != 4 {
		t.Fatalf("shards under sustained load = %d, want the configured ceiling 4", got)
	}

	// A burst that subsides gives width back one shard per quiet minute instead of
	// resetting, so the keys it warmed are not all abandoned at once. The first quiet
	// minute still reports the old width: the step-down judges the minute that just
	// closed, which was the busy one.
	quiet := now
	for _, want := range []int{4, 3, 2, 1, 1} {
		quiet = quiet.Add(time.Minute)
		if got := shards(quiet); got != want {
			t.Fatalf("shards after quiet minute ending %s = %d, want %d", quiet, got, want)
		}
	}
}

func TestCodexPromptCachePrefixShardsRespectSingleKeyAndUnknownPrefix(t *testing.T) {
	s := &Server{codexCacheMetrics: map[string]*codexCacheKeyMetric{}, identitySecretCached: []byte("deployment-a")}
	now := time.Unix(1_700_000_000, 0)
	// An operator who pinned the legacy single key is never widened, however hot it runs.
	for i := 0; i < 4*codexPromptCacheShardRPMPerShard; i++ {
		if got := s.codexPromptCachePrefixShards("account", "gpt-5.6-sol", "auto_0123456789abcdef01234567", 1, now); got != 1 {
			t.Fatalf("single-key deployment widened to %d shards", got)
		}
	}
	// A request with no derivable stable prefix carries no load signal, so it falls back
	// to the configured value rather than silently collapsing to one key.
	if got := s.codexPromptCachePrefixShards("account", "gpt-5.6-sol", "  ", 4, now); got != 4 {
		t.Fatalf("unkeyed prefix shards = %d, want the configured 4", got)
	}
}

func TestCodexPromptCacheKeyShardRecognizesLegacyMode(t *testing.T) {
	if got := codexPromptCacheKeyShardFromKey("auto_0123456789abcdef01234567", 1); got != 0 {
		t.Fatalf("legacy shard = %d", got)
	}
	if got := codexPromptCacheKeyShardFromKey("operator-key", 4); got != -1 {
		t.Fatalf("explicit key shard = %d", got)
	}
	if got := codexPromptCacheKeyShardFromKey("auto_operator_s03", 4); got != -1 {
		t.Fatalf("opaque auto-prefixed key shard = %d", got)
	}
}
