package api

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

const (
	usageDashboardFreshTTL = 5 * time.Second
	usageDashboardStaleTTL = 60 * time.Second
	usageDashboardCacheMax = 32
)

type dashboardSnapshot struct {
	status  int
	header  http.Header
	body    []byte
	created time.Time
}

type dashboardCacheEntry struct {
	snapshot   dashboardSnapshot
	refreshing bool
}

type usageDashboardResponseCache struct {
	mu      sync.Mutex
	entries map[string]*dashboardCacheEntry
	now     func() time.Time
	fresh   time.Duration
	stale   time.Duration
	max     int
}

func newUsageDashboardResponseCache() *usageDashboardResponseCache {
	return &usageDashboardResponseCache{
		entries: map[string]*dashboardCacheEntry{}, now: time.Now,
		fresh: usageDashboardFreshTTL, stale: usageDashboardStaleTTL, max: usageDashboardCacheMax,
	}
}

type dashboardCaptureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newDashboardCaptureWriter() *dashboardCaptureWriter {
	return &dashboardCaptureWriter{header: http.Header{}, status: http.StatusOK}
}

func (w *dashboardCaptureWriter) Header() http.Header { return w.header }
func (w *dashboardCaptureWriter) WriteHeader(status int) {
	if w.status != http.StatusOK || status == http.StatusOK {
		return
	}
	w.status = status
}
func (w *dashboardCaptureWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func dashboardCacheKey(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path + "?" + r.URL.Query().Encode()
}

func (c *usageDashboardResponseCache) Serve(w http.ResponseWriter, r *http.Request, render http.HandlerFunc) {
	key := dashboardCacheKey(r)
	now := c.now()
	c.mu.Lock()
	entry := c.entries[key]
	if entry != nil {
		age := now.Sub(entry.snapshot.created)
		if age <= c.fresh {
			snapshot := entry.snapshot
			c.mu.Unlock()
			writeDashboardSnapshot(w, snapshot, "fresh", age)
			return
		}
		if age <= c.stale {
			snapshot := entry.snapshot
			startRefresh := !entry.refreshing
			entry.refreshing = true
			c.mu.Unlock()
			if startRefresh {
				c.refreshAsync(key, r, render)
			}
			writeDashboardSnapshot(w, snapshot, "stale", age)
			return
		}
		delete(c.entries, key)
	}
	c.mu.Unlock()

	snapshot := renderDashboardSnapshot(r, render)
	if snapshot.status >= 200 && snapshot.status < 300 {
		c.store(key, snapshot)
	}
	writeDashboardSnapshot(w, snapshot, "miss", 0)
}

func renderDashboardSnapshot(r *http.Request, render http.HandlerFunc) dashboardSnapshot {
	capture := newDashboardCaptureWriter()
	render(capture, r)
	return dashboardSnapshot{
		status: capture.status, header: capture.header.Clone(),
		body: append([]byte(nil), capture.body.Bytes()...), created: time.Now(),
	}
}

func (c *usageDashboardResponseCache) refreshAsync(key string, original *http.Request, render http.HandlerFunc) {
	request := original.Clone(context.Background())
	request.Header = original.Header.Clone()
	supervisor.GoOnce("usage-dashboard-swr-refresh", func() {
		ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
		defer cancel()
		snapshot := renderDashboardSnapshot(request.WithContext(ctx), render)
		if snapshot.status >= 200 && snapshot.status < 300 {
			c.store(key, snapshot)
			return
		}
		c.mu.Lock()
		if entry := c.entries[key]; entry != nil {
			entry.refreshing = false
		}
		c.mu.Unlock()
	})
}

func (c *usageDashboardResponseCache) store(key string, snapshot dashboardSnapshot) {
	snapshot.created = c.now()
	c.mu.Lock()
	c.entries[key] = &dashboardCacheEntry{snapshot: snapshot}
	if len(c.entries) > c.max {
		keys := make([]string, 0, len(c.entries))
		for candidate := range c.entries {
			keys = append(keys, candidate)
		}
		sort.Slice(keys, func(i, j int) bool {
			return c.entries[keys[i]].snapshot.created.Before(c.entries[keys[j]].snapshot.created)
		})
		for len(c.entries) > c.max && len(keys) > 0 {
			delete(c.entries, keys[0])
			keys = keys[1:]
		}
	}
	c.mu.Unlock()
}

func writeDashboardSnapshot(w http.ResponseWriter, snapshot dashboardSnapshot, state string, age time.Duration) {
	for key, values := range snapshot.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Pool-Dashboard-Cache", state)
	w.Header().Set("Age", strconv.FormatInt(max(0, int64(age/time.Second)), 10))
	w.WriteHeader(snapshot.status)
	_, _ = w.Write(snapshot.body)
}
