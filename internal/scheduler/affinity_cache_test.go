package scheduler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func TestAffinityCacheShardEvictsOldestAndHonorsTTL(t *testing.T) {
	cache := newAffinityCacheStore()
	now := time.Unix(1_700_000_000, 0)
	keys := make([]string, 0, affinityCacheCapacity/affinityCacheShardCount+1)
	for i := 0; len(keys) <= affinityCacheCapacity/affinityCacheShardCount; i++ {
		key := "key-" + strconv.Itoa(i)
		if cache.shard(key) == &cache.shards[0] {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		cache.put(key, storage.AffinityBinding{RouteKeyHash: key}, true, now)
	}
	if got := cache.len(); got != affinityCacheCapacity/affinityCacheShardCount {
		t.Fatalf("cache len=%d", got)
	}
	if _, ok := cache.get(keys[0], now); ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if entry, ok := cache.get(keys[len(keys)-1], now.Add(affinityCachePositiveTTL-time.Second)); !ok || !entry.found {
		t.Fatal("positive cache entry expired early")
	}
	if _, ok := cache.get(keys[len(keys)-1], now.Add(affinityCachePositiveTTL)); ok {
		t.Fatal("positive cache entry outlived its TTL")
	}

	cache.put("negative", storage.AffinityBinding{}, false, now)
	if entry, ok := cache.get("negative", now.Add(affinityCacheNegativeTTL-time.Millisecond)); !ok || entry.found {
		t.Fatal("negative cache entry was not retained")
	}
	if _, ok := cache.get("negative", now.Add(affinityCacheNegativeTTL)); ok {
		t.Fatal("negative cache entry outlived its TTL")
	}
}

func TestAffinityCacheTotalCapacityIsBounded(t *testing.T) {
	cache := newAffinityCacheStore()
	now := time.Now()
	for i := 0; i < affinityCacheCapacity+8192; i++ {
		key := strconv.Itoa(i)
		cache.put(key, storage.AffinityBinding{RouteKeyHash: key}, true, now)
	}
	if got := cache.len(); got > affinityCacheCapacity {
		t.Fatalf("cache len=%d exceeds capacity=%d", got, affinityCacheCapacity)
	}
}

func TestSchedulerAffinityCacheUsesPersistedEpoch(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for _, id := range []string{"cache-epoch-a", "cache-epoch-b"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := New(store, config.Default())
	binding := storage.AffinityBinding{RouteKeyHash: "cache-epoch", RouteKey: "key", Source: "previous_response_id", AccountID: "cache-epoch-a", Provider: "codex", Model: "gpt-5", EgressID: "direct", Epoch: 6}
	if err := scheduler.upsertAffinity(ctx, binding); err != nil {
		t.Fatal(err)
	}
	binding.Epoch = 200
	if err := scheduler.upsertAffinity(ctx, binding); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.affinitySnapshot(ctx, binding.RouteKeyHash)
	if err != nil || got.Epoch != 6 {
		t.Fatalf("idempotent cached binding=%+v err=%v", got, err)
	}
	binding.AccountID = "cache-epoch-b"
	if err := scheduler.upsertAffinity(ctx, binding); err != nil {
		t.Fatal(err)
	}
	got, err = scheduler.affinitySnapshot(ctx, binding.RouteKeyHash)
	if err != nil || got.AccountID != binding.AccountID || got.Epoch != 7 || got.ExpiresAt <= storage.Now() {
		t.Fatalf("rebound cached binding=%+v err=%v", got, err)
	}
	lease, err := scheduler.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", Affinity: routing.AffinityKey{Hash: binding.RouteKeyHash, Key: binding.RouteKey, Source: binding.Source}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Account.ID != binding.AccountID || lease.RouteEpoch != 7 {
		t.Fatalf("lease account=%s route_epoch=%d", lease.Account.ID, lease.RouteEpoch)
	}
}
