package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func usageDayStartForTest(now time.Time) int64 {
	local := now.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).Unix()
}

func insertUsageAt(t *testing.T, h *testHarness, accountID, model, apiKeyHash, routeHash string, prompt, completion, total, cached, cacheRead, cacheCreation, createdAt int64) {
	t.Helper()
	if err := h.store.InsertUsageRecordWithCacheDetails(context.Background(), accountID, routeHash, apiKeyHash, "", model, prompt, completion, total, cached, cacheRead, cacheCreation, json.RawMessage(`{"usage":{"input_tokens":1}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE usage_records SET created_at = ? WHERE id = (SELECT MAX(id) FROM usage_records)`, createdAt); err != nil {
		t.Fatal(err)
	}
}

func TestAdminUsageDefaultWindowAndCustomBounds(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	_ = h.store.SetSetting(context.Background(), "usage_accuracy_cutover_at", "0")
	now := time.Now()
	dayStart := usageDayStartForTest(now)
	todayAt := dayStart + 60
	if todayAt >= now.Unix() {
		todayAt = now.Unix() - 1
	}
	yesterdayAt := dayStart - 60
	insertUsageAt(t, h, "acc-today", "gpt-5.5", "hash-today-123456", "route-today", 10, 2, 12, 4, 4, 0, todayAt)
	insertUsageAt(t, h, "acc-yesterday", "gpt-5.5", "hash-yday-123456", "route-yday", 40, 4, 44, 20, 20, 0, yesterdayAt)

	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/usage = %d: %s", code, raw)
	}
	var got struct {
		Rows   []storage.UsageSummaryRow `json:"rows"`
		Window struct {
			NowAt int64 `json:"now_at"`
		} `json:"window"`
		WindowMode       string `json:"window_mode"`
		EffectiveStartAt int64  `json:"effective_start_at"`
		EffectiveUntilAt int64  `json:"effective_until_at"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode /admin/usage envelope: %v (%s)", err, raw)
	}
	if got.WindowMode != "default" || got.EffectiveStartAt != dayStart || got.EffectiveUntilAt < todayAt {
		t.Fatalf("default window metadata wrong: %+v dayStart=%d todayAt=%d", got, dayStart, todayAt)
	}
	if got.EffectiveUntilAt != got.Window.NowAt {
		t.Fatalf("default window until must equal window.now_at, got until=%d now_at=%d", got.EffectiveUntilAt, got.Window.NowAt)
	}
	if len(got.Rows) != 1 || got.Rows[0].AccountID != "acc-today" || got.Rows[0].TotalTokens != 12 {
		t.Fatalf("default usage should include only today's row, got %+v", got.Rows)
	}

	code, raw = grpReq(t, h, http.MethodGet, fmt.Sprintf("/admin/usage?since=%d&until=%d", yesterdayAt, now.Unix()), "")
	if code != http.StatusOK {
		t.Fatalf("/admin/usage custom = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode custom /admin/usage: %v (%s)", err, raw)
	}
	if got.WindowMode != "custom" || got.EffectiveStartAt != yesterdayAt {
		t.Fatalf("custom window metadata wrong: %+v", got)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("custom usage should include both rows, got %+v", got.Rows)
	}

	for _, path := range []string{
		"/admin/usage/window?since=-1",
		fmt.Sprintf("/admin/usage/window?since=%d&until=%d", todayAt, todayAt),
		fmt.Sprintf("/admin/usage/window?until=%d", now.Unix()+600),
	} {
		if code, raw := grpReq(t, h, http.MethodGet, path, ""); code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (%s)", path, code, raw)
		}
	}
}

func TestAdminCacheResetRejectsCustomWindowBeforeWriting(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	before, beforeOK, err := h.store.GetSetting(context.Background(), usageCacheStatsResetSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/usage/cache/reset?since=-1", "")
	if code != http.StatusBadRequest {
		t.Fatalf("cache reset with custom window = %d, want 400: %s", code, raw)
	}
	after, afterOK, err := h.store.GetSetting(context.Background(), usageCacheStatsResetSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if beforeOK != afterOK || before != after {
		t.Fatalf("invalid reset query must not write baseline, before=(%v,%q) after=(%v,%q)", beforeOK, before, afterOK, after)
	}
}

func TestAdminCacheResetMovesOnlyCacheDiagnosticsWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	_ = h.store.SetSetting(context.Background(), "usage_accuracy_cutover_at", "0")
	code, raw := grpReq(t, h, http.MethodPost, "/admin/usage/cache/reset", "")
	if code != http.StatusOK {
		t.Fatalf("cache reset = %d: %s", code, raw)
	}
	var resetResp struct {
		ResetAt          int64  `json:"reset_at"`
		EffectiveStartAt int64  `json:"effective_start_at"`
		WindowMode       string `json:"window_mode"`
	}
	if err := json.Unmarshal(raw, &resetResp); err != nil {
		t.Fatalf("decode reset: %v (%s)", err, raw)
	}
	if resetResp.ResetAt == 0 || resetResp.EffectiveStartAt != resetResp.ResetAt {
		t.Fatalf("reset response missing persisted baseline: %+v", resetResp)
	}
	insertUsageAt(t, h, "acc-before-reset", "gpt-5.5", "hash-before-123456", "route-before", 100, 10, 110, 80, 80, 0, resetResp.ResetAt-1)
	insertUsageAt(t, h, "acc-after-reset", "gpt-5.5", "hash-after-123456", "route-after", 25, 5, 30, 15, 15, 0, resetResp.ResetAt)
	for time.Now().Unix() <= resetResp.ResetAt {
		time.Sleep(25 * time.Millisecond)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/usage/cache", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/usage/cache = %d: %s", code, raw)
	}
	var cacheGot struct {
		Summary struct {
			Requests        int64 `json:"requests"`
			CacheReadTokens int64 `json:"cache_read_tokens"`
		} `json:"summary"`
		EffectiveStartAt int64 `json:"effective_start_at"`
	}
	if err := json.Unmarshal(raw, &cacheGot); err != nil {
		t.Fatalf("decode cache diagnostics: %v (%s)", err, raw)
	}
	if cacheGot.EffectiveStartAt != resetResp.ResetAt || cacheGot.Summary.Requests != 1 || cacheGot.Summary.CacheReadTokens != 15 {
		t.Fatalf("cache reset should hide pre-reset diagnostics only, got %+v", cacheGot)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/usage", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/usage after reset = %d: %s", code, raw)
	}
	var usageGot struct {
		Rows []storage.UsageSummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &usageGot); err != nil {
		t.Fatalf("decode usage after reset: %v (%s)", err, raw)
	}
	if len(usageGot.Rows) != 2 {
		t.Fatalf("manual cache reset must not delete or hide today's usage rows, got %+v", usageGot.Rows)
	}
}

func TestAdminUsageInvalidResetFallsBackAndAudits(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.SetSetting(context.Background(), "usage_cache_stats_reset_at", "not-rfc3339"); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/cache", "")
	if code != http.StatusOK {
		t.Fatalf("invalid reset should not 500: %d %s", code, raw)
	}
	logs, err := h.store.ListAuditLog(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range logs {
		if row.Action == "usage_cache_stats_reset_invalid" {
			return
		}
	}
	t.Fatalf("missing usage_cache_stats_reset_invalid audit, logs=%+v", logs)
}

func TestAdminUsageCacheEmptyCollectionsAreArrays(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/cache", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/usage/cache = %d: %s", code, raw)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode cache response: %v (%s)", err, raw)
	}
	for _, field := range []string{
		"by_account",
		"by_model",
		"by_api_key",
		"by_account_model",
		"by_route",
		"by_route_account_model",
		"by_time_bucket",
	} {
		if got := strings.TrimSpace(string(payload[field])); got != "[]" {
			t.Errorf("empty %s = %s, want []", field, got)
		}
	}
	var summary struct {
		LatestUserCacheControl int64 `json:"latest_user_cache_control"`
	}
	if err := json.Unmarshal(payload["summary"], &summary); err != nil {
		t.Fatalf("decode cache summary: %v (%s)", err, payload["summary"])
	}
	if summary.LatestUserCacheControl != 0 {
		t.Fatalf("empty latest_user_cache_control = %d, want numeric zero", summary.LatestUserCacheControl)
	}
}

func TestAdminUsageDailyResetAuditDedupes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, path := range []string{"/admin/usage/window", "/admin/usage", "/admin/usage/cache"} {
		if code, raw := grpReq(t, h, http.MethodGet, path, ""); code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, code, raw)
		}
	}
	logs, err := h.store.ListAuditLog(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, row := range logs {
		if row.Action == "usage_daily_window_reset" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("usage_daily_window_reset audit count = %d, want 1; logs=%+v", count, logs)
	}
}

func TestAdminUsageCacheFieldsAndDynamicModelNormalization(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	now := time.Now().Unix()
	insertUsageAt(t, h, "acc-deepseek", " deepseek-chat ", "hash-deepseek-123", "route-deepseek", 100, 10, 110, 40, 40, 20, now-3)
	insertUsageAt(t, h, "acc-custom", "custom-new-2027", "hash-custom-123", "route-custom", 80, 8, 88, 10, 10, 15, now-2)
	insertUsageAt(t, h, "acc-claude-future", "claude-future-2027", "hash-claude-123", "route-claude", 70, 7, 77, 20, 20, 5, now-1)
	insertUsageAt(t, h, "acc-unknown", "", "hash-unknown-123", "route-unknown", 60, 6, 66, 0, 0, 0, now-1)

	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/cache?fields=summary,by_model", "")
	if code != http.StatusOK {
		t.Fatalf("cache fields = %d: %s", code, raw)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode cache fields: %v (%s)", err, raw)
	}
	for _, want := range []string{"summary", "by_model"} {
		if _, ok := payload[want]; !ok {
			t.Fatalf("cache fields response missing %s: %s", want, raw)
		}
	}
	for _, forbidden := range []string{"by_account", "by_api_key", "by_account_model", "by_route", "by_route_account_model", "by_time_bucket"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("cache fields response included unrequested %s: %s", forbidden, raw)
		}
	}
	var byModel []struct {
		Model      string `json:"model"`
		ModelKey   string `json:"model_key"`
		ModelLabel string `json:"model_label"`
	}
	if err := json.Unmarshal(payload["by_model"], &byModel); err != nil {
		t.Fatalf("decode by_model: %v", err)
	}
	got := map[string]string{}
	for _, row := range byModel {
		got[row.ModelKey] = row.ModelLabel
		if row.Model == " deepseek-chat " {
			t.Fatalf("model value should be trimmed, got %#v", row)
		}
	}
	for key, label := range map[string]string{
		"deepseek-chat":      "deepseek-chat",
		"custom-new-2027":    "custom-new-2027",
		"claude-future-2027": "claude-future-2027",
		"__unknown__":        "(未知)",
	} {
		if got[key] != label {
			t.Fatalf("model %q label = %q, want %q (all=%#v)", key, got[key], label, got)
		}
	}
	if code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/cache?fields=summary,nope", ""); code != http.StatusBadRequest {
		t.Fatalf("invalid cache fields = %d, want 400: %s", code, raw)
	}
}

func TestAdminUsageByModelAndSeriesAreDynamicUsageRecordModels(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	_ = h.store.SetSetting(context.Background(), "usage_accuracy_cutover_at", "0")
	base := time.Now().Unix() - 600
	insertUsageAt(t, h, "acc-a1", " model-a ", "hash-a1", "route-a1", 100, 0, 100, 0, 0, 0, base+10)
	insertUsageAt(t, h, "acc-b1", "model-b", "hash-b1", "route-b1", 60, 0, 60, 0, 0, 0, base+10)
	insertUsageAt(t, h, "acc-b2", "model-b", "hash-b2", "route-b2", 60, 0, 60, 0, 0, 0, base+70)
	insertUsageAt(t, h, "acc-c1", "custom-new-2027", "hash-c1", "route-c1", 90, 0, 90, 0, 0, 0, base+70)
	insertUsageAt(t, h, "acc-u", "", "hash-u", "route-u", 5, 0, 5, 0, 0, 0, base+70)

	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/by-model?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("by-model = %d: %s", code, raw)
	}
	var byModelResp struct {
		Models []struct {
			Model      string `json:"model"`
			ModelKey   string `json:"model_key"`
			ModelLabel string `json:"model_label"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &byModelResp); err != nil {
		t.Fatalf("decode by-model: %v (%s)", err, raw)
	}
	seen := map[string]string{}
	for _, row := range byModelResp.Models {
		seen[row.ModelKey] = row.ModelLabel
		if row.Model == " model-a " {
			t.Fatalf("by-model should trim model strings: %#v", row)
		}
	}
	for key, label := range map[string]string{"model-a": "model-a", "model-b": "model-b", "custom-new-2027": "custom-new-2027", "__unknown__": "(未知)"} {
		if seen[key] != label {
			t.Fatalf("by-model missing %q => %q: all=%#v", key, label, seen)
		}
	}

	code, raw = grpReq(t, h, http.MethodGet, fmt.Sprintf("/admin/usage/timeseries?since=%d&until=%d&bucket=60&series_dimension=model&series_limit=2", base, base+180), "")
	if code != http.StatusOK {
		t.Fatalf("model series = %d: %s", code, raw)
	}
	var seriesResp struct {
		ModelSeries []struct {
			Bucket      int64  `json:"bucket"`
			SeriesKey   string `json:"series_key"`
			SeriesLabel string `json:"series_label"`
			TotalTokens int64  `json:"total_tokens"`
		} `json:"model_series"`
		Series []struct {
			SeriesKey   string `json:"series_key"`
			SeriesLabel string `json:"series_label"`
		} `json:"series"`
	}
	if err := json.Unmarshal(raw, &seriesResp); err != nil {
		t.Fatalf("decode model series: %v (%s)", err, raw)
	}
	if len(seriesResp.Series) != 2 {
		t.Fatalf("series length = %d, want 2: %#v", len(seriesResp.Series), seriesResp.Series)
	}
	if seriesResp.Series[0].SeriesKey != "model-b" || seriesResp.Series[1].SeriesKey != "model-a" {
		t.Fatalf("series order = %#v, want whole-window top model-b then model-a", seriesResp.Series)
	}
	if len(seriesResp.ModelSeries) > 4 {
		t.Fatalf("model_series rows = %d, want at most series_limit * bucket_count: %#v", len(seriesResp.ModelSeries), seriesResp.ModelSeries)
	}
	for _, row := range seriesResp.ModelSeries {
		if row.SeriesKey == "custom-new-2027" || row.SeriesLabel == "Other" {
			t.Fatalf("model series must not use per-bucket topN or Other aggregation: %#v", seriesResp.ModelSeries)
		}
	}
}

