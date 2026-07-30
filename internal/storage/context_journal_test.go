package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextJournalEncryptedAtRestAndReadableAfterRestart(t *testing.T) {
	path := t.TempDir() + "/journal.sqlite3"
	key := bytes.Repeat([]byte{7}, 32)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(key)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := `{"input":[{"role":"user","content":"secret-context"}]}`
	if err := store.PutContextJournal(context.Background(), ContextJournal{ResponseID: "resp_1", Payload: payload, ExpiresAt: Now() + 60}); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := store.DB().QueryRow(`SELECT encrypted_payload FROM context_journal WHERE response_id='resp_1'`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, "secret-context") {
		t.Fatal("journal persisted plaintext")
	}
	_ = store.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetTokenEncryptionKey(key)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := store.GetContextJournal(context.Background(), "resp_1")
	if err != nil || journal.Payload != payload {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
}

func TestContextJournalDefaultTTLMigrationIsOneTime(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "ttl.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, "context_journal_ttl_seconds", "86400"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key='context_journal_ttl_1h_migrated'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	value, _, err := store.GetSetting(ctx, "context_journal_ttl_seconds")
	if err != nil || value != "3600" {
		t.Fatalf("migrated ttl=%q err=%v", value, err)
	}
	if err := store.SetSetting(ctx, "context_journal_ttl_seconds", "86400"); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	value, _, err = store.GetSetting(ctx, "context_journal_ttl_seconds")
	if err != nil || value != "86400" {
		t.Fatalf("one-time migration overwrote admin value: ttl=%q err=%v", value, err)
	}
}

func TestContextJournalCompressionRoundTripAndLegacyCompatibility(t *testing.T) {
	payload := strings.Repeat(`{"role":"assistant","content":"compressible context"}`, 400)
	compressed := compressContextPayload(payload)
	if compressed == payload || !strings.HasPrefix(compressed, compressedContextPrefix) {
		t.Fatal("large context was not compressed")
	}
	if len(compressed) >= len(payload)/2 {
		t.Fatalf("compression ineffective: before=%d after=%d", len(payload), len(compressed))
	}
	if got, err := decompressContextPayloadChecked(compressed, maxStoredContextPayloadBytes); err != nil || got != payload {
		t.Fatalf("compressed round trip changed payload: %v", err)
	}
	if got, err := decompressContextPayloadChecked(payload, maxStoredContextPayloadBytes); err != nil || got != payload {
		t.Fatalf("legacy compatibility failed: %v", err)
	}
}

func TestContextCodecEscapesMarkersReadsLegacyAndBoundsExpansion(t *testing.T) {
	var legacy bytes.Buffer
	zw := gzip.NewWriter(&legacy)
	_, _ = zw.Write([]byte("legacy-value"))
	_ = zw.Close()
	legacyValue := legacyCompressedContextPrefix + base64.RawStdEncoding.EncodeToString(legacy.Bytes())
	if got, err := decompressContextPayloadChecked(legacyValue, 1024); err != nil || got != "legacy-value" {
		t.Fatalf("legacy gzip got=%q err=%v", got, err)
	}

	for _, literal := range []string{
		legacyValue,
		"gz1:not-a-codec",
		compressedContextPrefix + "1:x",
		rawContextPrefix + "1:x",
	} {
		durable := compressContextPayload(literal)
		got, err := decompressContextPayloadChecked(durable, 4096)
		if err != nil || got != literal {
			t.Fatalf("literal marker changed: durable=%q got=%q err=%v", durable, got, err)
		}
	}

	bomb := compressContextPayload(strings.Repeat("x", 64<<10))
	if _, err := decompressContextPayloadChecked(bomb, 1024); err == nil {
		t.Fatal("length-bearing gzip exceeded caller decompression limit")
	}
	legacy.Reset()
	zw = gzip.NewWriter(&legacy)
	_, _ = zw.Write([]byte(strings.Repeat("y", 64<<10)))
	_ = zw.Close()
	if _, err := decompressContextPayloadChecked(legacyCompressedContextPrefix+base64.RawStdEncoding.EncodeToString(legacy.Bytes()), 1024); err == nil {
		t.Fatal("legacy gzip exceeded streaming decompression limit")
	}
}
