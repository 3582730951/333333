package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	DiagnosticJobQueued       = "queued"
	DiagnosticJobSnapshotting = "snapshotting"
	DiagnosticJobRendering    = "rendering"
	DiagnosticJobValidating   = "validating"
	DiagnosticJobReady        = "ready"
	DiagnosticJobFailed       = "failed"
	DiagnosticJobCancelled    = "cancelled"
	DiagnosticJobExpired      = "expired"
)

type DiagnosticJob struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	FormatVersion   string `json:"format_version"`
	ArtifactPath    string `json:"-"`
	ArtifactSize    int64  `json:"artifact_size,omitempty"`
	ArtifactSHA256  string `json:"etag,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	CancelRequested bool   `json:"cancel_requested,omitempty"`
	DownloadLeases  int64  `json:"-"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	StartedAt       int64  `json:"started_at,omitempty"`
	CompletedAt     int64  `json:"completed_at,omitempty"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	DownloadURL     string `json:"download_url,omitempty"`
}

func (s *Store) CreateDiagnosticJob(ctx context.Context, id string, maxActive int) (DiagnosticJob, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DiagnosticJob{}, errors.New("diagnostic job id is required")
	}
	if maxActive <= 0 {
		maxActive = 3
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiagnosticJob{}, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM diagnostic_jobs
WHERE status IN ('queued','snapshotting','rendering','validating')`).Scan(&active); err != nil {
		return DiagnosticJob{}, err
	}
	if active >= maxActive {
		return DiagnosticJob{}, ErrDiagnosticQueueFull
	}
	now := Now()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO diagnostic_jobs(id,status,format_version,created_at,updated_at)
VALUES(?,?,?,?,?)`, id, DiagnosticJobQueued, "v3", now, now); err != nil {
		return DiagnosticJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return DiagnosticJob{}, err
	}
	return DiagnosticJob{ID: id, Status: DiagnosticJobQueued, FormatVersion: "v3", CreatedAt: now, UpdatedAt: now}, nil
}

var ErrDiagnosticQueueFull = errors.New("diagnostic job queue is full")

func scanDiagnosticJob(scanner interface{ Scan(...interface{}) error }) (DiagnosticJob, error) {
	var job DiagnosticJob
	var cancelRequested int
	err := scanner.Scan(
		&job.ID, &job.Status, &job.FormatVersion, &job.ArtifactPath, &job.ArtifactSize,
		&job.ArtifactSHA256, &job.ErrorCode, &cancelRequested, &job.DownloadLeases,
		&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.CompletedAt, &job.ExpiresAt,
	)
	job.CancelRequested = cancelRequested != 0
	return job, err
}

const diagnosticJobSelect = `
SELECT id,status,format_version,artifact_path,artifact_size,artifact_sha256,error_code,
 cancel_requested,download_leases,created_at,updated_at,started_at,completed_at,expires_at
FROM diagnostic_jobs`

func (s *Store) GetDiagnosticJob(ctx context.Context, id string) (DiagnosticJob, error) {
	job, err := scanDiagnosticJob(s.rdb.QueryRowContext(ctx, diagnosticJobSelect+` WHERE id=?`, id))
	return job, err
}

func (s *Store) ListDiagnosticJobs(ctx context.Context, limit int) ([]DiagnosticJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, diagnosticJobSelect+` ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DiagnosticJob, 0)
	for rows.Next() {
		job, scanErr := scanDiagnosticJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) ResetInterruptedDiagnosticJobs(ctx context.Context) error {
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A process restart terminates its in-flight renderers. Preserve their files as
	// eligible resources; the new active worker will move them through trash instead
	// of unlinking an unowned path. Download leases are durable and are not reset:
	// an old A/B worker may still be draining a response.
	if _, err = tx.ExecContext(ctx, `
UPDATE storage_resources SET state='eligible',lease_expires_at=?,updated_at=?
WHERE resource_type=? AND state IN ('creating','active')
AND owner_id IN (
 SELECT id FROM diagnostic_jobs WHERE status IN ('snapshotting','rendering','validating')
)`, now, now, StorageResourceTypeDiagnosticArtifact); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs
SET status='queued',updated_at=?,started_at=0,error_code=''
WHERE status IN ('snapshotting','rendering','validating') AND cancel_requested=0`, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs
SET status='cancelled',updated_at=?,completed_at=?,error_code='cancelled'
WHERE status IN ('queued','snapshotting','rendering','validating') AND cancel_requested<>0`, now, now)
	if err != nil {
		return err
	}
	if err = expireDiagnosticDownloadLeasesTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimDiagnosticJob(ctx context.Context) (DiagnosticJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiagnosticJob{}, err
	}
	defer tx.Rollback()
	job, err := scanDiagnosticJob(tx.QueryRowContext(ctx, diagnosticJobSelect+`
 WHERE status='queued' AND cancel_requested=0 ORDER BY created_at,id LIMIT 1`))
	if err != nil {
		return DiagnosticJob{}, err
	}
	now := Now()
	result, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status='snapshotting',started_at=?,updated_at=?
