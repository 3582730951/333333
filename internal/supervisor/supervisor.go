package supervisor

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

const (
	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 30 * time.Second
	defaultResetAfter     = time.Minute
)

// Logf matches log.Printf. It is injectable so tests can assert on supervisor output
// without redirecting the global logger.
type Logf func(format string, args ...any)

// Options controls restart behavior for a long-running background module.
type Options struct {
	Name           string
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	ResetAfter     time.Duration
	Logf           Logf
}

// Restartable owns one generation of a supervised module at a time. Calling
// Start cancels the previous generation and waits for its supervisor boundary to
// finish before launching the replacement. This is intended for active-role
// workers whose parent context is replaced after an A/B demotion/promotion.
//
// A plain sync.Once is incorrect for those workers: the first demotion cancels
// the only generation permanently. Starting two independent supervisor.Go loops
// is also unsafe because the retiring generation can publish "stopped" after the
// replacement has already published "running". Restartable serializes that
// hand-off without blocking the caller.
type Restartable struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// Start replaces the currently owned generation, if any.
func (r *Restartable) Start(ctx context.Context, opts Options, run func(context.Context)) {
	if r == nil || run == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts = normalizeOptions(opts)
	childCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	previousCancel, previousDone := r.cancel, r.done
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	r.mu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	go func() {
		defer RecoverWithLogf(opts.Name+"-restartable-launch", opts.Logf)
		defer close(done)
		if previousDone != nil {
			<-previousDone
		}
		if childCtx.Err() != nil {
			return
		}
		runSupervised(childCtx, opts, run)
	}()
}

// Go runs a long-lived background module and restarts it when it panics or returns
// before ctx is cancelled. The restart is delayed with a bounded backoff so a broken
// module cannot spin in a tight crash loop.
func Go(ctx context.Context, name string, run func(context.Context)) {
	GoWithOptions(ctx, Options{Name: name}, run)
}

// GoWithOptions is Go with explicit restart/backoff settings.
func GoWithOptions(ctx context.Context, opts Options, run func(context.Context)) {
	opts = normalizeOptions(opts)
	go runSupervised(ctx, opts, run)
}

func runSupervised(ctx context.Context, opts Options, run func(context.Context)) {
	defer func() {
		if v := recover(); v != nil {
			LogPanicWithLogf(opts.Name, fmt.Sprintf("supervisor loop panic: %v", v), opts.Logf)
		}
	}()
	supervise(ctx, opts, run)
}

// GoOnce runs a one-shot goroutine and logs any panic with module context. Use this
// for short coordination tasks that should not be restarted but still must not crash
// the process silently.
func GoOnce(name string, run func()) {
	GoOnceWithLogf(name, log.Printf, run)
}

// GoOnceWithLogf is GoOnce with an injectable logger for tests.
func GoOnceWithLogf(name string, logf Logf, run func()) {
	if run == nil {
		return
	}
	go func() {
		defer RecoverWithLogf(name, logf)
		run()
	}()
}

// Recover logs a panic with a stack trace. Use it as `defer supervisor.Recover(name)`
// at goroutine or callback boundaries that should isolate a single unit of work rather
// than restart a whole loop.
func Recover(name string) {
	if v := recover(); v != nil {
		LogPanic(name, v)
	}
}

// RecoverEvent is Recover with structured operation/request metadata. It is the
// preferred panic boundary for newly added callbacks, goroutines, and handlers.
func RecoverEvent(event Event) {
	if v := recover(); v != nil {
		LogPanicEvent(event, v)
	}
}

// RecoverWithLogf is Recover with an injectable logger for tests.
func RecoverWithLogf(name string, logf Logf) {
	if v := recover(); v != nil {
		LogPanicWithLogf(name, v, logf)
	}
}

// LogPanic logs an already-recovered panic value with a stack trace.
func LogPanic(name string, panicVal any) {
	LogPanicWithLogf(name, panicVal, log.Printf)
}

// LogPanicWithLogf is LogPanic with an injectable logger for tests.
func LogPanicWithLogf(name string, panicVal any, logf Logf) {
	LogPanicEventWithLogf(Event{Module: name}, panicVal, logf)
}

// LogPanicEvent records and logs an already-recovered panic with structured
// metadata suitable for durable diagnostics.
func LogPanicEvent(event Event, panicVal any) {
	LogPanicEventWithLogf(event, panicVal, log.Printf)
}

// LogPanicEventWithLogf is LogPanicEvent with an injectable logger for tests.
func LogPanicEventWithLogf(event Event, panicVal any, logf Logf) {
	opts := normalizeOptions(Options{Name: event.Module, Logf: logf})
	event.Module = opts.Name
	recordPanicEvent(event, panicVal)
	opts.Logf("[SUPERVISOR] module=%s panic=%v\n%s", opts.Name, panicVal, debug.Stack())
}

