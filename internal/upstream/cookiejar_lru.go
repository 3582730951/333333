package upstream

import (
	"container/list"
	"net/http/cookiejar"
	"sync"
	"time"
)

// defaultJarLRUMax bounds how many per-(account,egress,host) cookie jars are kept in
// memory at once. Each jar is small; a few thousand is a modest footprint that still
// comfortably covers an active pool, and is conservative for the 1c/1gb target.
const (
	defaultJarLRUMax = 131072
	defaultJarTTL    = 24 * time.Hour
)

// jarLRU is a bounded, least-recently-used cache of cookie jars keyed by
// account:egress:host (plus any sticky passthrough CookieJarKey). The previous
// implementation was a plain map that gained an entry per unique key and never
// reclaimed it — an unbounded leak over a long-lived pool. Eviction drops the
// least-recently-used jar; a re-used account simply re-establishes cookies on its
// next request (cookies are an optimization / anti-detection aid here, not durable
// state we must never lose). All methods are safe for concurrent use.
type jarLRU struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	ll    *list.List               // front = most recently used, back = LRU
	items map[string]*list.Element // key -> element holding *jarEntry
}

type jarEntry struct {
	key  string
	jar  *cookiejar.Jar
	used time.Time
}

func newJarLRU(max int) *jarLRU {
	if max <= 0 {
		max = defaultJarLRUMax
	}
	return &jarLRU{max: max, ttl: defaultJarTTL, now: time.Now, ll: list.New(), items: make(map[string]*list.Element)}
}

// getOrCreate returns the jar for key, creating it if absent, and marks it most
// recently used. When the cache exceeds its bound it evicts the single LRU entry.
func (l *jarLRU) getOrCreate(key string) *cookiejar.Jar {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if el, ok := l.items[key]; ok {
		entry := el.Value.(*jarEntry)
		if now.Sub(entry.used) >= l.ttl {
			l.ll.Remove(el)
			delete(l.items, key)
		} else {
			entry.used = now
			l.ll.MoveToFront(el)
			return entry.jar
		}
	}
	for back := l.ll.Back(); back != nil; back = l.ll.Back() {
		entry := back.Value.(*jarEntry)
		if now.Sub(entry.used) < l.ttl {
			break
		}
		l.ll.Remove(back)
		delete(l.items, entry.key)
	}
	jar, _ := cookiejar.New(nil)
	el := l.ll.PushFront(&jarEntry{key: key, jar: jar, used: now})
	l.items[key] = el
	if l.ll.Len() > l.max {
		if back := l.ll.Back(); back != nil {
			l.ll.Remove(back)
			delete(l.items, back.Value.(*jarEntry).key)
		}
	}
	return jar
}

// len reports the number of jars currently held (for tests / introspection).
func (l *jarLRU) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ll.Len()
}