WHERE id=? AND status='queued' AND cancel_requested=0`, now, now, job.ID)
	if err != nil {
		return DiagnosticJob{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		if err == nil {
			err = sql.ErrNoRows
		}
		return DiagnosticJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return DiagnosticJob{}, err
	}
	job.Status, job.StartedAt, job.UpdatedAt = DiagnosticJobSnapshotting, now, now
	return job, nil
}

func validDiagnosticRunningStatus(status string) bool {
	switch status {
	case DiagnosticJobSnapshotting, DiagnosticJobRendering, DiagnosticJobValidating:
		return true
	default:
		return false
	}
}

func (s *Store) SetDiagnosticJobStatus(ctx context.Context, id, status string) error {
	if !validDiagnosticRunningStatus(status) {
		return errors.New("invalid diagnostic running status")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status=?,updated_at=?
WHERE id=? AND status IN ('snapshotting','rendering','validating') AND cancel_requested=0`, status, Now(), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) DiagnosticJobCancelled(ctx context.Context, id string) (bool, error) {
	var requested int
	var status string
	err := s.rdb.QueryRowContext(ctx, `SELECT cancel_requested,status FROM diagnostic_jobs WHERE id=?`, id).Scan(&requested, &status)
	return requested != 0 || status == DiagnosticJobCancelled, err
}

func (s *Store) CompleteDiagnosticJob(
	ctx context.Context,
	id, resourceID, artifactPath, sha256 string,
	size, expiresAt int64,
) error {
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	resource, err := scanStorageResource(tx.QueryRowContext(ctx, storageResourceSelect+` WHERE id=?`, resourceID))
	if err != nil {
		return err
	}
	if resource.OwnerID != id || resource.ResourceType != StorageResourceTypeDiagnosticArtifact {
		return fmt.Errorf("%w: diagnostic resource owner", ErrStorageResourceConflict)
	}
	if err = transitionStorageResource(ctx, tx, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceActive}, StorageResourceSealed, artifactPath, size, expiresAt); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status='ready',artifact_path=?,artifact_size=?,artifact_sha256=?,
 error_code='',updated_at=?,completed_at=?,expires_at=?
WHERE id=? AND status='validating' AND cancel_requested=0`,
		artifactPath, size, sha256, now, now, expiresAt, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func safeDiagnosticErrorCode(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "cancelled", "timeout", "snapshot_failed", "render_failed", "validation_failed",
		"dlp_failed", "capacity_exceeded", "storage_unavailable", "internal":
		return strings.ToLower(strings.TrimSpace(code))
	default:
		return "internal"
	}
}

func (s *Store) FailDiagnosticJob(ctx context.Context, id, code string) error {
	now := Now()
	status := DiagnosticJobFailed
	code = safeDiagnosticErrorCode(code)
	if code == "cancelled" {
		status = DiagnosticJobCancelled
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `
UPDATE storage_resources SET state='eligible',lease_expires_at=?,updated_at=?
WHERE resource_type=? AND owner_id=? AND state IN ('creating','active','sealed')`,
		now, now, StorageResourceTypeDiagnosticArtifact, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status=?,error_code=?,artifact_path='',artifact_size=0,
 artifact_sha256='',updated_at=?,completed_at=?
WHERE id=? AND status IN ('queued','snapshotting','rendering','validating')`,
		status, code, now, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func diagnosticLegacyResourceID(jobID string) string {
	return "diagnostic_legacy_" + strings.TrimSpace(jobID)
}

