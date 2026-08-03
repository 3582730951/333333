package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEventCallbackNormalizesAndDeliversBeforeReportReturns(t *testing.T) {
	clearRecentEventsForTest()
	clearEventCallbacksForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearEventCallbacksForTest)

	var delivered Event
	registration, err := RegisterEventCallback(CallbackOptions{
		Name: "capture",
		Callback: func(_ context.Context, event Event) error {
			delivered = event
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registration.Unregister)

	Report(Event{Type: "error", Module: "fixture", Operation: "read", Message: "raw detail"})
	if delivered.ID == "" || delivered.TimeUnix == 0 || delivered.Fingerprint == "" {
		t.Fatalf("callback received incomplete normalized event: %#v", delivered)
	}
	if !strings.HasPrefix(delivered.Fingerprint, "sha256:") || delivered.Severity != "error" {
		t.Fatalf("callback classification: %#v", delivered)
	}
	states := EventCallbackStates()
	if len(states) != 1 || states[0].Deliveries != 1 || states[0].PrimaryFailures != 0 {
		t.Fatalf("callback states: %#v", states)
	}
}

func TestEventFingerprintNeverDependsOnRawErrorText(t *testing.T) {
	first := normalizeEvent(Event{
		Type: "panic", Module: "fixture", Operation: "read",
		ErrorClass: "fixture_error", Route: "fixture.read", Status: 503,
		Message: "Bearer first-secret@example.test", Panic: "first-secret",
	})
	second := normalizeEvent(Event{
		Type: "panic", Module: "fixture", Operation: "read",
		ErrorClass: "fixture_error", Route: "fixture.read", Status: 503,
		Message: "Bearer second-secret@example.test", Panic: "second-secret",
	})
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("classification fingerprint changed with raw payload: %q != %q", first.Fingerprint, second.Fingerprint)
	}
}

func TestEventCallbackPanicUsesFallbackWithoutRecursiveEvent(t *testing.T) {
	clearRecentEventsForTest()
	clearEventCallbacksForTest()
	t.Cleanup(clearRecentEventsForTest)
	t.Cleanup(clearEventCallbacksForTest)

	var fallback Event
	registration, err := RegisterEventCallback(CallbackOptions{
		Name: "panic-primary",
		Callback: func(context.Context, Event) error {
			panic("callback raw panic")
		},
		Fallback: func(_ context.Context, event Event) error {
			fallback = event
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registration.Unregister)

	Report(Event{Type: "error", Module: "future-module", Message: "fixture"})
	if fallback.ID == "" || fallback.Module != "future-module" {
		t.Fatalf("fallback event: %#v", fallback)
	}
	states := EventCallbackStates()
	if len(states) != 1 || states[0].PrimaryPanics != 1 || states[0].FallbackSuccesses != 1 || states[0].FallbackFailures != 0 {
		t.Fatalf("callback panic state: %#v", states)
	}
	if events := RecentEvents(); len(events) != 1 {
		t.Fatalf("callback panic recursively published events: %#v", events)
	}
}

func TestEventCallbackTimeoutUsesBoundedFallback(t *testing.T) {
	clearEventCallbacksForTest()
	t.Cleanup(clearEventCallbacksForTest)

	blocked := make(chan struct{})
	fallbackCalled := make(chan struct{}, 1)
	registration, err := RegisterEventCallback(CallbackOptions{
		Name: "blocked-primary", Timeout: 15 * time.Millisecond,
		FallbackTimeout: time.Second,
		Callback: func(context.Context, Event) error {
			<-blocked
			return nil
		},
		Fallback: func(context.Context, Event) error {
			fallbackCalled <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registration.Unregister)

	started := time.Now()
	Report(Event{Type: "error", Module: "blocked"})
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("callback timeout was not bounded: %s", elapsed)
	}
	select {
	case <-fallbackCalled:
	default:
		t.Fatal("fallback was not called after primary timeout")
	}
	close(blocked)
	states := EventCallbackStates()
	if len(states) != 1 || states[0].PrimaryTimeouts != 1 || states[0].FallbackSuccesses != 1 || states[0].LastErrorClass != "timeout" {
		t.Fatalf("callback timeout state: %#v", states)
	}
}

func TestEventFallbackPanicIsBoundedAndClassified(t *testing.T) {
	clearEventCallbacksForTest()
	t.Cleanup(clearEventCallbacksForTest)

	registration, err := RegisterEventCallback(CallbackOptions{
		Name: "panic-fallback", Timeout: time.Second, FallbackTimeout: 100 * time.Millisecond,
		Callback: func(context.Context, Event) error {
			return errors.New("primary unavailable")
		},
		Fallback: func(context.Context, Event) error {
			panic("fallback raw panic")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registration.Unregister)

	Report(Event{Type: "error", Module: "fallback-panic"})
	states := EventCallbackStates()
	if len(states) != 1 || states[0].FallbackAttempts != 1 || states[0].FallbackFailures != 1 || states[0].LastErrorClass != "fallback_panic" {
		t.Fatalf("fallback panic state: %#v", states)
	}
}

func TestRegisterEventCallbackRequiresPrimary(t *testing.T) {
	if _, err := RegisterEventCallback(CallbackOptions{}); err == nil {
		t.Fatal("nil primary callback was accepted")
	}
	if !errors.Is(invokeEventCallback(time.Millisecond, nil, Event{}), nil) {
		t.Fatal("nil internal callback should remain a no-op")
	}
}
