package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func BenchmarkCommitFreshCodexSessionBatch256(b *testing.B) {
	store := newTestStore(b)
	store.SetTokenEncryptionKey([]byte("benchmark-codex-session-batch-key"))
	ctx := context.Background()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		commits := make([]CodexSessionCommit, 256)
		for index := range commits {
			id := fmt.Sprintf("%d-%d", iteration, index)
			commits[index] = CodexSessionCommit{
				Namespace: "benchmark",
				Binding: CodexSessionBinding{
					AccountID: "account", EgressID: "direct", State: "active",
					RootSessionID: "root-" + id, ThreadID: "root-" + id,
				},
				Aliases:             []CodexSessionAlias{{Type: "response", Value: "response-" + id}},
				ExpiresAt:           time.Now().Add(time.Hour).Unix(),
				InstructionSnapshot: &CodexInstructionSnapshot{Instructions: "stable instructions"},
			}
		}
		if _, err := store.CommitFreshCodexSessionBindings(ctx, commits); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCommitFreshCodexSessionBindingsPersistsWholeBatch(t *testing.T) {
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("test-codex-session-batch-key"))
	const count = 128
	commits := make([]CodexSessionCommit, count)
	for index := range commits {
		id := fmt.Sprintf("batch-%03d", index)
		commits[index] = CodexSessionCommit{
			Namespace: "batch-namespace",
			Binding: CodexSessionBinding{
				RootSessionID: id, ThreadID: id, AccountID: "account-" + id,
				EgressID: "egress-" + id, Epoch: 1,
			},
			Aliases:             []CodexSessionAlias{{Type: "response", Value: "response-" + id}},
			ExpiresAt:           time.Now().Add(time.Hour).Unix(),
			InstructionSnapshot: &CodexInstructionSnapshot{Instructions: "stable instructions " + id},
		}
	}
	bindings, err := store.CommitFreshCodexSessionBindings(context.Background(), commits)
	if err != nil || len(bindings) != count {
		t.Fatalf("batch bindings=%d err=%v", len(bindings), err)
	}
	for table, want := range map[string]int{
		"codex_session_binding":      count,
		"codex_session_alias":        count,
		"codex_instruction_snapshot": count,
	} {
		var got int
		if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s rows=%d want=%d err=%v", table, got, want, err)
		}
	}
}

func TestCommitFreshCodexSessionBindingsRollsBackConflictingBatch(t *testing.T) {
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("test-codex-session-batch-rollback-key"))
	commits := make([]CodexSessionCommit, 2)
	for index := range commits {
		id := fmt.Sprintf("conflict-%d", index)
		commits[index] = CodexSessionCommit{
			Namespace: "conflict-namespace",
			Binding: CodexSessionBinding{
				RootSessionID: id, ThreadID: id, AccountID: "account-" + id,
				EgressID: "egress-" + id, Epoch: 1,
			},
			Aliases:   []CodexSessionAlias{{Type: "response", Value: "shared-response"}},
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}
	}
	if _, err := store.CommitFreshCodexSessionBindings(context.Background(), commits); !errors.Is(err, ErrCodexSessionMappingAmbiguous) {
		t.Fatalf("batch conflict error=%v, want %v", err, ErrCodexSessionMappingAmbiguous)
	}
	for _, table := range []string{"codex_session_binding", "codex_session_alias", "codex_instruction_snapshot"} {
		var rows int
		if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s rollback rows=%d err=%v", table, rows, err)
		}
	}
}

