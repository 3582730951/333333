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
