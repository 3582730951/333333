package storage

import (
	"context"
	"strings"
	"testing"
)

func TestTurboGPTRegisterJobTokenAndConfigPersistence(t *testing.T) {
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("turbo-register-test-secret"))
	ctx := context.Background()
	job := TurboGPTRegisterJob{
		ID: "tgr_test", Status: "pending", Phase: "phase1", Password: "password-secret",
		ConfigJSON: `{}`, ResultJSON: `{}`, AutoImport: true,
	}
	if err := store.CreateTurboGPTRegisterJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTurboGPTRegisterJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != job.Password || !got.AutoImport {
		t.Fatalf("job round trip mismatch: %+v", got)
	}
	var storedPassword string
	if err := store.rdb.QueryRowContext(ctx, `SELECT password FROM turbo_gpt_register_jobs WHERE id=?`, job.ID).Scan(&storedPassword); err != nil {
		t.Fatal(err)
	}
	if storedPassword == job.Password || !strings.HasPrefix(storedPassword, "enc:v1:") {
		t.Fatalf("password not encrypted at rest: %q", storedPassword)
	}

	token := TurboGPTRegisterToken{
		JobID: job.ID, Email: "user@example.com", AccessToken: "access-secret",
		RefreshToken: "refresh-secret", IDToken: "id-secret", RawJSON: `{"refresh_token":"refresh-secret"}`,
	}
	if err := store.UpsertTurboGPTRegisterToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	gotToken, err := store.GetTurboGPTRegisterToken(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotToken.RefreshToken != token.RefreshToken || gotToken.RawJSON != token.RawJSON {
		t.Fatalf("token round trip mismatch: %+v", gotToken)
	}

	if err := store.SetTurboGPTRegisterConfig(ctx, "hero_sms_api_key", "secret-key"); err != nil {
		t.Fatal(err)
	}
	config, err := store.GetTurboGPTRegisterConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if config["hero_sms_api_key"] != "secret-key" {
		t.Fatalf("config round trip = %#v", config)
	}
}

func TestInitCreatesBuiltInBaseGroups(t *testing.T) {
	store := newTestStore(t)
	for _, name := range []string{"cyber", "kiro", "antigravity"} {
		if _, err := store.GetGroup(context.Background(), name); err != nil {
			t.Fatalf("missing built-in group %s: %v", name, err)
		}
	}
}
