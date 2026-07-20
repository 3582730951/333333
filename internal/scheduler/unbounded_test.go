package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/storage"
)

func TestUnboundedQueueResumesAfterLeaseRelease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "a", Label: "a", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	profile.MaxConcurrency = 1
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	selectCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		lease, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex"})
		if err == nil {
			lease.Release()
		}
		result <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if m := s.Metrics(); m.Queued != 1 {
		t.Fatalf("queued=%d", m.Queued)
	}
	held.Release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued selection did not resume")
	}
	if m := s.Metrics(); m.Queued != 0 || m.Waited != 1 {
		t.Fatalf("metrics=%+v", m)
	}
}
func TestUnboundedQueueCancellationRemovesWaiter(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "a", Label: "a", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	profile.MaxConcurrency = 1
	_ = store.UpsertEgressProfile(ctx, profile)
	s := New(store, config.Default())
	held, _ := s.Select(ctx, Route{Group: "cyber", Provider: "codex"})
	defer held.Release()
	selectCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex"}); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock")
	}
	if m := s.Metrics(); m.Queued != 0 || m.Cancelled != 1 {
		t.Fatalf("metrics=%+v", m)
	}
}

func TestWaitQueueRemovesHeadMiddleAndTailInConstantTimeStructure(t *testing.T) {
	s := &Scheduler{waitQueues: map[string]*waitQueue{}}
	route := Route{Group: "g", Provider: "codex", Model: "m"}
	key, head := s.enqueue(route, "concurrency")
	_, middle := s.enqueue(route, "concurrency")
	_, tail := s.enqueue(route, "concurrency")
	if !s.removeWaiter(key, middle) {
		if !s.waiterIsHead(key, head) {
			t.Fatal("removing the middle corrupted the queue head")
		}
	} else {
		t.Fatal("middle waiter was reported as the head")
	}
	q := s.waitQueues[key]
	if q == nil || q.len != 2 || q.head != head || q.tail != tail || head.next != tail || tail.prev != head {
		t.Fatalf("queue links after middle removal: %+v", q)
	}
	if !s.removeWaiter(key, head) || !s.waiterIsHead(key, tail) {
		t.Fatal("head removal did not promote the tail")
	}
	if !s.removeWaiter(key, tail) || s.waitQueues[key] != nil {
		t.Fatal("final removal did not delete the empty route queue")
	}
}

func BenchmarkWaitQueueCancel(b *testing.B) {
	s := &Scheduler{waitQueues: map[string]*waitQueue{}}
	route := Route{Group: "g", Provider: "codex", Model: "m"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key, w := s.enqueue(route, "concurrency")
		if !s.removeWaiter(key, w) {
			b.Fatal("single waiter must be the head")
		}
	}
}

func TestUnboundedQueuePreservesFIFOAgainstNewArrivals(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "a", Label: "a", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	profile, _ := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	profile.MaxConcurrency = 1
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}

	type acquired struct {
		order int
		lease Lease
		err   error
	}
	results := make(chan acquired, 2)
	releaseFirst := make(chan struct{})
	selectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		lease, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex"})
		results <- acquired{order: 1, lease: lease, err: err}
		if err == nil {
			<-releaseFirst
			lease.Release()
		}
	}()
	waitForQueued := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for s.Metrics().Queued != want {
			if time.Now().After(deadline) {
				t.Fatalf("queued=%d, want %d", s.Metrics().Queued, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitForQueued(1)
	go func() {
		lease, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex"})
		results <- acquired{order: 2, lease: lease, err: err}
		if err == nil {
			lease.Release()
		}
	}()
	waitForQueued(2)
	held.Release()

	first := <-results
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.order != 1 {
		first.lease.Release()
		t.Fatalf("request %d bypassed the FIFO head", first.order)
	}
	select {
	case second := <-results:
		if second.err == nil {
			second.lease.Release()
		}
		t.Fatalf("second request acquired before FIFO head released: %+v", second)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	second := <-results
	if second.err != nil || second.order != 2 {
		t.Fatalf("second result = %+v", second)
	}
}

func TestMixedClaudeKiroChoosesLeastLoaded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, a := range []storage.Account{{ID: "c", Label: "c", GroupName: "cyber", Provider: "claude", Status: "active"}, {ID: "k", Label: "k", GroupName: "cyber", Provider: "kiro", Status: "active"}} {
		if err := store.UpsertAccount(ctx, a, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: "c", ModelSlug: "claude-sonnet-4-6", AvailabilityState: capability.AvailabilityUnverified, Source: "claude_static_unverified"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, capability.StaticKiroModels("k")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: "k", AuthMethod: "api_key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	endpointHash, err := kirowire.EndpointHash("", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveKiroCapability(ctx, "k", endpointHash, "claude-sonnet-4.6", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	held, err := s.Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"claude", "kiro"}, Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	next, err := s.Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"claude", "kiro"}, Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if next.Account.ID == held.Account.ID {
		t.Fatalf("both leases selected %s", next.Account.ID)
	}
}

func TestKiroCapabilityRemovalPreventsStaticFallback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "k", Label: "k", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	caps := capability.StaticKiroModels(account.ID)
	filtered := caps[:0]
	for _, c := range caps {
		if c.ModelSlug != "claude-opus-4.8" {
			filtered = append(filtered, c)
		}
	}
	if err := store.UpsertCapabilities(ctx, filtered); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "kiro", Model: "claude-opus-4-8"})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) || noAccount.Counters.ModelUnsupported != 1 {
		t.Fatalf("removed Kiro capability selected through static fallback: %v", err)
	}
}
