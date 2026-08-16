package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-account-pool/internal/storage"
)

const (
	emergencyDiagnosticReadTimeout = 750 * time.Millisecond
	emergencyDiagnosticAuditLimit  = 500
	rescueDiagnosticFullTimeout    = 30 * time.Second
)

// streamRescueDiagnosticsExport first tries the complete, snapshot-consistent
// v3 archive. If the database, spool, optional renderer, or filesystem is not
// usable, it falls back to a small in-memory archive with bounded best-effort
// reads plus process-local health. The fallback does not create a diagnostic
// job, write the database, or touch the spool filesystem.
func (s *Server) streamRescueDiagnosticsExport(ctx context.Context, w http.ResponseWriter) (bool, error) {
	fullCtx, cancel := context.WithTimeout(ctx, rescueDiagnosticFullTimeout)
	err := s.streamDiagnosticsExport(fullCtx, w)
	cancel()
	if err == nil {
		return true, nil
	} else if diagnosticResponseStarted(w) {
		// Delivery itself failed after the ZIP response began (normally a client
		// disconnect). Appending another ZIP or a JSON error would corrupt it.
		return true, err
	} else {
		return s.streamEmergencyDiagnosticsExport(w, err)
	}
}

func diagnosticResponseStarted(w http.ResponseWriter) bool {
	if w == nil {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Type")))
	return strings.Contains(contentType, "application/zip") ||
		strings.Contains(contentType, "application/octet-stream")
}

func (s *Server) streamEmergencyDiagnosticsExport(w http.ResponseWriter, fullExportErr error) (bool, error) {
	archive, causeCode, err := s.buildEmergencyDiagnosticsArchive(fullExportErr)
	if err != nil {
		return false, err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics-emergency-v3.zip"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	w.Header().Set("X-Codex-Diagnostic-Degraded", "true")
	w.Header().Set("X-Codex-Diagnostic-Full-Error", causeCode)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(archive)
	return true, err
}

func (s *Server) buildEmergencyDiagnosticsArchive(fullExportErr error) ([]byte, string, error) {
	causeCode := emergencyDiagnosticFailureCode(fullExportErr)
	var auditRows []storage.AuditLogRow
	var auditErr error
	var goalRows [][]string
	var goalErr error

	// These reads are intentionally independent and short. A write-locked SQLite
	// database often still serves WAL readers, allowing the rescue package to keep
	// the exact audit/context evidence. A corrupt or unavailable database merely
	// turns the affected file into a declared gap instead of failing the archive.
	if s != nil && s.store != nil {
		readCtx, cancel := context.WithTimeout(context.Background(), emergencyDiagnosticReadTimeout)
		auditRows, auditErr = s.store.ListAuditLog(readCtx, emergencyDiagnosticAuditLimit)
		cancel()

		goalCtx, goalCancel := context.WithTimeout(context.Background(), emergencyDiagnosticReadTimeout)
		goalRows, goalErr = diagnosticGoalContinuityRows(goalCtx, s.store.ReadDB(), s.cfg.RuntimeDiagnosticAliasKey)
		goalCancel()
	} else {
		auditErr = errors.New("diagnostic store unavailable")
		goalErr = auditErr
	}

	aliasKey := []byte(nil)
	if s != nil {
		aliasKey = s.cfg.RuntimeDiagnosticAliasKey
	}
	codebook := buildDiagnosticCodebookWithKey(aliasKey, nil, auditRows, nil, nil, nil, nil)
	files, err := buildDiagnosticsZipFiles(
		nil, nil, auditRows, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, codebook,
	)
	if err != nil {
		return nil, causeCode, err
	}

	goalHeader := []string{
		"goal_code", "parent_goal_code", "protocol", "branch_present",
		"downstream_scope_present", "workspace_fingerprint_present",
		"initial_fingerprint_present", "last_response_present", "state",
		"checkpoint_code", "storage_bytes", "checkpoint_count", "segment_count",
		"last_segment_sequence", "alias_count", "active_run_count", "failed_run_count",
		"expires_at", "created_at", "updated_at",
	}
	if goalErr == nil {
		files["goal_continuity.csv"] = codebook.sanitize(csvString(goalHeader, goalRows))
	}

	gaps := make([]string, 0, 3)
	readStatus := map[string]interface{}{}
	if auditErr == nil {
		readStatus["audit_log"] = map[string]interface{}{"status": "included", "rows": len(auditRows)}
	} else {
		gaps = append(gaps, "audit_log")
		readStatus["audit_log"] = map[string]interface{}{
			"status": "unavailable", "error_code": emergencyDiagnosticFailureCode(auditErr),
		}
	}
	if goalErr == nil {
		readStatus["goal_continuity"] = map[string]interface{}{"status": "included", "rows": len(goalRows)}
	} else {
		gaps = append(gaps, "goal_continuity")
		readStatus["goal_continuity"] = map[string]interface{}{
			"status": "unavailable", "error_code": emergencyDiagnosticFailureCode(goalErr),
		}
	}
	gaps = append(gaps, "snapshot_consistency")

	emergency := map[string]interface{}{
		"mode":                   "emergency_memory",
		"degraded":               true,
		"full_export_error_code": causeCode,
		"snapshot_consistency":   "bounded_best_effort_reads",
		"read_status":            readStatus,
		"data_gaps":              gaps,
	}

	var summary map[string]interface{}
	if err := json.Unmarshal([]byte(files["diagnostic_summary.json"]), &summary); err != nil {
		return nil, causeCode, err
	}
	summary["emergency_export"] = emergency
	if err := setEmergencyDiagnosticJSON(files, "diagnostic_summary.json", summary, codebook); err != nil {
		return nil, causeCode, err
	}

	runtimeStorage := map[string]interface{}{}
	if s != nil {
		runtimeStorage = s.runtimeStorageDiagnostics()
	}
	runtimeStorage["emergency_export"] = emergency
	if err := setEmergencyDiagnosticJSON(files, "runtime_storage.json", runtimeStorage, codebook); err != nil {
		return nil, causeCode, err
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		return nil, causeCode, err
	}
	for key, value := range emergency {
		manifest[key] = value
	}
	manifest["format"] = "codex-pool-diagnostics-v3"
	manifest["files"] = diagnosticFileOrder()
	if rowCounts, ok := manifest["row_counts"].(map[string]interface{}); ok {
		rowCounts["audit_log.csv"] = len(auditRows)
		if goalErr == nil {
			rowCounts["goal_continuity.csv"] = len(goalRows)
		}
	}
	if err := setEmergencyDiagnosticJSON(files, "manifest.json", manifest, codebook); err != nil {
		return nil, causeCode, err
	}

	archive, err := emergencyDiagnosticZip(files)
	if err == nil {
		err = validateEmergencyDiagnosticArchive(archive)
	}
	if err == nil {
		return archive, causeCode, nil
	}

	// A newly introduced runtime field must never make the emergency route fail a
	// DLP check. Drop all dynamic rows and retry with the fixed schema. This last
	// resort still carries the failure class and proves which tables were absent.
	lastResort, fallbackErr := buildLastResortDiagnosticArchive(causeCode)
	if fallbackErr != nil {
		return nil, causeCode, errors.Join(err, fallbackErr)
	}
	return lastResort, causeCode, nil
}

func setEmergencyDiagnosticJSON(files map[string]string, name string, value interface{}, codebook diagnosticCodebook) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	files[name] = codebook.sanitize(string(raw) + "\n")
	return nil
}

func emergencyDiagnosticZip(files map[string]string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range diagnosticFileOrder() {
		content, ok := files[name]
		if !ok {
			content = ""
		}
		entry, err := writer.Create(name)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if _, err = entry.Write([]byte(content)); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func validateEmergencyDiagnosticArchive(raw []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return errDiagnosticDLP
	}
	for _, entry := range reader.File {
		if entry.Name == "account_map.csv" || strings.Contains(entry.Name, "..") || strings.HasPrefix(entry.Name, "/") {
			return errDiagnosticDLP
		}
		if err := validateDiagnosticEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func buildLastResortDiagnosticArchive(causeCode string) ([]byte, error) {
	files := make(map[string]string, len(diagnosticFileOrder()))
	for _, name := range diagnosticFileOrder() {
		if strings.HasSuffix(name, ".csv") {
			files[name] = "status\nnot_available\n"
		} else {
			files[name] = "{}\n"
		}
	}
	manifest := map[string]interface{}{
		"generated_at": time.Now().Unix(), "format": "codex-pool-diagnostics-v3",
		"mode": "emergency_memory_last_resort", "degraded": true,
		"full_export_error_code": causeCode, "files": diagnosticFileOrder(),
		"data_gaps": []string{"database_snapshot", "runtime_snapshot", "snapshot_consistency"},
	}
	summary := map[string]interface{}{
		"emergency_export": map[string]interface{}{
			"mode": "emergency_memory_last_resort", "degraded": true,
			"full_export_error_code": causeCode,
		},
	}
	for name, value := range map[string]interface{}{"manifest.json": manifest, "diagnostic_summary.json": summary} {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, err
		}
		files[name] = string(raw) + "\n"
	}
	archive, err := emergencyDiagnosticZip(files)
	if err != nil {
		return nil, err
	}
	if err := validateEmergencyDiagnosticArchive(archive); err != nil {
		return nil, fmt.Errorf("validate last-resort diagnostic archive: %w", err)
	}
	return archive, nil
}

func emergencyDiagnosticFailureCode(err error) string {
	if err == nil {
		return "explicit_emergency"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, errDiagnosticDLP):
		return "dlp_validation_failed"
	case errors.Is(err, errDiagnosticCapacity), errors.Is(err, syscall.ENOSPC):
		return "storage_capacity"
	case errors.Is(err, os.ErrPermission):
		return "storage_permission"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "database is locked"), strings.Contains(message, "database is busy"):
		return "database_locked"
	case strings.Contains(message, "readonly"), strings.Contains(message, "read-only"), strings.Contains(message, "not writable"):
		return "database_read_only"
	case strings.Contains(message, "no space"), strings.Contains(message, "disk full"):
		return "storage_capacity"
	case strings.Contains(message, "corrupt"), strings.Contains(message, "malformed"):
		return "database_corrupt"
	case strings.Contains(message, "unavailable"):
		return "storage_unavailable"
	default:
		return "full_export_unavailable"
	}
}
