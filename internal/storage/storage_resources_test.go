package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestStorageResourceStateMachineFencesGC(t *testing.T) {
	store := newMaintenanceLeaseStore(t)
	ctx := context.Background()
	resource, err := store.CreateStorageResource(ctx, StorageResource{
		ID:             "resource-1",
		ResourceType:   StorageResourceTypeDiagnosticArtifact,
		Path:           filepath.Join(t.TempDir(), ".job.partial"),
		OwnerID:        "job-1",
		LeaseExpiresAt: Now() + 60,
		FencingToken:   7,
		MountID:        "dev:1",
		RetentionClass: StorageRetentionDiagnosticArtifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStorageResource(ctx, resource); !errors.Is(err, ErrStorageResourceConflict) {
		t.Fatalf("duplicate create err=%v, want ErrStorageResourceConflict", err)
	}
	if err := store.ActivateStorageResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	resource.State = StorageResourceActive

	stale := resource
	stale.FencingToken++
	if err := store.MarkStorageResourceEligible(ctx, stale); !errors.Is(err, ErrStorageResourceFenced) {
		t.Fatalf("stale transition err=%v, want ErrStorageResourceFenced", err)
	}
	if err := store.MarkStorageResourceEligible(ctx, resource); err != nil {
		t.Fatal(err)
	}
	resource.State = StorageResourceEligible
	resource.LeaseExpiresAt = Now()
	candidates, err := store.ListStorageResourcesForGC(
		ctx, StorageResourceTypeDiagnosticArtifact, StorageRetentionDiagnosticArtifact, Now(), 10,
	)
	if err != nil || len(candidates) != 1 || candidates[0].ID != resource.ID {
		t.Fatalf("GC candidates=%+v err=%v", candidates, err)
	}
	resource = candidates[0]
	if err := store.ClaimStorageResourceTrash(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimStorageResourceTrash(ctx, resource); !errors.Is(err, ErrStorageResourceFenced) {
		t.Fatalf("duplicate claim err=%v, want ErrStorageResourceFenced", err)
	}
	resource.State = StorageResourceTrash
	trashPath := filepath.Join(t.TempDir(), "resource.trash")
	if err := store.UpdateStorageResourceTrashPath(ctx, resource, trashPath); err != nil {
		t.Fatal(err)
	}
	resource.Path = trashPath
	if err := store.MarkStorageResourceDeleted(ctx, resource); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetStorageResource(ctx, resource.ID)
	if err != nil || persisted.State != StorageResourceDeleted || persisted.SizeBytes != 0 {
		t.Fatalf("deleted resource=%+v err=%v", persisted, err)
	}
}

func TestDiagnosticCancellationWaitsForLastDownloadLease(t *testing.T) {
	store := newMaintenanceLeaseStore(t)
	ctx := context.Background()
	job, err := store.CreateDiagnosticJob(ctx, "diagnostic-job", 3)
	if err != nil {
		t.Fatal(err)
	}
	if job, err = store.ClaimDiagnosticJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiagnosticJobStatus(ctx, job.ID, DiagnosticJobRendering); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiagnosticJobStatus(ctx, job.ID, DiagnosticJobValidating); err != nil {
		t.Fatal(err)
	}
	resource, err := store.CreateStorageResource(ctx, StorageResource{
		ID:             "diagnostic-resource",
		ResourceType:   StorageResourceTypeDiagnosticArtifact,
		Path:           filepath.Join(t.TempDir(), ".diagnostic-job.partial"),
		OwnerID:        job.ID,
		LeaseExpiresAt: Now() + 60,
		FencingToken:   1,
		MountID:        "dev:7",
		RetentionClass: StorageRetentionDiagnosticArtifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateStorageResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(filepath.Dir(resource.Path), "diagnostic-job.zip")
	if err := store.CompleteDiagnosticJob(ctx, job.ID, resource.ID, finalPath, "digest", 42, Now()+60); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireDiagnosticDownloadLease(
		ctx, job.ID, "lease-1", "worker-1", "dev:7", Now()+60,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetInterruptedDiagnosticJobs(ctx); err != nil {
		t.Fatal(err)
	}
	afterRestart, err := store.GetDiagnosticJob(ctx, job.ID)
	if err != nil || afterRestart.DownloadLeases != 1 {
		t.Fatalf("restart cleared a live download lease: job=%+v err=%v", afterRestart, err)
	}
	if err := store.RequestDiagnosticJobCancellation(ctx, job.ID, "dev:7"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.GetDiagnosticJob(ctx, job.ID)
	if err != nil || pending.Status != DiagnosticJobReady || !pending.CancelRequested || pending.DownloadLeases != 1 {
		t.Fatalf("leased cancellation=%+v err=%v", pending, err)
	}
	if _, err := store.AcquireDiagnosticDownloadLease(
		ctx, job.ID, "lease-2", "worker-2", "dev:7", Now()+60,
	); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("new lease after cancellation err=%v, want sql.ErrNoRows", err)
	}
	if err := store.ReleaseDiagnosticDownloadLease(ctx, job.ID, "lease-1", "dev:7"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.GetDiagnosticJob(ctx, job.ID)
	if err != nil || cancelled.Status != DiagnosticJobCancelled || cancelled.DownloadLeases != 0 ||
		cancelled.ArtifactPath != "" {
		t.Fatalf("released cancellation=%+v err=%v", cancelled, err)
	}
	persisted, err := store.GetStorageResource(ctx, resource.ID)
	if err != nil || persisted.State != StorageResourceEligible {
		t.Fatalf("eligible resource=%+v err=%v", persisted, err)
	}
}
