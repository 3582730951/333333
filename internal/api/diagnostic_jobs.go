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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/google/uuid"
)

const (
	diagnosticJobTimeout       = 30 * time.Minute
	diagnosticArtifactTTL      = 24 * time.Hour
	diagnosticJobPollInterval  = time.Second
	diagnosticJobQueueCapacity = 3 // one running + two queued
	diagnosticArtifactMaxCount = 5
)

var (
	errDiagnosticDLP      = errors.New("diagnostic artifact failed DLP validation")
	errDiagnosticCapacity = errors.New("diagnostic artifact capacity exceeded")
)

func (s *Server) adminDiagnosticJobs(w http.ResponseWriter, r *http.Request) {
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
		path, err := s.store.RequestDiagnosticJobCancellation(r.Context(), id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, &PublicError{Code: "not_found", Message: "Diagnostic job not found."})
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		if path != "" {
			_ = s.removeDiagnosticArtifact(path)
		}
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
		"download": location + "/download",
	})
}

func (s *Server) downloadDiagnosticJob(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	job, err := s.store.AcquireDiagnosticDownloadLease(r.Context(), id)
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
		_ = s.store.ReleaseDiagnosticDownloadLease(releaseCtx, id)
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
	s.diagnosticJobsOnce.Do(func() {
		if err := s.store.ResetInterruptedDiagnosticJobs(ctx); err != nil {
			return
		}
		supervisor.Go(ctx, "diagnostic-job-worker", func(ctx context.Context) {
			ticker := time.NewTicker(diagnosticJobPollInterval)
			defer ticker.Stop()
			for {
				s.runNextDiagnosticJob(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
		supervisor.Go(ctx, "diagnostic-artifact-expiry", func(ctx context.Context) {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				s.cleanupExpiredDiagnosticJobs(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	})
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
				requested, checkErr := s.store.DiagnosticJobCancelled(context.Background(), job.ID)
				if checkErr == nil && requested {
					cancel()
					return
				}
			}
		}
	}()
	artifactPath, generateErr := s.generateDiagnosticArtifact(jobCtx, job.ID)
	close(pollDone)
	if generateErr == nil {
		return
	}
	if artifactPath != "" {
		_ = s.removeDiagnosticArtifact(artifactPath)
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
	_ = s.store.FailDiagnosticJob(context.Background(), job.ID, code)
}

func (s *Server) generateDiagnosticArtifact(ctx context.Context, jobID string) (artifactPath string, err error) {
	dir, err := s.diagnosticArtifactDirectory()
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
	file, err := os.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	cleanupPartial := true
	defer func() {
		_ = file.Close()
		if cleanupPartial {
			_ = os.Remove(partialPath)
		}
	}()

	snapshot, err := s.store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		return partialPath, err
	}
	defer snapshot.Close()
	snapshotStore, err := snapshot.Store(s.store)
	if err != nil {
		return partialPath, err
	}
	if err := s.store.SetDiagnosticJobStatus(ctx, jobID, storage.DiagnosticJobRendering); err != nil {
		return partialPath, err
	}
	limited := &diagnosticLimitedWriter{writer: file, remaining: singleLimit}
	if err := s.writeDiagnosticsExport(ctx, limited, snapshot.ID(), snapshotStore); err != nil {
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
	cleanupPartial = false
	if err := fsyncDiagnosticDirectory(dir); err != nil {
		_ = os.Remove(finalPath)
		return finalPath, err
	}
	if err := s.store.CompleteDiagnosticJob(ctx, jobID, finalPath, digest, size, storage.Now()+int64(diagnosticArtifactTTL/time.Second)); err != nil {
		_ = os.Remove(finalPath)
		return finalPath, err
	}
	return finalPath, nil
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
		diagnosticJWTRE.MatchString(value) || diagnosticEmailRE.MatchString(value) ||
		diagnosticURLRE.MatchString(value) || diagnosticIPv4RE.MatchString(value) ||
		diagnosticIPv6RE.MatchString(value) || diagnosticWindowsPathRE.MatchString(value) ||
		diagnosticUnixPathRE.MatchString(value) {
		return true
	}
	return diagnosticHighEntropyRE.MatchString(value)
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

func (s *Server) removeDiagnosticArtifact(path string) error {
	if !s.validDiagnosticArtifactPath(path) {
		return errors.New("invalid diagnostic artifact path")
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Server) cleanupExpiredDiagnosticJobs(ctx context.Context) {
	paths, err := s.store.ExpireDiagnosticJobs(ctx, storage.Now())
	if err != nil {
		return
	}
	for _, path := range paths {
		_ = s.removeDiagnosticArtifact(path)
	}
}

// The legacy synchronous endpoint now creates an asynchronous v3 job.
func (s *Server) adminDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.createDiagnosticJob(w, r)
}
