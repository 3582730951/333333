package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

func TestGoalPolicyDefaultMigrationUpgradesOnlyInheritedLegacyValues(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                string
		explicitStorage     string
		explicitDualWrite   string
		wantStorage         string
		wantDualWrite       string
		wantStorageUpgraded bool
		wantDualDisabled    bool
		wantAudits          int
	}{
		{name: "inherited legacy defaults", wantStorage: "1024", wantDualWrite: "false", wantStorageUpgraded: true, wantDualDisabled: true, wantAudits: 1},
		{name: "explicit runtime values win", explicitStorage: "256", explicitDualWrite: "true", wantStorage: "256", wantDualWrite: "true"},
		{name: "blank rows are not explicit", explicitStorage: " ", explicitDualWrite: " ", wantStorage: "1024", wantDualWrite: "false", wantStorageUpgraded: true, wantDualDisabled: true, wantAudits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := OpenInMemory()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.explicitStorage != "" {
				if err = store.SetSetting(ctx, "goal_storage_max_mb", tc.explicitStorage); err != nil {
					t.Fatal(err)
				}
			}
			if tc.explicitDualWrite != "" {
				if err = store.SetSetting(ctx, "goal_legacy_journal_dual_write", tc.explicitDualWrite); err != nil {
					t.Fatal(err)
				}
			}
			result, err := store.MigrateGoalPolicyDefaults(ctx, 256, 256, 1024, true)
			if err != nil {
				t.Fatal(err)
			}
			if result.StorageDefaultUpgraded != tc.wantStorageUpgraded || result.LegacyDualWriteDisabled != tc.wantDualDisabled {
				t.Fatalf("migration result=%+v", result)
			}
			storageValue, storageOK, err := store.GetSetting(ctx, "goal_storage_max_mb")
			if err != nil || !storageOK || storageValue != tc.wantStorage {
				t.Fatalf("storage setting=%q present=%t err=%v, want %q", storageValue, storageOK, err, tc.wantStorage)
			}
			dualValue, dualOK, err := store.GetSetting(ctx, "goal_legacy_journal_dual_write")
			if err != nil || !dualOK || dualValue != tc.wantDualWrite {
				t.Fatalf("dual-write setting=%q present=%t err=%v, want %q", dualValue, dualOK, err, tc.wantDualWrite)
			}
			var marker, audits int
			if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, goalPolicyDefaultsMigrationMarker).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_policy_defaults_migrated'`).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if marker != 1 || audits != tc.wantAudits {
				t.Fatalf("marker/audits=%d/%d, want 1/%d", marker, audits, tc.wantAudits)
			}
			second, err := store.MigrateGoalPolicyDefaults(ctx, 256, 256, 1024, true)
			if err != nil || !second.AlreadyCompleted || second.StorageDefaultUpgraded || second.LegacyDualWriteDisabled {
				t.Fatalf("idempotent migration=%+v err=%v", second, err)
			}
		})
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

func TestGoalAliasesAreScopedByClientNamespace(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	first := goalTurnForTest("same-visible-root", "namespace-response-a", "client-a", "answer-a")
	first.AliasNamespace = "client-scope-a"
	first.DownstreamKeyHash = "scoped-key-a"
	clientA, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := goalTurnForTest("same-visible-root", "namespace-response-b", "client-b", "answer-b")
	second.AliasNamespace = "client-scope-b"
	second.DownstreamKeyHash = "scoped-key-b"
	clientB, err := store.CommitGoalTurn(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if clientA.ID == clientB.ID {
		t.Fatalf("client namespaces merged into %s", clientA.ID)
	}
	for _, testCase := range []struct {
		namespace, want string
	}{
		{"client-scope-a", clientA.ID},
		{"client-scope-b", clientB.ID},
	} {
		resolved, resolveErr := store.ResolveGoalAliases(ctx, []GoalAlias{{
			Type: "codex_root_thread", Value: "same-visible-root", Namespace: testCase.namespace,
		}})
		if resolveErr != nil || resolved.Session.ID != testCase.want {
			t.Fatalf("namespace=%s resolution=%+v err=%v", testCase.namespace, resolved, resolveErr)
		}
	}
	if _, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "same-visible-root"}}); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("unscoped alias crossed namespaced goals: %v", err)
	}
}

