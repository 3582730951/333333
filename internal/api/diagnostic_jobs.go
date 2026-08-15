package api

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/google/uuid"
)

const (
	// Finish on the server before the browser's five-minute deadline so the
	// final failed/timeout state is observable instead of racing the UI timer.
	diagnosticJobTimeout           = 4*time.Minute + 30*time.Second
	diagnosticArtifactTTL          = 24 * time.Hour
	diagnosticJobPollInterval      = time.Second
	diagnosticJobQueueCapacity     = 3 // one running + two queued
	diagnosticArtifactMaxCount     = 5
	diagnosticDownloadLeaseTTL     = 24 * time.Hour
	diagnosticSnapshotGuardPoll    = 500 * time.Millisecond
	diagnosticSnapshotMaxWALGrowth = int64(512 << 20)
	diagnosticWALFinalizeThreshold = int64(256 << 20)
	diagnosticWALFinalizeTimeout   = 2 * time.Second
)

var (
	errDiagnosticDLP      = errors.New("diagnostic artifact failed DLP validation")
	errDiagnosticCapacity = errors.New("diagnostic artifact capacity exceeded")
)

func (s *Server) adminDiagnosticJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createDiagnosticJob(w, r)
	case http.MethodGet:
		jobs, err := s.store.ListDiagnosticJobs(r.Context(), 100)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for index := range jobs {
			populateDiagnosticJobLinks(&jobs[index])
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminDiagnosticJobAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/diagnostics/jobs/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || strings.Contains(parts[0], "..") {
		writeError(w, http.StatusNotFound, &PublicError{Code: "not_found", Message: "Diagnostic job not found."})
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "download" {
		s.downloadDiagnosticJob(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, &PublicError{Code: "not_found", Message: "Diagnostic job not found."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		job, err := s.store.GetDiagnosticJob(r.Context(), id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, &PublicError{Code: "not_found", Message: "Diagnostic job not found."})
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		populateDiagnosticJobLinks(&job)
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		mountID, err := s.diagnosticArtifactMountID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		err = s.store.RequestDiagnosticJobCancellation(r.Context(), id, mountID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, &PublicError{Code: "not_found", Message: "Diagnostic job not found."})
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		s.cleanupEligibleDiagnosticArtifacts(r.Context())
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func populateDiagnosticJobLinks(job *storage.DiagnosticJob) {
	if job == nil {
		return
	}
	if job.Status == storage.DiagnosticJobReady {
		// Kept outside the persisted row so artifacts can move between hosts without
		// baking an origin into the database.
		job.ErrorCode = ""
		job.DownloadURL = "/admin/diagnostics/jobs/" + job.ID + "/download"
	}
}

func (s *Server) createDiagnosticJob(w http.ResponseWriter, r *http.Request) {
	if s.diskGuardPausesBackground() {
		writePublicServiceUnavailable(w)
		return
	}
	s.cleanupExpiredDiagnosticJobs(r.Context())
	count, _, err := s.store.DiagnosticArtifactUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if count >= diagnosticArtifactMaxCount {
		writeError(w, http.StatusServiceUnavailable, errDiagnosticCapacity)
		return
	}
	id := "diagjob_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	job, err := s.store.CreateDiagnosticJob(r.Context(), id, diagnosticJobQueueCapacity)
	if err != nil {
		if errors.Is(err, storage.ErrDiagnosticQueueFull) {
			writeError(w, http.StatusServiceUnavailable, err)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	location := "/admin/diagnostics/jobs/" + job.ID
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job": job, "location": location,
	})
	s.wakeDiagnosticJobWorker()
}

func (s *Server) downloadDiagnosticJob(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	mountID, err := s.diagnosticArtifactMountID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	leaseID := "diaglease_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	leaseOwner := strings.TrimSpace(s.cfg.NodeID)
	if leaseOwner == "" {
		leaseOwner = "local"
	}
	leaseOwner += ":" + strconv.Itoa(os.Getpid())
	job, err := s.store.AcquireDiagnosticDownloadLease(
		r.Context(), id, leaseID, leaseOwner, mountID,
		storage.Now()+int64(diagnosticDownloadLeaseTTL/time.Second),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, &PublicError{Code: "not_ready", Message: "Diagnostic artifact is not ready."})
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if s.store.ReleaseDiagnosticDownloadLease(releaseCtx, id, leaseID, mountID) == nil {
			s.cleanupEligibleDiagnosticArtifacts(releaseCtx)
		}
	}()
	if !s.validDiagnosticArtifactPath(job.ArtifactPath) {
		writeError(w, http.StatusInternalServerError, errors.New("invalid diagnostic artifact path"))
		return
	}
	file, err := os.Open(job.ArtifactPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusInternalServerError, errors.New("diagnostic artifact unavailable"))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics-v3-`+id+`.zip"`)
	w.Header().Set("ETag", `"`+job.ArtifactSHA256+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, filepath.Base(job.ArtifactPath), time.Unix(job.CompletedAt, 0), file)
}

func (s *Server) startDiagnosticJobLoop(ctx context.Context) {
	// These loops belong to the active worker lease, not the process lifetime.
	// They must restart after an active -> standby -> active transition. A
	// process-scoped sync.Once permanently stranded queued jobs after the first
	// active context was cancelled.
	supervisor.Go(ctx, "diagnostic-job-worker", func(ctx context.Context) {
		if err := s.store.ResetInterruptedDiagnosticJobs(ctx); err != nil {
			// Returning lets the supervisor retry with bounded backoff. In
			// particular, a transient SQLite lock at activation must not disable
			// diagnostics until the next process restart.
			if !errors.Is(err, context.Canceled) {
				log.Printf("[DIAGNOSTICS] worker initialization failed; retrying: %v", err)
			}
			return
		}
		s.cleanupLegacyDiagnosticSnapshots()
		ticker := time.NewTicker(diagnosticJobPollInterval)
		defer ticker.Stop()
		for {
			s.runNextDiagnosticJob(ctx)
			select {
			case <-ctx.Done():
				return
			case <-s.diagnosticJobWake:
			case <-ticker.C:
			}
		}
	})
	supervisor.Go(ctx, "diagnostic-artifact-expiry", func(ctx context.Context) {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			s.cleanupExpiredDiagnosticJobs(ctx)
			// A previous active process can still hold an old snapshot during the
			// first cleanup pass. Retry in the maintenance loop so it is reclaimed
			// promptly after the final descriptor is released.
			s.cleanupLegacyDiagnosticSnapshots()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (s *Server) cleanupLegacyDiagnosticSnapshots() {
	if s == nil || s.store == nil {
		return
	}
	removed, err := s.store.CleanupLegacyDiagnosticSnapshots()
	if err != nil {
		log.Printf("[DIAGNOSTICS] legacy snapshot cleanup incomplete: %v", err)
	} else if removed > 0 {
		log.Printf("[DIAGNOSTICS] reclaimed %d legacy SQLite snapshot files", removed)
		s.diagnostics.walFinalizePending.Store(true)
	}
	s.finalizeDiagnosticWALIfNeeded()
}

// finalizeDiagnosticWALIfNeeded is deliberately called only by the diagnostics
// maintenance loops. A legacy physical snapshot or an unusually large WAL arms
// one short TRUNCATE attempt; a busy reader leaves a pending bit for the next
// minute instead of blocking request traffic or spinning.
func (s *Server) finalizeDiagnosticWALIfNeeded() {
	if s == nil || s.store == nil {
		return
	}
	walPath := s.store.DiagnosticWALPath()
	if walPath == "" {
		s.diagnostics.walFinalizePending.Store(false)
		return
	}
	before := diagnosticFileSize(walPath)
	if before < diagnosticWALFinalizeThreshold && !s.diagnostics.walFinalizePending.Load() {
		return
	}
	s.diagnostics.walFinalizePending.Store(true)
	if !s.diagnostics.walFinalizeRunning.CompareAndSwap(false, true) {
		return
	}
	defer s.diagnostics.walFinalizeRunning.Store(false)
	checkpointCtx, cancel := context.WithTimeout(context.Background(), diagnosticWALFinalizeTimeout)
	completed, err := s.store.TryTruncateDiagnosticWAL(checkpointCtx)
	cancel()
	if err != nil {
		// Cancellation, a live reader, or a transient SQLite lock is retried by the
		// next diagnostics maintenance pass. Keep logs for persistent non-timeout
		// failures without turning maintenance into a request-path dependency.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[DIAGNOSTICS] SQLite WAL finalization deferred: %v", err)
		}
		return
	}
	if !completed {
		return
	}
	s.diagnostics.walFinalizePending.Store(false)
	after := diagnosticFileSize(walPath)
	if before >= diagnosticWALFinalizeThreshold || before > after {
		log.Printf("[DIAGNOSTICS] finalized SQLite WAL before_bytes=%d after_bytes=%d", before, after)
	}
}

func (s *Server) wakeDiagnosticJobWorker() {
	if s == nil || s.diagnosticJobWake == nil {
		return
	}
	select {
	case s.diagnosticJobWake <- struct{}{}:
	default:
		// One pending notification is sufficient to avoid a missed wake; the
		// periodic poll handles any additional jobs coalesced behind it.
	}
}

func (s *Server) runNextDiagnosticJob(ctx context.Context) {
	if s.diskGuardPausesBackground() {
		return
	}
	job, err := s.store.ClaimDiagnosticJob(ctx)
	if errors.Is(err, sql.ErrNoRows) || ctx.Err() != nil {
		return
	}
	if err != nil {
		return
	}
	jobCtx, cancel := context.WithTimeout(ctx, diagnosticJobTimeout)
	defer cancel()
	pollDone := make(chan struct{})
	go func() {
		defer supervisor.Recover("diagnostic-cancel-poller")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollDone:
				return
			case <-ticker.C:
				checkCtx, checkCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				requested, checkErr := s.store.DiagnosticJobCancelled(checkCtx, job.ID)
				checkCancel()
				if checkErr == nil && requested {
					cancel()
					return
				}
			}
		}
	}()
	_, generateErr := s.generateDiagnosticArtifact(jobCtx, job.ID)
	close(pollDone)
	if generateErr == nil {
		return
	}
	code := "internal"
	switch {
	case errors.Is(generateErr, context.DeadlineExceeded):
		code = "timeout"
	case errors.Is(generateErr, context.Canceled):
		code = "cancelled"
	case errors.Is(generateErr, errDiagnosticDLP):
		code = "dlp_failed"
	case errors.Is(generateErr, errDiagnosticCapacity):
		code = "capacity_exceeded"
	}
	failCtx, failCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	_ = s.store.FailDiagnosticJob(failCtx, job.ID, code)
	s.cleanupEligibleDiagnosticArtifacts(failCtx)
	failCancel()
}

func (s *Server) generateDiagnosticArtifact(ctx context.Context, jobID string) (artifactPath string, err error) {
	dir, err := s.diagnosticArtifactDirectory()
	if err != nil {
		return "", err
	}
	mountID, err := diagnosticMountID(dir)
	if err != nil {
		return "", err
	}
	singleLimit, totalLimit, reserve, available, err := diagnosticFilesystemLimits(dir)
	if err != nil {
		return "", err
	}
	count, used, err := s.store.DiagnosticArtifactUsage(ctx)
	if err != nil {
		return "", err
	}
	if count >= diagnosticArtifactMaxCount || used >= totalLimit || available <= reserve {
		return "", errDiagnosticCapacity
	}
	partialPath := filepath.Join(dir, "."+jobID+".partial")
	finalPath := filepath.Join(dir, jobID+".zip")
	resource, err := s.store.CreateStorageResource(ctx, storage.StorageResource{
		ID:             "diagres_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		ResourceType:   storage.StorageResourceTypeDiagnosticArtifact,
		Path:           partialPath,
		OwnerID:        jobID,
		LeaseExpiresAt: storage.Now() + int64(diagnosticJobTimeout/time.Second),
		FencingToken:   1,
		MountID:        mountID,
		RetentionClass: storage.StorageRetentionDiagnosticArtifact,
	})
	if err != nil {
		return "", err
	}
	var file *os.File
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = s.store.MarkStorageResourceEligible(cleanupCtx, resource)
			s.cleanupEligibleDiagnosticArtifacts(cleanupCtx)
			cleanupCancel()
		}
	}()
	file, err = os.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if err = s.store.ActivateStorageResource(ctx, resource); err != nil {
		return partialPath, err
	}
	resource.State = storage.StorageResourceActive

	snapshot, err := s.store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		return partialPath, err
	}
	defer snapshot.Close()
	snapshotCtx, stopSnapshotGuard := s.startDiagnosticSnapshotGuard(
		ctx, dir, reserve, available,
	)
	defer stopSnapshotGuard()
	snapshotStore, err := snapshot.Store(s.store)
	if err != nil {
		return partialPath, err
	}
	if err := s.store.SetDiagnosticJobStatus(snapshotCtx, jobID, storage.DiagnosticJobRendering); err != nil {
		if errors.Is(context.Cause(snapshotCtx), errDiagnosticCapacity) {
			s.recordDiagnosticExportGap(jobID, "snapshot_storage_pressure")
			return partialPath, errDiagnosticCapacity
		}
		return partialPath, err
	}
	limited := &diagnosticLimitedWriter{writer: file, remaining: singleLimit}
	if err := s.writeDiagnosticsExport(snapshotCtx, limited, snapshot.ID(), snapshotStore); err != nil {
		if errors.Is(context.Cause(snapshotCtx), errDiagnosticCapacity) {
			s.recordDiagnosticExportGap(jobID, "snapshot_storage_pressure")
			return partialPath, errDiagnosticCapacity
		}
		return partialPath, err
	}
	if err := file.Sync(); err != nil {
		return partialPath, err
	}
	if err := file.Close(); err != nil {
		return partialPath, err
	}
	if err := snapshot.Close(); err != nil {
		return partialPath, err
	}
	if err := stopSnapshotGuard(); err != nil {
		s.recordDiagnosticExportGap(jobID, "snapshot_storage_pressure")
		return partialPath, err
	}
	cancelled, err := s.store.DiagnosticJobCancelled(ctx, jobID)
	if err != nil {
		return partialPath, err
	}
	if cancelled {
		return partialPath, context.Canceled
	}
	if err := s.store.SetDiagnosticJobStatus(ctx, jobID, storage.DiagnosticJobValidating); err != nil {
		return partialPath, err
	}
	size, digest, err := validateDiagnosticArtifact(partialPath)
	if err != nil {
		return partialPath, err
	}
	if size > singleLimit || used+size > totalLimit {
		return partialPath, errDiagnosticCapacity
	}
	_, _, _, availableAfter, err := diagnosticFilesystemLimits(dir)
	if err != nil || availableAfter < reserve {
		return partialPath, errDiagnosticCapacity
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		return partialPath, err
	}
	if err := fsyncDiagnosticDirectory(dir); err != nil {
		return finalPath, err
	}
	if err := s.store.CompleteDiagnosticJob(
		ctx, jobID, resource.ID, finalPath, digest, size,
		storage.Now()+int64(diagnosticArtifactTTL/time.Second),
	); err != nil {
		return finalPath, err
	}
	return finalPath, nil
}

func (s *Server) startDiagnosticSnapshotGuard(
	parent context.Context,
	artifactDir string,
	reserve, initialAvailable int64,
) (context.Context, func() error) {
	return s.startDiagnosticSnapshotGuardWithInterval(
		parent, artifactDir, reserve, initialAvailable, diagnosticSnapshotGuardPoll,
	)
}

func (s *Server) startDiagnosticSnapshotGuardWithInterval(
	parent context.Context,
	artifactDir string,
	reserve, initialAvailable int64,
	pollInterval time.Duration,
) (context.Context, func() error) {
	if pollInterval <= 0 {
		pollInterval = diagnosticSnapshotGuardPoll
	}
	ctx, cancel := context.WithCancelCause(parent)
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	databaseWAL := ""
	if s != nil && s.store != nil {
		databaseWAL = s.store.DiagnosticWALPath()
	}
	databaseDir := ""
	databaseReserve := int64(0)
	databaseAvailable := int64(0)
	var databaseStatErr error
	if databaseWAL != "" {
		databaseDir = filepath.Dir(databaseWAL)
		_, _, databaseReserve, databaseAvailable, databaseStatErr = diagnosticFilesystemLimits(databaseDir)
	}
	baselineWAL := diagnosticFileSize(databaseWAL)
	headroom := initialAvailable - reserve
	if databaseDir != "" && databaseAvailable-databaseReserve < headroom {
		headroom = databaseAvailable - databaseReserve
	}
	maxWALGrowth := diagnosticSnapshotMaxWALGrowth
	if half := headroom / 2; half < maxWALGrowth {
		maxWALGrowth = half
	}
	if maxWALGrowth < 0 {
		maxWALGrowth = 0
	}

	go func() {
		defer supervisor.Recover("diagnostic-snapshot-guard")
		defer close(done)
		if databaseStatErr != nil {
			cancel(errDiagnosticCapacity)
			return
		}
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				_, _, _, available, statErr := diagnosticFilesystemLimits(artifactDir)
				if statErr != nil || available <= reserve {
					cancel(errDiagnosticCapacity)
					return
				}
				if databaseDir != "" {
					_, _, currentReserve, currentAvailable, databaseErr := diagnosticFilesystemLimits(databaseDir)
					if databaseErr != nil || currentAvailable <= currentReserve {
						cancel(errDiagnosticCapacity)
						return
					}
				}
				if databaseWAL != "" && diagnosticFileSize(databaseWAL)-baselineWAL > maxWALGrowth {
					cancel(errDiagnosticCapacity)
					return
				}
			}
		}
	}()
	stopGuard := func() error {
		stopOnce.Do(func() { close(stop) })
		<-done
		cause := context.Cause(ctx)
		cancel(context.Canceled)
		if errors.Is(cause, errDiagnosticCapacity) {
			return errDiagnosticCapacity
		}
		return nil
	}
	return ctx, stopGuard
}

