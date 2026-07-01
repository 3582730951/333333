package api

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
)

// config_runtime.go generalizes the runtime-config overlay that flagEnabled
// (isolate.go) already uses for booleans to every value type. Each getter consults
// the settings table first — an admin-set override persisted via SetSetting — and
// falls back to the boot value from s.cfg. This is the single mechanism that makes
// configuration hot-editable from the admin UI without a restart: a handler writes
// SetSetting(key, value), and the next request reads the new value through one of
// these getters. Boolean flags continue to use flagEnabled (same shape).
//
// Only fields whose READ goes through one of these getters become hot. Fields baked
// into a subsystem at construction (upstream client / identity / scheduler) need a
// rebuild hook or to be re-read through a getter; truly bootstrap fields (listen
// addr, db path, subprocess launchers) stay restart-only.

type runtimeSettingsCacheContextKey struct{}

type runtimeSettingsCache struct {
	mu     sync.Mutex
	values map[string]runtimeSettingCacheEntry
}

type runtimeSettingCacheEntry struct {
	value string
	ok    bool
	err   error
}

func contextWithRuntimeSettingsCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, runtimeSettingsCacheContextKey{}, &runtimeSettingsCache{
		values: map[string]runtimeSettingCacheEntry{},
	})
}

func runtimeSettingsCacheFromContext(ctx context.Context) *runtimeSettingsCache {
	cache, _ := ctx.Value(runtimeSettingsCacheContextKey{}).(*runtimeSettingsCache)
	return cache
}

func (c *runtimeSettingsCache) get(key string) (runtimeSettingCacheEntry, bool) {
	if c == nil {
		return runtimeSettingCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.values[key]
	return entry, ok
}

func (c *runtimeSettingsCache) set(key string, entry runtimeSettingCacheEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = entry
}

// settingString returns the stored override for key (trimmed) when present and
// non-empty, otherwise def.
func (s *Server) settingString(ctx context.Context, key, def string) string {
	if v, ok := s.runtimeSetting(ctx, key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

// settingInt returns the stored override for key parsed as an int when present and
// valid, otherwise def.
func (s *Server) settingInt(ctx context.Context, key string, def int) int {
	if v, ok := s.runtimeSetting(ctx, key); ok {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil {
			return n
		} else {
			logInvalidRuntimeSetting(key, trimmed, "integer")
		}
	}
	return def
}

// settingInt64 mirrors settingInt for 64-bit values.
func (s *Server) settingInt64(ctx context.Context, key string, def int64) int64 {
	if v, ok := s.runtimeSetting(ctx, key); ok {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return n
		} else {
			logInvalidRuntimeSetting(key, trimmed, "int64")
		}
	}
	return def
}

// settingCSV returns the stored override for key parsed as a comma-separated list
// (trimmed, empties dropped) when present, otherwise def. An explicitly stored empty
// value yields an empty list (operator cleared the field), distinct from "unset".
func (s *Server) settingCSV(ctx context.Context, key string, def []string) []string {
	v, ok := s.runtimeSetting(ctx, key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) runtimeSetting(ctx context.Context, key string) (string, bool) {
	cache := runtimeSettingsCacheFromContext(ctx)
	if entry, cached := cache.get(key); cached {
		if entry.err != nil {
			return "", false
		}
		return entry.value, entry.ok
	}
	v, ok, err := s.store.GetSetting(ctx, key)
	if err != nil {
		log.Printf("[CONFIG-RUNTIME-ERROR] read setting %q failed: %v", key, err)
		cache.set(key, runtimeSettingCacheEntry{err: err})
		return "", false
	}
	cache.set(key, runtimeSettingCacheEntry{value: v, ok: ok})
	return v, ok
}

func logInvalidRuntimeSetting(key, value, kind string) {
	log.Printf("[CONFIG-RUNTIME-WARN] setting %q has invalid %s value %q; using configured default", key, kind, value)
}
