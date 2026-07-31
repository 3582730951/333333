package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func seedTeamWorkspace(t *testing.T, store *Store) TeamWorkspace {
	t.Helper()
	workspace, err := store.UpsertTeamWorkspace(context.Background(), TeamWorkspace{
		ID:              "workspace-fixture",
		Name:            "Lifecycle fixture",
		ParentAccountID: "parent-account-ref",
		WorkspaceRef:    "workspace-remote-ref",
		ConnectorKind:   "fixture",
		MaxMembers:      8,
		Status:          TeamWorkspaceStatusActive,
	})
	if err != nil {
		t.Fatalf("upsert team workspace: %v", err)
	}
	return workspace
}

func TestTeamLifecycleSchemaAndIdempotentCreate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	workspace := seedTeamWorkspace(t, store)

	input := CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:     "fixture-cycle-1",
		WorkspaceID:        workspace.ID,
		ParentAccountID:    workspace.ParentAccountID,
		ChildAccountID:     "child-account-ref",
		RotateThresholdBPS: 100,
		MaxAttempts:        4,
		ShadowMode:         true,
	}
	first, created, err := store.CreateTeamLifecycleWorkflow(ctx, input)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if !created || first.State != TeamLifecycleQueued || !first.ShadowMode {
		t.Fatalf("unexpected first workflow: created=%v item=%+v", created, first)
	}
	second, created, err := store.CreateTeamLifecycleWorkflow(ctx, input)
	if err != nil {
		t.Fatalf("repeat workflow: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("idempotency mismatch: created=%v first=%s second=%s", created, first.ID, second.ID)
	}
	events, err := store.ListTeamLifecycleEvents(ctx, first.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "created" {
		t.Fatalf("events=%+v", events)
	}
}

func TestTeamLifecycleLeaseTransitionConflictAndEventSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	workspace := seedTeamWorkspace(t, store)
	workflow, _, err := store.CreateTeamLifecycleWorkflow(ctx, CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:  "fixture-cycle-lease",
		WorkspaceID:     workspace.ID,
		ParentAccountID: workspace.ParentAccountID,
		ChildAccountID:  "child-account-ref",
		ShadowMode:      false,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimTeamLifecycleWorkflow(ctx, "worker-a", Now(), 60)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claimed.ID != workflow.ID || claimed.Version != workflow.Version+1 {
		t.Fatalf("claimed=%+v workflow=%+v", claimed, workflow)
	}
	if _, ok, err := store.ClaimTeamLifecycleWorkflow(ctx, "worker-b", Now(), 60); err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	if err := store.RenewTeamLifecycleLease(ctx, claimed.ID, claimed.Version, "worker-b", Now()+120); !errors.Is(err, ErrTeamLifecycleLeaseMismatch) {
		t.Fatalf("wrong-owner renewal err=%v", err)
	}
	next, err := store.TransitionTeamLifecycleWorkflow(ctx, claimed.ID, claimed.Version, "worker-a", TeamLifecycleUpdate{
		ToState:         TeamLifecycleInviting,
		Attempt:         0,
		EventType:       "prepared",
		EventDetailJSON: `{"operation_key":"fixture"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.State != TeamLifecycleInviting || next.LeaseOwner != "" || next.Version != claimed.Version+1 {
		t.Fatalf("transition=%+v", next)
	}
	if _, err := store.TransitionTeamLifecycleWorkflow(ctx, claimed.ID, claimed.Version, "worker-a", TeamLifecycleUpdate{
		ToState: TeamLifecycleInviting,
	}); !errors.Is(err, ErrTeamLifecycleVersionConflict) {
		t.Fatalf("stale transition err=%v", err)
	}
	events, err := store.ListTeamLifecycleEvents(ctx, workflow.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].FromState != TeamLifecycleQueued || events[1].ToState != TeamLifecycleInviting {
		t.Fatalf("transition events=%+v", events)
	}
}

func TestTeamLifecycleReviewRetryAndCancel(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	workspace := seedTeamWorkspace(t, store)
	_, _, err := store.CreateTeamLifecycleWorkflow(ctx, CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:  "fixture-cycle-review",
		WorkspaceID:     workspace.ID,
		ParentAccountID: workspace.ParentAccountID,
		ChildAccountID:  "child-account-ref",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimTeamLifecycleWorkflow(ctx, "worker-a", Now(), 60)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	inviting, err := store.TransitionTeamLifecycleWorkflow(ctx, claimed.ID, claimed.Version, "worker-a", TeamLifecycleUpdate{
		ToState: TeamLifecycleInviting, Attempt: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = store.ClaimTeamLifecycleWorkflow(ctx, "worker-a", Now(), 60)
	if err != nil || !ok {
		t.Fatalf("claim inviting ok=%v err=%v", ok, err)
	}
	if claimed.ID != inviting.ID {
		t.Fatalf("claimed workflow %s, want %s", claimed.ID, inviting.ID)
	}
	review, err := store.TransitionTeamLifecycleWorkflow(ctx, claimed.ID, claimed.Version, "worker-a", TeamLifecycleUpdate{
		ToState: TeamLifecycleReviewRequired, ResumeState: TeamLifecycleInviting,
		Attempt: 3, ErrorClass: "fixture_failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := store.RetryTeamLifecycleWorkflow(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != TeamLifecycleInviting || retried.Attempt != 0 || retried.ErrorClass != "" {
		t.Fatalf("retried=%+v", retried)
	}
	cancelled, err := store.CancelTeamLifecycleWorkflow(ctx, retried.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != TeamLifecycleCancelled || cancelled.CompletedAt == 0 {
		t.Fatalf("cancelled=%+v", cancelled)
	}
}

func TestTeamLifecycleInitUpgradesLegacyWorkspaceSchema(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "legacy-team.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`
CREATE TABLE team_workspaces(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  parent_account_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  workspace_type TEXT NOT NULL DEFAULT 'chatgpt_team',
  max_members INTEGER NOT NULL DEFAULT 10,
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init over legacy team schema: %v", err)
	}
	workspace := seedTeamWorkspace(t, store)
	workflow, created, err := store.CreateTeamLifecycleWorkflow(context.Background(), CreateTeamLifecycleWorkflowInput{
		IdempotencyKey:    "legacy-upgrade-cycle",
		WorkspaceID:       workspace.ID,
		ParentAccountID:   workspace.ParentAccountID,
		ChildAccountID:    "child-account-ref",
		ReplacementMethod: "browser_v3",
		ShadowMode:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || workflow.ReplacementMethod != "browser_v3" {
		t.Fatalf("workflow=%+v created=%v", workflow, created)
	}
	if workspace.MailboxProviderKey != "" || workspace.RequiredEmailDomain != "" || workspace.SameDomainRequired {
		t.Fatalf("legacy mailbox policy defaults changed: %+v", workspace)
	}
}

func TestTeamLifecycleEnforcesAndInheritsSameDomainMailboxPolicy(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{
		ID: "parent-account", Email: "mother@example.test",
		GroupName: "team", Status: "active",
	}, AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTeamWorkspace(ctx, TeamWorkspace{
		ID: "wrong-domain", Name: "Wrong domain", ParentAccountID: "parent-account",
		WorkspaceRef: "remote-wrong", ConnectorKind: "fixture",
		MailboxProviderKey: "cf_team", RequiredEmailDomain: "other.test",
		SameDomainRequired: true,
	}); err == nil {
		t.Fatal("workspace accepted a required domain that does not match its local parent")
	}

	workspace, err := store.UpsertTeamWorkspace(ctx, TeamWorkspace{
		ID: "same-domain", Name: "Same domain", ParentAccountID: "parent-account",
		WorkspaceRef: "remote-same", ConnectorKind: "fixture",
		MailboxProviderKey: "CF_TEAM", RequiredEmailDomain: "Example.Test.",
		SameDomainRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.MailboxProviderKey != "cf_team" ||
		workspace.RequiredEmailDomain != "example.test" || !workspace.SameDomainRequired {
		t.Fatalf("normalized workspace=%+v", workspace)
	}

	if _, _, err := store.CreateTeamLifecycleWorkflow(ctx, CreateTeamLifecycleWorkflowInput{
		IdempotencyKey: "wrong-child-domain", WorkspaceID: workspace.ID,
		ParentAccountID: workspace.ParentAccountID, ChildAccountID: "child@other.test",
	}); err == nil {
		t.Fatal("workflow accepted a child outside the required domain")
	}
	workflow, created, err := store.CreateTeamLifecycleWorkflow(ctx, CreateTeamLifecycleWorkflowInput{
		IdempotencyKey: "same-child-domain", WorkspaceID: workspace.ID,
		ParentAccountID: workspace.ParentAccountID, ChildAccountID: "child@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || workflow.MailboxProviderKey != "cf_team" ||
		workflow.RequiredEmailDomain != "example.test" ||
		workflow.ReplacementMethod != "protocol_v2" {
		t.Fatalf("workflow did not inherit mailbox policy: created=%v workflow=%+v", created, workflow)
	}
	if _, _, err := store.CreateTeamLifecycleWorkflow(ctx, CreateTeamLifecycleWorkflowInput{
		IdempotencyKey: "phone-only-replacement", WorkspaceID: workspace.ID,
		ParentAccountID: workspace.ParentAccountID, ChildAccountID: "another@example.test",
		ReplacementMethod: "browser",
	}); err == nil {
		t.Fatal("same-domain workflow accepted a phone-only replacement method")
	}
}
