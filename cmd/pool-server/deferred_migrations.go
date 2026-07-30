package main

import (
	"context"
	"log"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

// deferredMigrationTask serializes retries across active-role generations. A
// demoted worker's task releases the gate after its context is cancelled; a
// newly promoted generation can then finish the still-incomplete migration.
// Once one generation succeeds, later promotions do not repeat the work.
type deferredMigrationTask struct {
	mu        sync.Mutex
	gate      chan struct{}
	completed bool
	run       func(context.Context) error
	logf      supervisor.Logf
	options   supervisor.Options
}

func newDeferredMigrationTask(run func(context.Context) error, logf supervisor.Logf) *deferredMigrationTask {
	if logf == nil {
		logf = log.Printf
	}
	return &deferredMigrationTask{
		gate: make(chan struct{}, 1),
		run:  run,
		logf: logf,
		options: supervisor.Options{
			Name: "storage-deferred-migrations",
			Logf: logf,
		},
	}
}

func (t *deferredMigrationTask) Start(ctx context.Context) {
	if t == nil || t.run == nil {
		return
	}
	t.mu.Lock()
	completed := t.completed
	t.mu.Unlock()
	if completed {
		return
	}
	supervisor.GoUntilSuccessWithOptions(ctx, t.options, func(runCtx context.Context) error {
		select {
		case t.gate <- struct{}{}:
		case <-runCtx.Done():
			return runCtx.Err()
		}
		defer func() { <-t.gate }()

		t.mu.Lock()
		completed := t.completed
		t.mu.Unlock()
		if completed {
			return nil
		}

		started := time.Now()
		if err := t.run(runCtx); err != nil {
			return err
		}
		t.mu.Lock()
		t.completed = true
		t.mu.Unlock()
		t.logf("startup: deferred storage migrations completed in %s", time.Since(started).Round(time.Millisecond))
		return nil
	})
}

func (t *deferredMigrationTask) Completed() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.completed
}
