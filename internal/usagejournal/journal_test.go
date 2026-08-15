package usagejournal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestJournalReplayAckAndSegmentCleanup(t *testing.T) {
	dir := t.TempDir()
	journal, err := Open(dir, 320)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		sequence, appendErr := journal.Append(Record{Hold: &storage.BillingHoldWrite{ID: "hold-" + string(rune('a'+i)), EventID: "event", AccountID: "account", EstimatedTokens: int64(i + 1), Create: true}})
		if appendErr != nil || sequence != uint64(i+1) {
			t.Fatalf("append sequence=%d err=%v", sequence, appendErr)
		}
	}
	before, err := journal.Snapshot()
	if err != nil || before.Segments < 2 || before.Pending != 12 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(dir, 320)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	replayed, err := journal.Replay(20)
	if err != nil || len(replayed) != 12 || replayed[0].Sequence != 1 || replayed[11].Sequence != 12 {
		t.Fatalf("replay len=%d err=%v records=%+v", len(replayed), err, replayed)
	}
	if err = journal.Ack(8); err != nil {
		t.Fatal(err)
	}
	after, err := journal.Snapshot()
	if err != nil || after.Pending != 4 || after.Segments >= before.Segments {
		t.Fatalf("after=%+v before=%+v err=%v", after, before, err)
	}
	replayed, err = journal.Replay(20)
	if err != nil || len(replayed) != 4 || replayed[0].Sequence != 9 {
		t.Fatalf("replay after ack=%+v err=%v", replayed, err)
	}
}

func TestJournalReplayHonorsLimitBeforeScanningRemainder(t *testing.T) {
	dir := t.TempDir()
	journal, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for index := 0; index < 20; index++ {
		if _, err = journal.Append(Record{Hold: &storage.BillingHoldWrite{ID: "limited-" + string(rune('a'+index)), Create: true}}); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := journal.Replay(7)
	if err != nil || len(replayed) != 7 || replayed[0].Sequence != 1 || replayed[6].Sequence != 7 {
		t.Fatalf("limited replay=%+v err=%v", replayed, err)
	}
	if err = journal.Ack(7); err != nil {
		t.Fatal(err)
	}
	replayed, err = journal.Replay(7)
	if err != nil || len(replayed) != 7 || replayed[0].Sequence != 8 || replayed[6].Sequence != 14 {
		t.Fatalf("second limited replay=%+v err=%v", replayed, err)
	}
}

func TestJournalPreservesUsagePayloadAndRecoversTornTail(t *testing.T) {
	dir := t.TempDir()
	journal, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	usage := &storage.UsageRecordWrite{AccountID: "account", Total: 42, Raw: json.RawMessage(`{"total_tokens":42}`), Diagnostics: storage.UsageDiagnostics{UsageEventID: "event-42", RouteEpoch: 3}}
	if _, err = journal.Append(Record{Usage: usage}); err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := listSegments(dir)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
	file, err := os.OpenFile(segments[0].path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	replayed, err := journal.Replay(10)
	if err != nil || len(replayed) != 1 || replayed[0].Usage == nil || replayed[0].Usage.Total != 42 || replayed[0].Usage.Diagnostics.RouteEpoch != 3 {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
}

func TestJournalRejectsChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	journal, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = journal.Append(Record{Hold: &storage.BillingHoldWrite{ID: "hold-corrupt", Create: true}}); err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := listSegments(dir)
	if err != nil || len(segments) != 1 {
		t.Fatal(err)
	}
	file, err := os.OpenFile(segments[0].path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte{0xff}, 8); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(filepath.Clean(dir), 1<<20); err == nil {
		t.Fatal("checksum corruption was accepted")
	}
}
