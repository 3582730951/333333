package api

import (
	"archive/zip"
	"bytes"
	"context"
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
	for _, name := range []string{"manifest.json", "summary.csv", "by_api_key.csv", "by_account_model.csv", "by_route.csv", "by_route_account_model.csv", "by_time_bucket.csv", "account_map.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("cache hit zip missing %s; has %v", name, zipFileNames(files))
		}
	}
	if !strings.Contains(files["manifest.json"], "codex-pool-cache-hits-v1") {
		t.Fatalf("manifest format missing:\n%s", files["manifest.json"])
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
	if !strings.Contains(files["account_map.csv"], account.ID) || !strings.Contains(files["account_map.csv"], account.Email) {
		t.Fatalf("account_map.csv should contain local admin mapping:\n%s", files["account_map.csv"])
	}
	for _, name := range []string{"summary.csv", "by_account_model.csv", "by_route.csv", "by_route_account_model.csv", "by_time_bucket.csv"} {
		text := files[name]
		for _, forbidden := range []string{account.ID, account.Email, account.Label, account.UpstreamAccountID, account.ChatGPTUserID} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s leaked %q:\n%s", name, forbidden, text)
			}
		}
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
