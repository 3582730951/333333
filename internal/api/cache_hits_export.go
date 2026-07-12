package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
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
	kiroCapabilities, err := s.store.ListKiroRuntimeCapabilities(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	codebook := buildDiagnosticCodebook(accounts, nil, nil, usageRows, nil, nil)
	version := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("version")))
	if version == "" {
		version = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	}
	legacyV1 := version == "v1" || version == "codex-pool-cache-hits-v1"
	files, order, err := buildCacheHitsZipFiles(report, win, codebook, usageRows, kiroCapabilities, now, legacyV1)
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
	filenameVersion := "v2"
	if legacyV1 {
		filenameVersion = "v1"
	}
	filename := fmt.Sprintf("codex-pool-cache-hits-%s-%s.zip", filenameVersion, now.In(time.Local).Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(buf.Bytes())
}

func buildCacheHitsZipFiles(report storage.CacheUsageReport, win adminUsageWindow, codebook diagnosticCodebook, usageRows []diagnosticUsageRecord, kiroCapabilities []storage.KiroRuntimeCapability, generatedAt time.Time, legacyV1 bool) (map[string]string, []string, error) {
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
	if !legacyV1 {
		order = append(order, "kiro_capabilities.csv", "usage_sources.csv")
	}
	format := "codex-pool-cache-hits-v2"
	if legacyV1 {
		format = "codex-pool-cache-hits-v1"
	}
	manifest := map[string]interface{}{
		"generated_at":        generatedAt.Unix(),
		"format":              format,
		"window_mode":         win.WindowMode,
		"effective_start_at":  win.EffectiveStartAt,
		"effective_until_at":  win.EffectiveUntilAt,
		"timezone":            win.Window.Timezone,
		"utc_offset_seconds":  win.Window.UTCOffsetSeconds,
		"files":               order,
		"account_redaction":   "business files use account_code; account_map.csv contains only account_code and local account_id; email, label, and upstream identity columns are omitted",
		"api_key_redaction":   "api key hashes are truncated to 12-character prefixes",
		"hit_tokens_formula":  "cache_read_tokens",
		"token_hit_rate":      "clamp(hit_tokens / cache_input_tokens)",
		"eligible_hit_rate":   "cache_read_tokens / (cache_read_tokens + cache_creation_tokens)",
		"cache_input_formula": "cache_total_input_tokens with prompt_tokens fallback",
	}
	if !legacyV1 {
		manifest["kiro_unreported_policy"] = "Kiro requests without upstream cache fields are excluded from calculable hit-rate denominators; they are not cache misses"
		manifest["usage_source_policy"] = "only usage_source=upstream and cache fields explicitly present contribute to Kiro cache hit rates"
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
		"account_map.csv":            csvString([]string{"account_code", "account_id"}, accountMapRows(codebook)),
	}
	routes := newRouteExportCodebook()
	routes.addRows(report.ByRoute)
	routes.addRows(report.ByRouteAccountModel)
	files["by_route.csv"] = csvString(cacheRouteHeader(false), cacheRouteRows(report.ByRoute, codebook, routes, false))
	files["by_route_account_model.csv"] = csvString(cacheRouteHeader(true), cacheRouteRows(report.ByRouteAccountModel, codebook, routes, true))
	files["route_map.csv"] = csvString([]string{"route_code", "route_key_hash_prefix", "route_class", "affinity_source"}, routes.rows())
	if !legacyV1 {
		files["by_account_model.csv"] = csvString(cacheMetricHeader("account_code", "provider", "model", "cache_capability", "usage_sources"), cacheMetricRowsV2(report.ByAccountModel, codebook, usageRows, kiroCapabilities))
		files["kiro_capabilities.csv"] = csvString([]string{"account_code", "endpoint_hash", "model", "model_state", "thinking_state", "cache_capability", "observations", "metering_events", "cache_reported_observations", "cache_hit_observations", "consecutive_unreported", "updated_at", "cache_point_state"}, cacheKiroCapabilityRows(kiroCapabilities, codebook))
		files["usage_sources.csv"] = csvString([]string{"provider", "usage_source", "requests", "cache_read_reported", "cache_creation_reported"}, cacheUsageSourceRows(usageRows))
	}
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
	return append(prefix, "requests", "real_requests", "hit_requests", "request_hit_rate", "prompt_tokens", "cached_tokens", "hit_tokens", "cache_input_tokens", "cache_miss_tokens", "cache_creation_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "cache_creation_5m_share", "token_hit_rate", "cache_write_share", "eligible_cache_hit_rate", "real_token_hit_rate", "estimated_requests", "estimated_rate", "cache_creation_reported_requests")
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

func cacheMetricRowsV2(rows []storage.CacheUsageMetricRow, codebook diagnosticCodebook, usageRows []diagnosticUsageRecord, capabilities []storage.KiroRuntimeCapability) [][]string {
	sources := map[string]map[string]bool{}
	for _, usageRow := range usageRows {
		key := usageRow.AccountID + "\x00" + normalizedCacheExportModel(usageRow.Model)
		if sources[key] == nil {
			sources[key] = map[string]bool{}
		}
		source := strings.TrimSpace(usageRow.UsageSource)
		if source == "" {
			if usageRow.Estimated != 0 {
				source = "estimated"
			} else {
				source = "upstream"
			}
		}
		sources[key][source] = true
	}
	cacheState := map[string]string{}
	priority := map[string]int{"": 0, "unknown": 1, "unreported": 2, "explicitly_unsupported": 3, "reported": 4, "hit_observed": 5}
	for _, capability := range capabilities {
		key := capability.AccountID + "\x00" + normalizedCacheExportModel(capability.Model)
		if priority[capability.CacheCapability] > priority[cacheState[key]] {
			cacheState[key] = capability.CacheCapability
		}
	}
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		provider := ""
		if identity, ok := codebook.byID[row.AccountID]; ok {
			provider = identity.Provider
		}
		key := row.AccountID + "\x00" + normalizedCacheExportModel(row.Model)
		usageSources := make([]string, 0, len(sources[key]))
		for source := range sources[key] {
			usageSources = append(usageSources, source)
		}
		sort.Strings(usageSources)
		prefix := []string{codebook.code(row.AccountID), provider, row.Model, cacheState[key], strings.Join(usageSources, "|")}
		out = append(out, append(prefix, cacheMetricFieldsV2(row)...))
	}
	return out
}

func normalizedCacheExportModel(model string) string {
	if canonical, ok := capability.KiroCanonicalModel(model); ok {
		return canonical
	}
	return strings.TrimSpace(model)
}

func cacheKiroCapabilityRows(rows []storage.KiroRuntimeCapability, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			codebook.code(row.AccountID), row.EndpointHash, row.Model, row.ModelState, row.ThinkingState, row.CacheCapability,
			itoa64(row.Observations), itoa64(row.MeteringEvents), itoa64(row.CacheReportedObservations),
			itoa64(row.CacheHitObservations), itoa64(row.ConsecutiveUnreported), itoa64(row.UpdatedAt), row.CachePointState,
		})
	}
	return out
}