func TestNewAdminUsageEndpointsRejectNonAdminAndAPIKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	admin := jarClient(t)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register admin: %d", resp.StatusCode)
	}
	user := jarClient(t)
	if resp, _ := doReq(t, user, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"user@x.io","password":"password123"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register user: %d", resp.StatusCode)
	}
	apiKey := "cap_usage_admin_forbidden"
	if err := h.store.UpsertAPIKey(context.Background(), storage.APIKey{
		KeyHash: hashAPIKey(apiKey),
		Label:   "normal user key",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	adminCSRF := csrfFor(t, admin, h.pool.URL)
	apiKeyHeaders := []map[string]string{
		{"Authorization": "Bearer " + apiKey},
		{"x-api-key": apiKey},
		{"X-Downstream-Key": apiKey},
	}

	endpoints := []struct {
		method       string
		path         string
		adminHeaders map[string]string
	}{
		{method: http.MethodGet, path: "/admin/usage/window"},
		{method: http.MethodPost, path: "/admin/usage/cache/reset", adminHeaders: map[string]string{csrfHeaderName: adminCSRF}},
		{method: http.MethodGet, path: "/admin/export/cache-hits"},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			if resp, _ := doReq(t, admin, endpoint.method, h.pool.URL+endpoint.path, "", endpoint.adminHeaders); resp.StatusCode != http.StatusOK {
				t.Fatalf("admin cookie should be allowed, got %d", resp.StatusCode)
			}
			if resp, _ := doReq(t, user, endpoint.method, h.pool.URL+endpoint.path, "", nil); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("normal user cookie should be 403, got %d", resp.StatusCode)
			}
			for _, headers := range apiKeyHeaders {
				if resp, _ := doReq(t, jarClient(t), endpoint.method, h.pool.URL+endpoint.path, "", headers); resp.StatusCode != http.StatusForbidden {
					t.Fatalf("downstream API key %v should be 403, got %d", headers, resp.StatusCode)
				}
			}
		})
	}
}

