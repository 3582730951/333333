package storage

import (
	"context"
	"database/sql"
	"errors"
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
	_, err := s.db.ExecContext(ctx, `
UPDATE diagnostic_jobs
SET status='queued',updated_at=?,started_at=0,error_code=''
WHERE status IN ('snapshotting','rendering','validating') AND cancel_requested=0`, now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE diagnostic_jobs
SET status='cancelled',updated_at=?,completed_at=?,error_code='cancelled'
WHERE status IN ('queued','snapshotting','rendering','validating') AND cancel_requested<>0`, now, now)
	return err
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

func (s *Store) CompleteDiagnosticJob(ctx context.Context, id, artifactPath, sha256 string, size, expiresAt int64) error {
	now := Now()
	result, err := s.db.ExecContext(ctx, `
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
	return err
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
	_, err := s.db.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status=?,error_code=?,artifact_path='',artifact_size=0,
 artifact_sha256='',updated_at=?,completed_at=?
WHERE id=? AND status IN ('queued','snapshotting','rendering','validating')`,
		status, code, now, now, id)
	return err
}

// RequestDiagnosticJobCancellation returns a removable artifact path only when no
// active download lease exists. The caller may delete that exact path.
func (s *Store) RequestDiagnosticJobCancellation(ctx context.Context, id string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	job, err := scanDiagnosticJob(tx.QueryRowContext(ctx, diagnosticJobSelect+` WHERE id=?`, id))
	if err != nil {
		return "", err
	}
	now := Now()
	path := ""
	switch job.Status {
	case DiagnosticJobQueued, DiagnosticJobSnapshotting, DiagnosticJobRendering, DiagnosticJobValidating:
		_, err = tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET cancel_requested=1,status='cancelled',error_code='cancelled',
 updated_at=?,completed_at=? WHERE id=?`, now, now, id)
	case DiagnosticJobReady:
		if job.DownloadLeases > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE diagnostic_jobs SET cancel_requested=1,updated_at=? WHERE id=?`, now, id)
		} else {
			path = job.ArtifactPath
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
		return "", err
	}
	return path, tx.Commit()
}

func (s *Store) AcquireDiagnosticDownloadLease(ctx context.Context, id string) (DiagnosticJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DiagnosticJob{}, err
	}
	defer tx.Rollback()
	now := Now()
	result, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET download_leases=download_leases+1,updated_at=?
WHERE id=? AND status='ready' AND artifact_path<>'' AND expires_at>?`, now, id, now)
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
	if err := tx.Commit(); err != nil {
		return DiagnosticJob{}, err
	}
	return job, nil
}

func (s *Store) ReleaseDiagnosticDownloadLease(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE diagnostic_jobs SET download_leases=CASE WHEN download_leases>0 THEN download_leases-1 ELSE 0 END,
 updated_at=? WHERE id=?`, Now(), id)
	return err
}

func (s *Store) ExpireDiagnosticJobs(ctx context.Context, now int64) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT artifact_path FROM diagnostic_jobs
WHERE status='ready' AND expires_at>0 AND expires_at<=? AND download_leases=0 AND artifact_path<>''`, now)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE diagnostic_jobs SET status='expired',artifact_path='',artifact_size=0,artifact_sha256='',
 updated_at=? WHERE status='ready' AND expires_at>0 AND expires_at<=? AND download_leases=0`, now, now); err != nil {
		return nil, err
	}
	return paths, tx.Commit()
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
