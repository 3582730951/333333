package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func configureFastDeferredMigrationRetry(task *deferredMigrationTask) {
	task.options.InitialBackoff = time.Millisecond
	task.options.MaxBackoff = time.Millisecond
	task.options.ResetAfter = time.Second
}

func TestDeferredMigrationTaskRetriesTransientFailureAndRunsOnce(t *testing.T) {
	var calls atomic.Int32
	completed := make(chan struct{})
	task := newDeferredMigrationTask(func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("database is locked")
		}
		close(completed)
		return nil
	}, func(string, ...any) {})
	configureFastDeferredMigrationRetry(task)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task.Start(ctx)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("deferred migration did not retry the transient failure")
	}
	waitForDeferredMigrationCompletion(t, task)
	task.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("completed migration calls=%d, want 2", got)
	}
}

func TestDeferredMigrationTaskRetriesPanic(t *testing.T) {
	var calls atomic.Int32
	completed := make(chan struct{})
	task := newDeferredMigrationTask(func(context.Context) error {
		if calls.Add(1) == 1 {
			panic("migration panic")
		}
		close(completed)
		return nil
	}, func(string, ...any) {})
	configureFastDeferredMigrationRetry(task)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task.Start(ctx)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("deferred migration did not retry its panic")
	}
	waitForDeferredMigrationCompletion(t, task)
	if got := calls.Load(); got != 2 {
		t.Fatalf("panic retry calls=%d, want 2", got)
	}
}

func TestDeferredMigrationTaskRestartsIncompleteWorkAfterRoleTransition(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	completed := make(chan struct{})
	task := newDeferredMigrationTask(func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		close(completed)
		return nil
	}, func(string, ...any) {})
	configureFastDeferredMigrationRetry(task)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	task.Start(firstCtx)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first active generation did not start migration")
	}
	cancelFirst()

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	task.Start(secondCtx)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("second active generation did not resume incomplete migration")
	}
	waitForDeferredMigrationCompletion(t, task)
	if !task.Completed() || calls.Load() != 2 {
		t.Fatalf("transition result completed=%t calls=%d", task.Completed(), calls.Load())
	}
}

func waitForDeferredMigrationCompletion(t *testing.T, task *deferredMigrationTask) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !task.Completed() {
		if time.Now().After(deadline) {
			t.Fatal("deferred migration callback returned without recording completion")
		}
		time.Sleep(time.Millisecond)
	}
}
