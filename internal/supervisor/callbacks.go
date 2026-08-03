package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultEventCallbackTimeout = 2 * time.Second

var (
	ErrEventCallbackTimeout = errors.New("supervisor event callback timed out")
	ErrEventCallbackPanic   = errors.New("supervisor event callback panicked")
)

// EventCallback receives a normalized event. Implementations must honor ctx and
// must not persist Event.Message or Event.Panic; those fields can contain raw
// third-party errors intended only for the local operator log.
type EventCallback func(context.Context, Event) error

// CallbackOptions defines a primary event callback and a separately isolated
// fallback. A typical durable implementation writes the primary to its database
// and fsyncs the fallback into a replay journal.
type CallbackOptions struct {
	Name            string
	Timeout         time.Duration
	FallbackTimeout time.Duration
	Callback        EventCallback
	Fallback        EventCallback
}

// CallbackState is safe for the admin status endpoint and diagnostic ZIP. It
// contains delivery counters and error classes, never raw callback errors.
type CallbackState struct {
	Name              string `json:"name"`
	Deliveries        uint64 `json:"deliveries"`
	PrimaryFailures   uint64 `json:"primary_failures"`
	PrimaryTimeouts   uint64 `json:"primary_timeouts"`
	PrimaryPanics     uint64 `json:"primary_panics"`
	FallbackAttempts  uint64 `json:"fallback_attempts"`
	FallbackSuccesses uint64 `json:"fallback_successes"`
	FallbackFailures  uint64 `json:"fallback_failures"`
	LastErrorClass    string `json:"last_error_class,omitempty"`
	LastEventUnix     int64  `json:"last_event_unix,omitempty"`
}

type callbackRegistration struct {
	id                uint64
	name              string
	timeout           time.Duration
	fallbackTimeout   time.Duration
	callback          EventCallback
	fallback          EventCallback
	deliveries        atomic.Uint64
	primaryFailures   atomic.Uint64
	primaryTimeouts   atomic.Uint64
	primaryPanics     atomic.Uint64
	fallbackAttempts  atomic.Uint64
	fallbackSuccesses atomic.Uint64
	fallbackFailures  atomic.Uint64
	lastEventUnix     atomic.Int64
	lastMu            sync.Mutex
	lastErrorClass    string
}

type callbackRegistry struct {
	mu        sync.RWMutex
	nextID    atomic.Uint64
	callbacks map[uint64]*callbackRegistration
}

var eventCallbackRegistry = &callbackRegistry{callbacks: map[uint64]*callbackRegistration{}}

// EventCallbackRegistration owns one callback registration.
type EventCallbackRegistration struct {
	id   uint64
	once sync.Once
}

// RegisterEventCallback activates a bounded callback/fallback pair. Delivery is
// synchronous up to the configured timeout so a recovered panic is durably
// handed off before its request or goroutine boundary returns.
func RegisterEventCallback(options CallbackOptions) (*EventCallbackRegistration, error) {
	if options.Callback == nil {
		return nil, errors.New("supervisor event callback is required")
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = "event-callback"
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultEventCallbackTimeout
	}
	if options.FallbackTimeout <= 0 {
		options.FallbackTimeout = options.Timeout
	}
	id := eventCallbackRegistry.nextID.Add(1)
	registered := &callbackRegistration{
		id: id, name: name, timeout: options.Timeout,
		fallbackTimeout: options.FallbackTimeout,
		callback:        options.Callback, fallback: options.Fallback,
	}
	eventCallbackRegistry.mu.Lock()
	eventCallbackRegistry.callbacks[id] = registered
	eventCallbackRegistry.mu.Unlock()
	return &EventCallbackRegistration{id: id}, nil
}

// Unregister stops future delivery. An event that already copied the callback
// snapshot may finish its current bounded delivery.
func (registration *EventCallbackRegistration) Unregister() {
	if registration == nil {
		return
	}
	registration.once.Do(func() {
		eventCallbackRegistry.mu.Lock()
		delete(eventCallbackRegistry.callbacks, registration.id)
		eventCallbackRegistry.mu.Unlock()
	})
}

