package api

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

func driftTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPromptCacheDriftGuardStableKeyNoAudit(t *testing.T) {
	store := driftTestStore(t)
	ctx := context.Background()
	g := newPromptCacheDriftGuard()

	g.note(ctx, store, "aff-a", "auto_stable_prefix", "auto_key_1", "system v1")
	if g.note(ctx, store, "aff-a", "auto_stable_prefix", "auto_key_1", "system v1") {
		t.Fatal("identical turn facts must not report drift")
	}
	rows, err := store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Action == "prompt_cache_prefix_drift" {
			t.Fatalf("unexpected drift audit on stable key: %+v", row)
		}
	}
}

func TestPromptCacheDriftGuardKeyChangeAudits(t *testing.T) {
	store := driftTestStore(t)
	ctx := context.Background()
	g := newPromptCacheDriftGuard()

	g.note(ctx, store, "aff-b", "auto_stable_prefix", "auto_key_1", "system v1")
	// 第二轮 key 变化(例如上游剥离/客户端显式 key 轮转) → 应产生 audit。
	if !g.note(ctx, store, "aff-b", "auto_stable_prefix", "auto_key_2", "system v1") {
		t.Fatal("key change must report drift")
	}
	rows, err := store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "prompt_cache_prefix_drift" && row.Reason == "prefix_changed" &&
			len(row.Detail) > 0 && row.Detail != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected prompt_cache_prefix_drift audit, got %+v", rows)
	}
}

func TestPromptCacheDriftGuardSystemPromptChangeAudits(t *testing.T) {
	store := driftTestStore(t)
	ctx := context.Background()
	g := newPromptCacheDriftGuard()

	g.note(ctx, store, "aff-c", "auto_stable_prefix", "auto_key_1", "system v1")
	// 系统提示热更新: 前缀内容变化, key 不变 → 仍应报 drift(前缀已破坏)。
	if !g.note(ctx, store, "aff-c", "auto_stable_prefix", "auto_key_1", "system v2") {
		t.Fatal("system-prompt change must report drift")
	}
	rows, err := store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "prompt_cache_prefix_drift" && row.Detail != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected system-prompt drift audit, got %+v", rows)
	}
}

func TestPromptCacheDriftGuardDistinctConversationsIsolated(t *testing.T) {
	store := driftTestStore(t)
	ctx := context.Background()
	g := newPromptCacheDriftGuard()

	g.note(ctx, store, "aff-x", "auto_stable_prefix", "key_x1", "system v1")
	if g.note(ctx, store, "aff-y", "auto_stable_prefix", "key_y1", "system v1") {
		t.Fatal("first note for a conversation must never report drift")
	}
	if g.note(ctx, store, "aff-x", "auto_stable_prefix", "key_x1", "system v1") {
		t.Fatal("stable follow-up turn reported drift")
	}
}
