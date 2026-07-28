package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestLocalLeaseCoordinatorAtomicallyGatesTokensAndSharedResources(t *testing.T) {
	coordinator := newLocalLeaseCoordinator()
	request := LeaseRequest{
		AccountID: "account-a", EstimatedTokens: 60, TokenBudget: 100,
		Resources: []LeaseResource{{ID: "egress-shared", Limit: 2}}, TTL: time.Minute,
	}
	first, reason, err := coordinator.TryAcquire(context.Background(), request)
	if err != nil || first == nil || reason != leaseBlockNone || first.FencingToken() == 0 {
		t.Fatalf("first lease=%v reason=%s err=%v", first, reason, err)
	}
	if lease, reason, err := coordinator.TryAcquire(context.Background(), request); err != nil || lease != nil || reason != leaseBlockTokenBudget {
		t.Fatalf("token gate lease=%v reason=%s err=%v", lease, reason, err)
	}
	request.Compaction = true
	second, reason, err := coordinator.TryAcquire(context.Background(), request)
	if err != nil || second == nil || reason != leaseBlockNone {
		t.Fatalf("compaction lease=%v reason=%s err=%v", second, reason, err)
	}
	if lease, reason, err := coordinator.TryAcquire(context.Background(), request); err != nil || lease != nil || reason != leaseBlockConcurrency {
		t.Fatalf("resource gate lease=%v reason=%s err=%v", lease, reason, err)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, reason, err := coordinator.TryAcquire(context.Background(), request)
	if err != nil || third == nil || reason != leaseBlockNone {
		t.Fatalf("post-release lease=%v reason=%s err=%v", third, reason, err)
	}
	_ = second.Release(context.Background())
	_ = third.Release(context.Background())
}

type failingLeaseCoordinator struct{}

func (failingLeaseCoordinator) TryAcquire(context.Context, LeaseRequest) (CoordinatedLease, leaseBlockReason, error) {
	return nil, leaseBlockCoordinator, ErrLeaseCoordinatorUnavailable
}
func (failingLeaseCoordinator) Notifications() <-chan struct{} { return nil }
func (failingLeaseCoordinator) Close() error                   { return nil }

func TestClusterCoordinatorFailureNeverFallsBackToLocalLease(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.UpsertAccount(ctx, storage.Account{ID: "fail-closed", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	s := NewWithLeaseCoordinator(store, config.Default(), failingLeaseCoordinator{})
	defer s.Close()
	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex"})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) || noAccount.Counters.Coordinator == 0 {
		t.Fatalf("cluster failure err=%v", err)
	}
}

type silentLeaseCoordinator struct {
	allowed atomic.Bool
	fence   atomic.Uint64
}

func (c *silentLeaseCoordinator) TryAcquire(context.Context, LeaseRequest) (CoordinatedLease, leaseBlockReason, error) {
	if !c.allowed.Load() {
		return nil, leaseBlockConcurrency, nil
	}
	return &silentCoordinatedLease{fence: c.fence.Add(1)}, leaseBlockNone, nil
}
func (*silentLeaseCoordinator) Notifications() <-chan struct{} { return nil }
func (*silentLeaseCoordinator) Close() error                   { return nil }

type silentCoordinatedLease struct{ fence uint64 }

func (l *silentCoordinatedLease) FencingToken() uint64        { return l.fence }
func (*silentCoordinatedLease) Release(context.Context) error { return nil }

func TestSchedulerPollingRecoversLostCoordinatorNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store := testStore(t)
	if err := store.UpsertAccount(ctx, storage.Account{ID: "silent-wakeup", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	coordinator := &silentLeaseCoordinator{}
	s := NewWithLeaseCoordinator(store, config.Default(), coordinator)
	defer s.Close()
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex"})
		if err == nil {
			lease.Release()
		}
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	coordinator.allowed.Store(true)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("250ms recovery poll took %s", elapsed)
		}
	case <-ctx.Done():
		t.Fatal("lost notification left scheduler waiter stuck")
	}
}

func TestSchedulerRecoveryPollIsJitteredAround250Milliseconds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 32 {
		delay := schedulerRecoveryPollDelay()
		if delay < schedulerRecoveryPollInterval-schedulerRecoveryPollJitter || delay > schedulerRecoveryPollInterval+schedulerRecoveryPollJitter {
			t.Fatalf("delay=%s", delay)
		}
		seen[delay] = true
	}
	if len(seen) < 2 {
		t.Fatalf("recovery poll was not jittered: %v", seen)
	}
}

