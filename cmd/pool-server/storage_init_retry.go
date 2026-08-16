package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

const (
	// The installer gives a staged worker 180 seconds to reach /standbyz. Keep
	// lock recovery inside that envelope so encryption validation, socket bind,
	// and the readiness probe still have a full minute to complete.
	defaultStorageInitLockWait     = 2 * time.Minute
	defaultStorageInitRetryInitial = 250 * time.Millisecond
	defaultStorageInitRetryMaximum = 2 * time.Second
)

type storageInitRetryPolicy struct {
	MaxWait        time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

var defaultStorageInitRetryPolicy = storageInitRetryPolicy{
	MaxWait:        defaultStorageInitLockWait,
	InitialBackoff: defaultStorageInitRetryInitial,
	MaxBackoff:     defaultStorageInitRetryMaximum,
}

type storageInitializer func(context.Context, func(string)) error

// initStorageWithLockRetry keeps a staged SQLite worker alive while the active
// generation finishes a bounded write transaction. Retrying the complete init is
// safe because every startup schema and data migration is additive and idempotent.
// Non-lock failures are returned immediately; a genuinely wedged database still
// fails before the installer's readiness deadline and leaves the active release
// untouched.
func initStorageWithLockRetry(
	ctx context.Context,
	init storageInitializer,
	progress func(string),
	logf func(string, ...any),
) error {
	return initStorageWithLockRetryPolicy(ctx, init, progress, logf, defaultStorageInitRetryPolicy)
}

func initStorageWithLockRetryPolicy(
	ctx context.Context,
	init storageInitializer,
	progress func(string),
	logf func(string, ...any),
	policy storageInitRetryPolicy,
) error {
	if init == nil {
		return fmt.Errorf("storage initializer is nil")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if policy.MaxWait <= 0 {
		policy.MaxWait = defaultStorageInitLockWait
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = defaultStorageInitRetryInitial
	}
	if policy.MaxBackoff < policy.InitialBackoff {
		policy.MaxBackoff = policy.InitialBackoff
	}

	started := time.Now()
	backoff := policy.InitialBackoff
	attempt := 0
	for {
		attempt++
		err := init(ctx, progress)
		if err == nil {
			if attempt > 1 {
				logf("startup: storage lock cleared after %d attempts and %s", attempt, time.Since(started).Round(time.Millisecond))
			}
			return nil
		}
		if !isSQLiteLockError(err) {
			return err
		}

		elapsed := time.Since(started)
		remaining := policy.MaxWait - elapsed
		if remaining <= 0 {
			return fmt.Errorf("storage remained locked for %s after %d attempts: %w", elapsed.Round(time.Millisecond), attempt, err)
		}
		delay := backoff
		if delay > remaining {
			delay = remaining
		}
		logf("startup: storage is locked by another worker; retrying attempt=%d in=%s remaining=%s", attempt+1, delay.Round(time.Millisecond), remaining.Round(time.Millisecond))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < policy.MaxBackoff {
			backoff *= 2
			if backoff > policy.MaxBackoff {
				backoff = policy.MaxBackoff
			}
		}
	}
}

func isSQLiteLockError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "database schema is locked") ||
		strings.Contains(message, "database is busy") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked")
}