func TestAdminCacheHitsExportZip(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{
		ID:                "acc-real-cache",
		Label:             "Cache Sensitive",
		Email:             "cache-sensitive@example.com",
		Provider:          "codex",
		GroupName:         "cyber",
		Status:            "active",
		UpstreamAccountID: "upstream-cache-secret",
		ChatGPTUserID:     "chatgpt-cache-secret",
	}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "access-cache-secret"}); err != nil {
		t.Fatal(err)
	}
	insertUsageAt(t, h, account.ID, "gpt-5.5", "abcdef1234567890full", "route-secret-cache-123456", 100, 10, 110, 80, 80, 20, time.Now().Unix()-1)

	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("cache hit export = %d: %s", code, raw)
	}
	if _, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("response is not a zip: %v\n%s", err, raw)
	}
	files := readZipFiles(t, raw)
	for _, name := range []string{"manifest.json", "summary.csv", "by_api_key.csv", "by_account_model.csv", "by_route.csv", "by_route_account_model.csv", "by_time_bucket.csv", "route_map.csv", "account_map.csv", "kiro_capabilities.csv", "usage_sources.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("cache hit zip missing %s; has %v", name, zipFileNames(files))
		}
	}
	if !strings.Contains(files["manifest.json"], "codex-pool-cache-hits-v2") || !strings.Contains(files["manifest.json"], "excluded from calculable hit-rate denominators") {
		t.Fatalf("manifest format missing:\n%s", files["manifest.json"])
	}
	legacyCode, legacyRaw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0&version=v1", "")
	if legacyCode != http.StatusOK {
		t.Fatalf("v1 migration export unavailable: status=%d", legacyCode)
	}
	legacyFiles := readZipFiles(t, legacyRaw)
	if !strings.Contains(legacyFiles["manifest.json"], "codex-pool-cache-hits-v1") {
		t.Fatalf("v1 migration manifest missing: %s", legacyFiles["manifest.json"])
	}
	if !strings.Contains(files["summary.csv"], "token_hit_rate") || !strings.Contains(files["summary.csv"], "80") {
		t.Fatalf("summary.csv missing expected metrics:\n%s", files["summary.csv"])
	}
	if !strings.Contains(files["by_api_key.csv"], "abcdef123456") || strings.Contains(files["by_api_key.csv"], "abcdef1234567890full") {
		t.Fatalf("by_api_key must use 12-char prefix only:\n%s", files["by_api_key.csv"])
	}
	if strings.Contains(files["by_route.csv"], "route_key_hash_prefix") || strings.Contains(files["by_route.csv"], "route-secret") || !strings.Contains(files["by_route.csv"], "ROUTE-0001") {
		t.Fatalf("by_route must use per-export route_code instead of route hash prefix:\n%s", files["by_route.csv"])
	}
	if !strings.Contains(files["route_map.csv"], "route_key_hash_prefix") || !strings.Contains(files["route_map.csv"], "route-secret") || !strings.Contains(files["route_map.csv"], "route_class") {
		t.Fatalf("route_map.csv must expose the zip-level route codebook:\n%s", files["route_map.csv"])
	}
	for version, archiveFiles := range map[string]map[string]string{"v2": files, "v1": legacyFiles} {
		accountMap, err := csv.NewReader(strings.NewReader(archiveFiles["account_map.csv"])).ReadAll()
		if err != nil {
			t.Fatalf("%s account_map.csv: %v", version, err)
		}
		if len(accountMap) != 2 || len(accountMap[0]) != 2 || accountMap[0][0] != "account_code" || accountMap[0][1] != "account_id" || accountMap[1][0] != "ACC-0001" || accountMap[1][1] != account.ID {
			t.Fatalf("%s account_map.csv must contain only account_code and account_id: %v", version, accountMap)
		}
		for name, text := range archiveFiles {
			for _, forbidden := range []string{account.Email, account.Label, account.UpstreamAccountID, account.ChatGPTUserID, "access-cache-secret"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("%s %s leaked %q:\n%s", version, name, forbidden, text)
				}
			}
			if name != "account_map.csv" && strings.Contains(text, account.ID) {
				t.Fatalf("%s %s leaked raw account id %q:\n%s", version, name, account.ID, text)
			}
		}
	}
}