func diagnosticFileSize(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func (s *Server) recordDiagnosticExportGap(jobID, reason string) {
	eventCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	alias := ""
	if len(s.cfg.RuntimeDiagnosticAliasKey) > 0 {
		alias = diagnosticAlias(s.cfg.RuntimeDiagnosticAliasKey, "JOB", "diagnostic-job", jobID)
	}
	_ = s.store.AddDiagnosticEvent(eventCtx, storage.DiagnosticEvent{
		ID:            "diagevt_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		EventType:     "diagnostic_export",
		Severity:      "warning",
		EntityType:    "diagnostic_job",
		EntityAlias:   alias,
		DetailJSON:    `{"reason":"` + reason + `","stage":"snapshot"}`,
		DiagnosticGap: true,
	})
}

type diagnosticLimitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *diagnosticLimitedWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.remaining {
		return 0, errDiagnosticCapacity
	}
	n, err := w.writer.Write(value)
	w.remaining -= int64(n)
	return n, err
}

func (s *Server) diagnosticArtifactDirectory() (string, error) {
	dir := strings.TrimSpace(s.cfg.DiagnosticsDir)
	if dir == "" {
		if path := strings.TrimSpace(s.store.Path()); path != "" && path != ":memory:" {
			dir = filepath.Join(filepath.Dir(strings.SplitN(path, "?", 2)[0]), "diagnostics")
		} else {
			dir = filepath.Join(os.TempDir(), "codex-pool-diagnostics")
		}
	}
	if err := datadir.RecoverDirectory(dir); err != nil {
		return "", err
	}
	return filepath.Abs(dir)
}

