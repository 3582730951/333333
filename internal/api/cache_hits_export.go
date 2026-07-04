package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

func (s *Server) adminCacheHitsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.store.CacheUsageMetricsWindowFullRoutes(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	usageRows, err := listDiagnosticUsageRecordsWindow(r.Context(), s.store.DB(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	codebook := buildDiagnosticCodebook(accounts, nil, nil, usageRows, nil, nil)
	files, order, err := buildCacheHitsZipFiles(report, win, codebook, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range order {
		fw, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := fw.Write([]byte(files[name])); err != nil {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	filename := fmt.Sprintf("codex-pool-cache-hits-%s.zip", now.In(time.Local).Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(buf.Bytes())
}

func buildCacheHitsZipFiles(report storage.CacheUsageReport, win adminUsageWindow, codebook diagnosticCodebook, generatedAt time.Time) (map[string]string, []string, error) {
	order := []string{
		"manifest.json",
		"summary.csv",
		"by_api_key.csv",
		"by_account_model.csv",
		"by_route.csv",
		"by_route_account_model.csv",
		"by_time_bucket.csv",
		"route_map.csv",
		"account_map.csv",
	}
	manifest := map[string]interface{}{
		"generated_at":        generatedAt.Unix(),
		"format":              "codex-pool-cache-hits-v1",
		"window_mode":         win.WindowMode,
		"effective_start_at":  win.EffectiveStartAt,
		"effective_until_at":  win.EffectiveUntilAt,
		"timezone":            win.Window.Timezone,
		"utc_offset_seconds":  win.Window.UTCOffsetSeconds,
		"files":               order,
		"account_redaction":   "business files use account_code; real account identifiers are only in account_map.csv",
		"api_key_redaction":   "api key hashes are truncated to 12-character prefixes",
		"hit_tokens_formula":  "cache_read_tokens",
		"token_hit_rate":      "clamp(hit_tokens / cache_input_tokens)",
		"eligible_hit_rate":   "cache_read_tokens / (cache_read_tokens + cache_creation_tokens)",
		"cache_input_formula": "cache_total_input_tokens with prompt_tokens fallback",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	hasData := cacheReportHasRows(report)
	files := map[string]string{
		"manifest.json":              string(rawManifest) + "\n",
		"summary.csv":                csvString(cacheMetricHeader("window_start", "window_until"), cacheSummaryRows(report.Summary, win, hasData)),
		"by_api_key.csv":             csvString(cacheMetricHeader("api_key_hash_prefix"), cacheMetricRows(report.ByAPIKey, codebook, false, false)),
		"by_account_model.csv":       csvString(cacheMetricHeader("account_code", "model"), cacheMetricRows(report.ByAccountModel, codebook, true, true)),
		"by_route.csv":               "",
		"by_route_account_model.csv": "",
		"by_time_bucket.csv":         csvString(cacheBucketHeader(), cacheBucketRows(report.ByTimeBucket)),
		"route_map.csv":              "",
		"account_map.csv":            csvString([]string{"account_code", "account_id", "email", "label", "upstream_account_id", "chatgpt_user_id", "provider", "group_name", "status"}, accountMapRows(codebook)),
	}
	routes := newRouteExportCodebook()
	routes.addRows(report.ByRoute)
	routes.addRows(report.ByRouteAccountModel)
	files["by_route.csv"] = csvString(cacheRouteHeader(false), cacheRouteRows(report.ByRoute, codebook, routes, false))
	files["by_route_account_model.csv"] = csvString(cacheRouteHeader(true), cacheRouteRows(report.ByRouteAccountModel, codebook, routes, true))
	files["route_map.csv"] = csvString([]string{"route_code", "route_key_hash_prefix", "route_class", "affinity_source"}, routes.rows())
	return files, order, nil
}

func cacheReportHasRows(report storage.CacheUsageReport) bool {
	if report.Summary.Requests > 0 {
		return true
	}
	for _, rows := range [][]storage.CacheUsageMetricRow{report.ByAPIKey, report.ByAccountModel, report.ByRoute, report.ByRouteAccountModel} {
		if len(rows) > 0 {
			return true
		}
	}
	return len(report.ByTimeBucket) > 0
}

func cacheMetricHeader(prefix ...string) []string {
	return append(prefix, "requests", "real_requests", "hit_requests", "request_hit_rate", "prompt_tokens", "cached_tokens", "hit_tokens", "cache_input_tokens", "cache_miss_tokens", "cache_creation_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "token_hit_rate", "cache_write_share", "eligible_cache_hit_rate", "real_token_hit_rate", "estimated_requests", "estimated_rate")
}

func cacheSummaryRows(row storage.CacheUsageMetricRow, win adminUsageWindow, hasData bool) [][]string {
	if !hasData {
		return nil
	}
	return [][]string{append([]string{itoa64(win.EffectiveStartAt), itoa64(win.EffectiveUntilAt)}, cacheMetricFields(row)...)}
}

func cacheMetricRows(rows []storage.CacheUsageMetricRow, codebook diagnosticCodebook, accountCode, includeModel bool) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		prefix := []string{}
		if accountCode {
			prefix = append(prefix, codebook.code(row.AccountID))
		} else {
			prefix = append(prefix, row.APIKeyHashPrefix)
		}
		if includeModel {
			prefix = append(prefix, row.Model)
		}
		out = append(out, append(prefix, cacheMetricFields(row)...))
	}
	return out
}

func cacheRouteHeader(withAccountModel bool) []string {
	prefix := []string{"route_code", "route_class", "affinity_source", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "prompt_cache_key_present", "cache_control_injected", "cache_breakpoint_count", "latest_user_cache_control", "single_use_route", "risk_flags", "route_epoch"}
	if withAccountModel {
		prefix = append([]string{"account_code", "model"}, prefix...)
	}
	return cacheMetricHeader(prefix...)
}

func cacheRouteRows(rows []storage.CacheUsageMetricRow, codebook diagnosticCodebook, routes *routeExportCodebook, withAccountModel bool) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		prefix := []string{
			routes.code(row.RouteKeyHashPrefix),
			row.RouteClass,
			row.AffinitySource,
			row.PromptCacheKeySource,
			row.StablePrefixSource,
			row.StablePrefixReason,
			itoa64(row.StablePrefixBytes),
			row.RetentionEffective,
			row.RetentionSource,
			row.ClaudeCacheTTL,
			itoa64(row.PromptCacheKeyPresent),
			itoa64(row.CacheControlInjected),
			itoa64(row.CacheBreakpointCount),
			itoa64(row.LatestUserCacheControl),
			strconv.FormatBool(row.SingleUseRoute),
			strings.Join(row.RiskFlags, "|"),
			itoa64(row.RouteEpoch),
		}
		if withAccountModel {
			prefix = append([]string{codebook.code(row.AccountID), row.Model}, prefix...)
		}
		out = append(out, append(prefix, cacheMetricFields(row)...))
	}
	return out
}

func cacheMetricFields(row storage.CacheUsageMetricRow) []string {
	return []string{
		itoa64(row.Requests),
		itoa64(row.RealRequests),
		itoa64(row.HitRequests),
		floatString(row.RequestHitRate),
		itoa64(row.PromptTokens),
		itoa64(row.CachedTokens),
		itoa64(row.CacheReadTokens),
		itoa64(row.CacheInputTokens),
		itoa64(row.CacheMissTokens),
		itoa64(row.CacheCreationTokens),
		itoa64(row.CacheCreation5mTokens),
		itoa64(row.CacheCreation1hTokens),
		floatString(row.TokenHitRate),
		floatString(row.CacheWriteShare),
		floatString(row.EligibleHitRate),
		floatString(row.RealTokenHitRate),
		itoa64(row.EstimatedRequests),
		floatString(row.EstimatedRate),
	}
}

type routeExportCodebook struct {
	byPrefix map[string]string
	entries  []routeExportEntry
}

type routeExportEntry struct {
	Code           string
	HashPrefix     string
	RouteClass     string
	AffinitySource string
}

func newRouteExportCodebook() *routeExportCodebook {
	return &routeExportCodebook{byPrefix: map[string]string{}}
}

func (b *routeExportCodebook) addRows(rows []storage.CacheUsageMetricRow) {
	for _, row := range rows {
		prefix := strings.TrimSpace(row.RouteKeyHashPrefix)
		if prefix == "" {
			continue
		}
		if _, ok := b.byPrefix[prefix]; ok {
			continue
		}
		code := fmt.Sprintf("ROUTE-%04d", len(b.entries)+1)
		b.byPrefix[prefix] = code
		b.entries = append(b.entries, routeExportEntry{
			Code:           code,
			HashPrefix:     prefix,
			RouteClass:     row.RouteClass,
			AffinitySource: row.AffinitySource,
		})
	}
}

func (b *routeExportCodebook) code(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	return b.byPrefix[prefix]
}

func (b *routeExportCodebook) rows() [][]string {
	out := make([][]string, 0, len(b.entries))
	for _, row := range b.entries {
		out = append(out, []string{row.Code, row.HashPrefix, row.RouteClass, row.AffinitySource})
	}
	return out
}

func cacheBucketHeader() []string {
	return []string{"bucket", "requests", "real_requests", "hit_requests", "prompt_tokens", "hit_tokens", "cache_input_tokens", "cache_miss_tokens", "cache_creation_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "cache_read_share", "cache_write_share", "eligible_cache_hit_rate", "estimated_requests", "estimated_rate"}
}

func cacheBucketRows(rows []storage.CacheUsageBucket) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			itoa64(row.Bucket),
			itoa64(row.Requests),
			itoa64(row.RealRequests),
			itoa64(row.HitRequests),
			itoa64(row.PromptTokens),
			itoa64(row.CacheReadTokens),
			itoa64(row.CacheInputTokens),
			itoa64(row.CacheMissTokens),
			itoa64(row.CacheCreationTokens),
			itoa64(row.CacheCreation5mTokens),
			itoa64(row.CacheCreation1hTokens),
			floatString(row.CacheReadShare),
			floatString(row.CacheWriteShare),
			floatString(row.EligibleHitRate),
			itoa64(row.EstimatedRequests),
			floatString(row.EstimatedRate),
		})
	}
	return out
}

func floatString(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func listDiagnosticUsageRecordsWindow(ctx context.Context, db *sql.DB, since, until int64) ([]diagnosticUsageRecord, error) {
	rows, err := db.QueryContext(ctx, diagnosticUsageRecordSelectSQL()+` WHERE created_at >= ? AND created_at < ? ORDER BY id ASC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticUsageRecord
	for rows.Next() {
		var r diagnosticUsageRecord
		if err := scanDiagnosticUsageRecord(rows, &r); err != nil {
			return nil, err
		}
		r.RouteKeyHash = truncateHashPrefix(r.RouteKeyHash)
		r.APIKeyHash = truncateHashPrefix(r.APIKeyHash)
		out = append(out, r)
	}
	return out, rows.Err()
}

func truncateHashPrefix(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