func cacheUsageSourceRows(rows []diagnosticUsageRecord) [][]string {
	type count struct{ requests, read, creation int64 }
	counts := map[string]*count{}
	for _, row := range rows {
		provider := firstNonEmpty(row.UsageProvider, "unknown")
		source := strings.TrimSpace(row.UsageSource)
		if source == "" {
			if row.Estimated != 0 {
				source = "estimated"
			} else {
				source = "upstream"
			}
		}
		key := provider + "\x00" + source
		if counts[key] == nil {
			counts[key] = &count{}
		}
		counts[key].requests++
		if row.CacheReadPresent != 0 {
			counts[key].read++
		}
		if row.CacheCreationPresent != 0 {
			counts[key].creation++
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([][]string, 0, len(keys))
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		value := counts[key]
		out = append(out, []string{parts[0], parts[1], itoa64(value.requests), itoa64(value.read), itoa64(value.creation)})
	}
	return out
}

func cacheRouteHeader(withAccountModel bool) []string {
	prefix := []string{"route_code", "route_class", "affinity_source", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "prompt_cache_key_present", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "single_use_route", "risk_flags", "route_epoch"}
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
			codebook.sanitize(row.CacheBreakpointsJSON),
			itoa64(row.UnwrittenTailTokens),
			itoa64(row.MaxPossibleCacheReadTokens),
			itoa64(row.CacheHitAfterPrewarm),
			itoa64(row.SingleflightWaitedRequests),
			row.DiagnosticsMissReason,
			itoa64(row.LatestUserCacheControl),
			itoa64(row.LatestUserAutoContextCacheControl),
			itoa64(row.LatestUserTailCacheControl),
			itoa64(row.LatestUserToolResultCacheControl),
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
	fields := []string{
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
		floatString(row.CacheCreation5mShare),
		floatString(row.TokenHitRate),
		floatString(row.CacheWriteShare),
		floatString(row.EligibleHitRate),
		floatString(row.RealTokenHitRate),
		itoa64(row.EstimatedRequests),
		floatString(row.EstimatedRate),
		itoa64(row.CacheCreationReportedRequests),
	}
	if row.CacheCreationReportedRequests == 0 {
		fields[14] = ""
		fields[15] = ""
	}
	return fields
}

// cacheMetricFieldsV2 leaves rates blank when their upstream denominator is not
// available. In particular, a Kiro model with cache_capability=unreported must not
// appear as a numeric 0% cache miss merely because CSV has no null type.
func cacheMetricFieldsV2(row storage.CacheUsageMetricRow) []string {
	fields := cacheMetricFields(row)
	if row.RealRequests == 0 {
		fields[3] = ""  // request_hit_rate
		fields[16] = "" // real_token_hit_rate
	}
	if row.CacheInputTokens == 0 {
		fields[13] = "" // token_hit_rate
		fields[14] = "" // cache_write_share
	}
	if row.CacheCreationReportedRequests == 0 {
		fields[14] = "" // cache_write_share
		fields[15] = "" // eligible_cache_hit_rate
	}
	if row.CacheReadTokens+row.CacheCreationTokens == 0 {
		fields[15] = "" // eligible_cache_hit_rate
	}
	if row.CacheCreationTokens == 0 {
		fields[12] = "" // cache_creation_5m_share
	}
	return fields
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
	return []string{"bucket", "requests", "real_requests", "hit_requests", "prompt_tokens", "hit_tokens", "cache_input_tokens", "cache_miss_tokens", "cache_creation_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "cache_read_share", "cache_write_share", "eligible_cache_hit_rate", "estimated_requests", "estimated_rate", "cache_creation_reported_requests"}
}

func cacheBucketRows(rows []storage.CacheUsageBucket) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		writeShare := floatString(row.CacheWriteShare)
		eligibleHitRate := floatString(row.EligibleHitRate)
		if row.CacheCreationReportedRequests == 0 {
			writeShare = ""
			eligibleHitRate = ""
		}
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
			writeShare,
			eligibleHitRate,
			itoa64(row.EstimatedRequests),
			floatString(row.EstimatedRate),
			itoa64(row.CacheCreationReportedRequests),
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