func TestCodexSessionMappingEncryptsIdentityAndRetiresWholeTree(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	commit := CodexSessionCommit{
		Namespace: "key:test-namespace",
		Binding: CodexSessionBinding{
			ID:              "binding-root",
			TreeID:          "tree-root",
			AccountID:       "account-a",
			EgressID:        "direct",
			Epoch:           0,
			State:           "active",
			InstallationID:  "install-real-identity",
			DeviceOSHint:    "Mac OS",
			DeviceOSHintSet: true,
			RootSessionID:   "019f0000-0000-7000-8000-000000000001",
			ThreadID:        "019f0000-0000-7000-8000-000000000001",
		},
		Aliases: []CodexSessionAlias{
			{Type: "root", Value: "real-root-thread"},
			{Type: "response", Value: "resp-real-1"},
			{Type: "turn_state", Value: "opaque-real-state"},
		},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	committed, err := store.CommitCodexSessionBinding(ctx, commit)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed.InstallationID != commit.Binding.InstallationID || committed.DeviceOSHint != commit.Binding.DeviceOSHint || !committed.DeviceOSHintSet || committed.RootSessionID != commit.Binding.RootSessionID || committed.ThreadID != commit.Binding.ThreadID {
		t.Fatalf("identity round-trip = %+v", committed)
	}

	resolved, err := store.ResolveCodexSessionAliases(ctx, commit.Namespace, []CodexSessionAlias{
		{Type: "response", Value: "resp-real-1"},
		{Type: "turn_state", Value: "opaque-real-state"},
	})
	if err != nil || resolved.ID != committed.ID || resolved.AccountID != "account-a" || resolved.InstallationID != commit.Binding.InstallationID || resolved.DeviceOSHint != commit.Binding.DeviceOSHint || !resolved.DeviceOSHintSet {
		t.Fatalf("resolve = %+v, %v", resolved, err)
	}

	var namespaceHash, encryptedIdentity, aliasHash string
	if err := store.DB().QueryRowContext(ctx, `SELECT namespace_hash,encrypted_identity FROM codex_session_binding WHERE id=?`, committed.ID).Scan(&namespaceHash, &encryptedIdentity); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT alias_hash FROM codex_session_alias WHERE binding_id=? LIMIT 1`, committed.ID).Scan(&aliasHash); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"test-namespace", "real-root-thread", "resp-real-1", "opaque-real-state", commit.Binding.InstallationID, commit.Binding.DeviceOSHint, commit.Binding.RootSessionID} {
		if strings.Contains(namespaceHash, raw) || strings.Contains(aliasHash, raw) || strings.Contains(encryptedIdentity, raw) {
			t.Fatalf("raw identifier leaked into mapping storage: %q", raw)
		}
	}

	if _, err := store.RetireCodexSessionTree(ctx, committed.ID, committed.Epoch); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_, err = store.ResolveCodexSessionAliases(ctx, commit.Namespace, []CodexSessionAlias{{Type: "response", Value: "resp-real-1"}})
	if !errors.Is(err, ErrCodexSessionEpochRetired) {
		t.Fatalf("stateful alias after retirement err=%v, want ErrCodexSessionEpochRetired", err)
	}
}

func TestCodexSessionMappingGoalStateEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const goalTurnID = "goal-turn-private-019f0000"
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:goal-state-roundtrip",
		Binding: CodexSessionBinding{
			ID: "binding-goal-state", TreeID: "tree-goal-state", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "root-goal-state", ThreadID: "thread-goal-state",
			GoalModeActive: true, GoalTurnID: goalTurnID,
		},
		Aliases:   []CodexSessionAlias{{Type: "root", Value: "root-goal-state"}, {Type: "response", Value: "resp-goal-state"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !committed.GoalModeActive || committed.GoalTurnID != goalTurnID {
		t.Fatalf("committed goal state = active:%v turn:%q", committed.GoalModeActive, committed.GoalTurnID)
	}

	resolved, err := store.ResolveCodexSessionAliases(ctx, "key:goal-state-roundtrip", []CodexSessionAlias{{Type: "response", Value: "resp-goal-state"}})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.GoalModeActive || resolved.GoalTurnID != goalTurnID {
		t.Fatalf("resolved goal state = active:%v turn:%q", resolved.GoalModeActive, resolved.GoalTurnID)
	}
	var encryptedIdentity string
	if err := store.DB().QueryRowContext(ctx, `SELECT encrypted_identity FROM codex_session_binding WHERE id=?`, committed.ID).Scan(&encryptedIdentity); err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range []string{goalTurnID, "goal_mode_active", "goal_turn_id"} {
		if strings.Contains(encryptedIdentity, plaintext) {
			t.Fatalf("goal identity plaintext leaked into mapping storage: %q", plaintext)
		}
	}

	committed.GoalModeActive = false
	committed.GoalTurnID = ""
	if _, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:goal-state-roundtrip",
		Binding:   committed,
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-goal-state"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.ResolveCodexSessionAliases(ctx, "key:goal-state-roundtrip", []CodexSessionAlias{{Type: "response", Value: "resp-goal-state"}})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.GoalModeActive || cleared.GoalTurnID != "" {
		t.Fatalf("cleared goal state = active:%v turn:%q", cleared.GoalModeActive, cleared.GoalTurnID)
	}
}

func TestCodexSessionMappingCompactionAdvancesGenerationOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:test",
		Binding: CodexSessionBinding{
			ID: "binding-compact", TreeID: "tree-compact", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000011", ThreadID: "019f0000-0000-7000-8000-000000000011",
		},
		Aliases:   []CodexSessionAlias{{Type: "root", Value: "root-compact"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := store.AdvanceCodexSessionWindowGeneration(ctx, committed.ID, committed.Epoch, 0, time.Now().Add(time.Hour).Unix())
	if err != nil || advanced.WindowGeneration != 1 {
		t.Fatalf("advance = %+v, %v", advanced, err)
	}
	_, err = store.AdvanceCodexSessionWindowGeneration(ctx, committed.ID, committed.Epoch, 0, time.Now().Add(time.Hour).Unix())
	if !errors.Is(err, ErrCodexSessionEpochConflict) {
		t.Fatalf("second advance err=%v, want CAS conflict", err)
	}
}

func TestCodexSessionMappingCommitRefreshesAllAliasRetention(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	initialExpiry := time.Now().Add(time.Hour).Unix()
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:sliding",
		Binding: CodexSessionBinding{
			ID: "binding-sliding", TreeID: "tree-sliding", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000021", ThreadID: "019f0000-0000-7000-8000-000000000021",
		},
		Aliases:   []CodexSessionAlias{{Type: "root", Value: "root-sliding"}, {Type: "response", Value: "resp-sliding-1"}},
		ExpiresAt: initialExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshedExpiry := time.Now().Add(2 * time.Hour).Unix()
	if _, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:sliding",
		Binding:   committed,
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-sliding-2"}},
		ExpiresAt: refreshedExpiry,
	}); err != nil {
		t.Fatal(err)
	}
	var rootExpiry int64
	if err := store.DB().QueryRowContext(ctx, `SELECT expires_at FROM codex_session_alias WHERE binding_id=? AND alias_type='root'`, committed.ID).Scan(&rootExpiry); err != nil {
		t.Fatal(err)
	}
	if rootExpiry < refreshedExpiry {
		t.Fatalf("root alias expiry=%d, want sliding refresh to at least %d", rootExpiry, refreshedExpiry)
	}
}

func TestCodexSessionMappingResolvesAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mapping.sqlite3")
	secret := []byte("persistent-codex-mapping-secret")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(secret)
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "key:restart",
		Binding: CodexSessionBinding{
			ID: "binding-restart", TreeID: "tree-restart", AccountID: "account-a", EgressID: "direct", State: "active",
			InstallationID:  "install-restart",
			DeviceOSHint:    "Linux",
			DeviceOSHintSet: true,
			RootSessionID:   "019f0000-0000-7000-8000-000000000041", ThreadID: "019f0000-0000-7000-8000-000000000041",
		},
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-restart"}, {Type: "turn_state", Value: "state-restart"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Init(ctx); err != nil {
		t.Fatal(err)
	}
	reopened.SetTokenEncryptionKey(secret)
	resolved, err := reopened.ResolveCodexSessionAliases(ctx, "key:restart", []CodexSessionAlias{{Type: "response", Value: "resp-restart"}, {Type: "turn_state", Value: "state-restart"}})
	if err != nil || resolved.ID != committed.ID || resolved.InstallationID != committed.InstallationID || resolved.DeviceOSHint != committed.DeviceOSHint || !resolved.DeviceOSHintSet || resolved.RootSessionID != committed.RootSessionID || resolved.AccountID != "account-a" || resolved.EgressID != "direct" {
		t.Fatalf("restart mapping resolve=%+v err=%v", resolved, err)
	}
}

func TestCodexSessionMappingSharedTurnStateUsesResponseToDisambiguate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const namespace = "key:shared-turn-state"
	commit := func(id, tree, response, thread string) CodexSessionBinding {
		t.Helper()
		binding, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
			Namespace: namespace,
			Binding: CodexSessionBinding{
				ID: id, TreeID: tree, AccountID: "account-a", EgressID: "direct", State: "active",
				RootSessionID: "root-shared-state", ThreadID: thread,
			},
			Aliases: []CodexSessionAlias{
				{Type: "response", Value: response},
				{Type: "turn_state", Value: "shared-sibling-state"},
			},
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
		return binding
	}
	a := commit("binding-sibling-a", "tree-sibling-a", "resp-sibling-a", "thread-sibling-a")
	b := commit("binding-sibling-b", "tree-sibling-b", "resp-sibling-b", "thread-sibling-b")

	for _, tc := range []struct {
		name    string
		aliases []CodexSessionAlias
		binding CodexSessionBinding
	}{
		{name: "response_a_first", aliases: []CodexSessionAlias{{Type: "response", Value: "resp-sibling-a"}, {Type: "turn_state", Value: "shared-sibling-state"}}, binding: a},
		{name: "state_b_first", aliases: []CodexSessionAlias{{Type: "turn_state", Value: "shared-sibling-state"}, {Type: "response", Value: "resp-sibling-b"}}, binding: b},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := store.ResolveCodexSessionAliases(ctx, namespace, tc.aliases)
			if err != nil || resolved.ID != tc.binding.ID {
				t.Fatalf("resolved=%+v err=%v want=%s", resolved, err, tc.binding.ID)
			}
		})
	}
	if _, err := store.ResolveCodexSessionAliases(ctx, namespace, []CodexSessionAlias{{Type: "turn_state", Value: "shared-sibling-state"}}); !errors.Is(err, ErrCodexSessionMappingAmbiguous) {
		t.Fatalf("shared turn state alone err=%v, want ambiguity", err)
	}
	_, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: namespace,
		Binding: CodexSessionBinding{
			ID: "binding-response-conflict", TreeID: "tree-response-conflict", AccountID: "account-a", EgressID: "direct", State: "active",
			RootSessionID: "root-response-conflict", ThreadID: "root-response-conflict",
		},
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "resp-sibling-a"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if !errors.Is(err, ErrCodexSessionMappingAmbiguous) {
		t.Fatalf("response alias conflict err=%v, want ambiguity", err)
	}
}

func TestCodexInstructionSnapshotIsEncryptedStableAndTreeCAS(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("instruction-snapshot-test-secret"))
	const treeID = "tree-instruction-snapshot"
	const first = "first administrator instructions\n\nnever persist plaintext"
	stored, err := store.EnsureCodexInstructionSnapshot(ctx, CodexInstructionSnapshot{
		TreeID: treeID, Instructions: first, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Instructions != first || stored.Revision == "" {
		t.Fatalf("stored snapshot=%+v", stored)
	}
	var encrypted, revision string
	if err := store.DB().QueryRowContext(ctx, `SELECT encrypted_instructions,revision_hmac FROM codex_instruction_snapshot WHERE tree_id=?`, treeID).Scan(&encrypted, &revision); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, "administrator instructions") || strings.Contains(revision, "administrator instructions") || revision != stored.Revision {
		t.Fatalf("snapshot plaintext/revision leaked: encrypted=%q revision=%q", encrypted, revision)
	}

	// A second caller may have observed newer files, but an active tree must keep
	// the first elected instructions and revision.
	again, err := store.EnsureCodexInstructionSnapshot(ctx, CodexInstructionSnapshot{
		TreeID: treeID, Instructions: "new file content must not replace active tree", ExpiresAt: time.Now().Add(2 * time.Hour).Unix(),
	})
	if err != nil || again.Instructions != first || again.Revision != stored.Revision || again.ExpiresAt < stored.ExpiresAt {
		t.Fatalf("stable snapshot=%+v err=%v", again, err)
	}

	var wg sync.WaitGroup
	results := make(chan CodexInstructionSnapshot, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snapshot, callErr := store.EnsureCodexInstructionSnapshot(ctx, CodexInstructionSnapshot{
				TreeID: treeID, Instructions: "racing candidate " + string(rune('a'+i)), ExpiresAt: time.Now().Add(3 * time.Hour).Unix(),
			})
			if callErr != nil {
				errs <- callErr
				return
			}
			results <- snapshot
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for snapshot := range results {
		if snapshot.Instructions != first || snapshot.Revision != stored.Revision {
			t.Fatalf("CAS allowed concurrent replacement: %+v", snapshot)
		}
	}
}

func TestCodexInstructionSnapshotSurvivesStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "instruction-snapshot.sqlite3")
	key := []byte("persistent-instruction-snapshot-secret")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey(key)
	want := CodexInstructionSnapshot{
		TreeID: "tree-persistent-instruction-snapshot", Instructions: "durable administrator instructions", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	stored, err := store.EnsureCodexInstructionSnapshot(ctx, want)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Init(ctx); err != nil {
		t.Fatal(err)
	}
	reopened.SetTokenEncryptionKey(key)
	got, err := reopened.GetCodexInstructionSnapshot(ctx, want.TreeID)
	if err != nil || got.Instructions != want.Instructions || got.Revision != stored.Revision {
		t.Fatalf("restarted instruction snapshot=%+v err=%v", got, err)
	}
}

func TestCodexUpstreamAttemptDiagnosticsRedactTreeID(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.InsertCodexUpstreamAttempt(ctx, CodexUpstreamAttempt{
		TreeID:     "tree-real-upstream-attempt",
		AccountID:  "account-a",
		EgressID:   "egress-real",
		Epoch:      3,
		State:      "response_headers",
		StatusCode: 200,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListCodexUpstreamAttemptDiagnostics(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("attempt diagnostics=%+v err=%v", rows, err)
	}
	row := rows[0]
	if row.TreeHMACPrefix == "" || strings.Contains(row.TreeHMACPrefix, "tree-real") || row.AccountID != "account-a" || row.EgressID != "egress-real" || row.Epoch != 3 || row.StatusCode != 200 {
		t.Fatalf("redacted attempt diagnostic=%+v", row)
	}
}

func TestCodexUpstreamAttemptEventIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	attempt := CodexUpstreamAttempt{
		EventID: "attempt-idempotent", TreeID: "tree-idempotent", AccountID: "account-idempotent",
		EgressID: DefaultDirectEgressID, State: "terminal_success", StatusCode: 200, ExpiresAt: Now() + 60,
	}
	for iteration := 0; iteration < 2; iteration++ {
		if err := store.BatchWriteTelemetryAndAttempts(ctx, nil, nil, nil, nil, []CodexUpstreamAttempt{attempt}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_upstream_attempt WHERE event_id=?`, attempt.EventID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("idempotent attempt rows=%d, want 1", rows)
	}
}

