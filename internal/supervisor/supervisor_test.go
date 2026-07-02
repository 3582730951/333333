package supervisor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoWithOptionsRestartsAfterPanic(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	restarted := make(chan struct{})
	var once sync.Once
	logs := captureLogs()

	GoWithOptions(ctx, Options{
		Name:           "panic-module",
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Logf:           logs.logf,
	}, func(ctx context.Context) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		once.Do(func() { close(restarted) })
		<-ctx.Done()
	})

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("module was not restarted after panic")
	}

	got := logs.string()
	if !strings.Contains(got, "module=panic-module") || !strings.Contains(got, "panic=boom") {
		t.Fatalf("panic log missing module or panic value: %q", got)
	}
	if !strings.Contains(got, "goroutine") {
		t.Fatalf("panic log missing stack trace: %q", got)
	}

	state := moduleStateByName(t, "panic-module")
	if state.Status != StatusRunning || state.RestartCount != 1 || state.PanicCount != 1 {
		t.Fatalf("panic-module state = %#v, want running with one panic restart", state)
	}
}

func TestGoWithOptionsRestartsUnexpectedReturn(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	restarted := make(chan struct{})
	var once sync.Once
	logs := captureLogs()

	GoWithOptions(ctx, Options{
		Name:           "return-module",
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Logf:           logs.logf,
	}, func(ctx context.Context) {
		if calls.Add(1) == 1 {
			return
		}
		once.Do(func() { close(restarted) })
		<-ctx.Done()
	})

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("module was not restarted after unexpected return")
	}

	got := logs.string()
	if !strings.Contains(got, "module=return-module") || !strings.Contains(got, "exited unexpectedly") {
		t.Fatalf("return log missing restart context: %q", got)
	}

	state := moduleStateByName(t, "return-module")
	if state.Status != StatusRunning || state.RestartCount != 1 || state.UnexpectedExitCount != 1 {
		t.Fatalf("return-module state = %#v, want running with one unexpected-exit restart", state)
	}
}

func TestGoWithOptionsDoesNotStartBareSuperviseGoroutine(t *testing.T) {
	source, err := os.ReadFile("supervisor.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "go supervise(ctx, opts, run)") {
		t.Fatal("GoWithOptions must wrap supervise in a recovery boundary")
	}
	if !strings.Contains(text, "supervisor loop panic") {
		t.Fatal("GoWithOptions recovery boundary must identify supervisor loop panics")
	}
}

func TestRecoverWithLogfLogsPanic(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)
	logs := captureLogs()
	func() {
		defer RecoverWithLogf("callback", logs.logf)
		panic("bad write")
	}()

	got := logs.string()
	if !strings.Contains(got, "module=callback") || !strings.Contains(got, "panic=bad write") {
		t.Fatalf("recover log missing panic context: %q", got)
	}
	if !strings.Contains(got, "goroutine") {
		t.Fatalf("recover log missing stack trace: %q", got)
	}

	state := moduleStateByName(t, "callback")
	if state.Status != StatusPanic || state.PanicCount != 1 || state.LastPanic != "bad write" {
		t.Fatalf("callback state = %#v, want panic state", state)
	}
}

func TestRecoverDirectDeferSwallowsPanic(t *testing.T) {
	func() {
		defer Recover("direct-callback")
		panic("direct bad write")
	}()
}

func TestGoOnceWithLogfLogsPanic(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)
	logs := captureLogs()
	logged := make(chan struct{})
	var once sync.Once

	GoOnceWithLogf("once-task", func(format string, args ...any) {
		logs.logf(format, args...)
		once.Do(func() { close(logged) })
	}, func() {
		panic("once boom")
	})

	select {
	case <-logged:
	case <-time.After(time.Second):
		t.Fatal("GoOnce panic was not logged")
	}

	got := logs.string()
	if !strings.Contains(got, "module=once-task") || !strings.Contains(got, "panic=once boom") {
		t.Fatalf("GoOnce log missing panic context: %q", got)
	}
	if !strings.Contains(got, "goroutine") {
		t.Fatalf("GoOnce log missing stack trace: %q", got)
	}

	events := RecentEvents()
	if len(events) != 1 {
		t.Fatalf("recent events = %d, want 1", len(events))
	}
	if events[0].Module != "once-task" || events[0].Type != "panic" || events[0].Panic != "once boom" {
		t.Fatalf("recent event = %#v, want once-task panic", events[0])
	}

	state := moduleStateByName(t, "once-task")
	if state.Status != StatusPanic || state.PanicCount != 1 || state.LastPanic != "once boom" {
		t.Fatalf("once-task state = %#v, want panic state", state)
	}
}

