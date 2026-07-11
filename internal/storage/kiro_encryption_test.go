package storage

import (
	"codex-account-pool/internal/secretbox"
	"context"
	"testing"
)

func TestKiroCredentialsEncryptSecretsAndExposeSummary(t *testing.T) {
	s := newTestStore(t)
	s.SetTokenEncryptionKey([]byte("deployment-secret"))
	ctx := context.Background()
	if err := s.UpsertAccount(ctx, Account{ID: "k", Label: "k", Provider: "kiro", Status: "active"}, AccountToken{}); err != nil {
		t.Fatal(err)
	}
	c := KiroCredentials{AccountID: "k", AuthMethod: "idc", ClientID: "client", ClientSecret: "secret", KiroAPIKey: "ksk", AuthRegion: "us-east-1", APIRegion: "us-east-1", CredentialHash: "hash"}
	if err := s.UpsertKiroCredentials(ctx, c); err != nil {
		t.Fatal(err)
	}
	var sec, key string
	if err := s.rdb.QueryRowContext(ctx, `SELECT client_secret,kiro_api_key FROM account_kiro_credentials WHERE account_id='k'`).Scan(&sec, &key); err != nil {
		t.Fatal(err)
	}
	if !secretbox.IsSealed(sec) || !secretbox.IsSealed(key) {
		t.Fatalf("not encrypted: %q %q", sec, key)
	}
	got, err := s.GetKiroCredentials(ctx, "k")
	if err != nil || got.ClientSecret != "secret" || got.KiroAPIKey != "ksk" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	summary, err := s.KiroAuthSummary(ctx, "k")
	if err != nil || !summary.HasClientSecret || !summary.HasAPIKey {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
}