// EventCallbackStates returns registration states in registration order.
func EventCallbackStates() []CallbackState {
	eventCallbackRegistry.mu.RLock()
	callbacks := make([]*callbackRegistration, 0, len(eventCallbackRegistry.callbacks))
	for _, callback := range eventCallbackRegistry.callbacks {
		callbacks = append(callbacks, callback)
	}
	eventCallbackRegistry.mu.RUnlock()
	sortCallbacks(callbacks)
	out := make([]CallbackState, 0, len(callbacks))
	for _, callback := range callbacks {
		callback.lastMu.Lock()
		lastErrorClass := callback.lastErrorClass
		callback.lastMu.Unlock()
		out = append(out, CallbackState{
			Name: callback.name, Deliveries: callback.deliveries.Load(),
			PrimaryFailures:   callback.primaryFailures.Load(),
			PrimaryTimeouts:   callback.primaryTimeouts.Load(),
			PrimaryPanics:     callback.primaryPanics.Load(),
			FallbackAttempts:  callback.fallbackAttempts.Load(),
			FallbackSuccesses: callback.fallbackSuccesses.Load(),
			FallbackFailures:  callback.fallbackFailures.Load(),
			LastErrorClass:    lastErrorClass, LastEventUnix: callback.lastEventUnix.Load(),
		})
	}
	return out
}

func sortCallbacks(callbacks []*callbackRegistration) {
	for i := 1; i < len(callbacks); i++ {
		for j := i; j > 0 && callbacks[j].id < callbacks[j-1].id; j-- {
			callbacks[j], callbacks[j-1] = callbacks[j-1], callbacks[j]
		}
	}
}

func deliverEventCallbacks(event Event) {
	eventCallbackRegistry.mu.RLock()
	callbacks := make([]*callbackRegistration, 0, len(eventCallbackRegistry.callbacks))
	for _, callback := range eventCallbackRegistry.callbacks {
		callbacks = append(callbacks, callback)
	}
	eventCallbackRegistry.mu.RUnlock()
	sortCallbacks(callbacks)
	for _, callback := range callbacks {
		deliverEventCallback(callback, event)
	}
}

func deliverEventCallback(callback *callbackRegistration, event Event) {
	callback.deliveries.Add(1)
	callback.lastEventUnix.Store(time.Now().Unix())
	err := invokeEventCallback(callback.timeout, callback.callback, event)
	if err == nil {
		callback.setLastErrorClass("")
		return
	}
	callback.primaryFailures.Add(1)
	class := eventCallbackErrorClass(err)
	if errors.Is(err, ErrEventCallbackTimeout) {
		callback.primaryTimeouts.Add(1)
	}
	if errors.Is(err, ErrEventCallbackPanic) {
		callback.primaryPanics.Add(1)
	}
	callback.setLastErrorClass(class)
	if callback.fallback == nil {
		log.Printf("[SUPERVISOR-CALLBACK] name=%s event_id=%s primary=%s fallback=missing", callback.name, event.ID, class)
		return
	}
	callback.fallbackAttempts.Add(1)
	if fallbackErr := invokeEventCallback(callback.fallbackTimeout, callback.fallback, event); fallbackErr != nil {
		callback.fallbackFailures.Add(1)
		fallbackClass := eventCallbackErrorClass(fallbackErr)
		callback.setLastErrorClass("fallback_" + fallbackClass)
		log.Printf("[SUPERVISOR-CALLBACK] name=%s event_id=%s primary=%s fallback=%s", callback.name, event.ID, class, fallbackClass)
		return
	}
	callback.fallbackSuccesses.Add(1)
}

func (callback *callbackRegistration) setLastErrorClass(value string) {
	callback.lastMu.Lock()
	callback.lastErrorClass = value
	callback.lastMu.Unlock()
}

func eventCallbackErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrEventCallbackTimeout):
		return "timeout"
	case errors.Is(err, ErrEventCallbackPanic):
		return "panic"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "error"
	}
}

func invokeEventCallback(timeout time.Duration, callback EventCallback, event Event) error {
	if callback == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := make(chan error, 1)
	go runEventCallback(ctx, callback, event, result)
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrEventCallbackTimeout, ctx.Err())
	}
}

func runEventCallback(ctx context.Context, callback EventCallback, event Event, result chan<- error) {
	var err error
	defer func() { result <- err }()
	defer RecoverCallback(&err)
	err = callback(ctx, event)
}

// RecoverCallback turns a callback panic into a classified delivery failure.
// It deliberately does not publish another supervisor event, which would feed
// the failing callback back into itself recursively.
func RecoverCallback(err *error) {
	if value := recover(); value != nil && err != nil {
		*err = fmt.Errorf("%w: %T", ErrEventCallbackPanic, value)
	}
}

func clearEventCallbacksForTest() {
	eventCallbackRegistry.mu.Lock()
	eventCallbackRegistry.callbacks = map[uint64]*callbackRegistration{}
	eventCallbackRegistry.mu.Unlock()
}