func diagnosticFilesystemLimits(dir string) (single, totalQuota, reserve, available int64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(dir, &stat); err != nil {
		return
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	available = int64(stat.Bavail) * int64(stat.Bsize)
	single = minDiagnosticInt64(4<<30, total/10)
	totalQuota = minDiagnosticInt64(8<<30, total/5)
	reserve = maxDiagnosticInt64(1<<30, total/10)
	return
}

func minDiagnosticInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxDiagnosticInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func validateDiagnosticArtifact(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	_ = file.Close()
	if err != nil {
		return 0, "", err
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return 0, "", errDiagnosticDLP
	}
	defer reader.Close()
	var uncompressed uint64
	for _, entry := range reader.File {
		if entry.Name == "account_map.csv" || strings.Contains(entry.Name, "..") || strings.HasPrefix(entry.Name, "/") {
			return 0, "", errDiagnosticDLP
		}
		uncompressed += entry.UncompressedSize64
		if uncompressed > 16<<30 {
			return 0, "", errDiagnosticCapacity
		}
		if err := validateDiagnosticEntry(entry); err != nil {
			return 0, "", err
		}
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateDiagnosticEntry(entry *zip.File) error {
	stream, err := entry.Open()
	if err != nil {
		return errDiagnosticDLP
	}
	defer stream.Close()
	if strings.HasSuffix(strings.ToLower(entry.Name), ".csv") {
		reader := csv.NewReader(stream)
		reader.FieldsPerRecord = -1
		reader.ReuseRecord = true
		for {
			row, readErr := reader.Read()
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return errDiagnosticDLP
			}
			for _, cell := range row {
				if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
					return errDiagnosticDLP
				}
				if diagnosticDLPMatch(cell) {
					return errDiagnosticDLP
				}
			}
		}
	}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		if diagnosticDLPMatch(scanner.Text()) {
			return errDiagnosticDLP
		}
	}
	if scanner.Err() != nil {
		return errDiagnosticDLP
	}
	return nil
}