func TestAdminCacheHitsV2RendersKiroUnreportedRatesAsUnknown(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "kiro-unreported-export", Label: "Kiro", Provider: "kiro", GroupName: "cyber", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := h.store.ObserveKiroCapability(ctx, account.ID, "endpoint-hash", "claude-sonnet-4.6", storage.KiroCapabilityObservation{ModelSucceeded: true, MeteringEvents: 1, UnreportedThreshold: 20}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.SetKiroCachePointState(ctx, account.ID, "endpoint-hash", "claude-sonnet-4.6", "verified"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "route", "", "", "claude-sonnet-4.6", 100, 1, 101, 0, 0, 0,
		json.RawMessage(`{"input_tokens":100,"metadata_event_count":0,"metering_event_count":1,"credits_present":true,"kiro_credits":0.25,"cache_point_state":"verified"}`),
		storage.UsageDiagnostics{UsageProvider: "kiro", UsageSource: "estimated", Estimated: true, CacheControlInjected: true, CacheBreakpointCount: 4}); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("cache hit export = %d: %s", code, raw)
	}
	files := readZipFiles(t, raw)
	records, err := csv.NewReader(strings.NewReader(files["by_account_model.csv"])).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("by_account_model.csv records=%v err=%v\n%s", records, err, files["by_account_model.csv"])
	}
	columns := map[string]int{}
	for i, name := range records[0] {
		columns[name] = i
	}
	row := records[1]
	for _, name := range []string{"request_hit_rate", "token_hit_rate", "real_token_hit_rate"} {
		if row[columns[name]] != "" {
			t.Fatalf("%s=%q, want blank for unreported Kiro metering: %v", name, row[columns[name]], row)
		}
	}
	if row[columns["provider"]] != "kiro" || row[columns["cache_capability"]] != "unreported" || row[columns["real_requests"]] != "0" || row[columns["cache_miss_tokens"]] != "0" {
		t.Fatalf("unreported Kiro export row=%v", row)
	}
	for _, name := range []string{"summary.csv", "by_api_key.csv", "by_route.csv", "by_route_account_model.csv"} {
		records, err := csv.NewReader(strings.NewReader(files[name])).ReadAll()
		if err != nil || len(records) != 2 {
			t.Fatalf("%s records=%v err=%v", name, records, err)
		}
		columns := map[string]int{}
		for i, column := range records[0] {
			columns[column] = i
		}
		for _, field := range []string{"request_hit_rate", "token_hit_rate", "real_token_hit_rate"} {
			if got := records[1][columns[field]]; got != "" {
				t.Fatalf("%s %s=%q, want blank without a measured denominator: %v", name, field, got, records[1])
			}
		}
	}
	buckets, err := csv.NewReader(strings.NewReader(files["by_time_bucket.csv"])).ReadAll()
	if err != nil || len(buckets) != 2 {
		t.Fatalf("by_time_bucket.csv records=%v err=%v", buckets, err)
	}
	bucketColumns := map[string]int{}
	for i, column := range buckets[0] {
		bucketColumns[column] = i
	}
	if got := buckets[1][bucketColumns["cache_read_share"]]; got != "" {
		t.Fatalf("by_time_bucket cache_read_share=%q, want blank without a measured denominator: %v", got, buckets[1])
	}
	usageSources, err := csv.NewReader(strings.NewReader(files["usage_sources.csv"])).ReadAll()
	if err != nil || len(usageSources) != 2 {
		t.Fatalf("usage_sources.csv records=%v err=%v", usageSources, err)
	}
	usageColumns := map[string]int{}
	for i, column := range usageSources[0] {
		usageColumns[column] = i
	}
	usageRow := usageSources[1]
	for field, want := range map[string]string{
		"provider": "kiro", "usage_source": "estimated", "metadata_events": "0", "metering_events": "1",
		"credits_reported_requests": "1", "credits_total": "0.250000", "token_metadata_reported_requests": "0",
		"cache_point_injected_requests": "1", "cache_point_accepted_requests": "1",
	} {
		if got := usageRow[usageColumns[field]]; got != want {
			t.Fatalf("usage_sources %s=%q, want %q: %v", field, got, want, usageRow)
		}
	}
	capabilities, err := csv.NewReader(strings.NewReader(files["kiro_capabilities.csv"])).ReadAll()
	if err != nil || len(capabilities) != 2 {
		t.Fatalf("kiro_capabilities.csv records=%v err=%v", capabilities, err)
	}
	capabilityColumns := map[string]int{}
	for i, column := range capabilities[0] {
		capabilityColumns[column] = i
	}
	capabilityRow := capabilities[1]
	for field, want := range map[string]string{
		"cache_point_state": "verified", "cache_reuse_state": "unknown", "window_account_model_requests": "1",
		"window_account_model_metadata_events": "0", "window_account_model_metering_events": "1", "window_account_model_credits_reported_requests": "1",
		"window_account_model_cache_point_injected_requests": "1", "window_account_model_cache_point_accepted_requests": "1",
	} {
		if got := capabilityRow[capabilityColumns[field]]; got != want {
			t.Fatalf("kiro_capabilities %s=%q, want %q: %v", field, got, want, capabilityRow)
		}
	}
}

