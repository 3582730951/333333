package api

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCodexCacheSingleflightUsesAccountModelKeyAndExactPrefix(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	body := []byte(`{"model":"gpt","prompt_cache_key":"stable","instructions":"same","input":[{"role":"user","content":"same"}]}`)
	leader := s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt", body)
	if leader.Waited {
		t.Fatal("leader waited")
	}
	resumed := make(chan *codexCacheFlightAdmission, 1)
	go func() {
		resumed <- s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt", body)
	}()
	select {
	case <-resumed:
		t.Fatal("follower bypassed active leader")
	case <-time.After(20 * time.Millisecond):
	}
	leader.Release("response_headers")
	select {
	case follower := <-resumed:
		if !follower.Waited || follower.WaitReason != "matching_cold_prefix" || follower.ReleaseReason != "response_headers" {
			t.Fatalf("follower diagnostics = %+v", follower)
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not resume at leader response headers")
	}

	otherAccount := s.enterCodexCacheSingleflight(context.Background(), true, "account-b", "gpt", body)
	if otherAccount.Waited {
		t.Fatal("singleflight crossed account boundary")
	}
	otherModel := s.enterCodexCacheSingleflight(context.Background(), true, "account-a", "gpt-other", body)
	if otherModel.Waited {
		t.Fatal("singleflight crossed model boundary")
	}
	otherAccount.Release("test_done")
	otherModel.Release("test_done")
}

func TestCodexCacheSingleflightDifferentTurnsAndToolResultsDoNotWait(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	first := []byte(`{"prompt_cache_key":"stable","instructions":"same","input":[{"role":"user","content":"turn one"},{"type":"function_call_output","call_id":"call_1","output":"result one"}]}`)
	second := []byte(`{"prompt_cache_key":"stable","instructions":"same","input":[{"role":"user","content":"turn two"},{"type":"function_call_output","call_id":"call_1","output":"result two"}]}`)
	leader := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", first)
	follower := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", second)
	defer leader.Release("test_done")
	defer follower.Release("test_done")
	if follower.Waited {
		t.Fatal("different turns/tool results shared one flight")
	}
	if leader.PrefixHash == follower.PrefixHash || leader.PrefixSource != "implicit_exact_prefix" {
		t.Fatalf("prefix diagnostics leader=%+v follower=%+v", leader, follower)
	}
}

func TestCodexCacheSingleflightExplicitBreakpointIgnoresSuffix(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	build := func(suffix string) []byte {
		return []byte(`{"prompt_cache_key":"stable","instructions":"same","input":[` +
			`{"role":"developer","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":` + suffix + `}]},` +
			`{"role":"user","content":"different suffix"}]}`)
	}
	leader := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", build(`"one"`))
	resumed := make(chan *codexCacheFlightAdmission, 1)
	go func() {
		resumed <- s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", build(`"two"`))
	}()
	select {
	case <-resumed:
		t.Fatal("matching explicit prefix did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	leader.Release("first_sse_event")
	follower := <-resumed
	if !follower.Waited || follower.PrefixSource != "explicit_breakpoint" || follower.ReleaseReason != "first_sse_event" {
		t.Fatalf("explicit follower = %+v", follower)
	}
}

func TestCodexCacheSingleflightExplicitPrefixKeepsRawFieldOrderDistinct(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	first := []byte(`{"prompt_cache_key":"stable","input":[{"role":"developer","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}],"marker":1},{"role":"user","content":"turn"}]}`)
	second := []byte(`{"prompt_cache_key":"stable","input":[{"role":"developer","marker":1,"content":[{"type":"input_text","prompt_cache_breakpoint":{"mode":"explicit"},"text":"stable"}]},{"role":"user","content":"turn"}]}`)
	leader := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", first)
	follower := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", second)
	defer leader.Release("test_done")
	defer follower.Release("test_done")
	if follower.Waited {
		t.Fatal("byte-distinct explicit prefixes shared one flight")
	}
}

func TestCodexCacheSingleflightSkipsBodyWithoutCacheKey(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	admission := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt", []byte(`{"model":"gpt","input":"x"}`))
	admission.Release("test_done")
	if admission.Waited || len(s.codexCacheFlights) != 0 {
		t.Fatalf("unexpected flight admission=%+v flights=%d", admission, len(s.codexCacheFlights))
	}
}

func TestCodexCacheSingleflightFortyConcurrentFollowersReleaseAtFirstHeader(t *testing.T) {
	s := &Server{codexCacheFlights: map[string]*codexCacheFlight{}}
	body := []byte(`{"prompt_cache_key":"hot-shard","instructions":"stable","input":[{"role":"user","content":"identical cold request"}]}`)
	leader := s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt-5.6-sol", body)
	const followers = 40
	results := make(chan *codexCacheFlightAdmission, followers)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(followers)
	for i := 0; i < followers; i++ {
		go func() {
			ready.Done()
			<-start
			results <- s.enterCodexCacheSingleflight(context.Background(), true, "account", "gpt-5.6-sol", body)
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-results:
		t.Fatal("a hot-key follower escaped before the first response header")
	case <-time.After(30 * time.Millisecond):
	}
	leader.Release("response_headers")
	for i := 0; i < followers; i++ {
		select {
		case result := <-results:
			if !result.Waited || result.ReleaseReason != "response_headers" {
				t.Fatalf("follower %d = %+v", i, result)
			}
		case <-time.After(time.Second):
			t.Fatalf("follower %d remained blocked", i)
		}
	}
}