func TestRecentEventsNewestFirstAndBounded(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)

	for i := 0; i < recentEventLimit+5; i++ {
		recordEvent(Event{Type: "test", Module: fmt.Sprintf("module-%03d", i)})
	}
	events := RecentEvents()
	if len(events) != recentEventLimit {
		t.Fatalf("recent events = %d, want %d", len(events), recentEventLimit)
	}
	if events[0].Module != "module-104" {
		t.Fatalf("newest event = %s, want module-104", events[0].Module)
	}
	if events[len(events)-1].Module != "module-005" {
		t.Fatalf("oldest retained event = %s, want module-005", events[len(events)-1].Module)
	}
}

func TestModuleStatesSortedByHealthPriority(t *testing.T) {
	clearModuleStatesForTest()
	t.Cleanup(clearModuleStatesForTest)

	markModuleStarted("healthy")
	markModuleStopped("old-stopped")
	markModulePanic("panic-now", "bad")
	ModuleFailed("failed-now", fmt.Errorf("bind failed"))
	markModuleRestarting("restart-now", Event{Type: "unexpected_exit", Message: "restart", UptimeMillis: 2500, BackoffMillis: 1000})

	states := ModuleStates()
	if len(states) != 5 {
		t.Fatalf("states = %d, want 5: %#v", len(states), states)
	}
	first := map[string]bool{states[0].Name: true, states[1].Name: true, states[2].Name: true}
	if !first["restart-now"] || !first["panic-now"] || !first["failed-now"] {
		t.Fatalf("first states = %#v, want restarting/panic/failed before stopped/running (all=%#v)", first, states)
	}

	restarting := moduleStateByName(t, "restart-now")
	if restarting.LastUptimeMillis != 2500 || restarting.RestartBackoffMillis != 1000 || restarting.NextRestartUnix == 0 {
		t.Fatalf("restart diagnostics = %#v, want last uptime and backoff", restarting)
	}
}

func TestManualModuleFailedRecordsEventAndState(t *testing.T) {
	clearRecentEventsForTest()
	clearModuleStatesForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearModuleStatesForTest)

	ModuleStarted("http-server")
	ModuleFailed("http-server", fmt.Errorf("listen tcp: bind failed"))

	state := moduleStateByName(t, "http-server")
	if state.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", state.Status)
	}
	if state.UnexpectedExitCount != 1 {
		t.Fatalf("unexpected exit count = %d, want 1", state.UnexpectedExitCount)
	}
	if !strings.Contains(state.LastMessage, "bind failed") {
		t.Fatalf("last message = %q, want bind error", state.LastMessage)
	}

	events := RecentEvents()
	if len(events) != 1 || events[0].Type != "failed" || events[0].Module != "http-server" {
		t.Fatalf("events = %#v, want http-server failed event", events)
	}

	ModuleStopped("http-server")
	if state := moduleStateByName(t, "http-server"); state.Status != StatusStopped {
		t.Fatalf("status after stop = %s, want stopped", state.Status)
	}
}

type logCapture struct {
	mu sync.Mutex
	b  strings.Builder
}

func captureLogs() *logCapture {
	return &logCapture{}
}

func (c *logCapture) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = fmt.Fprintf(&c.b, format, args...)
	_ = c.b.WriteByte('\n')
}

func (c *logCapture) string() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

func moduleStateByName(t *testing.T, name string) ModuleState {
	t.Helper()
	for _, state := range ModuleStates() {
		if state.Name == name {
			return state
		}
	}
	t.Fatalf("module state %q not found in %#v", name, ModuleStates())
	return ModuleState{}
}
