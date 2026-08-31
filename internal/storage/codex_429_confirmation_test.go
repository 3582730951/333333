package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCodex429ConfirmationIsDurableAndBoundedByScope(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pool.db")
	open := func() *Store {
		t.Helper()
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Init(ctx); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
		return store
	}
	store := open()
	if err := store.UpsertAccount(ctx, Account{ID: "codex-429-account", Label: "Codex 429", GroupName: "cyber"}, AccountToken{}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}

	const (
		accountID = "codex-429-account"
		scope     = "responses"
		startedAt = int64(1_000)
		window    = int64(30)
	)
	confirmed, err := store.ObserveCodex429(ctx, accountID, scope, startedAt, window)
	if err != nil || confirmed {
		t.Fatalf("first signal confirmed=%v err=%v", confirmed, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = open()
	defer store.Close()

	// Reopening against the same database proves the first signal is not
	// process-local; the second in-window signal must still confirm it.
	confirmed, err = store.ObserveCodex429(ctx, accountID, scope, startedAt+window, window)
	if err != nil || !confirmed {
		t.Fatalf("second in-window signal confirmed=%v err=%v", confirmed, err)
	}

	// A separate scope must not borrow the first scope's evidence.
	confirmed, err = store.ObserveCodex429(ctx, accountID, "chat_completions", startedAt+window+1, window)
	if err != nil || confirmed {
		t.Fatalf("different scope confirmed=%v err=%v", confirmed, err)
	}
	if err := store.ResetCodex429(ctx, accountID, scope); err != nil {
		t.Fatal(err)
	}
	confirmed, err = store.ObserveCodex429(ctx, accountID, scope, startedAt+window+1, window)
	if err != nil || confirmed {
		t.Fatalf("reset signal confirmed=%v err=%v", confirmed, err)
	}
	confirmed, err = store.ObserveCodex429(ctx, accountID, scope, startedAt+window+2, window)
	if err != nil || !confirmed {
		t.Fatalf("post-reset second signal confirmed=%v err=%v", confirmed, err)
	}
}

func TestCodex429ConfirmationExpiresAndClearExpiredPreservesBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, Account{ID: "codex-429-expiry", Label: "Codex 429 expiry", GroupName: "cyber"}, AccountToken{}); err != nil {
		t.Fatal(err)
	}

	if confirmed, err := store.ObserveCodex429(ctx, "codex-429-expiry", "responses", 100, 30); err != nil || confirmed {
		t.Fatalf("first signal confirmed=%v err=%v", confirmed, err)
	}
	if confirmed, err := store.ObserveCodex429(ctx, "codex-429-expiry", "responses", 131, 30); err != nil || confirmed {
		t.Fatalf("expired signal confirmed=%v err=%v", confirmed, err)
	}
	if confirmed, err := store.ObserveCodex429(ctx, "codex-429-expiry", "responses", 132, 30); err != nil || !confirmed {
		t.Fatalf("fresh second signal confirmed=%v err=%v", confirmed, err)
	}

	if err := store.ClearExpiredCodex429(ctx, 131); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_429_confirmations WHERE account_id=?`, "codex-429-expiry").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("boundary state removed: count=%d", count)
	}
	if err := store.ClearExpiredCodex429(ctx, 132); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_429_confirmations WHERE account_id=?`, "codex-429-expiry").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired state retained: count=%d", count)
	}
}
