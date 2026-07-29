package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecoverInterruptedRegistrationWorkflows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "registration-recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO registration_jobs(id,platform,method,total,status,config_json,created_at,updated_at)
		 VALUES('job-running','chatgpt','protocol_v2',2,'running','{}',1,1)`,
		`INSERT INTO registration_jobs(id,platform,method,total,status,config_json,created_at,updated_at)
		 VALUES('job-queued','chatgpt','node',1,'queued','{}',1,1)`,
		`INSERT INTO registration_jobs(id,platform,method,total,status,config_json,created_at,updated_at)
		 VALUES('job-complete','chatgpt','browser',1,'completed','{}',1,1)`,
		`INSERT INTO registration_records(id,job_id,status,created_at)
		 VALUES('record-success','job-running','success',1)`,
		`INSERT INTO registration_records(id,job_id,status,created_at)
		 VALUES('record-pending','job-running','pending',1)`,
		`INSERT INTO registration_records(id,job_id,status,created_at)
		 VALUES('record-complete','job-complete','pending',1)`,
		`INSERT INTO registration_workflow_items(id,job_id,method,state,created_at,updated_at)
		 VALUES('item-running','job-running','protocol_v2','registering',1,1)`,
		`INSERT INTO registration_workflow_items(id,job_id,method,state,created_at,updated_at)
		 VALUES('item-retry','job-queued','node','retry_wait',1,1)`,
		`INSERT INTO registration_workflow_items(id,job_id,method,state,created_at,updated_at)
		 VALUES('item-active','job-running','protocol_v2','active',1,1)`,
		`INSERT INTO registration_workflow_items(id,job_id,method,state,created_at,updated_at)
		 VALUES('item-complete','job-complete','browser','preflight',1,1)`,
	} {
		if _, err := store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.RecoverInterruptedRegistrationWorkflows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobsFinalized != 2 || got.RecordsFailed != 1 || got.ItemsQuarantined != 2 {
		t.Fatalf("recovery result = %+v", got)
	}

	var status, errorClass string
	var succeeded, failed int
	if err := store.DB().QueryRowContext(ctx, `
SELECT status,succeeded,failed,error FROM registration_jobs WHERE id='job-running'`).
		Scan(&status, &succeeded, &failed, &errorClass); err != nil {
		t.Fatal(err)
	}
	if status != "completed_with_review" || succeeded != 1 || failed != 1 || errorClass != "interrupted_requires_review" {
		t.Fatalf("running job = status:%q succeeded:%d failed:%d error:%q", status, succeeded, failed, errorClass)
	}
	if err := store.DB().QueryRowContext(ctx, `
SELECT state,error_class FROM registration_workflow_items WHERE id='item-running'`).
		Scan(&status, &errorClass); err != nil {
		t.Fatal(err)
	}
	if status != RegistrationItemQuarantined || errorClass != "interrupted_requires_review" {
		t.Fatalf("interrupted item = state:%q error:%q", status, errorClass)
	}
	if err := store.DB().QueryRowContext(ctx, `
SELECT state FROM registration_workflow_items WHERE id='item-active'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != RegistrationItemActive {
		t.Fatalf("committed item changed to %q", status)
	}
	if err := store.DB().QueryRowContext(ctx, `
SELECT status FROM registration_jobs WHERE id='job-complete'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("completed job changed to %q", status)
	}

	again, err := store.RecoverInterruptedRegistrationWorkflows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != (RegistrationRecoveryResult{}) {
		t.Fatalf("second recovery was not idempotent: %+v", again)
	}
}

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
