package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
)

func TestGoalChunkFormatV2BootDefaultAndRuntimeOverride(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "goal-v2.sqlite3")
	cfg.GoalChunkFormatV2 = true
	store, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if !store.goalChunkFormatV2Enabled(ctx) {
		t.Fatal("boot-config v2 default was not applied")
	}
	if err := store.SetSetting(ctx, GoalChunkFormatV2Setting, "false"); err != nil {
		t.Fatal(err)
	}
	if store.goalChunkFormatV2Enabled(ctx) {
		t.Fatal("runtime false did not override boot-config v2 default")
	}
}

func TestGoalChunkFormatV2WholePayloadRoundTrip(t *testing.T) {
	payload := strings.Repeat(`{"role":"user","content":"shared-prefix"}`, 5000)
	stream := goalChunkStreamV2(payload)
	if !strings.HasPrefix(stream, goalChunkStreamV2Prefix) {
		t.Fatalf("stream prefix=%q", stream[:min(8, len(stream))])
	}
	decoded, err := decodeGoalChunkStreamV2(stream, maxStoredContextPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != payload {
		t.Fatalf("decoded payload differs: got %d bytes want %d", len(decoded), len(payload))
	}
}

func TestGoalChunkFormatV2RejectsDeclaredLengthTampering(t *testing.T) {
	payload := `{"input":"exact"}`
	stream := goalChunkStreamV2(payload)
	parts := strings.SplitN(strings.TrimPrefix(stream, goalChunkStreamV2Prefix), ":", 3)
	parts[0] = "999"
	_, err := decodeGoalChunkStreamV2(goalChunkStreamV2Prefix+strings.Join(parts, ":"), maxStoredContextPayloadBytes)
	if err == nil {
		t.Fatal("tampered plaintext length unexpectedly decoded")
	}
}

func TestGoalChunkFormatV2EncryptedCommitAndLegacyMigration(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := bytes.Repeat([]byte{0x5a}, 32)
	store.SetTokenEncryptionKey(key)

	// First write with the switch disabled, matching an existing deployment.
	legacyPayload := strings.Repeat("legacy-shared ", 4000)
	legacyTurn := goalTurnForTest("v2-migration-root", "v2-migration-response", legacyPayload, "legacy-output")
	legacy, err := store.CommitGoalTurn(ctx, legacyTurn)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetSetting(ctx, GoalChunkFormatV2Setting, "true"); err != nil {
		t.Fatal(err)
	}
	changed, err := store.MigrateGoalChunkFormatV2(ctx, legacy.ID)
	if err != nil || !changed {
		t.Fatalf("legacy migration changed=%v err=%v", changed, err)
	}
	replayed, _, err := store.BuildGoalReplay(ctx, legacy.ID)
	if err != nil || !bytes.Contains(replayed, []byte("legacy-shared")) {
		t.Fatalf("legacy replay err=%v body_prefix=%q", err, replayed[:min(len(replayed), 120)])
	}

	// New writes are whole-payload compressed and each SQL value remains
	// encrypted. The read path must decrypt, reassemble and verify the original
	// bytes rather than relying on a visible gc2 marker in ciphertext.
	v2Payload := strings.Repeat("v2-shared ", 5000)
	v2Turn := goalTurnForTest("v2-write-root", "v2-write-response", v2Payload, "v2-output")
	v2, err := store.CommitGoalTurn(ctx, v2Turn)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err = store.DB().QueryRowContext(ctx, `SELECT encrypted_payload FROM goal_payload_chunk WHERE goal_id=? ORDER BY chunk_index LIMIT 1`, v2.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encrypted, "enc:") || strings.Contains(encrypted, "v2-shared") {
		t.Fatalf("v2 chunk is not encrypted: %q", encrypted[:min(len(encrypted), 80)])
	}
	replayed, _, err = store.BuildGoalReplay(ctx, v2.ID)
	if err != nil || !bytes.Contains(replayed, []byte("v2-shared")) {
		t.Fatalf("v2 replay err=%v body_prefix=%q", err, replayed[:min(len(replayed), 120)])
	}
}