func TestListRecentCodexEgressOutcomesCountsOnlyExitAttributableResults(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := Now()
	insert := func(egress, state string, createdAt int64) {
		t.Helper()
		if err := store.InsertCodexUpstreamAttempt(ctx, CodexUpstreamAttempt{
			TreeID: "recent-outcome-tree", AccountID: "account-a", EgressID: egress,
			State: state, CreatedAt: createdAt, ExpiresAt: now + 3600,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"transport_attempted", "transport_attempted", "transport_attempted", "egress_failure", "terminal_success", "terminal_success", "response_headers", "attempted"} {
		insert("egress-a", state, now-60)
	}
	insert("egress-a", "egress_failure", now-3600)
	// A raw start with no classified exit result can be an account/quota response,
	// client cancellation, or an in-flight request. It is observable in diagnostics
	// but must not be inferred as a network-exit failure.
	insert("egress-b", "transport_attempted", now-30)

	rows, err := store.ListRecentCodexEgressOutcomes(ctx, now-1800)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows["egress-a"]; got.Attempts != 3 || got.Successes != 2 {
		t.Fatalf("egress-a recent outcome=%+v, want attempts=3 successes=2", got)
	}
	if _, ok := rows["egress-b"]; ok {
		t.Fatalf("unclassified raw transport start became an egress failure: %+v", rows["egress-b"])
	}
}

func TestRecentCodexEgressOutcomeIndexUsesAdditiveMigration(t *testing.T) {
	if strings.Contains(codexSessionMappingSchemaSQL, "idx_codex_upstream_attempt_recent_egress") {
		t.Fatal("additive index changed immutable PostgreSQL base schema checksum")
	}
	store := newTestStore(t)
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_codex_upstream_attempt_recent_egress'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recent outcome migration index count=%d, want 1", count)
	}
}

func TestCleanupCodexUpstreamAttemptsAggregatesExactlyOnceAndKeepsSevenDayDetail(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := Now()
	day := now - now%86400
	insert := func(tree, account, state string, status int, createdAt, expiresAt int64) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO codex_upstream_attempt(
tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			tree, account, "egress-a", 2, state, status, createdAt, expiresAt); err != nil {
			t.Fatal(err)
		}
	}
	insert("expired-a", "account-a", "response_headers", 429, day-10, now-1)
	insert("expired-b", "account-a", "response_headers", 429, day-5, now-1)
	insert("live-without-binding", "account-a", "request_sent", 0, now-60, now+3600)

	if _, err := store.CleanupCodexSessionMappings(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListCodexUpstreamAttemptDailyDiagnostics(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("daily rows=%+v err=%v", rows, err)
	}
	if rows[0].AttemptCount != 2 || rows[0].AccountID != "account-a" || rows[0].StatusCode != 429 || rows[0].DayStart != day-86400 {
		t.Fatalf("daily row=%+v", rows[0])
	}
	var dailyExpiry int64
	if err := store.DB().QueryRowContext(ctx, `SELECT expires_at FROM codex_upstream_attempt_daily`).Scan(&dailyExpiry); err != nil {
		t.Fatal(err)
	}
	if want := day - 86400 + int64((30*24*time.Hour)/time.Second); dailyExpiry != want {
		t.Fatalf("daily expiry=%d want event-day+30d=%d", dailyExpiry, want)
	}
	var detail int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_upstream_attempt`).Scan(&detail); err != nil || detail != 1 {
		t.Fatalf("retained detail=%d err=%v", detail, err)
	}
	if _, err := store.CleanupCodexSessionMappings(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListCodexUpstreamAttemptDailyDiagnostics(ctx)
	if err != nil || len(rows) != 1 || rows[0].AttemptCount != 2 {
		t.Fatalf("second cleanup duplicated aggregate: rows=%+v err=%v", rows, err)
	}
}

func TestCleanupCodexUpstreamAttemptsUsesBoundedExactlyOnceBatches(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := Now()
	day := now - now%86400
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const expiredTotal = 600
	for index := 0; index < expiredTotal; index++ {
		if _, err = tx.ExecContext(ctx, `INSERT INTO codex_upstream_attempt(
tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("expired-batch-%03d", index), "account-batch", "egress-batch", 1,
			"response_headers", 429, day+int64(index), now-1); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO codex_upstream_attempt(
tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		"live-batch", "account-batch", "egress-batch", 1, "request_sent", 0, now, now+3600); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for pass, wantExpired := range []int{344, 88, 0} {
		if _, err = store.CleanupCodexSessionMappings(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
		var expired, live, aggregated int
		if err = store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM codex_upstream_attempt WHERE expires_at<=?`, now).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM codex_upstream_attempt WHERE expires_at>?`, now).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRowContext(ctx,
			`SELECT COALESCE(SUM(attempt_count),0) FROM codex_upstream_attempt_daily`).Scan(&aggregated); err != nil {
			t.Fatal(err)
		}
		if expired != wantExpired || live != 1 || aggregated != expiredTotal-wantExpired {
			t.Fatalf("pass %d expired/live/aggregated=%d/%d/%d want=%d/1/%d",
				pass+1, expired, live, aggregated, wantExpired, expiredTotal-wantExpired)
		}
	}
	if _, err = store.CleanupCodexSessionMappings(ctx); err != nil {
		t.Fatal(err)
	}
	var aggregated int
	if err = store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(attempt_count),0) FROM codex_upstream_attempt_daily`).Scan(&aggregated); err != nil || aggregated != expiredTotal {
		t.Fatalf("retry aggregate=%d err=%v", aggregated, err)
	}
}
