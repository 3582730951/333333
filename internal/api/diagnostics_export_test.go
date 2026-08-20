package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
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
		"goal_continuity.csv",
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
		CacheCreationMap   string                 `json:"cache_creation_tokens_mapping"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatalf("manifest json: %v\n%s", err, files["manifest.json"])
	}
	if !strings.Contains(manifest.CacheCreationMap, "cache_write_tokens") {
		t.Fatalf("manifest cache creation mapping = %q", manifest.CacheCreationMap)
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
	for _, want := range []string{"usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "affinity_source", "route_class", "prompt_cache_key_source", "prompt_cache_key_hash", "prompt_cache_key_shard", "prompt_cache_key_minute_rpm", "prompt_cache_key_concurrency_peak", "stable_prefix_source", "stable_prefix_bytes", "retention_effective", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "coordination_prefix_source", "singleflight_wait_reason", "singleflight_release_reason", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch"} {
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
		Rows            map[string]int64 `json:"row_counts"`
		SourceRows      map[string]int64 `json:"source_row_counts"`
		SourceRowsExact map[string]bool  `json:"source_row_counts_exact"`
		Truncated       map[string]struct {
			SourceRows       int64  `json:"source_rows"`
			SourceRowsExact  bool   `json:"source_rows_exact"`
			ExportedRows     int64  `json:"exported_rows"`
			OmittedRows      int64  `json:"omitted_rows"`
			OmittedRowsExact bool   `json:"omitted_rows_exact"`
			Selection        string `json:"selection"`
		} `json:"truncated_tables"`
		RowLimit int64 `json:"large_table_row_limit"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RowLimit != diagnosticExportRowLimit ||
		manifest.Rows["audit_log.csv"] != diagnosticExportRowLimit ||
		manifest.SourceRows["audit_log.csv"] != diagnosticExportRowLimit+1 ||
		manifest.SourceRowsExact["audit_log.csv"] {
		t.Fatalf("manifest cap metadata=%+v", manifest)
	}
	truncated := manifest.Truncated["audit_log.csv"]
	if truncated.SourceRows != diagnosticExportRowLimit+1 ||
		truncated.SourceRowsExact ||
		truncated.ExportedRows != diagnosticExportRowLimit ||
		truncated.OmittedRows != 1 ||
		truncated.OmittedRowsExact ||
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

