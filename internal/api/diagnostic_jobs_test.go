package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestDiagnosticJobV3StableAliasesDLPAndRangeDownload(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	accountID := h.importAccount(t, "diagnostic@example.test", "upstream-sensitive-id", "secret-diagnostic-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.app.startDiagnosticJobLoop(ctx)
	if err := h.store.AddDiagnosticEvent(ctx, storage.DiagnosticEvent{
		ID:          "raw-event-internal-id",
		EventType:   "storage_failure",
		Severity:    "error",
		EntityType:  "account",
		EntityAlias: "raw-account-internal-id",
		DetailJSON: `{
			"reason":"disk_full",
			"contact":"event-leak@example.net",
			"credential":"Bearer abcdefghijklmnopqrstuvwxyz123456",
			"path":"/srv/private/secrets/token.txt",
			"unknown":"customer-internal-note"
		}`,
	}); err != nil {
		t.Fatal(err)
	}

	create := func() storage.DiagnosticJob {
		request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/admin/diagnostics/jobs", strings.NewReader(`{}`))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("create status=%d body=%s", response.StatusCode, raw)
		}
		var envelope struct {
			Job storage.DiagnosticJob `json:"job"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			job, getErr := h.store.GetDiagnosticJob(context.Background(), envelope.Job.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			switch job.Status {
			case storage.DiagnosticJobReady:
				return job
			case storage.DiagnosticJobFailed, storage.DiagnosticJobCancelled:
				t.Fatalf("job=%+v", job)
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("diagnostic job did not finish")
		return storage.DiagnosticJob{}
	}

	first := create()
	archive, err := zip.OpenReader(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var firstAlias string
	var diagnosticEvents []byte
	names := map[string]bool{}
	for _, entry := range archive.File {
		names[entry.Name] = true
		if entry.Name == "diagnostic_events.csv" {
			stream, openErr := entry.Open()
			if openErr != nil {
				t.Fatal(openErr)
			}
			diagnosticEvents, openErr = io.ReadAll(stream)
			stream.Close()
			if openErr != nil {
				t.Fatal(openErr)
			}
		}
		if entry.Name != "accounts_snapshot.csv" {
			continue
		}
		stream, openErr := entry.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		rows, readErr := csv.NewReader(stream).ReadAll()
		stream.Close()
		if readErr != nil || len(rows) < 2 {
			t.Fatalf("account snapshot rows=%v err=%v", rows, readErr)
		}
		firstAlias = rows[1][0]
	}
	archive.Close()
	if names["account_map.csv"] || !strings.HasPrefix(firstAlias, "ACC-") {
		t.Fatalf("v3 archive names=%v alias=%q", names, firstAlias)
	}
	if !names["diagnostic_events.csv"] ||
		!bytes.Contains(diagnosticEvents, []byte("storage_failure")) ||
		!bytes.Contains(diagnosticEvents, []byte("disk_full")) ||
		!bytes.Contains(diagnosticEvents, []byte("EVT-")) ||
		!bytes.Contains(diagnosticEvents, []byte("ACC-")) {
		t.Fatalf("diagnostic events missing or unaliased:\n%s", diagnosticEvents)
	}
	rawArchive, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if out := strings.TrimSpace(os.Getenv("CODEX_DIAGNOSTIC_CANARY_OUT")); out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, rawArchive, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, forbidden := range []string{
		accountID, "diagnostic@example.test", "upstream-sensitive-id", "secret-diagnostic-token",
		"raw-event-internal-id", "raw-account-internal-id", "event-leak@example.net",
		"abcdefghijklmnopqrstuvwxyz123456", "/srv/private/secrets/token.txt", "customer-internal-note",
	} {
		if bytes.Contains(rawArchive, []byte(forbidden)) {
			t.Fatalf("artifact contained forbidden value %q", forbidden)
		}
	}

	second := create()
	secondArchive, err := zip.OpenReader(second.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var secondAlias string
	for _, entry := range secondArchive.File {
		if entry.Name != "accounts_snapshot.csv" {
			continue
		}
		stream, _ := entry.Open()
		rows, _ := csv.NewReader(stream).ReadAll()
		stream.Close()
		if len(rows) > 1 {
			secondAlias = rows[1][0]
		}
	}
	secondArchive.Close()
	if secondAlias != firstAlias {
		t.Fatalf("aliases changed across bundles: %q != %q", firstAlias, secondAlias)
	}

	rangeRequest, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/diagnostics/jobs/"+first.ID+"/download", nil)
	rangeRequest.Header.Set("Range", "bytes=0-9")
	rangeResponse, err := http.DefaultClient.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResponse.Body)
	rangeResponse.Body.Close()
	if rangeResponse.StatusCode != http.StatusPartialContent || len(rangeBody) != 10 ||
		rangeResponse.Header.Get("ETag") == "" || rangeResponse.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range status=%d len=%d headers=%v", rangeResponse.StatusCode, len(rangeBody), rangeResponse.Header)
	}
}

func TestDiagnosticEventSafeEnumValueKeepsStructuredCleanupCodes(t *testing.T) {
	if !diagnosticEventSafeEnumValue("error_code",
		"context_cleanup_failed,goal_cleanup_failed,goal_budget_measure_failed,mapping_cleanup_failed,route_binding_cleanup_failed") {
		t.Fatal("stable comma-separated cleanup codes were aliased out of the diagnostic package")
	}
	for _, value := range []string{
		"context_cleanup_failed,/srv/private/key",
		"context_cleanup_failed,bearer token",
		strings.Repeat("cleanup_failed,", 17),
	} {
		if diagnosticEventSafeEnumValue("error_code", value) {
			t.Fatalf("unsafe enum list accepted: %q", value)
		}
	}
}

func TestDiagnosticDLPAllowsPublicSupportRequestID(t *testing.T) {
	for _, value := range []string{
		"REQ-89C6735FD8ABC561",
		"request_id=REQ-89C6735FD8ABC561",
		"REQ-ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	} {
		if diagnosticContainsUnsafeRequestID(value) {
			t.Fatalf("public or stable request ID was rejected: %q", value)
		}
	}
	if !diagnosticContainsUnsafeRequestID("request_id=req_011CdWLhB6LpPonmxYYxwQdD") {
		t.Fatal("upstream request ID bypassed DLP validation")
	}
	if diagnosticDLPMatch("request_id=REQ-89C6735FD8ABC561") {
		t.Fatal("public support request ID triggered another DLP rule")
	}
}

func TestLegacyDiagnosticExportReturnsAsyncLocationWithoutPrematureDownload(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	request, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/export/logs", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content-type=%q, want application/json", contentType)
	}
	location := response.Header.Get("Location")
	if !strings.HasPrefix(location, "/admin/diagnostics/jobs/diagjob_") {
		t.Fatalf("location=%q", location)
	}
	var envelope struct {
		Job      storage.DiagnosticJob `json:"job"`
		Location string                `json:"location"`
		Download string                `json:"download"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Job.Status != storage.DiagnosticJobQueued || envelope.Location != location {
		t.Fatalf("envelope=%+v", envelope)
	}
	if envelope.Download != "" || envelope.Job.DownloadURL != "" {
		t.Fatalf("queued job exposed a premature download link: %+v", envelope)
	}
}

func TestDiagnosticRescueExportDoesNotRequireBackgroundWorker(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if err := h.store.InsertAuditLog(context.Background(), storage.AuditLogRow{
		Action: "codex_context_migrated", State: "recovered",
		Reason: "stateless_durable_replay", Detail: "goal_checkpoint_new_root",
	}); err != nil {
		t.Fatal(err)
	}

	// Do not start startDiagnosticJobLoop: rescue mode exists specifically for a
	// stopped, crashed, or stranded optional renderer.
	request, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/export/logs?mode=rescue", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(response.Header.Get("Content-Type"), "application/zip") ||
		response.Header.Get("X-Codex-Diagnostic-Mode") != "rescue" {
		t.Fatalf("rescue status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}
	files := readZipFiles(t, raw)
	for _, name := range []string{"manifest.json", "diagnostic_summary.json", "goal_continuity.csv", "audit_log.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("rescue bundle missing %s: %v", name, zipFileNames(files))
		}
	}
	if !strings.Contains(files["audit_log.csv"], "stateless_durable_replay") {
		t.Fatalf("rescue bundle lost context recovery evidence: %s", files["audit_log.csv"])
	}
}

func TestDiagnosticRescueFallsBackToValidatedMemoryArchive(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if err := h.store.InsertAuditLog(context.Background(), storage.AuditLogRow{
		Action: "diagnostic_export_repro", State: "failed",
		Reason: "database_not_writable", Detail: "context_cleanup_failed,goal_cleanup_failed",
	}); err != nil {
		t.Fatal(err)
	}
	// Make the normal rescue renderer fail before it writes response headers. The
	// emergency archive must not need this spool path.
	blockedSpool := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedSpool, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.app.cfg.BodySpoolDir = blockedSpool

	request, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/export/logs?mode=rescue", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("X-Codex-Diagnostic-Mode") != "rescue" ||
		response.Header.Get("X-Codex-Diagnostic-Degraded") != "true" ||
		!strings.Contains(response.Header.Get("Content-Type"), "application/zip") {
		t.Fatalf("emergency response status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}
	if err := validateEmergencyDiagnosticArchive(raw); err != nil {
		t.Fatalf("memory archive validation failed: %v", err)
	}
	files := readZipFiles(t, raw)
	for _, name := range []string{"manifest.json", "diagnostic_summary.json", "runtime_storage.json", "goal_continuity.csv", "audit_log.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("memory archive missing %s: %v", name, zipFileNames(files))
		}
	}
	if !strings.Contains(files["audit_log.csv"], "diagnostic_export_repro") ||
		!strings.Contains(files["audit_log.csv"], "database_not_writable") {
		t.Fatalf("bounded audit evidence missing: %s", files["audit_log.csv"])
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["mode"] != "emergency_memory" || manifest["degraded"] != true {
		t.Fatalf("emergency manifest = %#v", manifest)
	}
	var runtimeStorage map[string]interface{}
	if err := json.Unmarshal([]byte(files["runtime_storage.json"]), &runtimeStorage); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtimeStorage["supervisor"]; !ok {
		t.Fatalf("emergency runtime omitted supervisor state: %#v", runtimeStorage)
	}
}

func TestEmergencyDiagnosticArchiveSurvivesClosedDatabase(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	// Stop request/async writers before closing the database, then exercise the
	// database-independent route directly. Cleanup is intentionally idempotent.
	h.closeRuntime()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	started, err := h.app.streamEmergencyDiagnosticsExport(recorder, errors.New("database is locked"))
	if err != nil || !started || recorder.Code != http.StatusOK {
		t.Fatalf("closed-database emergency started=%v status=%d err=%v body=%s", started, recorder.Code, err, recorder.Body.String())
	}
	if recorder.Header().Get("X-Codex-Diagnostic-Full-Error") != "database_locked" {
		t.Fatalf("closed-database failure code headers=%v", recorder.Header())
	}
	raw := recorder.Body.Bytes()
	if err := validateEmergencyDiagnosticArchive(raw); err != nil {
		t.Fatalf("closed-database archive failed validation: %v", err)
	}
	files := readZipFiles(t, raw)
	var manifest struct {
		Mode      string   `json:"mode"`
		Degraded  bool     `json:"degraded"`
		DataGaps  []string `json:"data_gaps"`
		ErrorCode string   `json:"full_export_error_code"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Mode != "emergency_memory" || !manifest.Degraded || manifest.ErrorCode != "database_locked" ||
		!containsDiagnosticGap(manifest.DataGaps, "audit_log") || !containsDiagnosticGap(manifest.DataGaps, "goal_continuity") {
		t.Fatalf("closed-database manifest = %#v", manifest)
	}
}

func TestEmergencyDiagnosticRouteStillRequiresAdminAuthentication(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	h.app.cfg.AdminToken = "diagnostic-admin-secret"
	response, err := http.Get(h.pool.URL + "/admin/export/logs?mode=emergency")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusUnauthorized || strings.Contains(response.Header.Get("Content-Type"), "application/zip") {
		t.Fatalf("unauthenticated emergency status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}
}

func containsDiagnosticGap(gaps []string, want string) bool {
	for _, gap := range gaps {
		if gap == want {
			return true
		}
	}
	return false
}

func TestDiagnosticJobCreateWakesActiveWorker(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/admin/diagnostics/jobs", strings.NewReader(`{}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("create status=%d body=%s", response.StatusCode, raw)
	}
	select {
	case <-h.app.diagnosticJobWake:
	case <-time.After(time.Second):
		t.Fatal("diagnostic job creation did not wake the worker")
	}
}

func TestDiagnosticJobAPIDisablesCaching(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	for _, fixture := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/admin/diagnostics/jobs"},
		{method: http.MethodPost, path: "/admin/diagnostics/jobs", body: `{}`},
		{method: http.MethodGet, path: "/admin/diagnostics/jobs/diagjob_missing"},
		{method: http.MethodGet, path: "/admin/diagnostics/jobs/diagjob_missing/download"},
	} {
		request, err := http.NewRequest(fixture.method, h.pool.URL+fixture.path, strings.NewReader(fixture.body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if got := response.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s %s Cache-Control=%q, want no-store", fixture.method, fixture.path, got)
		}
	}
}

func TestDiagnosticSnapshotGuardCancelsOnReservePressureAndStopsIdempotently(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	const unavailableReserve = int64(1 << 62)
	guardCtx, stop := h.app.startDiagnosticSnapshotGuardWithInterval(
		context.Background(), t.TempDir(), unavailableReserve, unavailableReserve, time.Millisecond,
	)
	select {
	case <-guardCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("diagnostic snapshot guard did not cancel under reserve pressure")
	}
	if !errors.Is(context.Cause(guardCtx), errDiagnosticCapacity) {
		t.Fatalf("guard cause=%v", context.Cause(guardCtx))
	}
	if err := stop(); !errors.Is(err, errDiagnosticCapacity) {
		t.Fatalf("first stop error=%v", err)
	}
	if err := stop(); !errors.Is(err, errDiagnosticCapacity) {
		t.Fatalf("idempotent stop error=%v", err)
	}
}

func TestDiagnosticMaintenanceRetriesBusyWALThenTruncatesAfterReaderCloses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("legacy snapshot family detection is Linux-specific")
	}
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	snapshot, err := h.store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 192; index++ {
		if err = h.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "diagnostic_wal_finalize",
			State:  "complete",
			Detail: strings.Repeat("w", 16<<10),
		}); err != nil {
			_ = snapshot.Close()
			t.Fatal(err)
		}
	}
	walPath := h.store.DiagnosticWALPath()
	before, err := os.Stat(walPath)
	if err != nil || before.Size() == 0 {
		_ = snapshot.Close()
		t.Fatalf("WAL was not created: info=%v err=%v", before, err)
	}
	legacyPath := filepath.Join(filepath.Dir(h.store.Path()), ".diagnostic-snapshot-wal-retry.sqlite3")
	if err = os.WriteFile(legacyPath, []byte("legacy"), 0o600); err != nil {
		_ = snapshot.Close()
		t.Fatal(err)
	}

	h.app.cleanupLegacyDiagnosticSnapshots()
	if !h.app.diagnostics.walFinalizePending.Load() {
		_ = snapshot.Close()
		t.Fatal("busy checkpoint did not remain pending for the next maintenance pass")
	}
	if _, err = os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		_ = snapshot.Close()
		t.Fatalf("legacy trigger was not reclaimed: %v", err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	h.app.cleanupLegacyDiagnosticSnapshots()
	if h.app.diagnostics.walFinalizePending.Load() {
		t.Fatal("WAL finalization remained pending after the reader closed")
	}
	if info, statErr := os.Stat(walPath); statErr == nil && info.Size() != 0 {
		t.Fatalf("WAL size after retry=%d, want 0", info.Size())
	}
	var rows int
	if err = h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action='diagnostic_wal_finalize'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 192 {
		t.Fatalf("durable audit rows=%d, want 192", rows)
	}
}

func TestDiagnosticJobWorkerRestartsAfterActiveRoleTransition(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })

	firstActive, cancelFirstActive := context.WithCancel(context.Background())
	h.app.startDiagnosticJobLoop(firstActive)
	cancelFirstActive()
	// Let the first lease context begin cancellation before simulating promotion
	// of the same process back to active.
	time.Sleep(25 * time.Millisecond)

	job, err := h.store.CreateDiagnosticJob(context.Background(), "diagjob_role_transition", diagnosticJobQueueCapacity)
	if err != nil {
		t.Fatal(err)
	}
	secondActive, cancelSecondActive := context.WithCancel(context.Background())
	defer cancelSecondActive()
	h.app.startDiagnosticJobLoop(secondActive)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err = h.store.GetDiagnosticJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch job.Status {
		case storage.DiagnosticJobReady:
			if job.ArtifactPath == "" {
				t.Fatal("ready diagnostic job has no artifact")
			}
			return
		case storage.DiagnosticJobFailed, storage.DiagnosticJobCancelled:
			t.Fatalf("diagnostic job failed after role transition: %+v", job)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("diagnostic job remained %q after active role restarted", job.Status)
}

func TestDiagnosticArtifactGCUsesTrashAndRefusesSymlinks(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	dir, err := h.app.diagnosticArtifactDirectory()
	if err != nil {
		t.Fatal(err)
	}
	mountID, err := diagnosticMountID(dir)
	if err != nil {
		t.Fatal(err)
	}
	createEligible := func(id, path string) storage.StorageResource {
		t.Helper()
		resource, createErr := h.store.CreateStorageResource(ctx, storage.StorageResource{
			ID:             id,
			ResourceType:   storage.StorageResourceTypeDiagnosticArtifact,
			Path:           path,
			OwnerID:        "job-" + id,
			LeaseExpiresAt: storage.Now(),
			FencingToken:   1,
			MountID:        mountID,
			RetentionClass: storage.StorageRetentionDiagnosticArtifact,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if activateErr := h.store.ActivateStorageResource(ctx, resource); activateErr != nil {
			t.Fatal(activateErr)
		}
		if eligibleErr := h.store.MarkStorageResourceEligible(ctx, resource); eligibleErr != nil {
			t.Fatal(eligibleErr)
		}
		return resource
	}

	regularPath := filepath.Join(dir, "gc-regular.zip")
	if err := os.WriteFile(regularPath, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	regular := createEligible("regular-resource", regularPath)
	h.app.cleanupEligibleDiagnosticArtifacts(ctx)
	if _, err := os.Lstat(regularPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("regular artifact still exists: %v", err)
	}
	persisted, err := h.store.GetStorageResource(ctx, regular.ID)
	if err != nil || persisted.State != storage.StorageResourceDeleted {
		t.Fatalf("regular resource=%+v err=%v", persisted, err)
	}
	var severity string
	var diagnosticGap int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT severity,diagnostic_gap FROM diagnostic_events WHERE event_type='storage_gc' ORDER BY created_at DESC LIMIT 1`).Scan(&severity, &diagnosticGap); err != nil {
		t.Fatal(err)
	}
	if severity != "info" || diagnosticGap != 0 {
		t.Fatalf("normal diagnostic GC severity/gap=%q/%d, want info/0", severity, diagnosticGap)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "gc-symlink.zip")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}
	symlink := createEligible("symlink-resource", symlinkPath)
	h.app.cleanupEligibleDiagnosticArtifacts(ctx)
	if _, err := os.Lstat(symlinkPath); err != nil {
		t.Fatalf("GC removed symlink instead of refusing it: %v", err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "must remain" {
		t.Fatalf("GC changed symlink target: raw=%q err=%v", raw, err)
	}
	persisted, err = h.store.GetStorageResource(ctx, symlink.ID)
	if err != nil || persisted.State != storage.StorageResourceTrash {
		t.Fatalf("symlink resource=%+v err=%v", persisted, err)
	}
}
