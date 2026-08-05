package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
)

func awaitLegacyDiagnosticExport(t *testing.T, h *testHarness) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.app.startDiagnosticJobLoop(ctx)

	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/logs", "")
	if code != http.StatusAccepted {
		t.Fatalf("legacy diagnostics enqueue = %d: %s", code, raw)
	}
	var envelope struct {
		Job storage.DiagnosticJob `json:"job"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Job.ID == "" {
		t.Fatalf("decode diagnostics job: %v (%s)", err, raw)
	}
	// Large capped-table fixtures are intentionally exercised under -race on the
	// shared CI profile. Instrumented SQLite/ZIP/DLP work can exceed 15 seconds
	// there even though the normal build completes in a few seconds.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, err := h.store.GetDiagnosticJob(context.Background(), envelope.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch job.Status {
		case storage.DiagnosticJobReady:
			artifact, err := os.ReadFile(job.ArtifactPath)
			if err != nil {
				t.Fatal(err)
			}
			return artifact
		case storage.DiagnosticJobFailed, storage.DiagnosticJobCancelled:
			t.Fatalf("diagnostics job failed: %+v", job)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("diagnostics job did not finish")
	return nil
}

func TestAdminDiagnosticsExportAnonymizesBusinessLogs(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "goal_storage_max_mb", "384"); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID:                "acc-real-1",
		Label:             "Alpha Sensitive",
		Email:             "sensitive@example.com",
		GroupName:         "cyber",
		Provider:          "codex",
		Status:            "active",
		UpstreamAccountID: "upstream-secret",
		ChatGPTUserID:     "chatgpt-secret",
		PlanType:          "plus",
	}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "access-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID:    account.ID,
		AccountLabel: account.Label,
		Action:       "permission_denied_no_quarantine",
		State:        "permission_denied",
		Reason:       "scope",
		Detail:       "account acc-real-1 sensitive@example.com Alpha Sensitive upstream-secret chatgpt-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertCFEvent(ctx, storage.CFEvent{
		AccountID: account.ID,
		EgressID:  storage.DefaultDirectEgressID,
		Status:    http.StatusForbidden,
		CFRay:     "ray-secret",
		Category:  "edge",
		Message:   "blocked acc-real-1 sensitive@example.com Alpha Sensitive upstream-secret chatgpt-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "route-secret", "api-key-hash", "user-1", "gpt-5.5", 10, 3, 13, 4, 4, 2,
		json.RawMessage(`{"account":"acc-real-1","email":"sensitive@example.com","label":"Alpha Sensitive","upstream":"upstream-secret","chatgpt":"chatgpt-secret"}`),
		storage.UsageDiagnostics{
			AffinitySource:             "cache_prefix_hash",
			PromptCacheKeyPresent:      true,
			PromptCacheKeySource:       "auto_stable_prefix",
			StablePrefixSource:         "anthropic_message_prefix",
			StablePrefixReason:         "ok",
			StablePrefixBytes:          4096,
			RetentionEffective:         "24h",
			RetentionSource:            "gateway_default",
			ClaudeCacheTTL:             "1h",
			CacheControlInjected:       true,
			CacheBreakpointCount:       2,
			CacheBreakpointsJSON:       `[{"section":"messages","message_index":2,"block_index":0,"type":"text","token_estimate":19,"hash":"abc123","ttl":"1h"}]`,
			UnwrittenTailTokens:        123,
			MaxPossibleCacheReadTokens: 456,
			CacheHitAfterPrewarm:       true,
			SingleflightWaitedRequests: 2,
			DiagnosticsMissReason:      "messages_changed",
			LatestUserCacheControl:     false,
			RouteEpoch:                 7,
		}); err != nil {
		t.Fatal(err)
	}
	holdID, err := h.store.CreateBillingHold(ctx, "route-secret", account.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SettleBillingHold(ctx, holdID, "failed_upstream"); err != nil {
		t.Fatal(err)
	}
	h.app.recordRouteAttempt("req-diagnostic", 2, "account_pool_group:cyber", "fair", "upstream_cyber_policy", "user_group:ug-secret-runtime", diagnosticRouteDetail{
		TerminalErrorClass:            "cyber_policy",
		EffectiveStatus:               http.StatusBadRequest,
		SuperInstructClientChoice:     "disabled",
		SuperInstructEffectiveModules: "none",
		UserGroupID:                   "ug-secret-runtime",
	})
	h.app.recordProviderAttempt("req-diagnostic", account.ID, "antigravity", "inference", http.StatusServiceUnavailable, "transient_resource_exhausted", "sha256:0123456789abcdef", "2")
	h.app.recordHTTPRequest("REQ-89C6735FD8ABC561", http.MethodGet, "admin.email-pool", http.StatusOK, 0, 84, 12*time.Millisecond)
	h.app.recordBodyStorageRejection(&bodysource.BodyStorageError{Class: bodysource.BodyStorageDiskReserve, Cause: bodysource.ErrDiskReserve})
	h.app.recordBodyStorageRejection(&bodysource.BodyStorageError{Class: bodysource.BodyStorageLocalCapacity, Cause: bodysource.ErrSpoolBudget})

	raw := awaitLegacyDiagnosticExport(t, h)
	files := readZipFiles(t, raw)
	required := []string{
		"manifest.json",
		"diagnostic_summary.json",
		"runtime_storage.json",
		"http_requests.csv",
		"route_attempts.csv",
		"provider_attempts.csv",
		"account_auth_metadata.csv",
		"account_model_capabilities.csv",
		"kiro_runtime_capabilities.csv",
		"account_rate_limits.csv",
		"affinity_bindings.csv",
		"codex_session_mappings.csv",
		"codex_instruction_snapshots.csv",
		"codex_upstream_attempts.csv",
		"codex_upstream_attempts_daily.csv",
		"codex_group_policy_revisions.csv",
		"sidecar_status.csv",
		"settings.csv",
		"custom_providers.csv",
		"upstream_error_rules.csv",
		"codex_reset_credit_consumptions.csv",
		"account_lifecycle_status.csv",
		"codex_reauth_config.csv",
		"codex_reauth_jobs.csv",
		"audit_log.csv",
		"cf_events.csv",
		"usage_records.csv",
		"billing_holds.csv",
		"accounts_snapshot.csv",
		"egress_snapshot.csv",
	}
	for _, name := range required {
		if _, ok := files[name]; !ok {
			t.Fatalf("diagnostics zip missing %s; has %v", name, zipFileNames(files))
		}
	}

	var manifest struct {
		Format             string                 `json:"format"`
		SnapshotID         string                 `json:"snapshot_id"`
		Files              []string               `json:"files"`
		Rows               map[string]int         `json:"row_counts"`
		CurrentAccounts    int                    `json:"current_account_count"`
		HistoricalAccounts int                    `json:"historical_reference_account_count"`
		Build              map[string]interface{} `json:"build"`
		TableTimeRanges    map[string]interface{} `json:"table_time_ranges"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatalf("manifest json: %v\n%s", err, files["manifest.json"])
	}
	if manifest.Format != "codex-pool-diagnostics-v3" || !strings.HasPrefix(manifest.SnapshotID, "diag_") {
		t.Fatalf("manifest format/snapshot = %q/%q", manifest.Format, manifest.SnapshotID)
	}
	if manifest.CurrentAccounts != 1 || manifest.HistoricalAccounts != 1 || manifest.Build == nil || manifest.TableTimeRanges["usage_records.csv"] == nil {
		t.Fatalf("manifest account/build/range metadata = %+v", manifest)
	}
	for _, name := range required {
		if _, ok := manifest.Rows[name]; !ok {
			t.Fatalf("manifest missing row count for %s: %+v", name, manifest.Rows)
		}
		if !strings.HasSuffix(name, ".csv") {
			continue
		}
		rows, csvErr := csv.NewReader(strings.NewReader(files[name])).ReadAll()
		if csvErr != nil {
			t.Fatalf("parse %s: %v", name, csvErr)
		}
		if actual := len(rows) - 1; actual != manifest.Rows[name] {
			t.Fatalf("manifest row count drift for %s: manifest=%d actual=%d", name, manifest.Rows[name], actual)
		}
	}

	accountRows, err := csv.NewReader(strings.NewReader(files["accounts_snapshot.csv"])).ReadAll()
	if err != nil {
		t.Fatalf("accounts_snapshot.csv: %v", err)
	}
	if len(accountRows) != 2 || len(accountRows[1]) == 0 || !strings.HasPrefix(accountRows[1][0], "ACC-") {
		t.Fatalf("accounts_snapshot.csv has no stable account alias: %v", accountRows)
	}
	accountAlias := accountRows[1][0]
	if _, ok := files["account_map.csv"]; ok {
		t.Fatal("diagnostics export contains a reversible account map")
	}

	for _, name := range required {
		text := files[name]
		for _, forbidden := range []string{account.Email, account.Label, account.UpstreamAccountID, account.ChatGPTUserID, "access-secret"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s leaked %q:\n%s", name, forbidden, text)
			}
		}
		if strings.Contains(text, account.ID) {
			t.Fatalf("%s leaked raw account id %q:\n%s", name, account.ID, text)
		}
	}
	for _, name := range []string{"audit_log.csv", "cf_events.csv", "usage_records.csv", "billing_holds.csv", "accounts_snapshot.csv", "egress_snapshot.csv", "account_auth_metadata.csv"} {
		if !strings.Contains(files[name], accountAlias) {
			t.Fatalf("%s should use stable account alias %s:\n%s", name, accountAlias, files[name])
		}
	}

	var runtimeStorage struct {
		Budget struct {
			MemoryLimit           int64 `json:"memory_limit"`
			RequestMemoryMinimum  int64 `json:"request_memory_minimum"`
			ResponseMemoryMinimum int64 `json:"response_memory_minimum"`
		} `json:"budget"`
		Filesystem struct {
			FilesystemTotalBytes     int64 `json:"filesystem_total_bytes"`
			FilesystemAvailableBytes int64 `json:"filesystem_available_bytes"`
			MinimumFreeBytes         int64 `json:"minimum_free_bytes"`
			GlobalLimitBytes         int64 `json:"global_limit_bytes"`
		} `json:"filesystem"`
		Rejections map[string]int64 `json:"rejection_counts"`
	}
	if err := json.Unmarshal([]byte(files["runtime_storage.json"]), &runtimeStorage); err != nil {
		t.Fatalf("runtime_storage.json: %v\n%s", err, files["runtime_storage.json"])
	}
	if runtimeStorage.Budget.MemoryLimit <= 0 || runtimeStorage.Budget.RequestMemoryMinimum <= 0 || runtimeStorage.Budget.ResponseMemoryMinimum <= 0 {
		t.Fatalf("runtime_storage split budget missing: %+v", runtimeStorage.Budget)
	}
	if runtimeStorage.Filesystem.FilesystemTotalBytes <= 0 || runtimeStorage.Filesystem.FilesystemAvailableBytes <= 0 || runtimeStorage.Filesystem.GlobalLimitBytes <= 0 {
		t.Fatalf("runtime_storage filesystem/reserver missing: %+v", runtimeStorage.Filesystem)
	}
	if runtimeStorage.Filesystem.MinimumFreeBytes < 128<<20 {
		t.Fatalf("runtime_storage does not preserve emergency filesystem headroom: %+v", runtimeStorage.Filesystem)
	}
	if runtimeStorage.Rejections["request_body_storage_exhausted"] != 1 || runtimeStorage.Rejections["local_spool_capacity"] != 1 {
		t.Fatalf("runtime_storage rejection counts: %+v", runtimeStorage.Rejections)
	}

	routeCSV := files["route_attempts.csv"]
	for _, want := range []string{"request_id,tier,target,selection_type,status_class,fallback_target,terminal_error_class,effective_status,super_instruct_client_choice,super_instruct_effective_modules,user_group_alias,created_at", "req-diagnostic", "account_pool_group:cyber", "upstream_cyber_policy", "user_group:UG-", "cyber_policy,400,disabled,none,UG-"} {
		if !strings.Contains(routeCSV, want) {
			t.Fatalf("route_attempts.csv missing %q:\n%s", want, routeCSV)
		}
	}
	if strings.Contains(routeCSV, "ug-secret-runtime") {
		t.Fatalf("route_attempts.csv leaked raw user-group id:\n%s", routeCSV)
	}
	providerCSV := files["provider_attempts.csv"]
	for _, want := range []string{"request_id,account_code,provider,phase,status,error_class,body_hash,retry_after,created_at", "req-diagnostic", accountAlias, "antigravity", "transient_resource_exhausted", "sha256:0123456789abcdef"} {
		if !strings.Contains(providerCSV, want) {
			t.Fatalf("provider_attempts.csv missing %q:\n%s", want, providerCSV)
		}
	}
	httpCSV := files["http_requests.csv"]
	for _, want := range []string{"request_id,method,route,status,request_bytes,response_bytes,duration_ms,created_at", "REQ-89C6735FD8ABC561", "admin.email-pool", ",200,0,84,12,"} {
		if !strings.Contains(httpCSV, want) {
			t.Fatalf("http_requests.csv missing %q:\n%s", want, httpCSV)
		}
	}
	usageCSV := files["usage_records.csv"]
	for _, want := range []string{"usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "affinity_source", "route_class", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_bytes", "retention_effective", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch"} {
		if !strings.Contains(usageCSV, want) {
			t.Fatalf("usage_records.csv missing diagnostic column %q:\n%s", want, usageCSV)
		}
	}
	for _, want := range []string{"cache_prefix_hash", "stable_prefix", "auto_stable_prefix", "anthropic_message_prefix", "4096", "24h", "1h", "messages_changed", "456", "123", "7"} {
		if !strings.Contains(usageCSV, want) {
			t.Fatalf("usage_records.csv missing diagnostic value %q:\n%s", want, usageCSV)
		}
	}

	authCSV := files["account_auth_metadata.csv"]
	for _, want := range []string{"auth_method", "billing_mode", "credential_present", "effective_provider", "codex"} {
		if !strings.Contains(authCSV, want) {
			t.Fatalf("account_auth_metadata.csv missing %q:\n%s", want, authCSV)
		}
	}
	for _, forbidden := range []string{"access_token_len", "refresh_token_len", "openai_api_key_len"} {
		if strings.Contains(authCSV, forbidden) {
			t.Fatalf("account_auth_metadata.csv leaked credential-shape metadata %q:\n%s", forbidden, authCSV)
		}
	}

	var summary map[string]interface{}
	if err := json.Unmarshal([]byte(files["diagnostic_summary.json"]), &summary); err != nil {
		t.Fatalf("diagnostic_summary.json: %v\n%s", err, files["diagnostic_summary.json"])
	}
	for _, key := range []string{"routing_409", "routing_audit", "health_test_models", "banned_accounts", "billing_holds", "groups", "codex_cpa", "goal_continuity", "goal_policy", "usage_journal"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("diagnostic_summary.json missing %q: %+v", key, summary)
		}
	}
	goalPolicy, ok := summary["goal_policy"].(map[string]interface{})
	if !ok || goalPolicy["storage_max_mb"] != float64(384) ||
		goalPolicy["storage_max_bytes"] != float64(384<<20) ||
		goalPolicy["storage_maintenance_target_bytes"] != float64(336<<20) ||
		goalPolicy["storage_maintenance_reserve_bytes"] != float64(48<<20) {
		t.Fatalf("diagnostic Goal policy does not expose effective storage limit: %+v", summary["goal_policy"])
	}
	sources, ok := goalPolicy["sources"].(map[string]interface{})
	if !ok || sources["goal_storage_max_mb"] != "runtime_setting" ||
		sources["goal_retention_days"] != "bootstrap_config" {
		t.Fatalf("diagnostic Goal policy sources are not actionable: %+v", goalPolicy["sources"])
	}
	journalJSON, err := json.Marshal(summary["usage_journal"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journalJSON, []byte("usage-journal")) || bytes.Contains(journalJSON, []byte(h.store.Path())) {
		t.Fatalf("usage journal diagnostics leaked a local path: %s", journalJSON)
	}
}

