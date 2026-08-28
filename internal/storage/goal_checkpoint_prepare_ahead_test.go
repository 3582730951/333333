package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func checkpointChunkCount(t *testing.T, store *Store, goalID string) int {
	t.Helper()
	var n int
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=?`,
		goalID, goalChunkCheckpoint).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// paddedCheckpoint builds a checkpoint payload of roughly the requested size that
// still parses, so the replay builder handles it the way it handles a real one.
func paddedCheckpoint(marker string, size int) string {
	filler := strings.Repeat("x", size)
	return fmt.Sprintf(`{"model":"gpt-test","marker":%q,"pad":%q,"tools":[{"type":"function","name":"keep"}],"input":[]}`,
		marker, filler)
}

// Checkpoint compression now happens before BeginTx when the payload is large and
// the commit is predicted to need it. The prediction is allowed to be wrong; writing
// zero chunks is not. A fill condition narrower than either insert guard would leave
// a goal with a checkpoint row and no payload chunks — data loss that looks like a
// successful commit, which no timing test would catch.
func TestCheckpointIsWrittenOnEveryCommitPath(t *testing.T) {
	for _, size := range []struct {
		name  string
		bytes int
	}{
		{"below prepare-ahead threshold", 4 << 10},
		{"above prepare-ahead threshold", goalCheckpointPrepareAheadMinBytes + (256 << 10)},
	} {
		t.Run(size.name, func(t *testing.T) {
			ctx := context.Background()
			store := benchStore(t)
			alias := "prepare-ahead-" + strings.ReplaceAll(size.name, " ", "-")

			// 1. Creation inserts the checkpoint.
			first := goalTurnForTest(alias, alias+"-r1", "first", "out-1")
			first.CheckpointPayload = paddedCheckpoint("original", size.bytes)
			first.StorageMaxBytes = 256 << 20
			goal, err := store.CommitGoalTurn(ctx, first)
			if err != nil {
				t.Fatal(err)
			}
			created := checkpointChunkCount(t, store, goal.ID)
			if created == 0 {
				t.Fatal("creation wrote a checkpoint row with zero payload chunks")
			}
			if goal.StorageBytes <= 0 {
				t.Fatalf("creation reported StorageBytes=%d", goal.StorageBytes)
			}

			// 2. An ordinary append must not touch the checkpoint.
			second := goalTurnForTest(alias, alias+"-r2", "second", "out-2")
			second.CheckpointPayload = paddedCheckpoint("ignored-on-append", size.bytes)
			second.StorageMaxBytes = 256 << 20
			appended, err := store.CommitGoalTurn(ctx, second)
			if err != nil {
				t.Fatal(err)
			}
			if appended.ID != goal.ID {
				t.Fatalf("append created a second goal %s != %s", appended.ID, goal.ID)
			}
			if got := checkpointChunkCount(t, store, goal.ID); got != created {
				t.Errorf("append changed checkpoint chunk count: %d -> %d", created, got)
			}

			// 3. ReplaceHistory reinserts it. This path is known before the transaction,
			// so it is always prepared ahead when large — and must still land.
			replacement := goalTurnForTest(alias, alias+"-r3", "third", "out-3")
			replacement.CheckpointPayload = paddedCheckpoint("replaced", size.bytes)
			replacement.SegmentPayload = `{"replace_input":true,"input":[{"role":"user","content":"summary"}],"output":[{"type":"message","content":[{"type":"output_text","text":"continued"}]}]}`
			replacement.ReplaceHistory = true
			replacement.StorageMaxBytes = 256 << 20
			replaced, err := store.CommitGoalTurn(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkpointChunkCount(t, store, replaced.ID); got == 0 {
				t.Fatal("ReplaceHistory wrote a checkpoint row with zero payload chunks")
			}
			if replaced.StorageBytes <= 0 {
				t.Fatalf("ReplaceHistory reported StorageBytes=%d", replaced.StorageBytes)
			}

			replay, _, err := store.BuildGoalReplay(ctx, goal.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(replay) == 0 {
				t.Fatal("replay is empty after the replacement")
			}
		})
	}
}

// The probe is a hint. If it guesses "absent" and the goal in fact exists, the
// prepared chunks are simply unused; if it guesses "present" and the goal is being
// created, the transaction prepares them itself. Neither may change what is stored,
// so this pins both directions against a directly-invoked probe.
func TestGoalAbsenceProbeIsOnlyAHint(t *testing.T) {
	ctx := context.Background()
	store := benchStore(t)

	sets := [][]GoalAlias{{{Type: "codex_root_thread", Value: "probe-alias"}}}
	family := GoalProtocolFamily("codex")

	if !store.goalProbablyAbsent(ctx, sets, family) {
		t.Fatal("probe reported an unknown goal as present")
	}

	turn := goalTurnForTest("probe-alias", "probe-r1", "input", "output")
	turn.StorageMaxBytes = 256 << 20
	goal, err := store.CommitGoalTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if store.goalProbablyAbsent(ctx, sets, family) {
		t.Error("probe reported an existing goal as absent")
	}

	// A wrong "absent" answer must not corrupt a subsequent append: the transaction's
	// own resolve is what decides, and it still finds the existing goal.
	again := goalTurnForTest("probe-alias", "probe-r2", "second", "out-2")
	again.StorageMaxBytes = 256 << 20
	appended, err := store.CommitGoalTurn(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if appended.ID != goal.ID {
		t.Errorf("append after the probe forked a new goal: %s != %s", appended.ID, goal.ID)
	}
}
