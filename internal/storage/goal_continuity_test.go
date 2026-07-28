package storage

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func goalTurnForTest(alias, response, input, output string) GoalTurn {
	return GoalTurn{
		Protocol:          "codex",
		DownstreamKeyHash: "downstream-hash",
		WorkspaceHash:     "workspace-hash",
		InitialGoalHash:   "initial-goal-hash",
		ResponseID:        response,
		Aliases:           []GoalAlias{{Type: "codex_root_thread", Value: alias}},
		CheckpointPayload: `{"model":"gpt-test","tools":[{"type":"function","name":"keep"}],"input":[]}`,
		SegmentPayload:    `{"input":[{"role":"user","content":"` + input + `"}],"output":[{"type":"message","content":[{"type":"output_text","text":"` + output + `"}]}]}`,
		WorkingState:      `{"latest":"` + input + `"}`,
		ExpiresAt:         Now() + 86400,
		StorageMaxBytes:   8 << 20,
		CompressionStages: 16,
	}
}

func TestGoalContinuityEncryptedReplaySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/goals.sqlite3"
	key := bytes.Repeat([]byte{9}, 32)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(key)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := store.CommitGoalTurn(ctx, goalTurnForTest("root-1", "resp-1", "secret-first-turn", "secret-output"))
	if err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := store.DB().QueryRowContext(ctx, `SELECT encrypted_payload FROM goal_segment WHERE goal_id=?`, created.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, "secret-first-turn") || strings.Contains(encrypted, "secret-output") {
		t.Fatal("goal segment was persisted in plaintext")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetTokenEncryptionKey(key)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "response_id", Value: "resp-1"}})
	if err != nil || resolved.Session.ID != created.ID {
		t.Fatalf("response alias resolution=%+v err=%v", resolved, err)
	}
	body, session, err := store.BuildGoalReplay(ctx, created.ID)
	if err != nil || session.ID != created.ID || !strings.Contains(string(body), "secret-first-turn") || !strings.Contains(string(body), "secret-output") {
		t.Fatalf("replay=%s session=%+v err=%v", body, session, err)
	}
}

