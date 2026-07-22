package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexSessionMappingEncryptsIdentityAndRetiresWholeTree(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	commit := CodexSessionCommit{
		Namespace: "key:test-namespace",
		Binding: CodexSessionBinding{
			ID:            "binding-root",
			TreeID:        "tree-root",
			AccountID:     "account-a",
			EgressID:      "direct",
			Epoch:         0,
			State:         "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000001",
			ThreadID:      "019f0000-0000-7000-8000-000000000001",
		},
		Aliases: []CodexSessionAlias{
			{Type: "root", Value: "real-root-thread"},
			{Type: "response", Value: "resp-real-1"},
			{Type: "turn_state", Value: "opaque-real-state"},
		},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	committed, err := store.CommitCodexSessionBinding(ctx, commit)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed.RootSessionID != commit.Binding.RootSessionID || committed.ThreadID != commit.Binding.ThreadID {
		t.Fatalf("identity round-trip = %+v", committed)
	}

	resolved, err := store.ResolveCodexSessionAliases(ctx, commit.Namespace, []CodexSessionAlias{
		{Type: "response", Value: "resp-real-1"},
		{Type: "turn_state", Value: "opaque-real-state"},
	})
	if err != nil || resolved.ID != committed.ID || resolved.AccountID != "account-a" {
		t.Fatalf("resolve = %+v, %v", resolved, err)
	}

	var namespaceHash, encryptedIdentity, aliasHash string
	if err := store.DB().QueryRowContext(ctx, `SELECT namespace_hash,encrypted_identity FROM codex_session_binding WHERE id=?`, committed.ID).Scan(&namespaceHash, &encryptedIdentity); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT alias_hash FROM codex_session_alias WHERE binding_id=? LIMIT 1`, committed.ID).Scan(&aliasHash); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"test-namespace", "real-root-thread", "resp-real-1", "opaque-real-state", commit.Binding.RootSessionID} {
		if strings.Contains(namespaceHash, raw) || strings.Contains(aliasHash, raw) || strings.Contains(encryptedIdentity, raw) {
			t.Fatalf("raw identifier leaked into mapping storage: %q", raw)
		}
	}

	if _, err := store.RetireCodexSessionTree(ctx, committed.ID, committed.Epoch); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_, err = store.ResolveCodexSessionAliases(ctx, commit.Namespace, []CodexSessionAlias{{Type: "response", Value: "resp-real-1"}})
	if !errors.Is(err, ErrCodexSessionEpochRetired) {
		t.Fatalf("stateful alias after retirement err=%v, want ErrCodexSessionEpochRetired", err)
	}
}

func TestCodexSessionMappingCompactionAdvancesGenerationOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:test",
		Binding: CodexSessionBinding{
			ID: "binding-compact", TreeID: "tree-compact", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000011", ThreadID: "019f0000-0000-7000-8000-000000000011",
		},
		Aliases:   []CodexSessionAlias{{Type: "root", Value: "root-compact"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := store.AdvanceCodexSessionWindowGeneration(ctx, committed.ID, committed.Epoch, 0, time.Now().Add(time.Hour).Unix())
	if err != nil || advanced.WindowGeneration != 1 {
		t.Fatalf("advance = %+v, %v", advanced, err)
	}
	_, err = store.AdvanceCodexSessionWindowGeneration(ctx, committed.ID, committed.Epoch, 0, time.Now().Add(time.Hour).Unix())
	if !errors.Is(err, ErrCodexSessionEpochConflict) {
		t.Fatalf("second advance err=%v, want CAS conflict", err)
	}
}

func TestCodexSessionMappingCommitRefreshesAllAliasRetention(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	initialExpiry := time.Now().Add(time.Hour).Unix()
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:sliding",
		Binding: CodexSessionBinding{
			ID: "binding-sliding", TreeID: "tree-sliding", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000021", ThreadID: "019f0000-0000-7000-8000-000000000021",
		},
		Aliases:   []CodexSessionAlias{{Type: "root", Value: "root-sliding"}, {Type: "response", Value: "resp-sliding-1"}},
		ExpiresAt: initialExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshedExpiry := time.Now().Add(2 * time.Hour).Unix()
	if _, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:sliding",
		Binding:   committed,
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-sliding-2"}},
		ExpiresAt: refreshedExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	var rootExpiry int64
	if err := store.DB().QueryRowContext(ctx, `SELECT expires_at FROM codex_session_alias WHERE binding_id=? AND alias_type='root'`, committed.ID).Scan(&rootExpiry); err != nil {
		t.Fatal(err)
	}
	if rootExpiry < refreshedExpiry {
		t.Fatalf("root alias expiry=%d, want sliding refresh to at least %d", rootExpiry, refreshedExpiry)
	}
}

func TestCodexSessionMappingResolvesAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mapping.sqlite3")
	secret := []byte("persistent-codex-mapping-secret")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(secret)
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:restart",
		Binding: CodexSessionBinding{
			ID: "binding-restart", TreeID: "tree-restart", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000041", ThreadID: "019f0000-0000-7000-8000-000000000041",
		},
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-restart"}, {Type: "turn_state", Value: "state-restart"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Init(ctx); err != nil {
		t.Fatal(err)
	}
	reopened.SetTokenEncryptionKey(secret)
	resolved, err := reopened.ResolveCodexSessionAliases(ctx, "key:restart", []CodexSessionAlias{{Type: "response", Value: "resp-restart"}, {Type: "turn_state", Value: "state-restart"}})
	if err != nil || resolved.ID != committed.ID || resolved.RootSessionID != committed.RootSessionID || resolved.AccountID != "account-a" || resolved.EgressID != "direct" {
		t.Fatalf("restart mapping resolve=%+v err=%v", resolved, err)
	}
}
