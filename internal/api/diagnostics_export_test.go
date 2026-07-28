package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminDiagnosticsExportAnonymizesBusinessLogs(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
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

	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/logs", "")
	if code != http.StatusOK {
		t.Fatalf("diagnostics export = %d: %s", code, raw)
	}
	files := readZipFiles(t, raw)
	required := []string{
		"manifest.json",
		"diagnostic_summary.json",
		"account_map.csv",
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
	if manifest.Format != "codex-pool-diagnostics-v2" || !strings.HasPrefix(manifest.SnapshotID, "diag_") {
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

	accountMap, err := csv.NewReader(strings.NewReader(files["account_map.csv"])).ReadAll()
	if err != nil {
		t.Fatalf("account_map.csv: %v", err)
	}
	if len(accountMap) != 2 || len(accountMap[0]) != 2 || accountMap[0][0] != "account_code" || accountMap[0][1] != "account_id" || accountMap[1][0] != "ACC-0001" || accountMap[1][1] != account.ID {
		t.Fatalf("account_map.csv must contain only account_code and account_id: %v", accountMap)
	}

	for _, name := range required {
		text := files[name]
		for _, forbidden := range []string{account.Email, account.Label, account.UpstreamAccountID, account.ChatGPTUserID, "access-secret"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s leaked %q:\n%s", name, forbidden, text)
			}
		}
		if name != "account_map.csv" && strings.Contains(text, account.ID) {
			t.Fatalf("%s leaked raw account id %q:\n%s", name, account.ID, text)
		}
	}
	for _, name := range []string{"audit_log.csv", "cf_events.csv", "usage_records.csv", "billing_holds.csv", "accounts_snapshot.csv", "egress_snapshot.csv", "account_auth_metadata.csv"} {
		if !strings.Contains(files[name], "ACC-0001") {
			t.Fatalf("%s should use stable account code ACC-0001:\n%s", name, files[name])
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
	for _, key := range []string{"routing_409", "health_test_models", "banned_accounts", "billing_holds", "groups", "codex_cpa", "usage_journal"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("diagnostic_summary.json missing %q: %+v", key, summary)
		}
	}
	journalJSON, err := json.Marshal(summary["usage_journal"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journalJSON, []byte("usage-journal")) || bytes.Contains(journalJSON, []byte(h.store.Path())) {
		t.Fatalf("usage journal diagnostics leaked a local path: %s", journalJSON)
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

	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/logs", "")
	if code != http.StatusOK {
		t.Fatalf("diagnostics export = %d: %s", code, raw)
	}
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