func TestAdminCacheHitsLeavesCreationRatesBlankWhenProviderDidNotReportWrites(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "codex-no-write-metric", Provider: "codex", GroupName: "cyber", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecord(ctx, account.ID, "route", "", "", "gpt-5.6", 100, 1, 101, 80, json.RawMessage(`{"input_tokens":100,"input_tokens_details":{"cached_tokens":80}}`)); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("cache hit export = %d: %s", code, raw)
	}
	records, err := csv.NewReader(strings.NewReader(readZipFiles(t, raw)["by_account_model.csv"])).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%v err=%v", records, err)
	}
	columns := map[string]int{}
	for i, name := range records[0] {
		columns[name] = i
	}
	for _, field := range []string{"cache_write_share", "eligible_cache_hit_rate"} {
		if got := records[1][columns[field]]; got != "" {
			t.Fatalf("%s=%q, want blank without explicit cache creation metering: %v", field, got, records[1])
		}
	}
}

func TestCacheMetricRowsV2UsesHistoricalUsageProviderAndCapability(t *testing.T) {
	accountID := "historical-kiro-account"
	codebook := buildDiagnosticCodebook(nil, nil, nil, []diagnosticUsageRecord{{AccountID: accountID}}, nil, nil)
	rows := cacheMetricRowsV2([]storage.CacheUsageMetricRow{{AccountID: accountID, Model: "claude-opus-4-8", Requests: 1, EstimatedRequests: 1}}, codebook,
		[]diagnosticUsageRecord{{AccountID: accountID, Model: "claude-opus-4.8", UsageProvider: "kiro", UsageSource: "estimated", CacheCapability: "unreported", Estimated: 1}}, nil)
	if len(rows) != 1 || len(rows[0]) < 5 {
		t.Fatalf("historical Kiro rows=%v", rows)
	}
	if rows[0][1] != "kiro" || rows[0][3] != "unreported" || rows[0][4] != "estimated" {
		t.Fatalf("historical Kiro identity was lost: %v", rows[0])
	}
}

