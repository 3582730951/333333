package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Before the retry existed, a cleanup stage that lost one race for the single
// writer connection incremented a counter and gave up until the next maintenance
// interval five minutes later — while the rows it was meant to reclaim kept
// accumulating, which is what produces the contention that lost the race.
func TestDiskCleanupStageRetriesLockContentionThenSucceeds(t *testing.T) {
	snap := &DiskGuardSnapshot{}
	attempts := 0

	runDiskCleanupStage(context.Background(), snap, "goal_cleanup", "goal_cleanup_failed",
		time.Second, func(context.Context) error {
			attempts++
			if attempts < 3 {
				return errors.New("database is locked")
			}
			return nil
		})

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if snap.CleanupFailureEvents != 0 {
		t.Errorf("CleanupFailureEvents = %d, want 0: the stage eventually succeeded",
			snap.CleanupFailureEvents)
	}
	if snap.CleanupRetries != 2 || snap.CleanupRetrySuccesses != 1 {
		t.Errorf("retries = %d, retry successes = %d, want 2 and 1",
			snap.CleanupRetries, snap.CleanupRetrySuccesses)
	}
}

// A timeout means the stage never got the writer inside its slice. Re-entering the
// queue behind the same holder would spend another full deadline failing the same
// way, and that budget belongs to the stages that have not run yet.
func TestDiskCleanupStageDoesNotRetryATimeout(t *testing.T) {
	snap := &DiskGuardSnapshot{}
	attempts := 0

	runDiskCleanupStage(context.Background(), snap, "goal_cleanup", "goal_cleanup_failed",
		time.Second, func(context.Context) error {
			attempts++
			return context.DeadlineExceeded
		})

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: a timeout must not be retried in the same run", attempts)
	}
	if snap.CleanupFailureEvents != 1 || snap.CleanupErrorClass != "timeout" {
		t.Errorf("failure not recorded as a timeout: events=%d class=%q",
			snap.CleanupFailureEvents, snap.CleanupErrorClass)
	}
	if snap.CleanupRetries != 0 {
		t.Errorf("CleanupRetries = %d, want 0", snap.CleanupRetries)
	}
}

// A read-only or full volume is a property of the disk, not of this attempt.
func TestDiskCleanupStageDoesNotRetryPermanentFailures(t *testing.T) {
	for _, tc := range []struct{ err, class string }{
		{"attempt to write a readonly database", "readonly"},
		{"database or disk is full", "full"},
	} {
		snap := &DiskGuardSnapshot{}
		attempts := 0
		runDiskCleanupStage(context.Background(), snap, "context_cleanup", "context_cleanup_failed",
			time.Second, func(context.Context) error {
				attempts++
				return errors.New(tc.err)
			})
		if attempts != 1 {
			t.Errorf("%s: attempts = %d, want 1", tc.class, attempts)
		}
		if snap.CleanupErrorClass != tc.class {
			t.Errorf("%s: class = %q", tc.class, snap.CleanupErrorClass)
		}
	}
}

// Retries are bounded: sustained contention must not let one stage consume the
// whole maintenance budget.
func TestDiskCleanupStageGivesUpAfterBoundedAttempts(t *testing.T) {
	snap := &DiskGuardSnapshot{}
	attempts := 0

	runDiskCleanupStage(context.Background(), snap, "mapping_cleanup", "mapping_cleanup_failed",
		time.Second, func(context.Context) error {
			attempts++
			return errors.New("sqlite_busy")
		})

	if attempts != diskCleanupMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, diskCleanupMaxAttempts)
	}
	if snap.CleanupFailureEvents != 1 {
		t.Errorf("CleanupFailureEvents = %d, want exactly 1 for one exhausted stage",
			snap.CleanupFailureEvents)
	}
	if snap.CleanupErrorOperation != "mapping_cleanup" {
		t.Errorf("CleanupErrorOperation = %q", snap.CleanupErrorOperation)
	}
}

// An exhausted maintenance budget stops the stage immediately rather than starting
// another attempt it cannot finish.
func TestDiskCleanupStageSkipsWhenBudgetAlreadySpent(t *testing.T) {
	snap := &DiskGuardSnapshot{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	runDiskCleanupStage(ctx, snap, "route_binding_cleanup", "route_binding_cleanup_failed",
		time.Second, func(context.Context) error {
			attempts++
			return nil
		})

	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 once the maintenance budget is gone", attempts)
	}
	if snap.CleanupFailureEvents != 0 {
		t.Errorf("a spent budget is not a cleanup failure: events = %d", snap.CleanupFailureEvents)
	}
}

// A multi-step stage must attribute its failure to the operation that actually
// failed: a measurement failure and a reclamation failure call for different
// responses, so collapsing them under one code loses the distinction.
func TestDiskCleanupStageAttributesWrappedOperation(t *testing.T) {
	snap := &DiskGuardSnapshot{}

	runDiskCleanupStage(context.Background(), snap, "goal_budget_cleanup", "goal_budget_cleanup_failed",
		time.Second, func(context.Context) error {
			return &cleanupStageError{"goal_budget_measure", "goal_budget_measure_failed",
				context.DeadlineExceeded}
		})

	if snap.CleanupErrorOperation != "goal_budget_measure" {
		t.Errorf("CleanupErrorOperation = %q, want goal_budget_measure", snap.CleanupErrorOperation)
	}
	// Classification must still see through the wrapper.
	if snap.CleanupErrorClass != "timeout" {
		t.Errorf("CleanupErrorClass = %q, want timeout through the wrapper", snap.CleanupErrorClass)
	}
}
