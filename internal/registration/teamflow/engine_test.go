package teamflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

type fixtureAdapter struct {
	credentialAvailable bool
	phoneRequired       bool
	quota               []int
	failInvite          int
	fallbackCredential  bool
	calls               []string
}

func (a *fixtureAdapter) called(name string) {
	a.calls = append(a.calls, name)
}

func (a *fixtureAdapter) Invite(context.Context, Operation) (string, error) {
	a.called("invite")
	if a.failInvite > 0 {
		a.failInvite--
		return "", Retryable("fixture_busy", errors.New("fixture busy"))
	}
	return "membership-ref", nil
}

func (a *fixtureAdapter) ResolveCredential(context.Context, Operation) (CredentialResolution, error) {
	a.called("resolve")
	return CredentialResolution{Available: a.credentialAvailable, CredentialRef: "credential-ref"}, nil
}

func (a *fixtureAdapter) LoginWithCredential(context.Context, Operation) (string, error) {
	a.called("credential_login")
	if a.fallbackCredential {
		return "", FallbackToOAuth(errors.New("fixture credential rejected"))
	}
	return "credential-ref", nil
}

func (a *fixtureAdapter) OAuthLogin(context.Context, Operation) (OAuthResult, error) {
	a.called("oauth")
	return OAuthResult{
		CredentialRef:     "oauth-credential-ref",
		PhoneRequired:     a.phoneRequired,
		PhoneChallengeRef: "phone-challenge-ref",
	}, nil
}

func (a *fixtureAdapter) VerifyPhone(context.Context, Operation) (string, error) {
	a.called("phone")
	return "phone-credential-ref", nil
}

func (a *fixtureAdapter) ImportAccount(context.Context, Operation) (string, error) {
	a.called("import")
	return "imported-account-ref", nil
}

func (a *fixtureAdapter) ObserveQuota(context.Context, Operation) (int, error) {
	a.called("quota")
	if len(a.quota) == 0 {
		return 10000, nil
	}
	value := a.quota[0]
	a.quota = a.quota[1:]
	return value, nil
}

func (a *fixtureAdapter) RemoveMember(context.Context, Operation) error {
	a.called("remove")
	return nil
}

func (a *fixtureAdapter) EnqueueReplacement(context.Context, Operation) (string, error) {
	a.called("replace")
	return "replacement-job-ref", nil
}

func newTeamFlowFixture(t *testing.T, shadow bool) (*storage.Store, storage.TeamLifecycleWorkflow) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "teamflow.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace, err := store.UpsertTeamWorkspace(context.Background(), storage.TeamWorkspace{
		ID:              "workspace-fixture",
		Name:            "Fixture",
		ParentAccountID: "parent-ref",
		WorkspaceRef:    "workspace-ref",
		ConnectorKind:   "fixture",
		Status:          storage.TeamWorkspaceStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow, _, err := store.CreateTeamLifecycleWorkflow(context.Background(), storage.CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:     "cycle-fixture",
		WorkspaceID:        workspace.ID,
		ParentAccountID:    workspace.ParentAccountID,
		ChildAccountID:     "child-ref",
		RotateThresholdBPS: 100,
		MaxAttempts:        4,
		ShadowMode:         shadow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, workflow
}

func driveWorkflow(
	t *testing.T,
	store *storage.Store,
	engine *Engine,
	now *time.Time,
	stop func(storage.TeamLifecycleWorkflow) bool,
) storage.TeamLifecycleWorkflow {
	t.Helper()
	ctx := context.Background()
	for index := 0; index < 80; index++ {
		workflow, ok, err := store.ClaimTeamLifecycleWorkflow(ctx, "fixture-worker", now.Unix(), 600)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			*now = now.Add(2 * time.Second)
			continue
		}
		next, err := engine.Advance(ctx, workflow, "fixture-worker")
		if err != nil {
			t.Fatal(err)
		}
		if stop(next) {
			return next
		}
		*now = now.Add(2 * time.Second)
	}
	t.Fatal("workflow did not reach expected state")
	return storage.TeamLifecycleWorkflow{}
}

func TestEngineAccessReferencePathAndRotation(t *testing.T) {
	store, _ := newTeamFlowFixture(t, false)
	adapter := &fixtureAdapter{credentialAvailable: true, quota: []int{600, 100}}
	now := time.Unix(1_800_000_000, 0)
	engine := NewEngine(store, adapter, Options{
		RetryBase: time.Second, RetryMax: time.Minute,
		QuotaPollInterval: time.Second, StepTimeout: time.Minute,
	})
	engine.now = func() time.Time { return now }
	completed := driveWorkflow(t, store, engine, &now, func(item storage.TeamLifecycleWorkflow) bool {
		return item.State == storage.TeamLifecycleCompleted
	})
	if completed.CredentialPath != "access_reference" || completed.ReplacementJobRef != "replacement-job-ref" {
		t.Fatalf("completed=%+v", completed)
	}
	want := []string{"invite", "resolve", "credential_login", "import", "quota", "quota", "remove", "replace"}
	if len(adapter.calls) != len(want) {
		t.Fatalf("calls=%v want=%v", adapter.calls, want)
	}
	for index := range want {
		if adapter.calls[index] != want[index] {
			t.Fatalf("calls=%v want=%v", adapter.calls, want)
		}
	}
}

