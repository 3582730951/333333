package scheduler

import (
	"container/list"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

const (
	affinityCacheShardCount  = 64
	affinityCacheCapacity    = 131072
	affinityCachePositiveTTL = 7 * 24 * time.Hour
	affinityCacheNegativeTTL = time.Second
)

type affinityCacheEntry struct {
	binding storage.AffinityBinding
	found   bool
	expires time.Time
}

type affinityCacheItem struct {
	key   string
	entry affinityCacheEntry
}

type affinityCacheShard struct {
	mu      sync.Mutex
	loadMu  sync.Mutex
	entries map[string]*list.Element
	lru     list.List
}

type affinityCacheStore struct {
	shards [affinityCacheShardCount]affinityCacheShard
}

func newAffinityCacheStore() *affinityCacheStore { return &affinityCacheStore{} }

func affinityCacheHash(key string) uint32 {
	const offset32 = uint32(2166136261)
	const prime32 = uint32(16777619)
	hash := offset32
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime32
	}
	return hash
}

func (c *affinityCacheStore) shard(key string) *affinityCacheShard {
	return &c.shards[affinityCacheHash(key)&(affinityCacheShardCount-1)]
}

func (c *affinityCacheStore) get(key string, now time.Time) (affinityCacheEntry, bool) {
	if c == nil {
		return affinityCacheEntry{}, false
	}
	shard := c.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	element := shard.entries[key]
	if element == nil {
		return affinityCacheEntry{}, false
	}
	item := element.Value.(affinityCacheItem)
	if !now.Before(item.entry.expires) {
		delete(shard.entries, key)
		shard.lru.Remove(element)
		return affinityCacheEntry{}, false
	}
	shard.lru.MoveToFront(element)
	return item.entry, true
}

func (c *affinityCacheStore) put(key string, binding storage.AffinityBinding, found bool, now time.Time) {
	if c == nil || key == "" {
		return
	}
	expires := now.Add(affinityCacheNegativeTTL)
	if found {
		expires = now.Add(affinityCachePositiveTTL)
		if binding.ExpiresAt > 0 {
			bindingExpiry := time.Unix(binding.ExpiresAt, 0)
			if bindingExpiry.Before(expires) {
				expires = bindingExpiry
			}
		}
	}
	shard := c.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.entries == nil {
		shard.entries = make(map[string]*list.Element)
	}
	entry := affinityCacheEntry{binding: binding, found: found, expires: expires}
	if element := shard.entries[key]; element != nil {
		element.Value = affinityCacheItem{key: key, entry: entry}
		shard.lru.MoveToFront(element)
		return
	}
	element := shard.lru.PushFront(affinityCacheItem{key: key, entry: entry})
	shard.entries[key] = element
	for shard.lru.Len() > affinityCacheCapacity/affinityCacheShardCount {
		oldest := shard.lru.Back()
		item := oldest.Value.(affinityCacheItem)
		delete(shard.entries, item.key)
		shard.lru.Remove(oldest)
	}
}

func (c *affinityCacheStore) clear() {
	if c == nil {
		return
	}
	for i := range c.shards {
		shard := &c.shards[i]
		shard.mu.Lock()
		shard.entries = nil
		shard.lru.Init()
		shard.mu.Unlock()
	}
}

func (c *affinityCacheStore) len() int {
	if c == nil {
		return 0
	}
	total := 0
	for i := range c.shards {
		shard := &c.shards[i]
		shard.mu.Lock()
		total += shard.lru.Len()
		shard.mu.Unlock()
	}
	return total
}
