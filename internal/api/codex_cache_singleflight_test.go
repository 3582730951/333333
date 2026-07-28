package api

import (
	"context"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
)

func TestCodexCacheSingleflightUsesAccountModelAndCacheKey(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]chan struct{}{}}
	body := []byte(`{"model":"gpt","prompt_cache_key":"stable","input":"same"}`)
	affinity := routing.AffinityFromKey("conversation", "test")
	release, waited := s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt", body, affinity)
	if waited {
		t.Fatal("leader waited")
	}
	resumed := make(chan bool, 1)
	go func() {
		followerRelease, followerWaited := s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt", body, affinity)
		followerRelease()
		resumed <- followerWaited
	}()
	select {
	case <-resumed:
		t.Fatal("follower bypassed active leader")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case followerWaited := <-resumed:
		if !followerWaited {
			t.Fatal("follower wait was not recorded")
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not resume at leader response")
	}
	otherAccount, waited := s.enterCodexCacheSingleflight(context.Background(), true, "account-b", "gpt", body, affinity)
	if waited {
		t.Fatal("singleflight crossed account boundary")
	}
	otherModel, waited := s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt-other", body, affinity)
	if waited {
		t.Fatal("singleflight crossed model boundary")
	}
	otherAccount()
	otherModel()
}

func TestCodexCacheSingleflightSkipsBodyWithoutCacheKey(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]chan struct{}{}}
	release, waited := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", []byte(`{"model":"gpt","input":"x"}`), routing.AffinityKey{})
	release()
	if waited || len(s.codexCacheFlights) != 0 {
		t.Fatalf("unexpected flight waited=%v flights=%d", waited, len(s.codexCacheFlights))
	}
}