func TestAdminCacheHitsExportEmptyZipHasHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("empty cache hit export = %d: %s", code, raw)
	}
	files := readZipFiles(t, raw)
	for _, name := range []string{"summary.csv", "by_api_key.csv", "by_account_model.csv", "by_route.csv", "by_route_account_model.csv", "by_time_bucket.csv", "account_map.csv"} {
		text := files[name]
		if firstLine := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0]); firstLine == "" {
			t.Fatalf("%s should contain a CSV header, got %q", name, text)
		}
	}
	if len(files) == 0 {
		t.Fatal("empty export zip had no files")
	}
	_ = io.EOF
}

func TestAdminCacheHitsExportUsesFullRouteAggregation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	accountID := h.importAccount(t, "full-route-export", "upstream-full-route-export", "access-full-route-export")
	now := time.Now().Unix() - 1
	for i := 0; i < 205; i++ {
		route := fmt.Sprintf("r%03d-full-export-route", i)
		if err := h.store.InsertUsageRecordWithCacheDetails(ctx, accountID, route, "apikey-full-export", "", "claude-opus-4-8", 100, 1, 101, 0, 0, int64(i+1), json.RawMessage(`{"usage":{"input_tokens":100}}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.DB().ExecContext(ctx, `UPDATE usage_records SET created_at = ? WHERE id = (SELECT MAX(id) FROM usage_records)`, now); err != nil {
			t.Fatal(err)
		}
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/export/cache-hits?since=0", "")
	if code != http.StatusOK {
		t.Fatalf("cache hit export = %d: %s", code, raw)
	}
	files := readZipFiles(t, raw)
	routeMap := files["route_map.csv"]
	lines := strings.Split(strings.TrimSpace(routeMap), "\n")
	if len(lines) != 206 {
		t.Fatalf("route_map.csv rows = %d including header, want 206; content starts:\n%s", len(lines), firstN(routeMap, 800))
	}
	if !strings.Contains(routeMap, "r000-full-ex") {
		t.Fatalf("route_map.csv should contain route hash prefixes:\n%s", firstN(routeMap, 800))
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