func normalizeOptions(opts Options) Options {
	if opts.Name == "" {
		opts.Name = "background"
	}
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = defaultInitialBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.MaxBackoff < opts.InitialBackoff {
		opts.MaxBackoff = opts.InitialBackoff
	}
	if opts.ResetAfter <= 0 {
		opts.ResetAfter = defaultResetAfter
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	return opts
}

type runResult struct {
	panicked bool
	panicVal any
	stack    []byte
}

// GoUntilSuccess runs a finite background task until it returns nil. Errors and
// panics are retried with the same bounded backoff used by long-lived modules,
// while a successful return is terminal rather than an "unexpected exit".
func GoUntilSuccess(ctx context.Context, name string, run func(context.Context) error) {
	GoUntilSuccessWithOptions(ctx, Options{Name: name}, run)
}

// GoUntilSuccessWithOptions is GoUntilSuccess with explicit retry/backoff
// settings. It is suitable for idempotent migrations and other finite startup
// work that must survive a transient database lock but must not restart forever
// after it has completed.
func GoUntilSuccessWithOptions(ctx context.Context, opts Options, run func(context.Context) error) {
	opts = normalizeOptions(opts)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				LogPanicWithLogf(opts.Name, fmt.Sprintf("supervisor task loop panic: %v", v), opts.Logf)
			}
		}()
		retryUntilSuccess(ctx, opts, run)
	}()
}

type taskRunResult struct {
	err      error
	panicked bool
	panicVal any
	stack    []byte
}

func retryUntilSuccess(ctx context.Context, opts Options, run func(context.Context) error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil {
		markModuleCompleted(opts.Name)
		return
	}
	backoff := opts.InitialBackoff
	for {
		if ctx.Err() != nil {
			markModuleStopped(opts.Name)
			return
		}

		started := time.Now()
		markModuleStarted(opts.Name)
		result := runTaskProtected(ctx, run)
		if !result.panicked && result.err == nil {
			markModuleCompleted(opts.Name)
			return
		}
		if ctx.Err() != nil {
			markModuleStopped(opts.Name)
			return
		}

		uptime := time.Since(started).Round(time.Millisecond)
		event := Event{
			Module:        opts.Name,
			UptimeMillis:  uptime.Milliseconds(),
			BackoffMillis: backoff.Milliseconds(),
		}
		if result.panicked {
			event.Type = "panic_restart"
			event.Severity = "error"
			event.Operation = "task_run"
			event.ErrorClass = errorClass(result.panicVal)
			event.Recovered = true
			event.Message = "task panic; retrying"
			event.Panic = fmt.Sprint(result.panicVal)
			opts.Logf("[SUPERVISOR] module=%s panic=%v uptime=%s; retrying after %s\n%s",
				opts.Name, result.panicVal, uptime, backoff, result.stack)
		} else {
			event.Type = "error_retry"
			event.Severity = "warning"
			event.Operation = "task_run"
			event.ErrorClass = errorClass(result.err)
			event.Message = fmt.Sprintf("task failed; retrying: %v", result.err)
			opts.Logf("[SUPERVISOR] module=%s error=%v uptime=%s; retrying after %s",
				opts.Name, result.err, uptime, backoff)
		}
		recordEvent(event)
		markModuleRestarting(opts.Name, event)

		if !sleepContext(ctx, backoff) {
			markModuleStopped(opts.Name)
			return
		}
		if uptime >= opts.ResetAfter {
			backoff = opts.InitialBackoff
		} else {
			backoff = nextBackoff(backoff, opts.MaxBackoff)
		}
	}
}

func runTaskProtected(ctx context.Context, run func(context.Context) error) (result taskRunResult) {
	defer func() {
		if v := recover(); v != nil {
			result.panicked = true
			result.panicVal = v
			result.stack = debug.Stack()
		}
	}()
	result.err = run(ctx)
	return result
}

func supervise(ctx context.Context, opts Options, run func(context.Context)) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer markModuleStopped(opts.Name)
	backoff := opts.InitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		markModuleStarted(opts.Name)
		result := runProtected(ctx, run)
		if ctx.Err() != nil {
			return
		}

		uptime := time.Since(started).Round(time.Millisecond)
		if result.panicked {
			event := Event{
				Type:          "panic_restart",
				Severity:      "error",
				Module:        opts.Name,
				Operation:     "module_run",
				ErrorClass:    errorClass(result.panicVal),
				Recovered:     true,
				Message:       "module panic; restarting",
				Panic:         fmt.Sprint(result.panicVal),
				UptimeMillis:  uptime.Milliseconds(),
				BackoffMillis: backoff.Milliseconds(),
			}
			recordEvent(event)
			markModuleRestarting(opts.Name, event)
			opts.Logf("[SUPERVISOR] module=%s panic=%v uptime=%s; restarting after %s\n%s",
				opts.Name, result.panicVal, uptime, backoff, result.stack)
		} else {
			event := Event{
				Type:          "unexpected_exit",
				Severity:      "warning",
				Module:        opts.Name,
				Operation:     "module_run",
				Message:       "module exited unexpectedly; restarting",
				UptimeMillis:  uptime.Milliseconds(),
				BackoffMillis: backoff.Milliseconds(),
			}
			recordEvent(event)
			markModuleRestarting(opts.Name, event)
			opts.Logf("[SUPERVISOR] module=%s exited unexpectedly uptime=%s; restarting after %s",
				opts.Name, uptime, backoff)
		}

		if !sleepContext(ctx, backoff) {
			return
		}
		if uptime >= opts.ResetAfter {
			backoff = opts.InitialBackoff
		} else {
			backoff = nextBackoff(backoff, opts.MaxBackoff)
		}
	}
}

func runProtected(ctx context.Context, run func(context.Context)) (result runResult) {
	defer func() {
		if v := recover(); v != nil {
			result.panicked = true
			result.panicVal = v
			result.stack = debug.Stack()
		}
	}()
	run(ctx)
	return result
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next < current || next > max {
		return max
	}
	return next
}
