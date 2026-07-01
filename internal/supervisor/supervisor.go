package supervisor

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
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

// Go runs a long-lived background module and restarts it when it panics or returns
// before ctx is cancelled. The restart is delayed with a bounded backoff so a broken
// module cannot spin in a tight crash loop.
func Go(ctx context.Context, name string, run func(context.Context)) {
	GoWithOptions(ctx, Options{Name: name}, run)
}

// GoWithOptions is Go with explicit restart/backoff settings.
func GoWithOptions(ctx context.Context, opts Options, run func(context.Context)) {
	opts = normalizeOptions(opts)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				LogPanicWithLogf(opts.Name, fmt.Sprintf("supervisor loop panic: %v", v), opts.Logf)
			}
		}()
		supervise(ctx, opts, run)
	}()
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
	opts := normalizeOptions(Options{Name: name, Logf: logf})
	recordPanicEvent(opts.Name, panicVal)
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
				Module:        opts.Name,
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
				Module:        opts.Name,
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
