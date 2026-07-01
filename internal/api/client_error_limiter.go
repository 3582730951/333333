package api

import (
	"sync"
	"time"
)

const (
	clientErrorLogLimit      = 60
	clientErrorLogWindow     = time.Minute
	clientErrorLogMaxClients = 2048
)

type clientErrorLimiter struct {
	mu         sync.Mutex
	hits       map[string]clientErrorWindow
	limit      int
	window     time.Duration
	maxClients int
}

type clientErrorWindow struct {
	start         time.Time
	lastSeen      time.Time
	count         int
	limitedLogged bool
}

func newClientErrorLimiter(limit int, window time.Duration, maxClients int) *clientErrorLimiter {
	if limit <= 0 {
		limit = clientErrorLogLimit
	}
	if window <= 0 {
		window = clientErrorLogWindow
	}
	if maxClients <= 0 {
		maxClients = clientErrorLogMaxClients
	}
	return &clientErrorLimiter{
		hits:       map[string]clientErrorWindow{},
		limit:      limit,
		window:     window,
		maxClients: maxClients,
	}
}

func (l *clientErrorLimiter) allow(client string, now time.Time) bool {
	allowed, _ := l.allowWithLimitLog(client, now)
	return allowed
}

func (l *clientErrorLimiter) allowWithLimitLog(client string, now time.Time) (bool, bool) {
	if l == nil {
		return true, false
	}
	if client == "" {
		client = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.hits[client]
	if !ok || now.Sub(w.start) >= l.window {
		l.hits[client] = clientErrorWindow{start: now, lastSeen: now, count: 1}
		l.pruneLocked(now)
		return true, false
	}
	w.lastSeen = now
	if w.count >= l.limit {
		shouldLogLimited := !w.limitedLogged
		w.limitedLogged = true
		l.hits[client] = w
		return false, shouldLogLimited
	}
	w.count++
	l.hits[client] = w
	l.pruneLocked(now)
	return true, false
}

func (l *clientErrorLimiter) retryAfterSeconds() int {
	if l == nil || l.window <= 0 {
		return int(clientErrorLogWindow / time.Second)
	}
	seconds := int(l.window / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (l *clientErrorLimiter) pruneLocked(now time.Time) {
	staleAfter := 2 * l.window
	for client, w := range l.hits {
		if now.Sub(w.lastSeen) > staleAfter {
			delete(l.hits, client)
		}
	}
	for len(l.hits) > l.maxClients {
		var oldestClient string
		var oldest time.Time
		for client, w := range l.hits {
			if oldestClient == "" || w.lastSeen.Before(oldest) {
				oldestClient = client
				oldest = w.lastSeen
			}
		}
		delete(l.hits, oldestClient)
	}
}
