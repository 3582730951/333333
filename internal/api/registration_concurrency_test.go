package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/storage"
)

// TestRunBoundedConcurrency verifies the registration worker pool never exceeds the
// limit, runs every task exactly once, and reports cancellation.
func TestRunBoundedConcurrency(t *testing.T) {
	const n, limit = 50, 4
	var (
		running   int32
		peak      int32
		completed int32
	)
	cancelled := runBounded(context.Background(), n, limit, func(i int) {
		cur := atomic.AddInt32(&running, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		atomic.AddInt32(&completed, 1)
	})
	if cancelled {
		t.Fatal("should not be cancelled")
	}
	if got := atomic.LoadInt32(&completed); got != n {
		t.Fatalf("completed = %d, want %d", got, n)
	}
	if got := atomic.LoadInt32(&peak); got > limit {
		t.Fatalf("peak concurrency = %d, exceeds limit %d", got, limit)
	}
	if atomic.LoadInt32(&peak) < 2 {
		t.Fatalf("peak concurrency = %d; expected real parallelism (>1)", atomic.LoadInt32(&peak))
	}
}

func TestRunBoundedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started int32
	var mu sync.Mutex
	cancel() // cancelled before any task launches
	cancelled := runBounded(ctx, 10, 3, func(i int) {
		mu.Lock()
		started++
		mu.Unlock()
	})
	if !cancelled {
		t.Fatal("want cancelled=true when ctx is already cancelled")
	}
	if atomic.LoadInt32(&started) != 0 {
		t.Fatalf("no tasks should start after cancellation, got %d", started)
	}
}

func TestRunBoundedLimitFloor(t *testing.T) {
	// limit<1 is treated as 1 (sequential), still runs all tasks.
	var c int32
	runBounded(context.Background(), 5, 0, func(i int) { atomic.AddInt32(&c, 1) })
	if c != 5 {
		t.Fatalf("ran %d tasks, want 5", c)
	}
}

func TestProcessBatchRecordsWorkerPanicAsFailure(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	const jobID = "job_worker_panic"
	now := time.Now().Unix()
	if _, err := store.DB().ExecContext(context.Background(),
		`INSERT INTO registration_jobs (id, platform, method, total, status, config_json, created_at, updated_at)
		 VALUES (?, 'chatgpt', 'node', 1, 'pending', '{}', ?, ?)`,
		jobID, now, now); err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		store:         store,
		defaultMethod: "node",
		concurrency:   1,
		jobCancels:    map[string]context.CancelFunc{},
	}
	h.processBatch(context.Background(), jobID, pipeline.RegisterRequest{
		Platform: "chatgpt",
		Method:   "node",
		Count:    1,
	})

	var status string
	var succeeded, failed int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT status, succeeded, failed FROM registration_jobs WHERE id=?`, jobID,
	).Scan(&status, &succeeded, &failed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || succeeded != 0 || failed != 1 {
		t.Fatalf("job status=%s succeeded=%d failed=%d, want failed 0 1", status, succeeded, failed)
	}

	var recordStatus, recordError string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT status, error FROM registration_records WHERE job_id=?`, jobID,
	).Scan(&recordStatus, &recordError); err != nil {
		t.Fatal(err)
	}
	if recordStatus != "failed" || recordError == "" {
		t.Fatalf("record status=%s error=%q, want failed with panic error", recordStatus, recordError)
	}
}