func diagnosticDLPMatch(value string) bool {
	if diagnosticPrivateKeyRE.MatchString(value) || diagnosticBearerRE.MatchString(value) ||
		diagnosticJWTRE.MatchString(value) || diagnosticSecretPrefixRE.MatchString(value) ||
		diagnosticContainsUnsafeRequestID(value) || diagnosticEmailRE.MatchString(value) ||
		diagnosticURLRE.MatchString(value) || diagnosticIPv4RE.MatchString(value) ||
		diagnosticIPv6RE.MatchString(value) || diagnosticWindowsPathRE.MatchString(value) ||
		diagnosticContainsUnixPath(value) {
		return true
	}
	return diagnosticContainsHighEntropy(value)
}

func diagnosticContainsUnsafeRequestID(value string) bool {
	for _, candidate := range diagnosticRequestIDRE.FindAllString(value, -1) {
		upper := strings.ToUpper(candidate)
		if !diagnosticPublicRequestIDRE.MatchString(upper) && !diagnosticStableAliasRE.MatchString(upper) {
			return true
		}
	}
	return false
}

var diagnosticIPv6RE = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}\b`)

func fsyncDiagnosticDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func (s *Server) validDiagnosticArtifactPath(path string) bool {
	dir, err := s.diagnosticArtifactDirectory()
	if err != nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return filepath.Dir(absolute) == dir && strings.HasSuffix(filepath.Base(absolute), ".zip")
}

func diagnosticMountID(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("diagnostic filesystem identity unavailable")
	}
	return "dev:" + strconv.FormatUint(uint64(stat.Dev), 10), nil
}

func (s *Server) diagnosticArtifactMountID() (string, error) {
	dir, err := s.diagnosticArtifactDirectory()
	if err != nil {
		return "", err
	}
	return diagnosticMountID(dir)
}

func validDiagnosticStorageResourcePath(dir, path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	dir = filepath.Clean(dir)
	parent, base := filepath.Dir(absolute), filepath.Base(absolute)
	if parent == dir {
		return strings.HasSuffix(base, ".zip") ||
			(strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".partial"))
	}
	return parent == filepath.Join(dir, ".trash") && strings.HasSuffix(base, ".trash")
}

func alternateDiagnosticArtifactPath(dir, path string) string {
	base := filepath.Base(path)
	switch {
	case filepath.Dir(path) == dir && strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".partial"):
		jobID := strings.TrimSuffix(strings.TrimPrefix(base, "."), ".partial")
		return filepath.Join(dir, jobID+".zip")
	case filepath.Dir(path) == dir && strings.HasSuffix(base, ".zip"):
		jobID := strings.TrimSuffix(base, ".zip")
		return filepath.Join(dir, "."+jobID+".partial")
	default:
		return ""
	}
}

func diagnosticTrashPath(dir string, resource storage.StorageResource) string {
	sum := sha256.Sum256([]byte(resource.ID))
	base := filepath.Base(resource.Path)
	return filepath.Join(dir, ".trash", base+"."+hex.EncodeToString(sum[:8])+".trash")
}

func regularDiagnosticFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("diagnostic resource is not a regular file")
	}
	return true, nil
}

// cleanupEligibleDiagnosticArtifacts is the only diagnostic artifact deletion
// path. It requires a durable eligible record, claims it with a fencing CAS,
// atomically moves the file into a private trash directory, then removes it.
func (s *Server) cleanupEligibleDiagnosticArtifacts(ctx context.Context) {
	dir, err := s.diagnosticArtifactDirectory()
	if err != nil {
		return
	}
	mountID, err := diagnosticMountID(dir)
	if err != nil {
		return
	}
	trashDir := filepath.Join(dir, ".trash")
	if err := datadir.RecoverDirectory(trashDir); err != nil {
		return
	}
	resources, err := s.store.ListStorageResourcesForGC(
		ctx,
		storage.StorageResourceTypeDiagnosticArtifact,
		storage.StorageRetentionDiagnosticArtifact,
		storage.Now(),
		32,
	)
	if err != nil {
		return
	}
	for _, resource := range resources {
		if resource.MountID != mountID || !validDiagnosticStorageResourcePath(dir, resource.Path) {
			continue
		}
		if resource.State == storage.StorageResourceEligible {
			if err := s.store.ClaimStorageResourceTrash(ctx, resource); err != nil {
				continue
			}
			resource.State = storage.StorageResourceTrash
		}
		if resource.State != storage.StorageResourceTrash {
			continue
		}
		if s.deleteTrashedDiagnosticResource(ctx, dir, trashDir, resource) == nil {
			s.recordDiagnosticGCEvent(ctx, resource)
		}
	}
}

func (s *Server) deleteTrashedDiagnosticResource(
	ctx context.Context,
	dir, trashDir string,
	resource storage.StorageResource,
) error {
	path := filepath.Clean(resource.Path)
	trashPath := diagnosticTrashPath(dir, resource)
	if filepath.Dir(path) == trashDir {
		trashPath = path
	}
	exists, err := regularDiagnosticFile(path)
	if err != nil {
		return err
	}
	if !exists && filepath.Dir(path) == dir {
		alternate := alternateDiagnosticArtifactPath(dir, path)
		if alternate != "" {
			if alternateExists, alternateErr := regularDiagnosticFile(alternate); alternateErr != nil {
				return alternateErr
			} else if alternateExists {
				path, exists = alternate, true
			}
		}
	}
	if !exists && path != trashPath {
		if trashExists, trashErr := regularDiagnosticFile(trashPath); trashErr != nil {
			return trashErr
		} else if trashExists {
			path, exists = trashPath, true
		}
	}
	if !exists {
		return s.store.MarkStorageResourceDeleted(ctx, resource)
	}
	if filepath.Dir(path) != trashDir {
		if destinationExists, destinationErr := regularDiagnosticFile(trashPath); destinationErr != nil {
			return destinationErr
		} else if destinationExists {
			return errors.New("diagnostic trash destination already exists")
		}
		if err := os.Rename(path, trashPath); err != nil {
			return err
		}
		if err := fsyncDiagnosticDirectory(dir); err != nil {
			return err
		}
		if err := fsyncDiagnosticDirectory(trashDir); err != nil {
			return err
		}
		if err := s.store.UpdateStorageResourceTrashPath(ctx, resource, trashPath); err != nil {
			return err
		}
		resource.Path = trashPath
	}
	if exists, err = regularDiagnosticFile(resource.Path); err != nil {
		return err
	} else if exists {
		if err := os.Remove(resource.Path); err != nil {
			return err
		}
		if err := fsyncDiagnosticDirectory(trashDir); err != nil {
			return err
		}
	}
	return s.store.MarkStorageResourceDeleted(ctx, resource)
}

// recordDiagnosticGCEvent records ordinary artifact lifecycle maintenance. Deleting
// an expired or explicitly cancelled ZIP does not remove the primary audit, usage,
// request, or diagnostic-event rows from which a future bundle is generated, so it
// is neither a warning nor a diagnostic data gap.
func (s *Server) recordDiagnosticGCEvent(ctx context.Context, resource storage.StorageResource) {
	alias := ""
	if len(s.cfg.RuntimeDiagnosticAliasKey) > 0 {
		alias = diagnosticAlias(s.cfg.RuntimeDiagnosticAliasKey, "JOB", "diagnostic-job", resource.OwnerID)
	}
	_ = s.store.AddDiagnosticEvent(ctx, storage.DiagnosticEvent{
		ID:            "diagevt_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		EventType:     "storage_gc",
		Severity:      "info",
		EntityType:    "diagnostic_job",
		EntityAlias:   alias,
		DetailJSON:    `{"reason":"retention_or_cancellation","resource_type":"diagnostic_artifact"}`,
		DiagnosticGap: false,
	})
}

func (s *Server) cleanupExpiredDiagnosticJobs(ctx context.Context) {
	mountID, err := s.diagnosticArtifactMountID()
	if err != nil {
		return
	}
	if _, err = s.store.ExpireDiagnosticJobs(ctx, storage.Now(), mountID); err != nil {
		return
	}
	s.cleanupEligibleDiagnosticArtifacts(ctx)
}

// The legacy synchronous endpoint now creates an asynchronous v3 job.
func (s *Server) adminDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	// The normal path remains the bounded asynchronous renderer. A diagnostic
	// export is also the recovery tool for a failed optional worker, so a stranded
	// queue must not make the evidence unobtainable. The UI invokes this same-origin
	// rescue mode only after the job path fails or times out; it snapshots and
	// streams directly on the requesting worker without relying on the background
	// diagnostic loop. Physical storage/database failure can still make any export
	// impossible, but a context-loss incident or dead renderer cannot.
	if r.Method == http.MethodGet && r.URL.Query().Get("mode") == "rescue" {
		w.Header().Set("X-Codex-Diagnostic-Mode", "rescue")
		if err := s.streamDiagnosticsExport(r.Context(), w); err != nil {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	s.createDiagnosticJob(w, r)
}
