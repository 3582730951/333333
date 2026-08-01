package mailbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestEmailPoolProviderExchangesEncodedMicrosoftCredentials(t *testing.T) {
	var receivedClientID, receivedRefresh, receivedScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		receivedClientID = r.Form.Get("client_id")
		receivedRefresh = r.Form.Get("refresh_token")
		receivedScope = r.Form.Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-fixture"}`))
	}))
	t.Cleanup(server.Close)
	provider := NewEmailPoolProvider(nil, server.Client())
	provider.tokenURL = server.URL
	token, err := provider.exchangeMicrosoftToken(context.Background(), storage.EmailAccount{
		ClientID: "client+id&value", RefreshToken: "refresh+token&value=1",
	})
	if err != nil {
		t.Fatalf("exchangeMicrosoftToken: %v", err)
	}
	if token != "access-fixture" || receivedClientID != "client+id&value" ||
		receivedRefresh != "refresh+token&value=1" || receivedScope != emailPoolMicrosoftScope {
		t.Fatalf("token=%q client=%q refresh=%q scope=%q", token, receivedClientID, receivedRefresh, receivedScope)
	}
}

func TestEmailPoolProviderSkipsIncompleteLegacyRowsWithoutLeakingSecrets(t *testing.T) {
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
		ID: "bad", Email: "bad@example.test", RefreshToken: "secret-invalid-refresh", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertEmailAccount(ctx, storage.EmailAccount{
		ID: "good", Email: "good@example.test", ClientID: "client", RefreshToken: "refresh", Status: "idle", CreatedAt: storage.Now() + 1,
	}); err != nil {
		t.Fatal(err)
	}
	provider := NewEmailPoolProvider(store, nil)
	email, _, leaseID, err := provider.CreateEmail(ctx)
	if err != nil {
		t.Fatalf("CreateEmail: %v", err)
	}
	if email != "good@example.test" || leaseID != "good" {
		t.Fatalf("email=%q lease=%q", email, leaseID)
	}
	invalid, found, err := store.GetEmailAccount(ctx, "bad")
	if err != nil || !found || invalid.Status != "error" || strings.Contains(invalid.ErrorMessage, "secret-invalid-refresh") {
		t.Fatalf("invalid row=%+v found=%v err=%v", invalid, found, err)
	}
	if err := provider.DeleteEmail(ctx, leaseID); err != nil {
		t.Fatal(err)
	}
}
