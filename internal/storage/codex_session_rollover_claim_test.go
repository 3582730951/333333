package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClaimCodexSessionRolloverPersistsSingleWinnerAndExactRetry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("codex-rollover-claim-test-key"))
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "rollover-claim-test",
		Binding: CodexSessionBinding{
			ID: "rollover-binding", TreeID: "rollover-tree", AccountID: "account-a", EgressID: "egress-a",
			Epoch: 7, WindowGeneration: 3, State: "active", RootSessionID: "old-root", ThreadID: "old-thread",
			ParentThreadID: "old-parent", ForkedFromThreadID: "old-fork",
			PendingRolloverAt: storageNowForTest(), PendingRolloverCause: "text_high_confidence_refusal",
			PendingRolloverDetectorVersion: "gpt-refusal-v1", PendingRolloverCheckpointRef: "checkpoint-safe-ref",
			PendingRolloverSourceEpoch: 7, PendingRolloverNonce: "pending-nonce",
		},
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "response-safe-alias"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint := store.CodexRolloverRequestFingerprint([]byte(`{"input":"next turn"}`))
	claim := CodexSessionRolloverClaim{
		BindingID: committed.ID, ExpectedEpoch: 7, PendingNonce: "pending-nonce", RequestFingerprint: firstFingerprint,
		RootSessionID: "new-root", ThreadID: "new-child", ParentThreadID: "", ForkedFromThreadID: "",
	}
	claimed, err := store.ClaimCodexSessionRollover(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Epoch != 8 || claimed.WindowGeneration != 4 || claimed.RootSessionID != "new-root" || claimed.ThreadID != "new-child" || claimed.ParentThreadID != "" || claimed.ForkedFromThreadID != "" || claimed.SafetyRotatedAt == 0 {
		t.Fatalf("unexpected claimed binding: %+v", claimed)
	}
	if claimed.PendingRolloverRequestFingerprint != firstFingerprint {
		t.Fatal("claim did not persist the request fingerprint")
	}

	// A crash after the CAS but before the replacement terminal must let the
	// byte-identical downstream request resume the same fresh identity.
	replayed, err := store.ClaimCodexSessionRollover(ctx, claim)
	if err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if replayed.Epoch != claimed.Epoch || replayed.RootSessionID != claimed.RootSessionID || replayed.ThreadID != claimed.ThreadID {
		t.Fatalf("exact retry changed the winner: %+v", replayed)
	}

	other := claim
	other.RequestFingerprint = store.CodexRolloverRequestFingerprint([]byte(`{"input":"different turn"}`))
	if _, err := store.ClaimCodexSessionRollover(ctx, other); !errors.Is(err, ErrCodexSessionRolloverInProgress) {
		t.Fatalf("different request error=%v, want %v", err, ErrCodexSessionRolloverInProgress)
	}
	persisted, err := store.ResolveCodexSessionAliases(ctx, "rollover-claim-test", []CodexSessionAlias{{Type: "response", Value: "response-safe-alias"}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Epoch != claimed.Epoch || persisted.RootSessionID != "new-root" || persisted.PendingRolloverRequestFingerprint != firstFingerprint {
		t.Fatalf("claim was not durable: %+v", persisted)
	}
}

func TestClaimCodexSessionRolloverRequiresCompletePendingIntent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	committed, err := store.CommitCodexSessionBinding(ctx, CodexSessionCommit{
		Namespace: "rollover-incomplete-test",
		Binding:   CodexSessionBinding{ID: "incomplete-binding", TreeID: "incomplete-tree", AccountID: "account", EgressID: "egress", Epoch: 2, State: "active", RootSessionID: "old", ThreadID: "old"},
		Aliases:   []CodexSessionAlias{{Type: "response", Value: "incomplete-response"}},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ClaimCodexSessionRollover(ctx, CodexSessionRolloverClaim{
		BindingID: committed.ID, ExpectedEpoch: 2, PendingNonce: "missing", RequestFingerprint: "fingerprint", RootSessionID: "new", ThreadID: "new",
	})
	if !errors.Is(err, ErrCodexSessionRolloverNotPending) {
		t.Fatalf("error=%v, want %v", err, ErrCodexSessionRolloverNotPending)
	}
	persisted, err := store.ResolveCodexSessionAliases(ctx, "rollover-incomplete-test", []CodexSessionAlias{{Type: "response", Value: "incomplete-response"}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Epoch != 2 || persisted.RootSessionID != "old" || persisted.SafetyRotatedAt != 0 {
		t.Fatalf("incomplete claim mutated binding: %+v", persisted)
	}
}

func storageNowForTest() int64 { return Now() }
