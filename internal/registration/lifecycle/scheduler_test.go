package lifecycle

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

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
