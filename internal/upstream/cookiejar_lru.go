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
	// The old 131K default could retain far more account/host jars than a
	// single-node pool can actively use. 16K is the balanced memory tier; the
	// client selects 4K/16K/32K from the configured body-memory envelope.
	defaultJarLRUMax = 16384
	defaultJarTTL    = 24 * time.Hour
)

// CacheStats is an aggregate-only snapshot. It deliberately contains no cache
// keys, hosts, account identifiers, cookies, or proxy endpoints.
type CacheStats struct {
	Entries   int    `json:"entries"`
	Capacity  int    `json:"capacity"`
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Evictions uint64 `json:"evictions"`
}

// jarLRU is a bounded, least-recently-used cache of cookie jars keyed by
// account:egress:host (plus any sticky passthrough CookieJarKey). The previous
// implementation was a plain map that gained an entry per unique key and never
// reclaimed it — an unbounded leak over a long-lived pool. Eviction drops the
// least-recently-used jar; a re-used account simply re-establishes cookies on its
// next request (cookies are an optimization / anti-detection aid here, not durable
// state we must never lose). All methods are safe for concurrent use.
type jarLRU struct {
	mu        sync.Mutex
	max       int
	ttl       time.Duration
	now       func() time.Time
	ll        *list.List               // front = most recently used, back = LRU
	items     map[string]*list.Element // key -> element holding *jarEntry
	hits      uint64
	misses    uint64
	evictions uint64
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
			l.evictions++
		} else {
			l.hits++
			entry.used = now
			l.ll.MoveToFront(el)
			return entry.jar
		}
	}
	l.misses++
	for back := l.ll.Back(); back != nil; back = l.ll.Back() {
		entry := back.Value.(*jarEntry)
		if now.Sub(entry.used) < l.ttl {
			break
		}
		l.ll.Remove(back)
		delete(l.items, entry.key)
		l.evictions++
	}
	jar, _ := cookiejar.New(nil)
	el := l.ll.PushFront(&jarEntry{key: key, jar: jar, used: now})
	l.items[key] = el
	if l.ll.Len() > l.max {
		if back := l.ll.Back(); back != nil {
			l.ll.Remove(back)
			delete(l.items, back.Value.(*jarEntry).key)
			l.evictions++
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

func (l *jarLRU) stats() CacheStats {
	if l == nil {
		return CacheStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return CacheStats{Entries: l.ll.Len(), Capacity: l.max, Hits: l.hits, Misses: l.misses, Evictions: l.evictions}
}
