package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
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
		Format string         `json:"format"`
		Files  []string       `json:"files"`
		Rows   map[string]int `json:"row_counts"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatalf("manifest json: %v\n%s", err, files["manifest.json"])
	}
	if manifest.Format != "codex-pool-diagnostics-v2" {
		t.Fatalf("manifest format = %q, want codex-pool-diagnostics-v2", manifest.Format)
	}
	for _, name := range required {
		if _, ok := manifest.Rows[name]; !ok {
			t.Fatalf("manifest missing row count for %s: %+v", name, manifest.Rows)
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
	for _, want := range []string{"access_token_present", "access_token_len", "effective_provider", "codex"} {
		if !strings.Contains(authCSV, want) {
			t.Fatalf("account_auth_metadata.csv missing %q:\n%s", want, authCSV)
		}
	}

	var summary map[string]interface{}
	if err := json.Unmarshal([]byte(files["diagnostic_summary.json"]), &summary); err != nil {
		t.Fatalf("diagnostic_summary.json: %v\n%s", err, files["diagnostic_summary.json"])
	}
	for _, key := range []string{"routing_409", "health_test_models", "banned_accounts", "billing_holds", "groups"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("diagnostic_summary.json missing %q: %+v", key, summary)
		}
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