func TestDiagnosticsExportCapsAppendHeavyTablesToRecentRows(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	var beforeRows, beforeMaxID int64
	if err := h.store.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM audit_log`).Scan(&beforeRows, &beforeMaxID); err != nil {
		t.Fatal(err)
	}
	const extraRows int64 = 7
	insertRows := diagnosticExportRowLimit + extraRows
	if _, err := h.store.DB().ExecContext(ctx, `
WITH RECURSIVE rows(value) AS (
	VALUES(1) UNION ALL SELECT value+1 FROM rows WHERE value<?
)
INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at)
SELECT 'historical-account-'||(value%10),'','request','alive','ok','row='||value,?
FROM rows`, insertRows, storage.Now()); err != nil {
		t.Fatal(err)
	}

	raw := awaitLegacyDiagnosticExport(t, h)
	files := readZipFiles(t, raw)
	var manifest struct {
		Rows       map[string]int64 `json:"row_counts"`
		SourceRows map[string]int64 `json:"source_row_counts"`
		Truncated  map[string]struct {
			SourceRows   int64  `json:"source_rows"`
			ExportedRows int64  `json:"exported_rows"`
			OmittedRows  int64  `json:"omitted_rows"`
			Selection    string `json:"selection"`
		} `json:"truncated_tables"`
		RowLimit int64 `json:"large_table_row_limit"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	wantSource := beforeRows + insertRows
	if manifest.RowLimit != diagnosticExportRowLimit ||
		manifest.Rows["audit_log.csv"] != diagnosticExportRowLimit ||
		manifest.SourceRows["audit_log.csv"] != wantSource {
		t.Fatalf("manifest cap metadata=%+v", manifest)
	}
	truncated := manifest.Truncated["audit_log.csv"]
	if truncated.SourceRows != wantSource ||
		truncated.ExportedRows != diagnosticExportRowLimit ||
		truncated.OmittedRows != wantSource-diagnosticExportRowLimit ||
		truncated.Selection != "most_recent" {
		t.Fatalf("audit truncation metadata=%+v", truncated)
	}
	rows, err := csv.NewReader(strings.NewReader(files["audit_log.csv"])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(len(rows) - 1); got != diagnosticExportRowLimit {
		t.Fatalf("exported audit rows=%d want=%d", got, diagnosticExportRowLimit)
	}
	firstID, err := strconv.ParseInt(rows[1][0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	lastID, err := strconv.ParseInt(rows[len(rows)-1][0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != beforeMaxID+extraRows+1 || lastID != beforeMaxID+insertRows {
		t.Fatalf("export did not keep newest rows: first=%d last=%d before=%d inserted=%d", firstID, lastID, beforeMaxID, insertRows)
	}
}

func TestDiagnosticSanitizerPreservesSchemaAndEnumText(t *testing.T) {
	codebook := buildDiagnosticCodebookWithKey(
		[]byte("stable-diagnostic-alias-key"),
		[]storage.Account{{ID: "account-short", Label: "rt"}},
		nil, nil, nil, nil, nil,
	)
	accountAlias := codebook.code("account-short")
	input := "started_at,import,supported,unsupported,unreported,rt,account=rt"
	got := codebook.sanitize(input)
	want := "started_at,import,supported,unsupported,unreported," + accountAlias + ",account=" + accountAlias
	if got != want {
		t.Fatalf("boundary-aware identity sanitization = %q, want %q", got, want)
	}

	longEnum := "model_available_but_account_not_capable_after_catalog_refresh"
	if got := codebook.sanitize(longEnum); got != longEnum {
		t.Fatalf("long snake_case enum was corrupted: %q", got)
	}
	secret := strings.Repeat("0123456789abcdef", 4)
	if got := codebook.sanitize(secret); !strings.HasPrefix(got, "TOKEN-") || strings.Contains(got, secret) {
		t.Fatalf("high-entropy secret was not aliased: %q", got)
	}
	upstreamRequestID := "req_011CdWLhB6LpPonmxYYxwQdD"
	if got := codebook.sanitize(`request_id="` + upstreamRequestID + `"`); !strings.Contains(got, "REQ-") || strings.Contains(got, upstreamRequestID) {
		t.Fatalf("upstream request id was not aliased: %q", got)
	}
	publicRequestID := "REQ-89C6735FD8ABC561"
	if got := codebook.sanitize(`request_id="` + publicRequestID + `"`); !strings.Contains(got, publicRequestID) {
		t.Fatalf("public support request id was not retained: %q", got)
	}
}

func TestDiagnosticWholeFileSanitizationPreservesCSVHeader(t *testing.T) {
	codebook := buildDiagnosticCodebookWithKey(
		[]byte("stable-diagnostic-alias-key"),
		[]storage.Account{{ID: "account-short", Label: "rt"}},
		nil, nil, nil, nil, nil,
	)
	content := csvString(
		[]string{"id", "created_at", "updated_at", "started_at", "finished_at"},
		[][]string{{"1", "2", "3", "4", "5"}},
	)
	rows, err := csv.NewReader(strings.NewReader(codebook.sanitize(content))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "created_at", "updated_at", "started_at", "finished_at"}
	if len(rows) != 2 || !slices.Equal(rows[0], want) {
		t.Fatalf("sanitized CSV header = %v, want %v", rows, want)
	}
}

func TestDiagnosticSummaryUsesSchedulerExhaustionAndSeparatesWindows(t *testing.T) {
	now := storage.Now()
	accounts := []storage.Account{
		{ID: "allowed", GroupName: "g", Provider: "codex", Status: "active"},
		{ID: "exhausted", GroupName: "g", Provider: "codex", Status: "active"},
	}
	rateLimits := []storage.AccountRateLimit{
		{AccountID: "allowed", Provider: "codex", LimiterType: "5h_polled", RemainingTokens: -1, RemainingRequests: 0, ResetAt: now + 3600, Status: "allowed_warning"},
		{AccountID: "exhausted", Provider: "codex", LimiterType: "5h_polled", RemainingTokens: 0, RemainingRequests: -1, ResetAt: now + 3600, Status: "rejected"},
	}
	audit := []storage.AuditLogRow{
		{AccountID: "allowed", Action: "routing_unavailable", Detail: "status=409 cooldown", CreatedAt: now - 48*3600},
		{AccountID: "allowed", Action: "routing_unavailable", Detail: "status=409 pending health re-check", CreatedAt: now - 60},
		{AccountID: "allowed", State: "banned", CreatedAt: now - 30},
		{AccountID: "allowed", Action: "ban_delete_failed", CreatedAt: now - 20},
	}
	holds := []diagnosticBillingHold{
		{Status: "held", CreatedAt: now - 30},
		{Status: "held", CreatedAt: now - 2*3600},
		{Status: "expired_unsettled", CreatedAt: now - 3*3600},
	}
	summary := diagnosticSummary(accounts, map[string]storage.AccountToken{}, audit, holds, nil, rateLimits)
	raw, _ := json.Marshal(summary)
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	groups := decoded["groups"].(map[string]interface{})
	group := groups["g"].(map[string]interface{})
	if group["rate_limit_cooldown"] != float64(1) {
		t.Fatalf("allowed_warning was counted as cooldown: %s", raw)
	}
	lifetime := decoded["lifetime"].(map[string]interface{})
	last24h := decoded["last_24h"].(map[string]interface{})
	if lifetime["routing_409_events"].(map[string]interface{})["rate_limit_cooldown"] != float64(1) {
		t.Fatalf("lifetime 409 events missing: %s", raw)
	}
	if _, found := last24h["routing_409_events"].(map[string]interface{})["rate_limit_cooldown"]; found {
		t.Fatalf("historical 409 leaked into last-24h/current view: %s", raw)
	}
	banned := last24h["banned_accounts"].(map[string]interface{})
	if banned["events"].(map[string]interface{})["discovered"] != float64(1) || banned["unique_accounts"].(map[string]interface{})["discovered"] != float64(1) {
		t.Fatalf("ban event/unique counts = %s", raw)
	}
	holdSummary := decoded["billing_holds"].(map[string]interface{})
	if holdSummary["current_fresh_held"] != float64(1) || holdSummary["historical_stale_held"] != float64(1) {
		t.Fatalf("fresh/stale holds were conflated: %s", raw)
	}
}

func TestApplyDiagnosticAuditSummaryUsesSQLAggregates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	now := storage.Now()
	rows := []storage.AuditLogRow{
		{AccountID: "acc-a", Action: "routing_unavailable", Detail: "status=409 rate-limit cooldown"},
		{AccountID: "acc-a", Action: "health_test", Detail: "model=claude-opus-4.8 result=ok"},
		{AccountID: "acc-a", State: "banned"},
		{AccountID: "acc-a", State: "banned"},
	}
	for _, row := range rows {
		if err := h.store.InsertAuditLog(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE audit_log SET created_at=? WHERE action='routing_unavailable'`, now-48*3600); err != nil {
		t.Fatal(err)
	}
	summary := diagnosticSummary(nil, nil, nil, nil, nil, nil)
	if err := applyDiagnosticAuditSummary(ctx, h.store.DB(), summary); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(summary)
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	lifetime := decoded["lifetime"].(map[string]interface{})
	last24h := decoded["last_24h"].(map[string]interface{})
	if lifetime["routing_409_events"].(map[string]interface{})["rate_limit_cooldown"] != float64(1) {
		t.Fatalf("SQL routing aggregate missing: %s", raw)
	}
	if _, ok := last24h["routing_409_events"].(map[string]interface{})["rate_limit_cooldown"]; ok {
		t.Fatalf("old SQL routing event leaked into recent window: %s", raw)
	}
	if last24h["health_test_events_by_model"].(map[string]interface{})["claude-opus-4.8"] != float64(1) {
		t.Fatalf("SQL health aggregate missing: %s", raw)
	}
	banned := last24h["banned_accounts"].(map[string]interface{})
	if banned["events"].(map[string]interface{})["discovered"] != float64(2) || banned["unique_accounts"].(map[string]interface{})["discovered"] != float64(1) {
		t.Fatalf("SQL ban event/unique aggregate mismatch: %s", raw)
	}
}

type discardDiagnosticResponseWriter struct {
	header   http.Header
	writes   int
	bytes    int64
	maxWrite int
}

func (w *discardDiagnosticResponseWriter) Header() http.Header { return w.header }
func (w *discardDiagnosticResponseWriter) WriteHeader(int)     {}
func (w *discardDiagnosticResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	w.bytes += int64(len(p))
	if len(p) > w.maxWrite {
		w.maxWrite = len(p)
	}
	return len(p), nil
}

func TestDiagnosticsExportStreamsLargeUsageTableInBoundedWrites(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "stream-export", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 8*1024)
	for i := 0; i < 400; i++ {
		raw := json.RawMessage(`{"row":"` + strconv.Itoa(i) + `","payload":"` + large + `"}`)
		if err := h.store.InsertUsageRecord(ctx, account.ID, "route-"+strconv.Itoa(i), "hash", "user", "gpt-5.5", 10, 1, 11, 0, raw); err != nil {
			t.Fatal(err)
		}
	}
	w := &discardDiagnosticResponseWriter{header: http.Header{}}
	if err := h.app.streamDiagnosticsExport(ctx, w); err != nil {
		t.Fatal(err)
	}
	if w.header.Get("Content-Type") != "application/zip" || w.bytes == 0 || w.writes < 4 {
		t.Fatalf("stream output header=%v bytes=%d writes=%d", w.header, w.bytes, w.writes)
	}
	if w.maxWrite >= 1<<20 {
		t.Fatalf("export buffered an oversized archive chunk: max write=%d", w.maxWrite)
	}
}

func TestDiagnosticsExportDeduplicatesAffinityAliasesAndOmitsExpiredRows(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	for _, id := range []string{"affinity-export-a", "affinity-export-b"} {
		if err := h.store.UpsertAccount(ctx, storage.Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	now := storage.Now()
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO affinity_bindings(route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,0)`, "overlap-hash", "legacy", "previous_response_id", "affinity-export-a", "codex", "gpt-5", "direct", 1, now-10, now-10); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []storage.AffinityBinding{
		{RouteKeyHash: "overlap-hash", RouteKey: "current", Source: "previous_response_id", AccountID: "affinity-export-b", Provider: "codex", Model: "gpt-5", EgressID: "direct", Epoch: 2},
		{RouteKeyHash: "active-hash", RouteKey: "active", Source: "prompt_cache_key", AccountID: "affinity-export-a", Provider: "codex", Model: "gpt-5", EgressID: "direct"},
		{RouteKeyHash: "expired-hash", RouteKey: "expired", Source: "previous_response_id", AccountID: "affinity-export-a", Provider: "codex", Model: "gpt-5", EgressID: "direct"},
	} {
		if err := h.store.UpsertAffinityBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE affinity_aliases SET expires_at=? WHERE route_key_hash='expired-hash'`, now-1); err != nil {
		t.Fatal(err)
	}

	raw := awaitLegacyDiagnosticExport(t, h)
	files := readZipFiles(t, raw)
	rows, err := csv.NewReader(strings.NewReader(files["affinity_bindings.csv"])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("affinity CSV rows=%d, want header plus two bindings: %v", len(rows), rows)
	}
	seen := map[string]int{}
	for _, row := range rows[1:] {
		seen[row[0]]++
	}
	if seen["overlap-hash"] != 1 || seen["active-hash"] != 1 || seen["expired-hash"] != 0 {
		t.Fatalf("affinity hashes=%v", seen)
	}
	var manifest struct {
		Rows map[string]int `json:"row_counts"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Rows["affinity_bindings.csv"] != 2 {
		t.Fatalf("manifest affinity rows=%d", manifest.Rows["affinity_bindings.csv"])
	}
}

func TestDiagnosticRuntimeRecordersRemainBoundedAndOrdered(t *testing.T) {
	s := &Server{}
	for i := 0; i < diagnosticRuntimeRecordLimit+3; i++ {
		s.recordRouteAttempt("req-"+strconv.Itoa(i), i%3, "account_pool_group:pool", "fair", "success", "")
	}
	rows := s.diagnosticRouteAttempts()
	if len(rows) != diagnosticRuntimeRecordLimit {
		t.Fatalf("route attempt ring size=%d want=%d", len(rows), diagnosticRuntimeRecordLimit)
	}
	if rows[0].RequestID != "req-3" || rows[len(rows)-1].RequestID != "req-"+strconv.Itoa(diagnosticRuntimeRecordLimit+2) {
		t.Fatalf("route attempt ring order first=%q last=%q", rows[0].RequestID, rows[len(rows)-1].RequestID)
	}
}

func readZipFiles(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open diagnostics zip: %v\n%s", err, raw)
	}
	out := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, copyErr := buf.ReadFrom(rc)
		closeErr := rc.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		out[f.Name] = buf.String()
	}
	return out
}

func zipFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}
