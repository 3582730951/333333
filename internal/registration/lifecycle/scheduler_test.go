package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

type recordingLifecycleChecker struct {
	batches       []int
	concurrencies []int
}

func (c *recordingLifecycleChecker) CheckAccount(context.Context, storage.Account) HealthStatus {
	return HealthStatus{Alive: true}
}

func (c *recordingLifecycleChecker) BatchCheckAccountsN(_ context.Context, accounts []storage.Account, concurrency int) []HealthStatus {
	c.batches = append(c.batches, len(accounts))
	c.concurrencies = append(c.concurrencies, concurrency)
	results := make([]HealthStatus, len(accounts))
	for index := range results {
		results[index].Alive = true
	}
	return results
}

func TestSchedulerStartIgnoresCancelledContextAndInvalidInterval(t *testing.T) {
	s := NewScheduler(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.Start(ctx, 0)

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if running {
		t.Fatal("scheduler is running after Start with a cancelled context")
	}
}

func TestSchedulerCanStopAndRestart(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := NewScheduler(store)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	done1 := make(chan struct{})
	go func() {
		s.Start(ctx1, 10*time.Millisecond)
		close(done1)
	}()
	waitForSchedulerRunning(t, s)

	s.Stop()
	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		s.Start(ctx2, 10*time.Millisecond)
		close(done2)
	}()
	waitForSchedulerRunning(t, s)
	cancel2()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit after restarted context cancellation")
	}

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if running {
		t.Fatal("scheduler running flag was not reset after context cancellation")
	}
}

func TestSchedulerSurvivesHealthCyclePanic(t *testing.T) {
	s := NewScheduler(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Start(ctx, time.Hour)
		close(done)
	}()

	waitForSchedulerRunning(t, s)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit after recovered health-check panic")
	}
}

func TestHealthCyclePagesAccountsBeforeChecking(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := 0; index < 53; index++ {
		id := fmt.Sprintf("account-%03d", index)
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO accounts(
  id,label,upstream_account_id,chatgpt_user_id,email,plan_type,provider,status,created_at,updated_at
) VALUES(?,?,'','','','','codex','active',?,?)`,
			id, id, int64(index+1), int64(index+1)); err != nil {
			t.Fatal(err)
		}
	}

	checker := &recordingLifecycleChecker{}
	scheduler := NewScheduler(store)
	scheduler.checker = checker
	scheduler.batchSize = 17
	scheduler.concurrency = 4
	scheduler.runHealthCheckCycle(ctx)

	want := []int{17, 17, 17, 2}
	if len(checker.batches) != len(want) {
		t.Fatalf("batch sizes=%v want=%v", checker.batches, want)
	}
	for index := range want {
		if checker.batches[index] != want[index] || checker.concurrencies[index] != 4 {
			t.Fatalf("batch sizes=%v concurrency=%v", checker.batches, checker.concurrencies)
		}
	}
	stats := scheduler.GetStatistics()
	if stats.TotalChecks != 53 || stats.AliveCount != 53 || stats.DeadCount != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func waitForSchedulerRunning(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("scheduler did not enter running state")
		case <-ticker.C:
			s.mu.RLock()
			running := s.running
			s.mu.RUnlock()
			if running {
				return
			}
		}
	}
}
