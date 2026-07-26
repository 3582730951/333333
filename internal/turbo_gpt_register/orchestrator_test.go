package turbo_gpt_register

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

type fakeExecutor struct{}

func (fakeExecutor) Execute(_ context.Context, phase string, _ ExecutorInput) (ExecutorResult, error) {
	switch phase {
	case Phase1:
		return ExecutorResult{Success: true, Data: map[string]interface{}{
			"phone": "+15550001111", "email": "user@example.com", "password": "pw",
			"completed_through": Phase2,
		}}, nil
	case Phase3:
		return ExecutorResult{Success: true, Data: map[string]interface{}{
			"token": map[string]interface{}{
				"email": "user@example.com", "access_token": "access", "refresh_token": "refresh",
			},
		}}, nil
	default:
		return ExecutorResult{Success: true, Data: map[string]interface{}{}}, nil
	}
}

func TestOrchestratorAdvancesLegacyPhaseBundleAndStoresToken(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("orchestrator-test-secret"))
	o := New(store, fakeExecutor{}, Options{MaxConcurrent: 1})
	job, err := o.CreateJob(ctx, CreateJobRequest{AutoImport: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunNext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = o.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Phase != Phase3 || job.Phase2CompletedAt == 0 {
		t.Fatalf("phase1 bundle did not advance to phase3: %+v", job)
	}
	if err := o.RunNext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job, _ = o.GetJob(ctx, job.ID)
	if job.Status != StatusCompleted || job.Phase != PhaseDone {
		t.Fatalf("job not completed: %+v", job)
	}
	token, err := o.GetToken(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "refresh" {
		t.Fatalf("refresh token = %q", token.RefreshToken)
	}
}
