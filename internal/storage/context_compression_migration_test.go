package storage

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestGoalAndVirtualContextCompressionIsLosslessAtRest(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.SetTokenEncryptionKey(bytes.Repeat([]byte{0x42}, 32))

	repeated := strings.Repeat("byte-exact-context-保留-", 12<<10)
	turn := goalTurnForTest("compressed-root", "compressed-response", repeated, repeated)
	turn.StorageMaxBytes = 64 << 20
	turn.WorkingState = `{"state":"` + repeated + `"}`
	goal, err := store.CommitGoalTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}

	var encrypted string
	if err = store.DB().QueryRowContext(ctx, `SELECT encrypted_payload FROM goal_payload_chunk
WHERE goal_id=? AND payload_kind=? AND segment_sequence=1 ORDER BY chunk_index LIMIT 1`,
		goal.ID, goalChunkSegment).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	durable := store.openToken(encrypted)
	if !strings.HasPrefix(durable, compressedContextPrefix) {
		t.Fatalf("large goal chunk is not compressed at rest: stored=%d", len(durable))
	}
	decoded, err := decompressContextPayloadChecked(durable, maxStoredContextPayloadBytes)
	if err != nil || len(decoded) == 0 {
		t.Fatalf("decode stored goal chunk len=%d err=%v", len(decoded), err)
	}

	body, session, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), repeated) || session.WorkingState != turn.WorkingState {
		t.Fatal("goal replay or working state changed after storage compression")
	}

	item := VirtualLedgerItem{
		RouteKeyHash: "compressed-route",
		AccountID:    "compressed-account",
		Content:      repeated,
		RawJSON:      `{"payload":"` + repeated + `"}`,
	}
	if err = store.InsertVirtualLedger(ctx, item); err != nil {
		t.Fatal(err)
	}
	var storedContent, storedRaw string
	if err = store.DB().QueryRowContext(ctx, `SELECT content,raw_json FROM virtual_context_ledger
WHERE route_key_hash=?`, item.RouteKeyHash).Scan(&storedContent, &storedRaw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedContent, compressedContextPrefix) || !strings.HasPrefix(storedRaw, compressedContextPrefix) {
		t.Fatalf("virtual context was not compressed: content=%d raw=%d", len(storedContent), len(storedRaw))
	}
	items, err := store.ListVirtualLedger(ctx, item.RouteKeyHash, 1)
	if err != nil || len(items) != 1 || items[0].Content != item.Content || items[0].RawJSON != item.RawJSON {
		t.Fatalf("virtual context round trip changed: items=%d err=%v", len(items), err)
	}
}

func TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.SetTokenEncryptionKey(bytes.Repeat([]byte{0x24}, 32))

	repeated := strings.Repeat("legacy-context-to-compress-", 8<<10)
	turn := goalTurnForTest("legacy-compress-root", "legacy-compress-response", repeated, "done")
	turn.WorkingState = repeated
	turn.StorageMaxBytes = 32 << 20
	goal, err := store.CommitGoalTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}

	var rowID, payloadBytes int64
	var oldCipher string
	if err = store.DB().QueryRowContext(ctx, `SELECT rowid,payload_bytes,encrypted_payload
FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=1
ORDER BY chunk_index LIMIT 1`, goal.ID, goalChunkSegment).Scan(&rowID, &payloadBytes, &oldCipher); err != nil {
		t.Fatal(err)
	}
	legacyPlain := turn.SegmentPayload[:int(payloadBytes)]
	legacyCipher := store.sealToken(legacyPlain)
	if _, err = store.DB().ExecContext(ctx, `UPDATE goal_payload_chunk SET encrypted_payload=? WHERE rowid=?`, legacyCipher, rowID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE goal_session
SET encrypted_working_state=?,storage_bytes=MAX(0,storage_bytes+?) WHERE id=?`,
		store.sealToken(turn.WorkingState), len(legacyCipher)-len(oldCipher), goal.ID); err != nil {
		t.Fatal(err)
	}
	journalPayload := `{"journal":"` + repeated + `"}`
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO context_journal
(response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at)
VALUES(?,?,?,?,?,?)`, "legacy-journal", "affinity", "account", store.sealToken(journalPayload), Now(), Now()+3600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO virtual_context_ledger
(route_key_hash,account_id,content,raw_json,created_at) VALUES(?,?,?,?,?)`,
		"legacy-virtual", "account", repeated, journalPayload, Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key=?`, contextPayloadCompressionMigrationMarker); err != nil {
		t.Fatal(err)
	}

	if err = store.migrateStoredContextCompression(ctx); err != nil {
		t.Fatal(err)
	}
	var migratedCipher string
	if err = store.DB().QueryRowContext(ctx, `SELECT encrypted_payload FROM goal_payload_chunk WHERE rowid=?`, rowID).Scan(&migratedCipher); err != nil {
		t.Fatal(err)
	}
	if migratedCipher == legacyCipher || !strings.HasPrefix(store.openToken(migratedCipher), compressedContextPrefix) || len(migratedCipher) >= len(legacyCipher) {
		t.Fatalf("legacy goal chunk was not reduced: before=%d after=%d", len(legacyCipher), len(migratedCipher))
	}
	body, session, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(body), repeated) || session.WorkingState != turn.WorkingState {
		t.Fatalf("migrated goal changed body/session: body=%d err=%v", len(body), err)
	}
	journal, err := store.GetContextJournal(ctx, "legacy-journal")
	if err != nil || journal.Payload != journalPayload {
		t.Fatalf("migrated journal changed: len=%d err=%v", len(journal.Payload), err)
	}
	items, err := store.ListVirtualLedger(ctx, "legacy-virtual", 1)
	if err != nil || len(items) != 1 || items[0].Content != repeated || items[0].RawJSON != journalPayload {
		t.Fatalf("migrated virtual context changed: items=%d err=%v", len(items), err)
	}
	var marker int
	if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, contextPayloadCompressionMigrationMarker).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("compression marker=%d err=%v", marker, err)
	}

	firstCipher := migratedCipher
	if err = store.migrateStoredContextCompression(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT encrypted_payload FROM goal_payload_chunk WHERE rowid=?`, rowID).Scan(&migratedCipher); err != nil {
		t.Fatal(err)
	}
	if migratedCipher != firstCipher {
		t.Fatal("completed compression migration rewrote an already-compressed row")
	}
}
