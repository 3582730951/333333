package storage

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestContextJournalEncryptedAtRestAndReadableAfterRestart(t *testing.T) {
	path := t.TempDir() + "/journal.sqlite3"
	key := bytes.Repeat([]byte{7}, 32)
	store, err := Open(path)
	if err != nil { t.Fatal(err) }
	store.SetTokenEncryptionKey(key)
	if err := store.Init(context.Background()); err != nil { t.Fatal(err) }
	payload := `{"input":[{"role":"user","content":"secret-context"}]}`
	if err := store.PutContextJournal(context.Background(), ContextJournal{ResponseID: "resp_1", Payload: payload, ExpiresAt: Now()+60}); err != nil { t.Fatal(err) }
	var encrypted string
	if err := store.DB().QueryRow(`SELECT encrypted_payload FROM context_journal WHERE response_id='resp_1'`).Scan(&encrypted); err != nil { t.Fatal(err) }
	if strings.Contains(encrypted, "secret-context") { t.Fatal("journal persisted plaintext") }
	_ = store.Close()

	store, err = Open(path)
	if err != nil { t.Fatal(err) }
	defer store.Close()
	store.SetTokenEncryptionKey(key)
	if err := store.Init(context.Background()); err != nil { t.Fatal(err) }
	journal, err := store.GetContextJournal(context.Background(), "resp_1")
	if err != nil || journal.Payload != payload { t.Fatalf("journal=%+v err=%v", journal, err) }
}

