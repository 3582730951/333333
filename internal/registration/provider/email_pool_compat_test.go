package provider

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestBuildManagerIncludesLegacyEmailPoolMailbox(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertEmailAccount(ctx, storage.EmailAccount{
		ID: "legacy-mail", Email: "legacy@example.test", ClientID: "oauth-client",
		RefreshToken: "oauth-refresh", Status: "idle", GroupName: "legacy-group",
	}); err != nil {
		t.Fatal(err)
	}

	manager, err := BuildManagerWithError(ctx, store, &http.Client{})
	if err != nil {
		t.Fatalf("BuildManagerWithError: %v", err)
	}
	var legacy MailboxProvider
	for _, candidate := range manager.Mailbox {
		if candidate.Name() == "email_pool" {
			legacy = candidate
			break
		}
	}
	if legacy == nil {
		t.Fatalf("email_pool provider missing: %#v", manager.Mailbox)
	}
	email, _, leaseID, err := legacy.CreateEmail(ctx)
	if err != nil {
		t.Fatalf("CreateEmail: %v", err)
	}
	if email != "legacy@example.test" || leaseID != "legacy-mail" {
		t.Fatalf("lease email=%q id=%q", email, leaseID)
	}
	reserved, found, err := store.GetEmailAccount(ctx, leaseID)
	if err != nil || !found || reserved.Status != "in_use" {
		t.Fatalf("reserved row=%+v found=%v err=%v", reserved, found, err)
	}
	if err := legacy.DeleteEmail(ctx, leaseID); err != nil {
		t.Fatalf("DeleteEmail: %v", err)
	}
	released, found, err := store.GetEmailAccount(ctx, leaseID)
	if err != nil || !found || released.Status != "idle" {
		t.Fatalf("released row=%+v found=%v err=%v", released, found, err)
	}
}