func TestDiagnosticGoalContinuityCountsLiveRunStates(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	goal, err := h.store.CommitGoalTurn(ctx, storage.GoalTurn{
		Protocol:          "codex",
		DownstreamKeyHash: "diagnostic-live-run-key",
		WorkspaceHash:     "diagnostic-live-run-workspace",
		InitialGoalHash:   "diagnostic-live-run-initial",
		ResponseID:        "diagnostic-live-run-response",
		Aliases: []storage.GoalAlias{{
			Type: "codex_root_thread", Value: "diagnostic-live-run-root",
		}},
		CheckpointPayload: `{"model":"gpt-test","input":[]}`,
		SegmentPayload:    `{"input":"work","output":"pending"}`,
		ExpiresAt:         storage.Now() + 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.AcquireGoalRun(ctx, goal.ID, "diagnostic-live-run-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	rows, err := diagnosticGoalContinuityRows(ctx, h.store.ReadDB(), []byte("diagnostic-alias-key"), storage.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0]) <= 15 || rows[0][15] != "1" {
		t.Fatalf("goal continuity active run count = %#v", rows)
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

// Versioned identifiers are the ones an operator most needs to read, and they were
// the ones being destroyed: the identifier exemption rejected any value containing a
// digit, so every long key carrying a `v3`/`v4`/`1gib` version fell through to the
// entropy test and was replaced by a `TOKEN-…` alias. Both keys below are real,
// taken from an exported production package where they appeared only as aliases.
func TestDiagnosticSanitizerPreservesVersionedIdentifiers(t *testing.T) {
	codebook := buildDiagnosticCodebookWithKey(
		[]byte("stable-diagnostic-alias-key"),
		nil, nil, nil, nil, nil, nil,
	)
	// Every snake_case literal of 40+ characters in internal/ — the exact population the
	// high-entropy catch-all can reach. Harvested rather than hand-picked so that a future
	// tightening of the exemption fails here instead of silently aliasing a setting key,
	// reason code, or window counter in a production export.
	for _, identifier := range []string{
		"goal_policy_defaults_v3_1gib_no_legacy_dual_write",
		"usage_cache_diagnostics_v4_openai_nested_cache_backfilled",
		"codex_prompt_cache_key_affinity_v2_rollout_percent",
		"account_failure_streak_cooldown_seconds_v1_default",
		"bounded_reclaim_exact_current_consolidation",
		"codex_reset_credits_unknown_consume_enabled",
		"codex_stateless_continuation_unavailable",
		"http_insufficient_quota_is_not_fixed_type",
		"idx_codex_upstream_attempt_recent_egress",
		"internal_chat_message_metadata_passthrough",
		"kiro_model_absent_from_static_capabilities",
		"known_expired_token_uses_account_failover",
		"known_missing_scope_uses_account_failover",
		"legacy_lifecycle_account_requires_review",
		"model_available_but_account_not_capable_after_catalog_refresh",
		"non_codex_provider_does_not_receive_grace",
		"official_codex_or_custom_native_responses",
		"provider_api_key_inference_probe_pending",
		"same_account_same_egress_https_sse_bridge",
		"sse_usage_limit_reached_is_not_fixed_code",
		"stateful_cf_can_rotate_egress_without_alternate",
		"stateful_expired_token_rebuilds_for_alternate",
		"stateful_expired_token_without_alternate_stays_native",
		"structured_error_overrides_retryable_status",
		"team_personal_access_token_persist_failed",
		"window_account_model_cache_point_accepted_requests",
		"window_account_model_cache_point_injected_requests",
		"window_account_model_cache_point_unsupported_requests",
		"window_account_model_credits_reported_requests",
	} {
		if got := codebook.sanitize(identifier); got != identifier {
			t.Fatalf("versioned identifier was aliased: %q -> %q", identifier, got)
		}
	}
	// The exemption must stay narrow: real secrets of the same length are still aliased.
	for _, secret := range []string{
		strings.Repeat("0123456789abcdef", 4),
		"AKIAIOSFODNN7EXAMPLE_wJalrXUtnFEMI_K7MDENGbPxRfiCYEXAMPLEKEY",
		"9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928",
		// Lowercase base64url-ish blob with underscores, but digit-leading segments.
		"a1b2c3d4e5f6a7b8_9c0d1e2f3a4b5c6d_7e8f9a0b1c2d3e4f_5a6b7c8d9e0f",
		// Shaped to imitate an identifier: lowercase alnum words, underscore-joined, and
		// a 24% digit share that clears the digit-rate ceiling. It is caught because
		// every word carries a digit, which no real key does. Taken verbatim from a run
		// where a digit-share bound alone exempted 14% of such blobs.
		"5v04qsbs1qrg_4mdd2dup4ji_ew45rritydl_oc4vffo6lis",
	} {
		if got := codebook.sanitize(secret); !strings.HasPrefix(got, "TOKEN-") || strings.Contains(got, secret) {
			t.Fatalf("secret was not aliased: %q -> %q", secret, got)
		}
	}
}

// A relay's transport profile is the field that decides how we frame every request to
// it, and per-route overrides mean the provider-level protocol is not authoritative for
// a given downstream path. The export carried neither, so a package from a relay that
// failed under `claude_code` framing looked identical to one that never used it.
func TestDiagnosticsExportRecordsCustomProviderTransport(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertCustomProvider(ctx, storage.CustomProvider{
		ID:               "relay-claude-code",
		Name:             "Relay",
		BaseURL:          "https://user:pass@relay.example.com/v1",
		UpstreamProtocol: "anthropic_messages",
		TransportProfile: "claude_code",
		EgressIDs:        []string{"egress-jp-1", "egress-jp-2"},
		Routes: []storage.CustomProviderRoute{{
			ID:               "route-messages",
			DownstreamPath:   storage.CustomProviderDownstreamMessages,
			BaseURL:          "https://route-user:route-pass@messages.example.com/v1",
			UpstreamProtocol: "anthropic_messages",
			TransportProfile: "claude_code",
		}, {
			ID:               "route-chat",
			DownstreamPath:   storage.CustomProviderDownstreamChat,
			UpstreamProtocol: "chat_completions",
			TransportProfile: "generic",
		}},
		Enabled: true,
		Models:  []string{"claude-sonnet-4-5"},
	}); err != nil {
		t.Fatal(err)
	}

	files := readZipFiles(t, awaitLegacyDiagnosticExport(t, h))
	rows, err := csv.NewReader(strings.NewReader(files["custom_providers.csv"])).ReadAll()
	if err != nil {
		t.Fatalf("custom_providers.csv: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("custom_providers.csv has no data rows: %v", rows)
	}
	column := map[string]int{}
	for i, name := range rows[0] {
		column[name] = i
	}
	for _, name := range []string{"transport_profile", "routes", "egress_ids"} {
		if _, ok := column[name]; !ok {
			t.Fatalf("custom_providers.csv is missing the %q column: %v", name, rows[0])
		}
	}
	var row []string
	for _, candidate := range rows[1:] {
		if candidate[column["id"]] == "relay-claude-code" {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatalf("custom_providers.csv has no row for the relay: %v", rows)
	}
	if got := row[column["transport_profile"]]; got != "claude_code" {
		t.Fatalf("transport_profile = %q, want claude_code", got)
	}
	if got := row[column["egress_ids"]]; got != "egress-jp-1 egress-jp-2" {
		t.Fatalf("egress_ids = %q", got)
	}
	var routes []storage.CustomProviderRoute
	if err := json.Unmarshal([]byte(row[column["routes"]]), &routes); err != nil {
		t.Fatalf("routes column is not JSON: %v (%q)", err, row[column["routes"]])
	}
	byPath := map[string]storage.CustomProviderRoute{}
	for _, route := range routes {
		byPath[route.DownstreamPath] = route
	}
	if got := byPath[storage.CustomProviderDownstreamChat]; got.TransportProfile != "generic" ||
		got.UpstreamProtocol != "chat_completions" {
		t.Fatalf("per-route override lost: %+v", got)
	}
	if got := byPath[storage.CustomProviderDownstreamMessages]; got.TransportProfile != "claude_code" {
		t.Fatalf("messages route profile = %q, want claude_code", got.TransportProfile)
	}
	// Route base URLs go through the same userinfo redaction as the provider's own.
	if strings.Contains(files["custom_providers.csv"], "route-pass") ||
		strings.Contains(files["custom_providers.csv"], "pass@") {
		t.Fatalf("custom_providers.csv leaked URL credentials: %s", files["custom_providers.csv"])
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

// provider_attempts is the table that explains provider wire failures, but only the
// antigravity path ever wrote to it, so the column could hold exactly one value. A
// production export whose largest failure cluster was 1512 relay 5xx had zero rows for
// the relay that produced them.
func TestCustomProviderWireAttemptsAreRecorded(t *testing.T) {
	var calls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat"}]}`))
			return
		}
		calls++
		if calls == 1 {
			// A terminal 400, not a 429: with one account in the pool a rate limit puts it
			// into cooldown and the next request blocks in the scheduler wait, which would
			// make this test about failover timing rather than about recording.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown model"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dsChatResp))
	})
	acc := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	for i := 0; i < 2; i++ {
		resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	rows := h.app.diagnosticProviderAttempts()
	byClass := map[string]diagnosticProviderAttempt{}
	for _, row := range rows {
		if row.Provider != "deepseek" {
			t.Fatalf("provider = %q, want the custom provider id: %+v", row.Provider, row)
		}
		if row.AccountID != acc {
			t.Fatalf("attempt not attributed to the leased account: %+v", row)
		}
		byClass[row.ErrorClass] = row
	}
	rejected, ok := byClass["client_error"]
	if !ok {
		t.Fatalf("400 attempt was not recorded as client_error: %+v", rows)
	}
	if rejected.Status != http.StatusBadRequest {
		t.Fatalf("rejected row status = %d: %+v", rejected.Status, rejected)
	}
	success, ok := byClass["none"]
	if !ok {
		t.Fatalf("successful attempt was not recorded: %+v", rows)
	}
	if success.Status != http.StatusOK {
		t.Fatalf("success row status = %d: %+v", success.Status, success)
	}
	// The digest must be present and must not be the body itself.
	if len(success.BodyHash) != 64 || strings.Contains(success.BodyHash, "hi") {
		t.Fatalf("body hash is not a bare sha256 digest: %q", success.BodyHash)
	}
}

func TestDiagnosticWireErrorClassSeparatesPermanentFromTransient(t *testing.T) {
	for _, tc := range []struct {
		status int
		err    error
		want   string
	}{
		{status: 200, want: "none"},
		{status: 401, want: "auth"},
		{status: 403, want: "auth"},
		{status: 429, want: "rate_limit"},
		{status: 503, want: "capacity"},
		{status: 529, want: "capacity"},
		{status: 500, want: "transient"},
		{status: 408, want: "transient"},
		// A relay answering 400 for an unmapped model is misconfigured, not busy;
		// classifying it transient would advertise a permanent failure as retryable.
		{status: 400, want: "client_error"},
		{status: 404, want: "client_error"},
		{status: 0, err: errors.New("dial tcp: connection refused"), want: "transient"},
	} {
		if got := diagnosticWireErrorClass(tc.status, tc.err); got != tc.want {
			t.Fatalf("class(%d, %v) = %q, want %q", tc.status, tc.err, got, tc.want)
		}
	}
}

// A sidecar with runtime state used to be reported twice: once from its profile with
// every runtime column blank, once from its adaptive status. Both rows carried the same
// sidecar id and health, so an export of one sidecar read as two, and the blank row
// looked like a second instance stuck outside the circuit breaker.
func TestSidecarStatusRowsDoNotDuplicateSidecarsWithRuntimeState(t *testing.T) {
	sidecar := func(id string) storage.EgressProfile {
		return storage.EgressProfile{ID: id, Type: storage.CurlCFFISidecarEgressType, Health: "healthy", MaxConcurrency: 16}
	}
	profiles := []storage.EgressProfile{
		sidecar("warp-1-ja3"),
		sidecar("egress_sidecar"),
		{ID: "plain-http", Type: "http", Health: "healthy"},
	}
	adaptive := []upstream.SidecarAdaptiveStatus{
		{SidecarEgressID: "warp-1-ja3", RealEgressID: "warp-1-ja3", Limit: 1, CircuitState: "half_open", UpdatedAt: 1785994568},
	}

	rows := sidecarStatusRows(profiles, nil, adaptive)

	counts := map[string]int{}
	for _, row := range rows {
		counts[row[0]]++
	}
	if counts["warp-1-ja3"] != 1 {
		t.Fatalf("sidecar with runtime state emitted %d rows, want 1: %v", counts["warp-1-ja3"], rows)
	}
	// A sidecar with no runtime state is still reported, otherwise a configured but
	// never-exercised sidecar would vanish from the export entirely.
	if counts["egress_sidecar"] != 1 {
		t.Fatalf("idle sidecar emitted %d rows, want 1: %v", counts["egress_sidecar"], rows)
	}
	if counts["plain-http"] != 0 {
		t.Fatalf("non-sidecar egress leaked into sidecar_status: %v", rows)
	}
	for _, row := range rows {
		if row[0] == "warp-1-ja3" && row[8] != "half_open" {
			t.Fatalf("surviving row lost its runtime circuit state: %v", row)
		}
	}
}

// Adaptive state is keyed by (sidecar, real egress), so one sidecar fronting several
// exits legitimately holds several statuses. Collapsing on sidecar id alone would hide
// all but one exit's circuit state.
func TestSidecarStatusRowsKeepEveryRealEgressAndOrphanedStatus(t *testing.T) {
	profiles := []storage.EgressProfile{
		{ID: "shared-ja3", Type: storage.CurlCFFISidecarEgressType, Health: "healthy", MaxConcurrency: 8},
	}
	adaptive := []upstream.SidecarAdaptiveStatus{
		{SidecarEgressID: "shared-ja3", RealEgressID: "exit-jp", CircuitState: "closed"},
		{SidecarEgressID: "shared-ja3", RealEgressID: "exit-us", CircuitState: "open"},
		// Runtime state whose profile has since been deleted must not be dropped: it is
		// the only record that the sidecar was tripped before it disappeared.
		{SidecarEgressID: "removed-ja3", RealEgressID: "exit-de", CircuitState: "bypass"},
	}

	rows := sidecarStatusRows(profiles, nil, adaptive)

	seen := map[string]string{}
	for _, row := range rows {
		seen[row[0]+"|"+row[1]] = row[8]
	}
	for key, want := range map[string]string{
		"shared-ja3|exit-jp":  "closed",
		"shared-ja3|exit-us":  "open",
		"removed-ja3|exit-de": "bypass",
	} {
		if seen[key] != want {
			t.Fatalf("row %q circuit_state = %q, want %q: %v", key, seen[key], want, rows)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want exactly the 3 runtime rows: %v", len(rows), rows)
	}
}