func TestGoalHistoryReplacementReclaimsStorageAtBudgetBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	oldDetail := strings.Repeat("old-context-0123456789abcdef", 8192)
	first := goalTurnForTest("replace-budget-root", "replace-budget-r1", "unused", "unused")
	first.Protocol = "claude"
	first.CheckpointPayload = `{"model":"claude","messages":[]}`
	first.SegmentPayload = fmt.Sprintf(`{"history_key":"messages","input":[{"role":"user","content":%q}],"output":[{"role":"assistant","content":"old answer"}]}`, oldDetail)
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	used := goalEncryptedPayloadBytes(t, store)

	replacement := goalTurnForTest("replace-budget-root", "replace-budget-r2", "unused", "unused")
	replacement.Protocol = "claude"
	replacement.CheckpointPayload = `{"model":"claude","messages":[]}`
	replacement.SegmentPayload = `{"history_key":"messages","replace_input":true,"input":[{"role":"user","content":"bounded summary"}],"output":[{"role":"assistant","content":"continued"}]}`
	replacement.ReplaceHistory = true
	replacement.StorageMaxBytes = used
	updated, err := store.CommitGoalTurn(ctx, replacement)
	if err != nil {
		t.Fatalf("shrinking replacement at full budget: %v", err)
	}
	if updated.ID != goal.ID || updated.StorageBytes >= used {
		t.Fatalf("replacement goal=%s/%s bytes=%d before=%d", updated.ID, goal.ID, updated.StorageBytes, used)
	}
	replay, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(replay), "bounded summary") || !strings.Contains(string(replay), "continued") ||
		strings.Contains(string(replay), "old-context") || strings.Contains(string(replay), "old answer") {
		t.Fatalf("replacement replay retained superseded history: %s", replay)
	}
	var checkpoints, segments int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?`, goal.ID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_segment WHERE goal_id=?`, goal.ID).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 || segments != 1 {
		t.Fatalf("physical replacement checkpoints=%d segments=%d", checkpoints, segments)
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
	if stepDeleted, err := store.CleanupGoalContinuity(ctx); err != nil || stepDeleted != 0 {
		t.Fatalf("first expired reclaim phase deleted=%d err=%v, want bounded progress", stepDeleted, err)
	}
	var reclaimState string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, goal.ID).Scan(&reclaimState); err != nil || reclaimState != goalReclaimingState {
		t.Fatalf("expired goal state=%q err=%v, want reclaiming", reclaimState, err)
	}
	var deleted int64
	for i := 0; i < 7 && deleted == 0; i++ {
		stepDeleted, err := store.CleanupGoalContinuity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		deleted += stepDeleted
	}
	if deleted != 1 {
		t.Fatalf("completed expired goal cleanup deleted=%d, want 1", deleted)
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

func goalPressurePayload(seed uint32, size int) string {
	raw := make([]byte, size)
	for index := range raw {
		seed = seed*1664525 + 1013904223
		raw[index] = byte(seed >> 24)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestGoalStoragePressureReclaimsColdGoalsAndRetriesCurrentCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	var cold []GoalSession
	for index := 0; index < 3; index++ {
		payload := goalPressurePayload(uint32(index+1), 80<<10)
		turn := goalTurnForTest(fmt.Sprintf("pressure-cold-%d", index), fmt.Sprintf("pressure-cold-response-%d", index), payload, payload)
		turn.DownstreamKeyHash = fmt.Sprintf("pressure-cold-key-%d", index)
		turn.WorkspaceHash = fmt.Sprintf("pressure-cold-workspace-%d", index)
		turn.InitialGoalHash = fmt.Sprintf("pressure-cold-initial-%d", index)
		goal, commitErr := store.CommitGoalTurn(ctx, turn)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		if _, updateErr := store.DB().ExecContext(ctx, `UPDATE goal_session SET state='completed',updated_at=? WHERE id=?`, Now()-100-int64(index), goal.ID); updateErr != nil {
			t.Fatal(updateErr)
		}
		cold = append(cold, goal)
	}

	current, err := store.CommitGoalTurn(ctx, goalTurnForTest("pressure-current", "pressure-current-r1", "current", "current-output"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AcquireGoalRun(ctx, current.ID, "pressure-current-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	awaitingTurn := goalTurnForTest("pressure-awaiting", "pressure-awaiting-r1", "awaiting", "tool-call")
	awaitingTurn.DownstreamKeyHash = "pressure-awaiting-key"
	awaitingTurn.WorkspaceHash = "pressure-awaiting-workspace"
	awaitingTurn.InitialGoalHash = "pressure-awaiting-initial"
	awaitingTurn.AwaitingTool = true
	awaiting, err := store.CommitGoalTurn(ctx, awaitingTurn)
	if err != nil {
		t.Fatal(err)
	}

	var currentBytes, awaitingBytes, used int64
	if err = store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, current.ID).Scan(&currentBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, awaiting.ID).Scan(&awaitingBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT SUM(storage_bytes) FROM goal_session`).Scan(&used); err != nil {
		t.Fatal(err)
	}

	appendTurn := goalTurnForTest("pressure-current", "pressure-current-r2", goalPressurePayload(99, 96<<10), "continued-checkpoint")
	appendTurn.StorageMaxBytes = used + 1
	_, err = store.CommitGoalTurn(ctx, appendTurn)
	var probe *GoalStorageBudgetError
	if !errors.As(err, &probe) || probe.AdditionalBytes <= 0 || probe.GoalID != current.ID {
		t.Fatalf("budget probe error=%v structured=%+v, want current goal %s", err, probe, current.ID)
	}

	// The hard limit is intentionally tiny: it fits the protected current goal,
	// awaiting-tool goal, and one more current checkpoint, but not the three cold
	// goals. This reproduces a saturated store without increasing its configured cap.
	appendTurn.StorageMaxBytes = currentBytes + awaitingBytes + probe.AdditionalBytes + 1024
	_, err = store.CommitGoalTurn(ctx, appendTurn)
	var budget *GoalStorageBudgetError
	if !errors.As(err, &budget) || budget.GoalID != current.ID {
		t.Fatalf("pressure commit error=%v structured=%+v", err, budget)
	}
	reclaimed, err := store.ReclaimGoalStorageHeadroom(ctx, budget.ReclaimTarget(), budget.GoalID, 16)
	if err != nil || !reclaimed.Progressed || reclaimed.Goals != int64(len(cold)) {
		t.Fatalf("bounded reclaim=%+v err=%v, want %d cold goals", reclaimed, err, len(cold))
	}
	continued, err := store.CommitGoalTurn(ctx, appendTurn)
	if err != nil || continued.ID != current.ID {
		t.Fatalf("current checkpoint retry goal=%+v err=%v, want %s", continued, err, current.ID)
	}
	for _, old := range cold {
		if _, err = store.GetGoalSession(ctx, old.ID); !errors.Is(err, ErrGoalNotFound) {
			t.Fatalf("cold goal %s remained visible after reclaim: %v", old.ID, err)
		}
	}
	if _, err = store.GetGoalSession(ctx, current.ID); err != nil {
		t.Fatalf("protected live current goal was reclaimed: %v", err)
	}
	if got, getErr := store.GetGoalSession(ctx, awaiting.ID); getErr != nil || got.State != "awaiting_tool_result" {
		t.Fatalf("awaiting-tool goal=%+v err=%v", got, getErr)
	}
	replay, _, err := store.BuildGoalReplay(ctx, current.ID)
	if err != nil || !strings.Contains(string(replay), "continued-checkpoint") {
		t.Fatalf("continued checkpoint was not durable replay=%s err=%v", replay, err)
	}
}

func TestGoalStorageTargetReservesCapacityForExistingGoal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := store.CommitGoalTurn(ctx, goalTurnForTest("reserve-current", "reserve-r1", "first", "first-output"))
	if err != nil {
		t.Fatal(err)
	}
	used := goalEncryptedPayloadBytes(t, store)
	newGoal := goalTurnForTest("reserve-new", "reserve-new-r1", goalPressurePayload(7, 16<<10), "new-output")
	newGoal.DownstreamKeyHash = "reserve-new-key"
	newGoal.WorkspaceHash = "reserve-new-workspace"
	newGoal.InitialGoalHash = "reserve-new-initial"
	newGoal.StorageMaxBytes = used + 1<<20
	newGoal.StorageTargetBytes = used + 1
	_, err = store.CommitGoalTurn(ctx, newGoal)
	var budget *GoalStorageBudgetError
	if !errors.As(err, &budget) || budget.LimitBytes != newGoal.StorageTargetBytes || budget.GoalID != "" {
		t.Fatalf("new-goal reserve error=%v structured=%+v", err, budget)
	}
	existing := goalTurnForTest("reserve-current", "reserve-r2", "second", "second-output")
	existing.StorageMaxBytes = newGoal.StorageMaxBytes
	existing.StorageTargetBytes = newGoal.StorageTargetBytes
	updated, err := store.CommitGoalTurn(ctx, existing)
	if err != nil || updated.ID != current.ID {
		t.Fatalf("existing goal could not consume reserved capacity goal=%+v err=%v", updated, err)
	}
}

func TestReclaimGoalStorageHeadroomStopsImmediatelyWithoutEligibleGoal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("bounded-live", "bounded-live-r1", "live", "output"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AcquireGoalRun(ctx, goal.ID, "bounded-live-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReclaimGoalStorageHeadroom(ctx, 0, goal.ID, 1_000_000)
	if err != nil || result.Progressed || result.BytesFreed != 0 || result.Goals != 0 {
		t.Fatalf("no-eligible bounded reclaim=%+v err=%v", result, err)
	}
}

func TestGoalReplaySnapshotFenceRejectsConcurrentAppendWithoutLosingSegments(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("snapshot-race", "snapshot-r1", "first-marker", "first-output"))
	if err != nil {
		t.Fatal(err)
	}
	middle := goalTurnForTest("snapshot-race", "snapshot-r2", "middle-marker", "middle-output")
	if _, err = store.CommitGoalTurn(ctx, middle); err != nil {
		t.Fatal(err)
	}
	staleReplay, _, version, err := store.BuildGoalReplaySnapshot(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := goalTurnForTest("snapshot-race", "snapshot-r3", "concurrent-marker", "concurrent-output")
	if _, err = store.CommitGoalTurn(ctx, concurrent); err != nil {
		t.Fatal(err)
	}
	replacement := goalTurnForTest("snapshot-race", "snapshot-r4", "stale-replacement-marker", "stale-output")
	replacement.ReplaceHistory = true
	replacement.CheckpointPayload = string(staleReplay)
	replacement.ExpectedCurrentCheckpoint = version.CurrentCheckpoint
	replacement.ExpectedLastSegmentSequence = version.LastSegmentSequence
	if _, err = store.CommitGoalTurn(ctx, replacement); !errors.Is(err, ErrGoalInProgress) {
		t.Fatalf("stale exact replay commit error=%v, want %v", err, ErrGoalInProgress)
	}
	replay, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"first-marker", "middle-marker", "concurrent-marker"} {
		if !strings.Contains(string(replay), marker) {
			t.Fatalf("concurrent CAS lost %s replay=%s", marker, replay)
		}
	}
	if strings.Contains(string(replay), "stale-replacement-marker") {
		t.Fatalf("rejected stale replacement mutated replay=%s", replay)
	}

	staleCheckpointReplay, _, checkpointVersion, err := store.BuildGoalReplaySnapshot(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpointConcurrent := goalTurnForTest("snapshot-race", "snapshot-r5", "checkpoint-concurrent-marker", "checkpoint-concurrent-output")
	checkpointConcurrent.ReplaceHistory = true
	checkpointConcurrent.CheckpointPayload = string(staleCheckpointReplay)
	if _, err = store.CommitGoalTurn(ctx, checkpointConcurrent); err != nil {
		t.Fatal(err)
	}
	staleCheckpoint := goalTurnForTest("snapshot-race", "snapshot-r6", "stale-checkpoint-marker", "stale-checkpoint-output")
	staleCheckpoint.ReplaceHistory = true
	staleCheckpoint.CheckpointPayload = string(staleCheckpointReplay)
	staleCheckpoint.ExpectedCurrentCheckpoint = checkpointVersion.CurrentCheckpoint
	staleCheckpoint.ExpectedLastSegmentSequence = checkpointVersion.LastSegmentSequence
	if _, err = store.CommitGoalTurn(ctx, staleCheckpoint); !errors.Is(err, ErrGoalInProgress) {
		t.Fatalf("stale checkpoint commit error=%v, want %v", err, ErrGoalInProgress)
	}
	replay, _, err = store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(replay), "checkpoint-concurrent-marker") || strings.Contains(string(replay), "stale-checkpoint-marker") {
		t.Fatalf("checkpoint CAS replay=%s err=%v", replay, err)
	}
}

func TestGoalStorageBudgetDefersInactiveReclaimToMaintenance(t *testing.T) {
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
	if _, err := store.CommitGoalTurn(ctx, second); !errors.Is(err, ErrGoalStorageBudget) {
		t.Fatalf("foreground storage error=%v, want %v", err, ErrGoalStorageBudget)
	}
	if _, err := store.GetGoalSession(ctx, oldGoal.ID); err != nil {
		t.Fatalf("foreground budget check reclaimed existing goal: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, _, err := store.EnforceGoalStorageBudget(ctx, used-1); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_session WHERE id=?`, oldGoal.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			break
		}
	}
	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_session WHERE id=?`, oldGoal.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("maintenance did not finish old goal remaining=%d err=%v", remaining, err)
	}
	newGoal, err := store.CommitGoalTurn(ctx, second)
	if err != nil {
		t.Fatalf("new goal after maintenance reclaim: %v", err)
	}
	if newGoal.ID == oldGoal.ID {
		t.Fatal("new goal unexpectedly reused the reclaimed identity")
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

func TestEnforceGoalStorageBudgetReclaimsExistingInactiveGoals(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("m", 4096)
	oldGoal, err := store.CommitGoalTurn(ctx, goalTurnForTest("maintenance-old", "maintenance-old-response", large, large))
	if err != nil {
		t.Fatal(err)
	}
	newTurn := goalTurnForTest("maintenance-new", "maintenance-new-response", large, large)
	newTurn.DownstreamKeyHash = "maintenance-new-key"
	newTurn.WorkspaceHash = "maintenance-new-workspace"
	newTurn.InitialGoalHash = "maintenance-new-initial"
	newGoal, err := store.CommitGoalTurn(ctx, newTurn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE goal_session SET updated_at=? WHERE id=?`, Now()-100, oldGoal.ID); err != nil {
		t.Fatal(err)
	}
	var oldBytes, totalBytes int64
	if err := store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, oldGoal.ID).Scan(&oldBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT SUM(storage_bytes) FROM goal_session`).Scan(&totalBytes); err != nil {
		t.Fatal(err)
	}
	var freed, deleted int64
	for i := 0; i < 8 && deleted == 0; i++ {
		stepFreed, stepDeleted, err := store.EnforceGoalStorageBudget(ctx, totalBytes-oldBytes+1)
		if err != nil {
			t.Fatal(err)
		}
		freed += stepFreed
		deleted += stepDeleted
	}
	if deleted != 1 || freed != oldBytes {
		t.Fatalf("maintenance budget freed=%d deleted=%d, want %d/1", freed, deleted, oldBytes)
	}
	if _, err := store.GetGoalSession(ctx, oldGoal.ID); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("old goal survived maintenance budget: %v", err)
	}
	if _, err := store.GetGoalSession(ctx, newGoal.ID); err != nil {
		t.Fatalf("new goal was reclaimed: %v", err)
	}
	var audits int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_storage_reclaimed' AND reason='storage_budget_maintenance'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("maintenance reclaim audit count=%d err=%v", audits, err)
	}
}

func TestEnforceGoalStorageBudgetPreservesLiveRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("maintenance-live", "maintenance-live-response", strings.Repeat("l", 4096), strings.Repeat("l", 4096)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireGoalRun(ctx, goal.ID, "maintenance-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	freed, deleted, err := store.EnforceGoalStorageBudget(ctx, 1)
	if err != nil || freed != 0 || deleted != 0 {
		t.Fatalf("live maintenance budget freed=%d deleted=%d err=%v", freed, deleted, err)
	}
	if _, err := store.GetGoalSession(ctx, goal.ID); err != nil {
		t.Fatalf("live goal was reclaimed: %v", err)
	}
}

func TestEnforceGoalStorageBudgetPrioritizesTerminalGoals(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goals := map[string]GoalSession{}
	for index, state := range []string{"retryable", "ready", "failed", "completed"} {
		turn := goalTurnForTest("priority-"+state, "priority-response-"+state, strings.Repeat(state, 256), "output")
		turn.DownstreamKeyHash = fmt.Sprintf("priority-key-%d", index)
		turn.WorkspaceHash = fmt.Sprintf("priority-workspace-%d", index)
		turn.InitialGoalHash = fmt.Sprintf("priority-goal-%d", index)
		goal, commitErr := store.CommitGoalTurn(ctx, turn)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		if _, updateErr := store.DB().ExecContext(ctx, `UPDATE goal_session SET state=?,updated_at=? WHERE id=?`, state, Now()-100+int64(index), goal.ID); updateErr != nil {
			t.Fatal(updateErr)
		}
		goals[state] = goal
	}
	step, err := store.EnforceGoalStorageBudgetStep(ctx, 1)
	if err != nil || !step.Progressed {
		t.Fatalf("terminal-priority reclaim step=%+v err=%v", step, err)
	}
	for state, goal := range goals {
		var current string
		if err := store.DB().QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, goal.ID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		want := state
		if state == "completed" {
			want = goalReclaimingState
		}
		if current != want {
			t.Fatalf("goal %s state=%q, want %q", state, current, want)
		}
	}
}

func TestEnforceGoalStorageBudgetStepReportsNoProgressWhenAllGoalsAreLive(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("no-progress-live", "no-progress-response", strings.Repeat("live", 1024), "output"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireGoalRun(ctx, goal.ID, "no-progress-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	step, err := store.EnforceGoalStorageBudgetStep(ctx, 1)
	if err != nil || step.Progressed || step.BytesFreed != 0 || step.Goals != 0 {
		t.Fatalf("all-live reclaim step=%+v err=%v", step, err)
	}
}

func TestEnforceGoalStorageBudgetPreservesAwaitingToolSessionWithoutRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("maintenance-awaiting", "maintenance-awaiting-response", "input", "out"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE goal_session SET state='awaiting_tool_result' WHERE id=?`, goal.ID); err != nil {
		t.Fatal(err)
	}
	freed, deleted, err := store.EnforceGoalStorageBudget(ctx, 1)
	if err != nil || freed != 0 || deleted != 0 {
		t.Fatalf("awaiting maintenance budget freed=%d deleted=%d err=%v", freed, deleted, err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, goal.ID).Scan(&state); err != nil || state != "awaiting_tool_result" {
		t.Fatalf("awaiting goal state=%q err=%v", state, err)
	}
	if _, err := store.GetGoalSession(ctx, goal.ID); err != nil {
		t.Fatalf("awaiting goal was hidden or reclaimed: %v", err)
	}
}

func TestEnforceGoalStorageBudgetUsesHiddenBoundedReclaimingSteps(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, goalTurnForTest("maintenance-batch", "maintenance-batch-response", "input", "out"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < goalReclaimRowsPerStep+1; i++ {
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO goal_payload_chunk(goal_id,payload_kind,segment_sequence,chunk_index,payload_hash,payload_bytes,encrypted_payload,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			goal.ID, "test-extra", 999, i, fmt.Sprint(i), 1, "x", Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE goal_session SET storage_bytes=storage_bytes+? WHERE id=?`, goalReclaimRowsPerStep+1, goal.ID); err != nil {
		t.Fatal(err)
	}
	var chunksBefore int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_payload_chunk WHERE goal_id=?`, goal.ID).Scan(&chunksBefore); err != nil {
		t.Fatal(err)
	}
	freed, deleted, err := store.EnforceGoalStorageBudget(ctx, 1)
	if err != nil || freed <= 0 || deleted != 0 {
		t.Fatalf("first bounded reclaim freed=%d deleted=%d err=%v", freed, deleted, err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, goal.ID).Scan(&state); err != nil || state != goalReclaimingState {
		t.Fatalf("physical goal state=%q err=%v, want reclaiming", state, err)
	}
	var chunksAfter int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_payload_chunk WHERE goal_id=?`, goal.ID).Scan(&chunksAfter); err != nil {
		t.Fatal(err)
	}
	if removed := chunksBefore - chunksAfter; removed != goalReclaimRowsPerStep {
		t.Fatalf("first reclaim removed %d chunks, want bounded %d", removed, goalReclaimRowsPerStep)
	}
	if _, err := store.GetGoalSession(ctx, goal.ID); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("reclaiming goal remained visible by id: %v", err)
	}
	if _, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "maintenance-batch"}}); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("reclaiming goal remained visible by alias: %v", err)
	}
	sessions, err := store.ListGoalSessions(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == goal.ID {
			t.Fatal("reclaiming goal remained visible in list")
		}
	}
	if _, _, err := store.BuildGoalReplay(ctx, goal.ID); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("reclaiming goal remained directly replayable: %v", err)
	}
	if _, err := store.getGoalCheckpoint(ctx, goal.CurrentCheckpoint); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("reclaiming checkpoint remained directly readable: %v", err)
	}
	var totalDeleted int64
	for i := 0; i < 8 && totalDeleted == 0; i++ {
		_, stepDeleted, err := store.EnforceGoalStorageBudget(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		totalDeleted += stepDeleted
	}
	if totalDeleted != 1 {
		t.Fatalf("bounded reclaim completion deleted=%d, want 1", totalDeleted)
	}
}

func TestReclaimingGoalAliasTransfersToNewGoal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	oldGoal, err := store.CommitGoalTurn(ctx, goalTurnForTest("reused-after-reclaim", "reused-old-response", "old", "old-out"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnforceGoalStorageBudget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, oldGoal.ID).Scan(&state); err != nil || state != goalReclaimingState {
		t.Fatalf("old goal state=%q err=%v", state, err)
	}
	replacement := goalTurnForTest("reused-after-reclaim", "reused-new-response", "new", "new-out")
	replacement.DownstreamKeyHash = "replacement-key"
	replacement.WorkspaceHash = "replacement-workspace"
	replacement.InitialGoalHash = "replacement-initial"
	newGoal, err := store.CommitGoalTurn(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if newGoal.ID == oldGoal.ID {
		t.Fatal("replacement reused reclaiming goal id")
	}
	resolved, err := store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "reused-after-reclaim"}})
	if err != nil || resolved.Session.ID != newGoal.ID {
		t.Fatalf("transferred alias resolved=%+v err=%v, want %s", resolved, err, newGoal.ID)
	}
	var oldDeleted int64
	for i := 0; i < 8 && oldDeleted == 0; i++ {
		_, stepDeleted, err := store.EnforceGoalStorageBudget(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		oldDeleted += stepDeleted
	}
	if oldDeleted != 1 {
		t.Fatalf("old goal physical deletion=%d, want 1", oldDeleted)
	}
	resolved, err = store.ResolveGoalAliases(ctx, []GoalAlias{{Type: "codex_root_thread", Value: "reused-after-reclaim"}})
	if err != nil || resolved.Session.ID != newGoal.ID {
		t.Fatalf("old reclaim removed transferred alias resolved=%+v err=%v", resolved, err)
	}
}

func TestLegacyOversizedGoalRowStillConverges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := Now()
	legacy := strings.Repeat("l", goalReclaimBytesPerStep+1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO goal_session(id,protocol,state,storage_bytes,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		"legacy-oversized-reclaim", "codex", "ready", len(legacy), now+3600, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO goal_segment(id,goal_id,sequence,payload_hash,payload_bytes,encrypted_payload,format_version,state,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"legacy-oversized-segment", "legacy-oversized-reclaim", 1, "legacy", len(legacy), legacy, 1, "committed", now); err != nil {
		t.Fatal(err)
	}
	freed, deleted, err := store.EnforceGoalStorageBudget(ctx, 1)
	if err != nil || freed != int64(len(legacy)) || deleted != 0 {
		t.Fatalf("legacy oversized reclaim freed=%d deleted=%d err=%v", freed, deleted, err)
	}
	if _, deleted, err = store.EnforceGoalStorageBudget(ctx, 1); err != nil || deleted != 1 {
		t.Fatalf("legacy oversized completion deleted=%d err=%v", deleted, err)
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
