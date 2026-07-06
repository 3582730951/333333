package storage

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/secretbox"
)

func TestCodexReauthConfigEncryptsSecretsAndPublicReadOmitsThem(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "reauth.sqlite3")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("deployment-secret"))
	if err := store.UpsertAccount(ctx, Account{ID: "acc-reauth", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	cfg := AccountCodexReauthConfig{
		AccountID:         "acc-reauth",
		LoginEmail:        "teacher@example.internal",
		Password:          "correct horse battery staple",
		OTPURL:            "https://otp.example/internal",
		TargetWorkspaceID: "workspace-teacher",
		AutoEnabled:       true,
		LastStatus:        "configured",
	}
	if err := store.UpsertCodexReauthConfig(ctx, cfg); err != nil {
		t.Fatalf("upsert config: %v", err)
	}

	got, found, err := store.GetCodexReauthConfig(ctx, "acc-reauth")
	if err != nil || !found {
		t.Fatalf("get config found=%v err=%v", found, err)
	}
	if got.Password != cfg.Password || got.OTPURL != cfg.OTPURL || !got.AutoEnabled || got.TargetWorkspaceID != cfg.TargetWorkspaceID {
		t.Fatalf("decrypted config mismatch: %+v", got)
	}
	pub, found, err := store.GetCodexReauthConfigPublic(ctx, "acc-reauth")
	if err != nil || !found {
		t.Fatalf("get public found=%v err=%v", found, err)
	}
	if pub.Password != "" || pub.OTPURL != "" {
		t.Fatalf("public config leaked secrets: %+v", pub)
	}
	if !pub.PasswordConfigured || !pub.OTPURLConfigured {
		t.Fatalf("public config should expose configured booleans: %+v", pub)
	}

	store.Close()
	raw, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var storedPassword, storedOTP string
	if err := raw.DB().QueryRow(`SELECT encrypted_password, encrypted_otp_url FROM account_codex_reauth_config WHERE account_id = ?`, "acc-reauth").Scan(&storedPassword, &storedOTP); err != nil {
		t.Fatal(err)
	}
	if storedPassword == cfg.Password || storedOTP == cfg.OTPURL || !secretbox.IsSealed(storedPassword) || !secretbox.IsSealed(storedOTP) {
		t.Fatalf("reauth secrets not encrypted at rest: password=%q otp=%q", storedPassword, storedOTP)
	}
}

func TestCodexReauthJobQueueDedupesQueuedAndRunningJobs(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "jobs.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, Account{ID: "acc-job", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	first, created, err := store.EnqueueCodexReauthJob(ctx, "acc-job", "auth_expired")
	if err != nil || !created {
		t.Fatalf("first enqueue created=%v err=%v job=%+v", created, err, first)
	}
	second, created, err := store.EnqueueCodexReauthJob(ctx, "acc-job", "duplicate")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("queued duplicate should return existing job: created=%v err=%v job=%+v first=%+v", created, err, second, first)
	}
	if err := store.UpdateCodexReauthJobStatus(ctx, first.ID, "running", ""); err != nil {
		t.Fatal(err)
	}
	third, created, err := store.EnqueueCodexReauthJob(ctx, "acc-job", "duplicate-running")
	if err != nil || created || third.ID != first.ID {
		t.Fatalf("running duplicate should return existing job: created=%v err=%v job=%+v first=%+v", created, err, third, first)
	}
	if err := store.UpdateCodexReauthJobStatus(ctx, first.ID, "failed", "bad password"); err != nil {
		t.Fatal(err)
	}
	fourth, created, err := store.EnqueueCodexReauthJob(ctx, "acc-job", "retry")
	if err != nil || !created || fourth.ID == first.ID {
		t.Fatalf("terminal job should allow retry: created=%v err=%v job=%+v first=%+v", created, err, fourth, first)
	}
}