func TestGoalContinuityFallbackRejectsAmbiguity(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitGoalTurn(ctx, goalTurnForTest("root-a", "resp-a", "first", "out-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitGoalTurn(ctx, goalTurnForTest("root-b", "resp-b", "second", "out-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveFallbackGoal(ctx, "downstream-hash", "workspace-hash", "initial-goal-hash"); !errors.Is(err, ErrGoalAmbiguous) {
		t.Fatalf("fallback ambiguity error=%v, want %v", err, ErrGoalAmbiguous)
	}
}

func TestGoalRunLeasePreventsDuplicateResumeAndSafeCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("root-lease", "resp-lease", "work", "out"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.AcquireGoalRun(ctx, goal.ID, "request-a", "running", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireGoalRun(ctx, goal.ID, "request-b", "running", time.Minute); !errors.Is(err, ErrGoalInProgress) {
		t.Fatalf("second resume error=%v, want %v", err, ErrGoalInProgress)
	}
	if err := store.DeleteGoalSafely(ctx, goal.ID); !errors.Is(err, ErrGoalActiveCannotBePurged) {
		t.Fatalf("active cleanup error=%v, want %v", err, ErrGoalActiveCannotBePurged)
	}
	if err := store.MarkGoalRetryable(ctx, goal.ID, "upstream_eof_without_terminal"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishGoalRun(ctx, run.ID, "request-a", "completed", ""); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetGoalDetail(ctx, goal.ID)
	if err != nil || detail.Session.State != "retryable" || detail.LatestRun == nil || detail.LatestRun.State != "retryable" || detail.LatestRun.FailureCode != "goal_stream_interrupted" {
		t.Fatalf("interrupted goal must remain retryable detail=%+v err=%v", detail, err)
	}
	if err := store.DeleteGoalSafely(ctx, goal.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGoalChildBranchDoesNotMergeIntoParentHistory(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := store.CommitGoalTurn(ctx, goalTurnForTest("root-tree", "resp-root-tree", "root-input", "root-output"))
	if err != nil {
		t.Fatal(err)
	}
	childTurn := goalTurnForTest("root-tree", "resp-child-tree", "child-input", "child-output")
	childTurn.ParentGoalID = root.ID
	childTurn.BranchHash = "child-thread-hash"
	childTurn.Aliases = append(childTurn.Aliases, GoalAlias{Type: "codex_branch_thread", Value: "child-thread"})
	child, err := store.CommitGoalTurn(ctx, childTurn)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID == root.ID || child.ParentGoalID != root.ID {
		t.Fatalf("child=%+v root=%+v", child, root)
	}
	rootReplay, _, err := store.BuildGoalReplay(ctx, root.ID)
	if err != nil || strings.Contains(string(rootReplay), "child-input") || !strings.Contains(string(rootReplay), "root-input") {
		t.Fatalf("root replay=%s err=%v", rootReplay, err)
	}
	childReplay, _, err := store.BuildGoalReplay(ctx, child.ID)
	if err != nil || strings.Contains(string(childReplay), "root-input") || !strings.Contains(string(childReplay), "child-input") {
		t.Fatalf("child replay=%s err=%v", childReplay, err)
	}
}

func TestGoalClaudeReplayUsesNativeMessagesAndAssistantBlocks(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	turn := GoalTurn{
		Protocol:          "claude",
		DownstreamKeyHash: "downstream-hash",
		WorkspaceHash:     "workspace-hash",
		InitialGoalHash:   "initial-goal-hash",
		ResponseID:        "msg-goal-1",
		Aliases:           []GoalAlias{{Type: "claude_code_session", Value: "claude-session-1"}},
		CheckpointPayload: `{"model":"claude-sonnet-4.6","system":"keep system","messages":[]}`,
		SegmentPayload:    `{"history_key":"messages","input":[{"role":"user","content":"first"}],"output":[{"id":"msg-goal-1","role":"assistant","content":[{"type":"text","text":"answer"}]}]}`,
		WorkingState:      `{}`,
		ExpiresAt:         Now() + 86400,
		StorageMaxBytes:   8 << 20,
		CompressionStages: 16,
	}
	goal, err := store.CommitGoalTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"input"`) || !strings.Contains(string(body), `"messages"`) || !strings.Contains(string(body), `"first"`) || !strings.Contains(string(body), `"answer"`) || !strings.Contains(string(body), `"keep system"`) {
		t.Fatalf("claude replay lost native history/body=%s", body)
	}
}

func TestGoalCompactionRunsInResumableChunksWithoutLosingTurns(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	first := goalTurnForTest("compact-root", "compact-r1", "turn-one", "out-one")
	first.CompressionStages = 1
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ response, input, output string }{
		{"compact-r2", "turn-two", "out-two"},
		{"compact-r3", "turn-three", "out-three"},
	} {
		turn := goalTurnForTest("compact-root", item.response, item.input, item.output)
		turn.CompressionStages = 1
		if _, err := store.CommitGoalTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	needed, err := store.NeedsGoalCompaction(ctx, goal.ID, 1)
	if err != nil || !needed {
		t.Fatalf("need compaction=%v err=%v", needed, err)
	}
	if err := store.CompactGoalSegmentsWithRatio(ctx, goal.ID, 1, 0.5); err != nil {
		t.Fatal(err)
	}
	needed, err = store.NeedsGoalCompaction(ctx, goal.ID, 1)
	if err != nil || !needed {
		t.Fatalf("one bounded chunk should leave resumable work need=%v err=%v", needed, err)
	}
	if err := store.CompactGoalSegmentsWithRatio(ctx, goal.ID, 1, 0.5); err != nil {
		t.Fatal(err)
	}
	needed, err = store.NeedsGoalCompaction(ctx, goal.ID, 1)
	if err != nil || needed {
		t.Fatalf("compaction should finish after next chunk need=%v err=%v", needed, err)
	}
	body, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(body), "turn-one") || !strings.Contains(string(body), "turn-two") || !strings.Contains(string(body), "turn-three") || !strings.Contains(string(body), "out-three") || strings.Count(string(body), "turn-one") != 1 || strings.Count(string(body), "turn-two") != 1 || strings.Count(string(body), "turn-three") != 1 {
		t.Fatalf("chunked compaction lost replay body=%s err=%v", body, err)
	}
}

func TestGoalV2UsesBoundedEncryptedChunksAndAdvancesCheckpointWithoutRewriting(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(bytes.Repeat([]byte{7}, 32))
	largeInput := strings.Repeat("chunk-data-", 20<<10)
	first := goalTurnForTest("chunk-root", "chunk-r1", largeInput, "out-one")
	first.StorageMaxBytes = 16 << 20
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ response, input, output string }{{"chunk-r2", "turn-two", "out-two"}, {"chunk-r3", "turn-three", "out-three"}} {
		turn := goalTurnForTest("chunk-root", item.response, item.input, item.output)
		turn.StorageMaxBytes = 16 << 20
		if _, err = store.CommitGoalTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	var chunks, maxPlainChunk, legacyPayloadBytes int64
	if err = store.DB().QueryRow(`SELECT COUNT(*),COALESCE(MAX(payload_bytes),0) FROM goal_payload_chunk WHERE goal_id=?`, goal.ID).Scan(&chunks, &maxPlainChunk); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT (SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_checkpoint WHERE goal_id=?)+(SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_segment WHERE goal_id=?)`, goal.ID, goal.ID).Scan(&legacyPayloadBytes); err != nil {
		t.Fatal(err)
	}
	if chunks < 5 || maxPlainChunk > goalPayloadChunkSize || legacyPayloadBytes != 0 {
		t.Fatalf("chunks=%d max_plain=%d legacy_bytes=%d", chunks, maxPlainChunk, legacyPayloadBytes)
	}
	before, err := store.GetGoalSession(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	type chunkRecord struct {
		index     int
		encrypted string
	}
	rows, err := store.DB().Query(`SELECT chunk_index,encrypted_payload FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=1 ORDER BY chunk_index`, goal.ID, goalChunkSegment)
	if err != nil {
		t.Fatal(err)
	}
	var immutable []chunkRecord
	for rows.Next() {
		var item chunkRecord
		if err = rows.Scan(&item.index, &item.encrypted); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		immutable = append(immutable, item)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.CompactGoalSegmentsWithRatio(ctx, goal.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetGoalSession(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints, segments int
	if err = store.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?),(SELECT COUNT(*) FROM goal_segment WHERE goal_id=?)`, goal.ID, goal.ID).Scan(&checkpoints, &segments); err != nil {
		t.Fatal(err)
	}
	if before.StorageBytes != after.StorageBytes || checkpoints != 1 || segments != 2 {
		t.Fatalf("storage before=%d after=%d checkpoints=%d segments=%d", before.StorageBytes, after.StorageBytes, checkpoints, segments)
	}
	for _, item := range immutable {
		var encrypted string
		if err = store.DB().QueryRow(`SELECT encrypted_payload FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=1 AND chunk_index=?`, goal.ID, goalChunkSegment, item.index).Scan(&encrypted); err != nil || encrypted != item.encrypted {
			t.Fatalf("checkpoint rewrote immutable chunk index=%d err=%v", item.index, err)
		}
	}
	body, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(body), largeInput) || !strings.Contains(string(body), "turn-two") || !strings.Contains(string(body), "turn-three") {
		t.Fatalf("chunked replay lost history len=%d err=%v", len(body), err)
	}
}

func TestGoalV2MigratesLegacyCheckpointIncrementally(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "goal-v1.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetTokenEncryptionKey(bytes.Repeat([]byte{8}, 32))
	if _, err = store.DB().ExecContext(ctx, `
CREATE TABLE goal_session(id TEXT PRIMARY KEY,protocol TEXT NOT NULL,parent_goal_id TEXT NOT NULL DEFAULT '',branch_hash TEXT NOT NULL DEFAULT '',downstream_key_hash TEXT NOT NULL DEFAULT '',workspace_hash TEXT NOT NULL DEFAULT '',initial_goal_hash TEXT NOT NULL DEFAULT '',last_response_hash TEXT NOT NULL DEFAULT '',state TEXT NOT NULL DEFAULT 'ready',current_checkpoint_id TEXT NOT NULL DEFAULT '',encrypted_working_state TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE goal_checkpoint(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL,sequence INTEGER NOT NULL,through_segment_sequence INTEGER NOT NULL DEFAULT 0,payload_hash TEXT NOT NULL,payload_bytes INTEGER NOT NULL,encrypted_payload TEXT NOT NULL,created_at INTEGER NOT NULL,UNIQUE(goal_id,sequence));
CREATE TABLE goal_segment(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL,sequence INTEGER NOT NULL,payload_hash TEXT NOT NULL,payload_bytes INTEGER NOT NULL,encrypted_payload TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'committed',created_at INTEGER NOT NULL,UNIQUE(goal_id,sequence));`); err != nil {
		t.Fatal(err)
	}
	now := Now()
	base := `{"model":"gpt-test","input":[]}`
	segments := []string{
		`{"history_key":"input","input":[{"role":"user","content":"legacy-one"}],"output":[{"type":"message","content":[{"type":"output_text","text":"legacy-out-one"}]}]}`,
		`{"history_key":"input","input":[{"role":"user","content":"legacy-two"}],"output":[{"type":"message","content":[{"type":"output_text","text":"legacy-out-two"}]}]}`,
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO goal_session(id,protocol,current_checkpoint_id,encrypted_working_state,expires_at,created_at,updated_at) VALUES('legacy-goal','codex','legacy-cp',?,?,?,?)`, store.sealToken(`{"legacy":true}`), now+86400, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,created_at) VALUES('legacy-cp','legacy-goal',1,0,?,?,?,?)`, hashGoalPayload(base), len(base), store.sealToken(base), now); err != nil {
		t.Fatal(err)
	}
	for i, payload := range segments {
		if _, err = store.DB().ExecContext(ctx, `INSERT INTO goal_segment(id,goal_id,sequence,payload_hash,payload_bytes,encrypted_payload,state,created_at) VALUES(?,?,?,?,?,?,'committed',?)`, "legacy-segment-"+string(rune('1'+i)), "legacy-goal", i+1, hashGoalPayload(payload), len(payload), store.sealToken(payload), now); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.BuildGoalReplay(ctx, "legacy-goal")
	if err != nil || !strings.Contains(string(before), "legacy-one") || !strings.Contains(string(before), "legacy-two") {
		t.Fatalf("legacy replay before migration=%s err=%v", before, err)
	}
	if err = store.CompactGoalSegmentsWithRatio(ctx, "legacy-goal", 1, 1); err != nil {
		t.Fatal(err)
	}
	after, _, err := store.BuildGoalReplay(ctx, "legacy-goal")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy replay changed during migration\nbefore=%s\nafter=%s\nerr=%v", before, after, err)
	}
	var format, checkpoints, chunks int
	var stored, actual int64
	if err = store.DB().QueryRow(`SELECT format_version FROM goal_checkpoint WHERE id=(SELECT current_checkpoint_id FROM goal_session WHERE id='legacy-goal')`).Scan(&format); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id='legacy-goal'`).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM goal_payload_chunk WHERE goal_id='legacy-goal'`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT storage_bytes FROM goal_session WHERE id='legacy-goal'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_checkpoint WHERE goal_id='legacy-goal'),0)+COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_segment WHERE goal_id='legacy-goal'),0)+COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_payload_chunk WHERE goal_id='legacy-goal'),0)`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if format != 2 || checkpoints != 1 || chunks == 0 || stored != actual {
		t.Fatalf("format=%d checkpoints=%d chunks=%d storage=%d actual=%d", format, checkpoints, chunks, stored, actual)
	}
}

func TestGoalV2StorageAccountingMigrationIsMarked(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.DB().ExecContext(ctx, `UPDATE settings SET value='1' WHERE key=?`, goalContinuityV2MigrationMarker); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO goal_session(id,protocol,expires_at,created_at,updated_at,storage_bytes) VALUES('post-marker','codex',?,?,?,123)`, Now()+3600, Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateGoalContinuityV2(ctx); err != nil {
		t.Fatal(err)
	}
	var stored int64
	if err := store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id='post-marker'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 123 {
		t.Fatalf("marked migration recalculated storage_bytes=%d, want 123", stored)
	}
}

func TestGoalCleanupNeverEvictsExpiredLiveRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("expired-live", "expired-live-r1", "work", "out"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.AcquireGoalRun(ctx, goal.ID, "live-owner", "running", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE goal_session SET expires_at=? WHERE id=?`, Now()-1, goal.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.CleanupGoalContinuity(ctx); err != nil || deleted != 0 {
		t.Fatalf("live expired goal cleanup deleted=%d err=%v", deleted, err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_session WHERE id=?`, goal.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("live goal missing count=%d err=%v", count, err)
	}
	if err := store.FinishGoalRun(ctx, run.ID, "live-owner", "completed", ""); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.CleanupGoalContinuity(ctx); err != nil || deleted != 1 {
		t.Fatalf("completed expired goal cleanup deleted=%d err=%v", deleted, err)
	}
}

func goalEncryptedPayloadBytes(t *testing.T, store *Store) int64 {
	t.Helper()
	var stored int64
	if err := store.DB().QueryRow(`SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestGoalStorageBudgetReclaimsLeastRecentlyUsedInactiveGoal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 4096)
	first := goalTurnForTest("budget-old", "budget-old-response", large, large)
	oldGoal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	used := goalEncryptedPayloadBytes(t, store)
	second := goalTurnForTest("budget-new", "budget-new-response", large, large)
	second.DownstreamKeyHash = "other-key"
	second.WorkspaceHash = "other-workspace"
	second.InitialGoalHash = "other-initial"
	second.StorageMaxBytes = used + 256
	newGoal, err := store.CommitGoalTurn(ctx, second)
	if err != nil {
		t.Fatalf("new goal should reclaim inactive storage: %v", err)
	}
	if newGoal.ID == oldGoal.ID {
		t.Fatal("new goal unexpectedly reused the old identity")
	}
	if _, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "budget-old"}}); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("least-recently-used goal was not reclaimed: %v", err)
	}
	if _, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "budget-new"}}); err != nil {
		t.Fatalf("new goal was not committed: %v", err)
	}
	var reclaimed int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='goal_storage_reclaimed' AND reason='storage_budget_lru'`).Scan(&reclaimed); err != nil || reclaimed != 1 {
		t.Fatalf("reclaim audit count=%d err=%v", reclaimed, err)
	}
}

func TestGoalStorageBudgetNeverReclaimsLiveGoal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("y", 4096)
	oldGoal, err := store.CommitGoalTurn(ctx, goalTurnForTest("budget-live", "budget-live-response", large, large))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireGoalRun(ctx, oldGoal.ID, "live-budget-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	used := goalEncryptedPayloadBytes(t, store)
	second := goalTurnForTest("budget-blocked", "budget-blocked-response", large, large)
	second.DownstreamKeyHash = "blocked-key"
	second.WorkspaceHash = "blocked-workspace"
	second.InitialGoalHash = "blocked-initial"
	second.StorageMaxBytes = used + 256
	if _, err := store.CommitGoalTurn(ctx, second); !errors.Is(err, ErrGoalStorageBudget) {
		t.Fatalf("live goal storage error=%v, want %v", err, ErrGoalStorageBudget)
	}
	if _, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "budget-live"}}); err != nil {
		t.Fatalf("live goal was reclaimed: %v", err)
	}
}

func TestGoalCompactionPreservesUnknownAttachmentBlocks(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	first := goalTurnForTest("unknown-root", "unknown-r1", "attachment task", "unused")
	first.CompressionStages = 1
	first.SegmentPayload = `{"history_key":"input","input":[{"role":"user","content":"attachment task"}],"output":[{"type":"future_attachment","file_id":"opaque-file","metadata":{"nested":[1,{"keep":true}]}}]}`
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := goalTurnForTest("unknown-root", "unknown-r2", "tail", "tail-out")
	second.CompressionStages = 1
	if _, err := store.CommitGoalTurn(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGoalSegmentsWithRatio(ctx, goal.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(body), `"type":"future_attachment"`) || !strings.Contains(string(body), `"file_id":"opaque-file"`) || !strings.Contains(string(body), `"keep":true`) {
		t.Fatalf("unknown attachment lost or textified body=%s err=%v", body, err)
	}
}
