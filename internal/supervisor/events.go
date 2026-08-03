package supervisor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const recentEventLimit = 100

// Event is the common exception/health event contract used by the supervisor,
// HTTP middleware, and durable diagnostic callbacks. Message and Panic remain
// available to the local operator status view; durable callbacks must persist
// only the classification fields (ID through ResponseCommitted) and never the
// raw values.
type Event struct {
	ID                string `json:"id"`
	TimeUnix          int64  `json:"time_unix"`
	Type              string `json:"type"`
	Severity          string `json:"severity"`
	Module            string `json:"module"`
	Operation         string `json:"operation,omitempty"`
	ErrorClass        string `json:"error_class,omitempty"`
	Fingerprint       string `json:"fingerprint,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	Route             string `json:"route,omitempty"`
	Status            int    `json:"status,omitempty"`
	Recovered         bool   `json:"recovered,omitempty"`
	ResponseCommitted bool   `json:"response_committed,omitempty"`
	Message           string `json:"message"`
	Panic             string `json:"panic,omitempty"`
	UptimeMillis      int64  `json:"uptime_millis,omitempty"`
	BackoffMillis     int64  `json:"backoff_millis,omitempty"`
}

var (
	recentEvents    = newEventRing(recentEventLimit)
	eventIDFallback atomic.Uint64
)

type eventRing struct {
	mu     sync.Mutex
	limit  int
	events []Event
}

func newEventRing(limit int) *eventRing {
	if limit <= 0 {
		limit = recentEventLimit
	}
	return &eventRing{limit: limit}
}

// RecentEvents returns newest-first supervisor events.
func RecentEvents() []Event {
	return recentEvents.snapshot()
}

// Report publishes a classified event through both the bounded in-memory view
// and every registered durable callback. This is the extension point for new
// modules that encounter a handled error which does not cross a panic boundary.
func Report(event Event) {
	recordEvent(event)
}

// ReportError is the compact common path for handled errors in future modules.
// The raw error remains local while durable callbacks receive an error class and
// a one-way fingerprint for correlation.
func ReportError(module, operation string, err error) {
	if err == nil {
		return
	}
	recordEvent(Event{
		Type:       "error",
		Severity:   "error",
		Module:     module,
		Operation:  operation,
		ErrorClass: errorClass(err),
		Message:    fmt.Sprintf("operation failed: %v", err),
	})
}

func recordEvent(event Event) Event {
	event = normalizeEvent(event)
	recentEvents.append(event)
	deliverEventCallbacks(event)
	return event
}

func normalizeEvent(event Event) Event {
	if strings.TrimSpace(event.ID) == "" {
		event.ID = newEventID()
	}
	if event.TimeUnix == 0 {
		event.TimeUnix = time.Now().Unix()
	}
	if strings.TrimSpace(event.Type) == "" {
		event.Type = "event"
	}
	if strings.TrimSpace(event.Module) == "" {
		event.Module = "background"
	}
	if strings.TrimSpace(event.Severity) == "" {
		event.Severity = eventSeverity(event.Type)
	}
	if strings.TrimSpace(event.ErrorClass) == "" && event.Panic != "" {
		event.ErrorClass = "panic"
	}
	if strings.TrimSpace(event.Fingerprint) == "" {
		event.Fingerprint = eventFingerprint(event)
	}
	return event
}

func eventSeverity(eventType string) string {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "panic", "panic_restart", "failed", "error", "http_error":
		return "error"
	case "unexpected_exit", "error_retry", "callback_failure":
		return "warning"
	default:
		return "info"
	}
}

func eventFingerprint(event Event) string {
	h := sha256.New()
	for _, value := range []string{
		event.Type, event.Module, event.Operation, event.ErrorClass,
		event.Route, fmt.Sprintf("%d", event.Status),
	} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func newEventID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "SEVT-" + strings.ToUpper(hex.EncodeToString(value[:]))
	}
	return fmt.Sprintf("SEVT-%016X%016X", uint64(time.Now().UnixNano()), eventIDFallback.Add(1))
}

func errorClass(value any) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%T", value)
}

func recordPanicEvent(event Event, panicVal any) Event {
	if strings.TrimSpace(event.Type) == "" {
		event.Type = "panic"
	}
	if strings.TrimSpace(event.Severity) == "" {
		event.Severity = "error"
	}
	if strings.TrimSpace(event.Module) == "" {
		event.Module = "background"
	}
	event.Recovered = true
	event.ErrorClass = errorClass(panicVal)
	if strings.TrimSpace(event.Message) == "" {
		event.Message = fmt.Sprintf("module panic: %v", panicVal)
	}
	if strings.TrimSpace(event.Panic) == "" {
		event.Panic = fmt.Sprint(panicVal)
	}
	markModulePanic(event.Module, panicVal)
	return recordEvent(event)
}

func clearRecentEventsForTest() {
	recentEvents.clear()
}

func (r *eventRing) append(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == r.limit {
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = event
		return
	}
	r.events = append(r.events, event)
}

func (r *eventRing) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	for i := range r.events {
		out[i] = r.events[len(r.events)-1-i]
	}
	return out
}

func (r *eventRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}