func ensureDiagnosticArtifactResourceTx(
	ctx context.Context,
	tx *sql.Tx,
	job DiagnosticJob,
	mountID string,
) (StorageResource, error) {
	mountID = strings.TrimSpace(mountID)
	if mountID == "" {
		return StorageResource{}, errors.New("diagnostic artifact mount id is required")
	}
	resource, err := scanStorageResource(tx.QueryRowContext(ctx, storageResourceSelect+`
 WHERE resource_type=? AND owner_id=? AND path=? AND state='sealed'
 ORDER BY created_at DESC,id DESC LIMIT 1`,
		StorageResourceTypeDiagnosticArtifact, job.ID, job.ArtifactPath))
	if err == nil {
		if resource.MountID != mountID {
			return StorageResource{}, fmt.Errorf("%w: diagnostic mount changed", ErrStorageResourceConflict)
		}
		_, err = tx.ExecContext(ctx, `
UPDATE storage_resources SET lease_expires_at=?,size_bytes=?,updated_at=?
WHERE id=? AND owner_id=? AND fencing_token=? AND state='sealed'`,
			job.ExpiresAt, job.ArtifactSize, Now(), resource.ID, resource.OwnerID, resource.FencingToken)
		resource.LeaseExpiresAt, resource.SizeBytes = job.ExpiresAt, job.ArtifactSize
		return resource, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return StorageResource{}, err
	}
	resource = StorageResource{
		ID:             diagnosticLegacyResourceID(job.ID),
		ResourceType:   StorageResourceTypeDiagnosticArtifact,
		Path:           job.ArtifactPath,
		State:          StorageResourceSealed,
		OwnerID:        job.ID,
		LeaseExpiresAt: job.ExpiresAt,
		FencingToken:   1,
		MountID:        mountID,
		SizeBytes:      job.ArtifactSize,
		RetentionClass: StorageRetentionDiagnosticArtifact,
		CreatedAt:      job.CompletedAt,
		UpdatedAt:      Now(),
	}
	if resource.CreatedAt == 0 {
		resource.CreatedAt = resource.UpdatedAt
	}
	if _, err = validateStorageResource(resource); err != nil {
		return StorageResource{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO storage_resources(
 id,resource_type,path,state,owner_id,lease_expires_at,fencing_token,mount_id,
 size_bytes,retention_class,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		resource.ID, resource.ResourceType, resource.Path, resource.State, resource.OwnerID,
		resource.LeaseExpiresAt, resource.FencingToken, resource.MountID, resource.SizeBytes,
		resource.RetentionClass, resource.CreatedAt, resource.UpdatedAt)
	return resource, err
}

func markDiagnosticArtifactEligibleTx(
	ctx context.Context,
	tx *sql.Tx,
	job DiagnosticJob,
	mountID string,
) error {
	resource, err := ensureDiagnosticArtifactResourceTx(ctx, tx, job, mountID)
	if err != nil {
		return err
	}
	return transitionStorageResource(ctx, tx, resource.ID, resource.OwnerID, resource.FencingToken,
		[]string{StorageResourceSealed}, StorageResourceEligible, "", resource.SizeBytes, Now())
}

// RequestDiagnosticJobCancellation makes a ready artifact eligible only when no
// active download lease exists. A leased artifact remains sealed until the last
// downloader releases it.
func (s *Store) RequestDiagnosticJobCancellation(ctx context.Context, id, mountID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = expireDiagnosticDownloadLeasesTx(ctx, tx, Now()); err != nil {
		return err
	}
	job, err := scanDiagnosticJob(tx.QueryRowContext(ctx, diagnosticJobSelect+` WHERE id=?`, id))
	if err != nil {
		return err
	}
	now := Now()
	switch job.Status {
	case DiagnosticJobQueued, DiagnosticJobSnapshotting, DiagnosticJobRendering, DiagnosticJobValidating:
		_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET cancel_requested=1,status='cancelled',error_code='cancelled',
 updated_at=?,completed_at=? WHERE id=?`, now, now, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `
UPDATE storage_resources SET state='eligible',lease_expires_at=?,updated_at=?
WHERE resource_type=? AND owner_id=? AND state IN ('creating','active')`,
				now, now, StorageResourceTypeDiagnosticArtifact, id)
		}
	case DiagnosticJobReady:
		if job.DownloadLeases > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE diagnostic_jobs SET cancel_requested=1,updated_at=? WHERE id=?`, now, id)
		} else {
			if err = markDiagnosticArtifactEligibleTx(ctx, tx, job, mountID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status='cancelled',cancel_requested=1,error_code='cancelled',
 artifact_path='',artifact_size=0,artifact_sha256='',updated_at=?,completed_at=? WHERE id=?`, now, now, id)
		}
	case DiagnosticJobFailed, DiagnosticJobCancelled, DiagnosticJobExpired:
		// Idempotent.
	default:
		err = errors.New("unknown diagnostic job status")
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func expireDiagnosticDownloadLeasesTx(ctx context.Context, tx *sql.Tx, now int64) error {
	rows, err := tx.QueryContext(ctx, `
SELECT job_id,COUNT(*) FROM diagnostic_download_leases
WHERE expires_at<=? GROUP BY job_id`, now)
	if err != nil {
		return err
	}
	expiredByJob := make(map[string]int64)
	for rows.Next() {
		var jobID string
		var count int64
		if err := rows.Scan(&jobID, &count); err != nil {
			rows.Close()
			return err
		}
		expiredByJob[jobID] = count
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM diagnostic_download_leases WHERE expires_at<=?`, now); err != nil {
		return err
	}
	for jobID, count := range expiredByJob {
		if _, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET download_leases=CASE
 WHEN download_leases>? THEN download_leases-? ELSE 0 END,updated_at=?
WHERE id=?`, count, count, now, jobID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AcquireDiagnosticDownloadLease(
	ctx context.Context,
	id, leaseID, ownerID, mountID string,
	leaseExpiresAt int64,
) (DiagnosticJob, error) {
	leaseID, ownerID = strings.TrimSpace(leaseID), strings.TrimSpace(ownerID)
	now := Now()
	if leaseID == "" || len(leaseID) > 256 || ownerID == "" || len(ownerID) > 256 || leaseExpiresAt <= now {
		return DiagnosticJob{}, errors.New("invalid diagnostic download lease")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiagnosticJob{}, err
	}
	defer tx.Rollback()
	if err = expireDiagnosticDownloadLeasesTx(ctx, tx, now); err != nil {
		return DiagnosticJob{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET download_leases=download_leases+1,updated_at=?
WHERE id=? AND status='ready' AND cancel_requested=0 AND artifact_path<>'' AND expires_at>?`, now, id, now)
	if err != nil {
		return DiagnosticJob{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		if err == nil {
			err = sql.ErrNoRows
		}
		return DiagnosticJob{}, err
	}
	job, err := scanDiagnosticJob(tx.QueryRowContext(ctx, diagnosticJobSelect+` WHERE id=?`, id))
	if err != nil {
		return DiagnosticJob{}, err
	}
	if _, err = ensureDiagnosticArtifactResourceTx(ctx, tx, job, mountID); err != nil {
		return DiagnosticJob{}, err
	}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO diagnostic_download_leases(lease_id,job_id,owner_id,expires_at,created_at,updated_at)
VALUES(?,?,?,?,?,?)`, leaseID, id, ownerID, leaseExpiresAt, now, now); err != nil {
		return DiagnosticJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return DiagnosticJob{}, err
	}
	return job, nil
}

func (s *Store) ReleaseDiagnosticDownloadLease(ctx context.Context, id, leaseID, mountID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return errors.New("diagnostic download lease id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	if err = expireDiagnosticDownloadLeasesTx(ctx, tx, now); err != nil {
		return err
	}
	job, err := scanDiagnosticJob(tx.QueryRowContext(ctx, diagnosticJobSelect+` WHERE id=?`, id))
	if err != nil {
		return err
	}
	remaining := job.DownloadLeases
	result, err := tx.ExecContext(ctx, `
DELETE FROM diagnostic_download_leases WHERE lease_id=? AND job_id=?`, leaseID, id)
	if err != nil {
		return err
	}
	released, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if released == 1 && remaining > 0 {
		remaining--
	}
	if job.Status == DiagnosticJobReady && job.CancelRequested && remaining == 0 {
		if err = markDiagnosticArtifactEligibleTx(ctx, tx, job, mountID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status='cancelled',download_leases=0,error_code='cancelled',
 artifact_path='',artifact_size=0,artifact_sha256='',updated_at=?,completed_at=?
WHERE id=? AND status='ready' AND cancel_requested<>0`, now, now, id)
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET download_leases=?,updated_at=? WHERE id=?`, remaining, now, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ExpireDiagnosticJobs(ctx context.Context, now int64, mountID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = expireDiagnosticDownloadLeasesTx(ctx, tx, now); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, diagnosticJobSelect+`
 WHERE status='ready' AND download_leases=0 AND artifact_path<>''
 AND (cancel_requested<>0 OR (expires_at>0 AND expires_at<=?))
 ORDER BY updated_at,id`, now)
	if err != nil {
		return 0, err
	}
	var jobs []DiagnosticJob
	for rows.Next() {
		job, scanErr := scanDiagnosticJob(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		jobs = append(jobs, job)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, job := range jobs {
		if err := markDiagnosticArtifactEligibleTx(ctx, tx, job, mountID); err != nil {
			return 0, err
		}
		status, code := DiagnosticJobExpired, ""
		if job.CancelRequested {
			status, code = DiagnosticJobCancelled, "cancelled"
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status=?,error_code=?,artifact_path='',artifact_size=0,
 artifact_sha256='',updated_at=?,completed_at=CASE WHEN completed_at=0 THEN ? ELSE completed_at END
WHERE id=? AND status='ready' AND download_leases=0`,
			status, code, now, now, job.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(jobs), nil
}

func (s *Store) DiagnosticArtifactUsage(ctx context.Context) (count int, bytes int64, err error) {
	err = s.rdb.QueryRowContext(ctx, `
SELECT COUNT(*),COALESCE(SUM(artifact_size),0) FROM diagnostic_jobs
WHERE status='ready' AND artifact_path<>''`).Scan(&count, &bytes)
	return
}

type DiagnosticEvent struct {
	ID            string `json:"id"`
	EventType     string `json:"event_type"`
	Severity      string `json:"severity"`
	EntityType    string `json:"entity_type,omitempty"`
	EntityAlias   string `json:"entity_alias,omitempty"`
	DetailJSON    string `json:"detail_json"`
	DiagnosticGap bool   `json:"diagnostic_gap"`
	CreatedAt     int64  `json:"created_at"`
}

func (s *Store) AddDiagnosticEvent(ctx context.Context, event DiagnosticEvent) error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.EventType) == "" {
		return errors.New("diagnostic event id and type are required")
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = Now()
	}
	if event.DetailJSON == "" {
		event.DetailJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO diagnostic_events(id,event_type,severity,entity_type,entity_alias,detail_json,diagnostic_gap,created_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`,
		event.ID, event.EventType, event.Severity, event.EntityType, event.EntityAlias,
		event.DetailJSON, boolInt(event.DiagnosticGap), event.CreatedAt)
	if err != nil {
		return err
	}
	// Bound retention by both age and count without a long-lived transaction.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM diagnostic_events WHERE created_at<?`, Now()-180*24*60*60)
	var count int
	if s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM diagnostic_events`).Scan(&count) == nil && count > 100_000 {
		var cutoff int64
		if s.rdb.QueryRowContext(ctx, `
SELECT created_at FROM diagnostic_events ORDER BY created_at DESC,id DESC LIMIT 1 OFFSET 99999`).Scan(&cutoff) == nil {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM diagnostic_events WHERE created_at<?`, cutoff)
		}
	}
	return nil
}
