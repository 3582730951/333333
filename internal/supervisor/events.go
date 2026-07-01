package supervisor

import (
	"fmt"
	"sync"
	"time"
)

const recentEventLimit = 100

// Event is a compact, JSON-safe record of supervisor activity. It is intentionally
// small so the admin status endpoint can expose recent module failures without
// shipping full stack traces to the browser.
type Event struct {
	TimeUnix      int64  `json:"time_unix"`
	Type          string `json:"type"`
	Module        string `json:"module"`
	Message       string `json:"message"`
	Panic         string `json:"panic,omitempty"`
	UptimeMillis  int64  `json:"uptime_millis,omitempty"`
	BackoffMillis int64  `json:"backoff_millis,omitempty"`
}

var recentEvents = newEventRing(recentEventLimit)

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

func recordEvent(event Event) {
	if event.TimeUnix == 0 {
		event.TimeUnix = time.Now().Unix()
	}
	if event.Type == "" {
		event.Type = "event"
	}
	if event.Module == "" {
		event.Module = "background"
	}
	recentEvents.append(event)
}

func recordPanicEvent(name string, panicVal any) {
	markModulePanic(name, panicVal)
	recordEvent(Event{
		Type:    "panic",
		Module:  normalizeOptions(Options{Name: name}).Name,
		Message: fmt.Sprintf("module panic: %v", panicVal),
		Panic:   fmt.Sprint(panicVal),
	})
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