func TestLocalLeaseCoordinatorConcurrentLimit(t *testing.T) {
	coordinator := newLocalLeaseCoordinator()
	const workers = 64
	var granted int
	var mu sync.Mutex
	leases := make([]CoordinatedLease, 0, 4)
	var wait sync.WaitGroup
	wait.Add(workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			lease, _, _ := coordinator.TryAcquire(context.Background(), LeaseRequest{AccountID: "a", Resources: []LeaseResource{{ID: "e", Limit: 4}}})
			if lease != nil {
				mu.Lock()
				granted++
				leases = append(leases, lease)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wait.Wait()
	if granted != 4 {
		t.Fatalf("granted=%d want=4", granted)
	}
	for _, lease := range leases {
		_ = lease.Release(context.Background())
	}
}

func TestLocalLeaseCoordinatorAcrossUsesTargetCapacityAndGlobalRoundRobin(t *testing.T) {
	coordinator := newLocalLeaseCoordinator()
	makeCandidates := func(left, right int) []LeaseCandidateRequest {
		candidates := make([]LeaseCandidateRequest, 0, left+right)
		for index := range left {
			id := fmt.Sprintf("left-%d", index)
			candidates = append(candidates, LeaseCandidateRequest{ChoiceKey: "left", Request: LeaseRequest{AccountID: id, Resources: []LeaseResource{{ID: "egress-" + id}}}})
		}
		for index := range right {
			id := fmt.Sprintf("right-%d", index)
			candidates = append(candidates, LeaseCandidateRequest{ChoiceKey: "right", Request: LeaseRequest{AccountID: id, Resources: []LeaseResource{{ID: "egress-" + id}}}})
		}
		return candidates
	}

	assertHeldDistribution := func(name string, candidates []LeaseCandidateRequest, acquisitions, wantLeft, wantRight int) {
		t.Helper()
		counts := map[string]int{}
		leases := make([]CoordinatedLease, 0, acquisitions)
		for range acquisitions {
			selected, reason, err := coordinator.TryAcquireAcross(context.Background(), candidates)
			if err != nil || selected.Lease == nil || reason != leaseBlockNone {
				t.Fatalf("%s acquire lease=%v reason=%s err=%v", name, selected.Lease, reason, err)
			}
			counts[selected.ChoiceKey]++
			leases = append(leases, selected.Lease)
		}
		if counts["left"] != wantLeft || counts["right"] != wantRight {
			t.Fatalf("%s distribution=%v want left=%d right=%d", name, counts, wantLeft, wantRight)
		}
		for _, lease := range leases {
			if err := lease.Release(context.Background()); err != nil {
				t.Fatalf("%s release: %v", name, err)
			}
		}
	}

	assertHeldDistribution("equal", makeCandidates(2, 2), 1000, 500, 500)
	assertHeldDistribution("five-to-two-first-wave", makeCandidates(5, 2), 7, 5, 2)
}

func TestLocalLeaseCoordinatorAcrossBarrierConcurrentFairness(t *testing.T) {
	coordinator := newLocalLeaseCoordinator()
	candidates := []LeaseCandidateRequest{
		{ChoiceKey: "left", Request: LeaseRequest{AccountID: "left-0", Resources: []LeaseResource{{ID: "left-egress"}}}},
		{ChoiceKey: "left", Request: LeaseRequest{AccountID: "left-1", Resources: []LeaseResource{{ID: "left-egress"}}}},
		{ChoiceKey: "right", Request: LeaseRequest{AccountID: "right-0", Resources: []LeaseResource{{ID: "right-egress"}}}},
		{ChoiceKey: "right", Request: LeaseRequest{AccountID: "right-1", Resources: []LeaseResource{{ID: "right-egress"}}}},
	}
	const requests = 1000
	start := make(chan struct{})
	results := make(chan CoordinatedLeaseSelection, requests)
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			selection, reason, err := coordinator.TryAcquireAcross(context.Background(), candidates)
			if err != nil {
				errs <- err
				return
			}
			if selection.Lease == nil || reason != leaseBlockNone {
				errs <- fmt.Errorf("selection=%+v reason=%s", selection, reason)
				return
			}
			results <- selection
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	counts := map[string]int{}
	leases := make([]CoordinatedLease, 0, requests)
	for selection := range results {
		counts[selection.ChoiceKey]++
		leases = append(leases, selection.Lease)
	}
	if delta := counts["left"] - counts["right"]; delta < -1 || delta > 1 {
		t.Fatalf("barrier-concurrent distribution=%v delta=%d", counts, delta)
	}
	for _, lease := range leases {
		if err := lease.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalLeaseCoordinatorAcrossFallsBackFromBlockedTarget(t *testing.T) {
	coordinator := newLocalLeaseCoordinator()
	blockedResource := "blocked-egress"
	held, reason, err := coordinator.TryAcquire(context.Background(), LeaseRequest{
		AccountID: "occupant", Resources: []LeaseResource{{ID: blockedResource, Limit: 1}},
	})
	if err != nil || held == nil || reason != leaseBlockNone {
		t.Fatalf("seed lease=%v reason=%s err=%v", held, reason, err)
	}
	defer held.Release(context.Background())

	selected, reason, err := coordinator.TryAcquireAcross(context.Background(), []LeaseCandidateRequest{
		{ChoiceKey: "blocked", Request: LeaseRequest{AccountID: "blocked-account", Resources: []LeaseResource{{ID: blockedResource, Limit: 1}}}},
		{ChoiceKey: "healthy", Request: LeaseRequest{AccountID: "healthy-account", Resources: []LeaseResource{{ID: "healthy-egress", Limit: 1}}}},
	})
	if err != nil || selected.Lease == nil || reason != leaseBlockNone || selected.ChoiceKey != "healthy" {
		t.Fatalf("fallback selection=%+v reason=%s err=%v", selected, reason, err)
	}
	_ = selected.Lease.Release(context.Background())
}

func TestRedisLeaseCoordinatorAtomicAcrossNodes(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	firstCoordinator, err := newRedisLeaseCoordinator(rawURL, "integration-node-a")
	if err != nil {
		t.Fatalf("first coordinator: %v", err)
	}
	defer firstCoordinator.Close()
	secondCoordinator, err := newRedisLeaseCoordinator(rawURL, "integration-node-b")
	if err != nil {
		t.Fatalf("second coordinator: %v", err)
	}
	defer secondCoordinator.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	request := LeaseRequest{
		AccountID: "redis-account-" + suffix, EstimatedTokens: 60, TokenBudget: 100,
		Resources: []LeaseResource{{ID: "redis-egress-" + suffix, Limit: 1}}, TTL: time.Minute,
	}
	first, reason, err := firstCoordinator.TryAcquire(context.Background(), request)
	if err != nil || first == nil || reason != leaseBlockNone {
		t.Fatalf("first lease=%v reason=%s err=%v", first, reason, err)
	}
	if lease, reason, err := secondCoordinator.TryAcquire(context.Background(), request); err != nil || lease != nil || reason != leaseBlockConcurrency {
		t.Fatalf("cross-node gate lease=%v reason=%s err=%v", lease, reason, err)
	}
	if err = first.Release(context.Background()); err != nil {
		t.Fatalf("release first: %v", err)
	}
	second, reason, err := secondCoordinator.TryAcquire(context.Background(), request)
	if err != nil || second == nil || reason != leaseBlockNone || second.FencingToken() <= first.FencingToken() {
		t.Fatalf("second lease=%v reason=%s first_fence=%d err=%v", second, reason, first.FencingToken(), err)
	}
	if err = second.Release(context.Background()); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

func TestRedisLeaseCoordinatorAcrossNodesIsGloballyFair(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	firstCoordinator, err := newRedisLeaseCoordinator(rawURL, "across-fair-node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer firstCoordinator.Close()
	secondCoordinator, err := newRedisLeaseCoordinator(rawURL, "across-fair-node-b")
	if err != nil {
		t.Fatal(err)
	}
	defer secondCoordinator.Close()

	suffix := fmt.Sprint(time.Now().UnixNano())
	candidates := make([]LeaseCandidateRequest, 0, 4)
	for _, choice := range []string{"left", "right"} {
		for index := range 2 {
			id := fmt.Sprintf("redis-across-%s-%s-%d", suffix, choice, index)
			candidates = append(candidates, LeaseCandidateRequest{
				ChoiceKey: choice,
				Request:   LeaseRequest{AccountID: id, Resources: []LeaseResource{{ID: "egress-" + id, Limit: 1000}}, TTL: time.Minute},
			})
		}
	}

	counts := map[string]int{}
	leases := make([]CoordinatedLease, 0, 1000)
	for index := range 1000 {
		coordinator := firstCoordinator
		if index%2 != 0 {
			coordinator = secondCoordinator
		}
		selected, reason, acquireErr := coordinator.TryAcquireAcross(context.Background(), candidates)
		if acquireErr != nil || selected.Lease == nil || reason != leaseBlockNone {
			t.Fatalf("acquire %d selection=%+v reason=%s err=%v", index, selected, reason, acquireErr)
		}
		counts[selected.ChoiceKey]++
		leases = append(leases, selected.Lease)
	}
	left, right := float64(counts["left"]), float64(counts["right"])
	jain := (left + right) * (left + right) / (2 * (left*left + right*right))
	if jain < 0.99 || absInt(counts["left"]-counts["right"]) > 1 {
		t.Fatalf("distribution=%v Jain=%f", counts, jain)
	}
	for _, lease := range leases {
		if releaseErr := lease.Release(context.Background()); releaseErr != nil {
			t.Fatal(releaseErr)
		}
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func TestRedisLeaseCoordinatorFailsClosedAfterEpochLoss(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	coordinator, err := newRedisLeaseCoordinator(rawURL, "epoch-loss-node")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	request := LeaseRequest{AccountID: "epoch-loss-account", Resources: []LeaseResource{{ID: "epoch-loss-egress", Limit: 1}}, TTL: time.Minute}
	lease, reason, err := coordinator.TryAcquire(context.Background(), request)
	if err != nil || lease == nil || reason != leaseBlockNone {
		t.Fatalf("lease=%v reason=%s err=%v", lease, reason, err)
	}
	if err = coordinator.client.Del(context.Background(), coordinator.epochKey).Err(); err != nil {
		t.Fatal(err)
	}
	if next, blocked, acquireErr := coordinator.TryAcquire(context.Background(), request); next != nil || blocked != leaseBlockCoordinator || !errors.Is(acquireErr, ErrLeaseCoordinatorUnavailable) {
		t.Fatalf("post-loss lease=%v reason=%s err=%v", next, blocked, acquireErr)
	}
	if releaseErr := lease.Release(context.Background()); !errors.Is(releaseErr, ErrLeaseCoordinatorUnavailable) {
		t.Fatalf("stale release err=%v", releaseErr)
	}
	if err = coordinator.client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisLeaseCoordinatorSurvivesDurableRestart(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	container := strings.TrimSpace(os.Getenv("TEST_REDIS_RESTART_CONTAINER"))
	if rawURL == "" || container == "" {
		t.Skip("TEST_REDIS_URL and TEST_REDIS_RESTART_CONTAINER are required")
	}
	firstCoordinator, err := newRedisLeaseCoordinator(rawURL, "restart-node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer firstCoordinator.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	request := LeaseRequest{AccountID: "restart-account-" + suffix, Resources: []LeaseResource{{ID: "restart-egress-" + suffix, Limit: 1}}, TTL: time.Minute}
	first, reason, err := firstCoordinator.TryAcquire(context.Background(), request)
	if err != nil || first == nil || reason != leaseBlockNone {
		t.Fatalf("first lease=%v reason=%s err=%v", first, reason, err)
	}
	if output, restartErr := exec.Command("docker", "restart", container).CombinedOutput(); restartErr != nil {
		t.Fatalf("restart Redis: %v output=%s", restartErr, output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		pingErr := firstCoordinator.client.Ping(pingCtx).Err()
		cancel()
		if pingErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Redis did not recover: %v", pingErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	secondCoordinator, err := newRedisLeaseCoordinator(rawURL, "restart-node-b")
	if err != nil {
		t.Fatal(err)
	}
	defer secondCoordinator.Close()
	if lease, blocked, acquireErr := secondCoordinator.TryAcquire(context.Background(), request); acquireErr != nil || lease != nil || blocked != leaseBlockConcurrency {
		t.Fatalf("restart lost live lease: lease=%v reason=%s err=%v", lease, blocked, acquireErr)
	}
	if err = first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, reason, err := secondCoordinator.TryAcquire(context.Background(), request)
	if err != nil || second == nil || reason != leaseBlockNone || second.FencingToken() <= first.FencingToken() {
		t.Fatalf("second lease=%v reason=%s first_fence=%d err=%v", second, reason, first.FencingToken(), err)
	}
	_ = second.Release(context.Background())
}

func TestRedisLeaseExpiresAfterNodeExit(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	firstCoordinator, err := newRedisLeaseCoordinator(rawURL, "exit-node-a")
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	request := LeaseRequest{AccountID: "exit-account-" + suffix, Resources: []LeaseResource{{ID: "exit-egress-" + suffix, Limit: 1}}, TTL: 300 * time.Millisecond}
	first, reason, err := firstCoordinator.TryAcquire(context.Background(), request)
	if err != nil || first == nil || reason != leaseBlockNone {
		t.Fatalf("first lease=%v reason=%s err=%v", first, reason, err)
	}
	firstFence := first.FencingToken()
	if err = firstCoordinator.Close(); err != nil {
		t.Fatal(err)
	}
	secondCoordinator, err := newRedisLeaseCoordinator(rawURL, "exit-node-b")
	if err != nil {
		t.Fatal(err)
	}
	defer secondCoordinator.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		second, blocked, acquireErr := secondCoordinator.TryAcquire(context.Background(), request)
		if acquireErr == nil && second != nil {
			if blocked != leaseBlockNone || second.FencingToken() <= firstFence {
				t.Fatalf("post-expiry lease=%v reason=%s first_fence=%d", second, blocked, firstFence)
			}
			_ = second.Release(context.Background())
			break
		}
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire after node exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRedisLeaseRenewalKeepsLiveNodeFenced(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("TEST_REDIS_URL"))
	if rawURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	firstCoordinator, err := newRedisLeaseCoordinator(rawURL, "renew-node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer firstCoordinator.Close()
	secondCoordinator, err := newRedisLeaseCoordinator(rawURL, "renew-node-b")
	if err != nil {
		t.Fatal(err)
	}
	defer secondCoordinator.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	request := LeaseRequest{AccountID: "renew-account-" + suffix, Resources: []LeaseResource{{ID: "renew-egress-" + suffix, Limit: 1}}, TTL: 600 * time.Millisecond}
	first, reason, err := firstCoordinator.TryAcquire(context.Background(), request)
	if err != nil || first == nil || reason != leaseBlockNone {
		t.Fatalf("first lease=%v reason=%s err=%v", first, reason, err)
	}
	time.Sleep(1300 * time.Millisecond)
	if second, blocked, acquireErr := secondCoordinator.TryAcquire(context.Background(), request); acquireErr != nil || second != nil || blocked != leaseBlockConcurrency {
		t.Fatalf("renewal lost live lease: lease=%v reason=%s err=%v", second, blocked, acquireErr)
	}
	if err = first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}
