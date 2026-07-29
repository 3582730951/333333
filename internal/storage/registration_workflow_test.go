package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCommitRegistrationKeepsCanaryQuarantinedAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "registration-canary.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := Now()
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO registration_jobs(id,platform,method,total,status,config_json,created_at,updated_at)
VALUES('job-canary','chatgpt','protocol',1,'running','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO registration_records(id,job_id,status,created_at)
VALUES('record-canary','job-canary','pending',?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRegistrationWorkflowItem(
		ctx, "workflow-canary", "job-canary", "protocol", "chatgpt",
	); err != nil {
		t.Fatal(err)
	}
	account := Account{
		ID: "account-canary", Label: "canary", GroupName: "default",
		UpstreamAccountID: "upstream-canary", ChatGPTUserID: "user-canary",
		Provider: "codex", Status: "quarantined", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CommitRegistration(ctx, RegistrationCommit{
		Account: account,
		Token: AccountToken{
			AccountID: account.ID, AuthMethod: "oauth", CredentialMode: "chatgpt_auth_tokens",
			AccessToken: "verified-token", CreatedAt: now, UpdatedAt: now,
		},
		EgressID: storageDefaultDirectEgressIDForTest(t, store),
		Method:   "protocol", JobID: "job-canary", RecordID: "record-canary",
		WorkflowItemID: "workflow-canary", RemoteIdentityAlias: "ACC-CANARY",
	}); err != nil {
		t.Fatal(err)
	}
	var accountStatus, workflowState string
	if err := store.DB().QueryRowContext(ctx, `SELECT status FROM accounts WHERE id=?`, account.ID).
		Scan(&accountStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM registration_workflow_items WHERE id='workflow-canary'`).
		Scan(&workflowState); err != nil {
		t.Fatal(err)
	}
	if accountStatus != "quarantined" || workflowState != RegistrationItemQuarantined {
		t.Fatalf("canary state = account:%q workflow:%q", accountStatus, workflowState)
	}
}

func storageDefaultDirectEgressIDForTest(t *testing.T, store *Store) string {
	t.Helper()
	if _, err := store.GetEgressProfile(context.Background(), DefaultDirectEgressID); err != nil {
		t.Fatal(err)
	}
	return DefaultDirectEgressID
}