func TestEngineOAuthPhoneFallbackPath(t *testing.T) {
	store, _ := newTeamFlowFixture(t, false)
	adapter := &fixtureAdapter{phoneRequired: true, quota: []int{100}}
	now := time.Unix(1_800_000_000, 0)
	engine := NewEngine(store, adapter, Options{
		QuotaPollInterval: time.Second, StepTimeout: time.Minute,
	})
	engine.now = func() time.Time { return now }
	completed := driveWorkflow(t, store, engine, &now, func(item storage.TeamLifecycleWorkflow) bool {
		return item.State == storage.TeamLifecycleCompleted
	})
	if completed.CredentialPath != "oauth" {
		t.Fatalf("credential path=%q", completed.CredentialPath)
	}
	want := []string{"invite", "resolve", "oauth", "phone", "import", "quota", "remove", "replace"}
	for index := range want {
		if index >= len(adapter.calls) || adapter.calls[index] != want[index] {
			t.Fatalf("calls=%v want=%v", adapter.calls, want)
		}
	}
}

func TestEngineCredentialLoginCanCheckpointIntoOAuthFallback(t *testing.T) {
	store, _ := newTeamFlowFixture(t, false)
	adapter := &fixtureAdapter{
		credentialAvailable: true,
		fallbackCredential:  true,
		quota:               []int{100},
	}
	now := time.Unix(1_800_000_000, 0)
	engine := NewEngine(store, adapter, Options{
		QuotaPollInterval: time.Second, StepTimeout: time.Minute,
	})
	engine.now = func() time.Time { return now }
	completed := driveWorkflow(t, store, engine, &now, func(item storage.TeamLifecycleWorkflow) bool {
		return item.State == storage.TeamLifecycleCompleted
	})
	if completed.CredentialPath != "oauth" {
		t.Fatalf("credential path=%q", completed.CredentialPath)
	}
	want := []string{"invite", "resolve", "credential_login", "oauth", "import", "quota", "remove", "replace"}
	for index := range want {
		if index >= len(adapter.calls) || adapter.calls[index] != want[index] {
			t.Fatalf("calls=%v want=%v", adapter.calls, want)
		}
	}
}

func TestEngineRetryBackoffPreservesAttemptBudget(t *testing.T) {
	store, _ := newTeamFlowFixture(t, false)
	adapter := &fixtureAdapter{credentialAvailable: true, quota: []int{100}, failInvite: 2}
	now := time.Unix(1_800_000_000, 0)
	engine := NewEngine(store, adapter, Options{
		RetryBase: time.Second, RetryMax: time.Minute,
		QuotaPollInterval: time.Second, StepTimeout: time.Minute,
	})
	engine.now = func() time.Time { return now }
	completed := driveWorkflow(t, store, engine, &now, func(item storage.TeamLifecycleWorkflow) bool {
		return item.State == storage.TeamLifecycleCompleted
	})
	if completed.State != storage.TeamLifecycleCompleted {
		t.Fatalf("completed=%+v", completed)
	}
	inviteCalls := 0
	for _, call := range adapter.calls {
		if call == "invite" {
			inviteCalls++
		}
	}
	if inviteCalls != 3 {
		t.Fatalf("invite calls=%d calls=%v", inviteCalls, adapter.calls)
	}
}

func TestEngineShadowModeCreatesReviewablePlanWithoutAdapterCalls(t *testing.T) {
	store, _ := newTeamFlowFixture(t, true)
	adapter := &fixtureAdapter{}
	now := time.Unix(1_800_000_000, 0)
	engine := NewEngine(store, adapter, Options{})
	engine.now = func() time.Time { return now }
	review := driveWorkflow(t, store, engine, &now, func(item storage.TeamLifecycleWorkflow) bool {
		return item.State == storage.TeamLifecycleReviewRequired
	})
	if review.ErrorClass != "shadow_plan_ready" || len(adapter.calls) != 0 {
		t.Fatalf("review=%+v calls=%v", review, adapter.calls)
	}
	events, err := store.ListTeamLifecycleEvents(context.Background(), review.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[len(events)-1].EventType != "shadow_plan_ready" {
		t.Fatalf("events=%+v", events)
	}
}
