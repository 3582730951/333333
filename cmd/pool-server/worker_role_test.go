package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestWorkerRoleControllerFencesAndRestartsAcrossLinkSwitch(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "role.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	workerSocket := filepath.Join(dir, "worker-release-a.sock")
	otherSocket := filepath.Join(dir, "worker-release-b.sock")
	if err := os.WriteFile(workerSocket, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherSocket, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	activeLink := filepath.Join(dir, "active-worker.sock")
	replaceWorkerLink(t, activeLink, otherSocket)

	deployment := newDeploymentHandler(nil, "release-a", workerSocket)
	var mu sync.Mutex
	var tokens []int64
	var cancellations int
	controller, err := newWorkerRoleController(store, deployment, "auto", workerSocket, "release-a:test", func(ctx context.Context, token int64) error {
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		go func() {
			<-ctx.Done()
			mu.Lock()
			cancellations++
			mu.Unlock()
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.pollInterval = 5 * time.Millisecond
	controller.leaseTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		controller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitWorkerRole(t, time.Second, func() bool { return deployment.standbyReady.Load() && !deployment.ready.Load() })
	replaceWorkerLink(t, activeLink, workerSocket)
	waitWorkerRole(t, time.Second, func() bool { return deployment.ready.Load() && deployment.fencingToken.Load() == 1 })
	replaceWorkerLink(t, activeLink, otherSocket)
	waitWorkerRole(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !deployment.ready.Load() && cancellations == 1
	})
	replaceWorkerLink(t, activeLink, workerSocket)
	waitWorkerRole(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return deployment.ready.Load() && len(tokens) == 2 && tokens[1] > tokens[0]
	})
}

func TestWorkerLinkTargetsUsesResolvedExactPath(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.sock")
	other := filepath.Join(dir, "worker.sock.extra")
	if err := os.WriteFile(worker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "active-worker.sock")
	replaceWorkerLink(t, link, other)
	if workerLinkTargets(link, worker) {
		t.Fatal("substring path was accepted as active worker")
	}
	replaceWorkerLink(t, link, worker)
	if !workerLinkTargets(link, worker) {
		t.Fatal("exact resolved worker path was not accepted")
	}
}

func TestWorkerLinkTargetsAcceptsPromotedDanglingSocket(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker-promoted.sock")
	link := filepath.Join(dir, "active-worker.sock")
	// The pre-init listener removes its socket before the promoted full worker
	// rebinds it. The atomic target is still authoritative during this interval.
	replaceWorkerLink(t, link, worker)
	if _, err := os.Stat(worker); !os.IsNotExist(err) {
		t.Fatalf("fixture socket unexpectedly exists: %v", err)
	}
	if !workerLinkTargets(link, worker) {
		t.Fatal("promoted dangling socket target was not recognized")
	}
}

func TestLeaseRenewTimeoutLeavesExpiryHeadroom(t *testing.T) {
	for _, test := range []struct {
		ttl, want time.Duration
	}{
		{100 * time.Millisecond, 50 * time.Millisecond},
		{time.Second, 500 * time.Millisecond},
		{15 * time.Second, 7500 * time.Millisecond},
		{time.Minute, 10 * time.Second},
	} {
		if got := leaseRenewTimeout(test.ttl); got != test.want {
			t.Fatalf("leaseRenewTimeout(%s)=%s want=%s", test.ttl, got, test.want)
		}
	}
}

func replaceWorkerLink(t *testing.T, link, target string) {
	t.Helper()
	next := link + ".next"
	_ = os.Remove(next)
	if err := os.Symlink(target, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatal(err)
	}
}

func waitWorkerRole(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker role transition timed out")
}
