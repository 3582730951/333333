package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

type diagnosticUsageRecord struct {
	ID                                int64
	AccountID                         string
	RouteKeyHash                      string
	APIKeyHash                        string
	UserID                            string
	Model                             string
	PromptTokens                      int64
	CompletionTokens                  int64
	TotalTokens                       int64
	CachedTokens                      int64
	CacheReadTokens                   int64
	CacheCreationTokens               int64
	UsageProvider                     string
	UsageSource                       string
	CacheReadPresent                  int64
	CacheCreationPresent              int64
	CompatibilityLossesJSON           string
	CacheCapability                   string
	Estimated                         int64
	CacheMissTokens                   int64
	CacheTotalInputTokens             int64
	CacheCreation5mTokens             int64
	CacheCreation1hTokens             int64
	AffinitySource                    string
	PromptCacheKeyPresent             int64
	PromptCacheKeySource              string
	StablePrefixSource                string
	StablePrefixReason                string
	StablePrefixBytes                 int64
	RetentionEffective                string
	RetentionSource                   string
	ClaudeCacheTTL                    string
	CacheControlInjected              int64
	CacheBreakpointCount              int64
	CacheBreakpointsJSON              string
	UnwrittenTailTokens               int64
	MaxPossibleCacheReadTokens        int64
	CacheHitAfterPrewarm              int64
	SingleflightWaitedRequests        int64
	DiagnosticsMissReason             string
	LatestUserCacheControl            int64
	LatestUserAutoContextCacheControl int64
	LatestUserTailCacheControl        int64
	LatestUserToolResultCacheControl  int64
	RouteEpoch                        int64
	RawUsageJSON                      string
	CreatedAt                         int64
}

type diagnosticBillingHold struct {
	ID              string
	RouteKeyHash    string
	AccountID       string
	EstimatedTokens int64
	Status          string
	CreatedAt       int64
	UpdatedAt       int64
}

type diagnosticSetting struct {
	Key       string
	Value     string
	UpdatedAt int64
}

type diagnosticLifecycleStatus struct {
	AccountID             string
	ValidityStatus        string
	SubscriptionTier      string
	SubscriptionExpiresAt int64
	LastHealthCheckAt     int64
	LastTokenRefreshAt    int64
	HealthCheckFailCount  int64
	SummaryJSON           string
	CreatedAt             int64
	UpdatedAt             int64
}

type diagnosticAccountIdentity struct {
	Code              string
	AccountID         string
	Email             string
	Label             string
	UpstreamAccountID string
	ChatGPTUserID     string
	Provider          string
	GroupName         string
	Status            string
}

type diagnosticReplacement struct {
	Needle string
	Code   string
}

type diagnosticCodebook struct {
	byID         map[string]diagnosticAccountIdentity
	replacements []diagnosticReplacement
	replacer     *strings.Replacer
}

type diagnosticEventWindow struct {
	Routing409   map[string]int         `json:"routing_409_events"`
	HealthModels map[string]int         `json:"health_test_events_by_model"`
	Banned       map[string]interface{} `json:"banned_accounts"`
	BillingHolds map[string]int         `json:"billing_hold_events_by_status"`
}

func (s *Server) adminDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if err := s.streamDiagnosticsExport(r.Context(), w); err != nil {
		// All database reads and manifest construction happen before headers are
		// committed. A later error means the client receives a truncated ZIP, which
		// is preferable to retaining the entire export in memory just to rewrite an
		// HTTP status after streaming has begun.
		if w.Header().Get("Content-Type") == "" {
			writeError(w, http.StatusInternalServerError, err)
		}
	}
}

func diagnosticFileOrder() []string {
	return []string{
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
}

type diagnosticExportStat struct {
	Rows int64
	Min  int64
	Max  int64
}

func (s *Server) streamDiagnosticsExport(ctx context.Context, w http.ResponseWriter) error {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	bindings, err := s.store.ListEgressBindings(ctx)
	if err != nil {
		return err
	}
	egressProfiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		return err
	}
	tokensByID, err := s.store.ListTokensByAccountIDs(ctx, accountIDsForDiagnostics(accounts))
	if err != nil {
		return err
	}
	capabilities, err := s.store.ListCapabilities(ctx, "")
	if err != nil {
		return err
	}
	kiroCapabilities, err := s.store.ListKiroRuntimeCapabilities(ctx, "")
	if err != nil {
		return err
	}
	rateLimits, err := s.store.ListAccountRateLimits(ctx)
	if err != nil {
		return err
	}
	affinityBindings, err := listDiagnosticAffinityBindings(ctx, s.store.DB())
	if err != nil {
		return err
	}
	settings, err := listDiagnosticSettings(ctx, s.store.DB())
	if err != nil {
		return err
	}
	customProviders, err := s.store.ListCustomProviders(ctx)
	if err != nil {
		return err
	}
	upstreamRules, err := s.store.ListUpstreamErrorRules(ctx)
	if err != nil {
		return err
	}
	resetConsumptions, err := listDiagnosticCodexResetCreditConsumptions(ctx, s.store.DB())
	if err != nil {
		return err
	}
	lifecycleStatuses, err := listDiagnosticLifecycleStatuses(ctx, s.store.DB())
	if err != nil {
		return err
	}
	reauthConfigs, err := listDiagnosticCodexReauthConfigs(ctx, s.store.DB())
	if err != nil {
		return err
	}
	reauthJobs, err := listDiagnosticCodexReauthJobs(ctx, s.store.DB())
	if err != nil {
		return err
	}
	codebook, err := buildStreamingDiagnosticCodebook(ctx, s.store.DB(), accounts, bindings)
	if err != nil {
		return err
	}
	summary := diagnosticSummary(accounts, tokensByID, nil, nil, bindings, rateLimits)
	if err := applyDiagnosticAuditSummary(ctx, s.store.DB(), summary); err != nil {
		return err
	}
	if err := applyDiagnosticBillingHoldSummary(ctx, s.store.DB(), summary); err != nil {
		return err
	}
	stats, err := diagnosticLargeTableStats(ctx, s.store.DB())
	if err != nil {
		return err
	}

	small := map[string]string{}
	rowCounts := map[string]int64{}
	addCSV := func(name string, header []string, rows [][]string) {
		small[name] = csvString(header, rows)
		rowCounts[name] = int64(len(rows))
	}
	addJSON := func(name string, value interface{}) error {
		raw, marshalErr := json.MarshalIndent(value, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		small[name] = string(raw) + "\n"
		rowCounts[name] = 1
		return nil
	}
	addCSV("account_map.csv", []string{"account_code", "account_id"}, accountMapRows(codebook))
	addCSV("account_auth_metadata.csv", []string{"account_code", "declared_provider", "effective_provider", "token_provider_hint", "access_token_present", "access_token_len", "access_token_type", "refresh_token_present", "refresh_token_len", "openai_api_key_present", "openai_api_key_len", "openai_api_key_type", "id_token_present", "id_token_len", "scopes", "expires_at", "last_refresh", "oauth_rate_limit_tier", "created_at", "updated_at"}, accountAuthMetadataRows(accounts, tokensByID, codebook))
	addCSV("account_model_capabilities.csv", []string{"account_code", "model_slug", "native_context_window", "native_max_context_window", "effective_context_window_percent", "auto_compact_token_limit", "visibility", "etag", "raw_model_json_hash", "source", "last_probe_at"}, modelCapabilityRows(capabilities, codebook))
	addCSV("kiro_runtime_capabilities.csv", []string{"account_code", "endpoint_hash", "model", "model_state", "thinking_state", "cache_capability", "observations", "metering_events", "cache_reported_observations", "cache_hit_observations", "consecutive_unreported", "unknown_cache_schema_json", "updated_at", "cache_point_state", "cache_reuse_state", "cache_reuse_evidence", "cache_reuse_credit_reduction_percent", "cache_reuse_probed_at"}, kiroRuntimeCapabilityRows(kiroCapabilities, codebook))
	addCSV("account_rate_limits.csv", []string{"account_code", "provider", "model", "limiter_type", "source", "used_percent", "limit_tokens", "remaining_tokens", "limit_requests", "remaining_requests", "reset_at", "status", "raw_json", "updated_at"}, accountRateLimitRows(rateLimits, codebook))
	addCSV("affinity_bindings.csv", []string{"route_key_hash", "route_key", "source", "account_code", "provider", "model", "egress_id", "epoch", "created_at", "updated_at"}, affinityBindingRows(affinityBindings, codebook))
	addCSV("settings.csv", []string{"key", "value", "updated_at"}, settingRows(settings))
	addCSV("custom_providers.csv", []string{"id", "name", "base_url", "upstream_protocol", "enabled", "auto_discover_models", "models", "created_at", "updated_at"}, customProviderRows(customProviders))
	addCSV("upstream_error_rules.csv", []string{"id", "name", "enabled", "priority", "providers", "entrypoints", "model_patterns", "status_codes", "body_keywords", "match_mode", "account_action", "downstream_action", "response_status", "custom_message", "cooldown_seconds", "prefer_retry_after", "idle_seconds", "idle_ping_seconds", "skip_log", "description", "created_at", "updated_at"}, upstreamErrorRuleRows(upstreamRules))
	addCSV("codex_reset_credit_consumptions.csv", []string{"account_code", "seven_day_reset_at", "redeem_request_id", "status", "created_at", "updated_at"}, codexResetCreditRows(resetConsumptions, codebook))
	addCSV("account_lifecycle_status.csv", []string{"account_code", "validity_status", "subscription_tier", "subscription_expires_at", "last_health_check_at", "last_token_refresh_at", "health_check_fail_count", "summary_json", "created_at", "updated_at"}, lifecycleStatusRows(lifecycleStatuses, codebook))
	addCSV("codex_reauth_config.csv", []string{"account_code", "login_email_present", "password_configured", "otp_url_configured", "target_workspace_id", "auto_enabled", "last_status", "last_error", "created_at", "updated_at"}, codexReauthConfigRows(reauthConfigs, codebook))
	addCSV("codex_reauth_jobs.csv", []string{"id", "account_code", "status", "reason", "last_error", "created_at", "updated_at", "started_at", "finished_at"}, codexReauthJobRows(reauthJobs, codebook))
	addCSV("accounts_snapshot.csv", []string{"account_code", "group_name", "declared_provider", "effective_provider", "status", "plan_type", "is_fedramp", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, tokensByID, bindings, codebook))
	addCSV("egress_snapshot.csv", []string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
	if err := addJSON("diagnostic_summary.json", summary); err != nil {
		return err
	}
	for name, stat := range stats {
		rowCounts[name] = stat.Rows
	}
	rowCounts["manifest.json"] = 1
	timeRanges := map[string]interface{}{}
	for name, stat := range stats {
		timeRanges[name] = map[string]int64{"min_created_at": stat.Min, "max_created_at": stat.Max}
	}
	addRange := func(name string, values []int64) {
		var minValue, maxValue int64
		for _, value := range values {
			if value <= 0 {
				continue
			}
			if minValue == 0 || value < minValue {
				minValue = value
			}
			if value > maxValue {
				maxValue = value
			}
		}
		timeRanges[name] = map[string]int64{"min_created_at": minValue, "max_created_at": maxValue}
	}
	accountTimes := make([]int64, 0, len(accounts))
	for _, row := range accounts {
		accountTimes = append(accountTimes, row.CreatedAt)
	}
	addRange("account_map.csv", accountTimes)
	addRange("accounts_snapshot.csv", accountTimes)
	tokenTimes := make([]int64, 0, len(tokensByID))
	for _, row := range tokensByID {
		tokenTimes = append(tokenTimes, row.CreatedAt)
	}
	addRange("account_auth_metadata.csv", tokenTimes)
	modelTimes := make([]int64, 0, len(capabilities))
	for _, row := range capabilities {
		modelTimes = append(modelTimes, row.LastProbeAt)
	}
	addRange("account_model_capabilities.csv", modelTimes)
	kiroTimes := make([]int64, 0, len(kiroCapabilities))
	for _, row := range kiroCapabilities {
		kiroTimes = append(kiroTimes, row.UpdatedAt)
	}
	addRange("kiro_runtime_capabilities.csv", kiroTimes)
	rateTimes := make([]int64, 0, len(rateLimits))
	for _, row := range rateLimits {
		rateTimes = append(rateTimes, row.UpdatedAt)
	}
	addRange("account_rate_limits.csv", rateTimes)
	affinityTimes := make([]int64, 0, len(affinityBindings))
	for _, row := range affinityBindings {
		affinityTimes = append(affinityTimes, row.CreatedAt)
	}
	addRange("affinity_bindings.csv", affinityTimes)
	settingTimes := make([]int64, 0, len(settings))
	for _, row := range settings {
		settingTimes = append(settingTimes, row.UpdatedAt)
	}
	addRange("settings.csv", settingTimes)
	providerTimes := make([]int64, 0, len(customProviders))
	for _, row := range customProviders {
		providerTimes = append(providerTimes, row.CreatedAt)
	}
	addRange("custom_providers.csv", providerTimes)
	ruleTimes := make([]int64, 0, len(upstreamRules))
	for _, row := range upstreamRules {
		ruleTimes = append(ruleTimes, row.CreatedAt)
	}
	addRange("upstream_error_rules.csv", ruleTimes)
	resetTimes := make([]int64, 0, len(resetConsumptions))
	for _, row := range resetConsumptions {
		resetTimes = append(resetTimes, row.CreatedAt)
	}
	addRange("codex_reset_credit_consumptions.csv", resetTimes)
	lifecycleTimes := make([]int64, 0, len(lifecycleStatuses))
	for _, row := range lifecycleStatuses {
		lifecycleTimes = append(lifecycleTimes, row.CreatedAt)
	}
	addRange("account_lifecycle_status.csv", lifecycleTimes)
	reauthConfigTimes := make([]int64, 0, len(reauthConfigs))
	for _, row := range reauthConfigs {
		reauthConfigTimes = append(reauthConfigTimes, row.CreatedAt)
	}
	addRange("codex_reauth_config.csv", reauthConfigTimes)
	reauthJobTimes := make([]int64, 0, len(reauthJobs))
	for _, row := range reauthJobs {
		reauthJobTimes = append(reauthJobTimes, row.CreatedAt)
	}
	addRange("codex_reauth_jobs.csv", reauthJobTimes)
	egressTimes := make([]int64, 0, len(egressProfiles))
	for _, row := range egressProfiles {
		egressTimes = append(egressTimes, row.CreatedAt)
	}
	addRange("egress_snapshot.csv", egressTimes)
	manifest := map[string]interface{}{
		"generated_at": time.Now().Unix(), "format": "codex-pool-diagnostics-v2",
		"account_count": len(accounts), "current_account_count": len(accounts),
		"historical_reference_account_count": len(codebook.byID),
		"files":                              diagnosticFileOrder(), "row_counts": rowCounts,
		"build": diagnosticBuildInfo(), "table_time_ranges": timeRanges,
		"account_redaction":   "business files use account_code; account_map.csv preserves the complete local account id mapping; route/session hashes, user ids, exit IPs and correlation fields remain complete",
		"account_code_format": "ACC-0001",
	}
	if err := addJSON("manifest.json", manifest); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics-v2.zip"`)
	zw := zip.NewWriter(w)
	for _, name := range diagnosticFileOrder() {
		switch name {
		case "audit_log.csv":
			err = streamDiagnosticAuditCSV(ctx, s.store.DB(), zw, codebook)
		case "cf_events.csv":
			err = streamDiagnosticCFCSV(ctx, s.store.DB(), zw, codebook)
		case "usage_records.csv":
			err = streamDiagnosticUsageCSV(ctx, s.store.DB(), zw, codebook)
		case "billing_holds.csv":
			err = streamDiagnosticBillingHoldsCSV(ctx, s.store.DB(), zw, codebook)
		default:
			content, ok := small[name]
			if !ok {
				continue
			}
			var writer io.Writer
			writer, err = zw.Create(name)
			if err == nil {
				_, err = io.WriteString(writer, content)
			}
		}
		if err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func buildStreamingDiagnosticCodebook(ctx context.Context, db *sql.DB, accounts []storage.Account, bindings []storage.AccountEgressBinding) (diagnosticCodebook, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_id, MAX(account_label) FROM audit_log WHERE account_id <> '' GROUP BY account_id
UNION SELECT account_id, '' FROM cf_events WHERE account_id <> ''
UNION SELECT account_id, '' FROM usage_records WHERE account_id <> ''
UNION SELECT account_id, '' FROM billing_holds WHERE account_id <> ''
UNION SELECT account_id, '' FROM affinity_bindings WHERE account_id <> ''`)
	if err != nil {
		return diagnosticCodebook{}, err
	}
	defer rows.Close()
	historical := []storage.AuditLogRow{}
	for rows.Next() {
		var accountID, label string
		if err := rows.Scan(&accountID, &label); err != nil {
			return diagnosticCodebook{}, err
		}
		historical = append(historical, storage.AuditLogRow{AccountID: accountID, AccountLabel: label})
	}
	if err := rows.Err(); err != nil {
		return diagnosticCodebook{}, err
	}
	return buildDiagnosticCodebook(accounts, historical, nil, nil, nil, bindings), nil
}

func applyDiagnosticAuditSummary(ctx context.Context, db *sql.DB, summary map[string]interface{}) error {
	cutoff := storage.Now() - 24*60*60
	lifetime, _ := summary["lifetime"].(diagnosticEventWindow)
	last24h, _ := summary["last_24h"].(diagnosticEventWindow)
	lifetime.Routing409, last24h.Routing409 = map[string]int{}, map[string]int{}
	lifetime.HealthModels, last24h.HealthModels = map[string]int{}, map[string]int{}

	rows, err := db.QueryContext(ctx, `SELECT
CASE
  WHEN lower(detail) LIKE '%token budget%' THEN 'token_budget_exceeded'
  WHEN lower(detail) LIKE '%pending health re-check%' OR lower(detail) LIKE '%recheck%' THEN 'pending_health_recheck'
  WHEN lower(detail) LIKE '%rate-limit cooldown%' OR lower(detail) LIKE '%cooldown%' THEN 'rate_limit_cooldown'
  WHEN lower(detail) LIKE '%quarantined%' THEN 'quarantined'
  ELSE 'other'
END AS category,
COUNT(*), COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0)
FROM audit_log
WHERE action='routing_unavailable' AND detail LIKE '%status=409%'
GROUP BY category`, cutoff)
	if err != nil {
		return err
	}
	for rows.Next() {
		var category string
		var total, recent int
		if err := rows.Scan(&category, &total, &recent); err != nil {
			rows.Close()
			return err
		}
		lifetime.Routing409[category] = total
		if recent > 0 {
			last24h.Routing409[category] = recent
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rows, err = db.QueryContext(ctx, `SELECT detail, COUNT(*),
COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0)
FROM audit_log
WHERE action IN ('health_test','health_test_model_unsupported') AND detail LIKE '%model=%'
GROUP BY detail`, cutoff)
	if err != nil {
		return err
	}
	for rows.Next() {
		var detail string
		var total, recent int
		if err := rows.Scan(&detail, &total, &recent); err != nil {
			rows.Close()
			return err
		}
		model := extractDiagnosticDetailValue(detail, "model")
		if model == "" {
			continue
		}
		lifetime.HealthModels[model] += total
		if recent > 0 {
			last24h.HealthModels[model] += recent
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	banEvents := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
	banUnique := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
	recentBanEvents := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
	recentBanUnique := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
	rows, err = db.QueryContext(ctx, `SELECT
CASE WHEN action='ban_delete' THEN 'deleted'
     WHEN action='ban_delete_failed' THEN 'delete_failed'
     ELSE 'discovered' END AS category,
COUNT(*), COUNT(DISTINCT CASE WHEN account_id <> '' THEN account_id END),
COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0),
COUNT(DISTINCT CASE WHEN created_at >= ? AND account_id <> '' THEN account_id END)
FROM audit_log
WHERE state='banned' OR action IN ('ban_delete','ban_delete_failed')
GROUP BY category`, cutoff, cutoff)
	if err != nil {
		return err
	}
	for rows.Next() {
		var category string
		var total, unique, recent, recentUnique int
		if err := rows.Scan(&category, &total, &unique, &recent, &recentUnique); err != nil {
			rows.Close()
			return err
		}
		banEvents[category], banUnique[category] = total, unique
		recentBanEvents[category], recentBanUnique[category] = recent, recentUnique
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	lifetime.Banned = map[string]interface{}{"events": banEvents, "unique_accounts": banUnique}
	last24h.Banned = map[string]interface{}{"events": recentBanEvents, "unique_accounts": recentBanUnique}
	summary["lifetime"], summary["last_24h"] = lifetime, last24h
	summary["routing_409"] = lifetime.Routing409
	summary["health_test_models"] = lifetime.HealthModels
	summary["banned_accounts"] = lifetime.Banned
	return nil
}

func applyDiagnosticBillingHoldSummary(ctx context.Context, db *sql.DB, summary map[string]interface{}) error {
	now := storage.Now()
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*),
SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),
SUM(CASE WHEN status='held' AND created_at >= ? THEN 1 ELSE 0 END),
SUM(CASE WHEN status='held' AND created_at < ? THEN 1 ELSE 0 END),
MIN(CASE WHEN status='held' AND created_at >= ? THEN created_at END)
FROM billing_holds GROUP BY status`, now-24*60*60, now-60*60, now-60*60, now-60*60)
	if err != nil {
		return err
	}
	defer rows.Close()
	lifetime, last24h := map[string]int{}, map[string]int{}
	fresh, stale, expired, oldestFresh := int64(0), int64(0), int64(0), int64(0)
	for rows.Next() {
		var status string
		var total, day, freshForStatus, staleForStatus int64
		var oldest sql.NullInt64
		if err := rows.Scan(&status, &total, &day, &freshForStatus, &staleForStatus, &oldest); err != nil {
			return err
		}
		lifetime[firstNonEmpty(status, "unknown")] = int(total)
		last24h[firstNonEmpty(status, "unknown")] = int(day)
		fresh += freshForStatus
		stale += staleForStatus
		if status == "expired_unsettled" {
			expired = total
		}
		if oldest.Valid && now-oldest.Int64 > oldestFresh {
			oldestFresh = now - oldest.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if window, ok := summary["lifetime"].(diagnosticEventWindow); ok {
		window.BillingHolds = lifetime
		summary["lifetime"] = window
	}
	if window, ok := summary["last_24h"].(diagnosticEventWindow); ok {
		window.BillingHolds = last24h
		summary["last_24h"] = window
	}
	summary["billing_holds"] = map[string]interface{}{
		"current_fresh_held": fresh, "historical_stale_held": stale,
		"expired_unsettled": expired, "oldest_fresh_held_age_seconds": oldestFresh,
	}
	if current, ok := summary["current_state"].(map[string]interface{}); ok {
		current["fresh_billing_holds"] = fresh
		current["stale_historical_holds"] = stale
	}
	return nil
}

func diagnosticLargeTableStats(ctx context.Context, db *sql.DB) (map[string]diagnosticExportStat, error) {
	tables := map[string]string{
		"audit_log.csv": "audit_log", "cf_events.csv": "cf_events",
		"usage_records.csv": "usage_records", "billing_holds.csv": "billing_holds",
	}
	out := map[string]diagnosticExportStat{}
	for file, table := range tables {
		var stat diagnosticExportStat
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM `+table).Scan(&stat.Rows, &stat.Min, &stat.Max); err != nil {
			return nil, err
		}
		out[file] = stat
	}
	return out, nil
}

func streamDiagnosticCSV(zw *zip.Writer, name string, header []string, writeRows func(*csv.Writer) error) error {
	writer, err := zw.Create(name)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(writer)
	if err := cw.Write(header); err != nil {
		return err
	}
	if err := writeRows(cw); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func streamDiagnosticAuditCSV(ctx context.Context, db *sql.DB, zw *zip.Writer, codebook diagnosticCodebook) error {
	return streamDiagnosticCSV(zw, "audit_log.csv", []string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, func(cw *csv.Writer) error {
		rows, err := db.QueryContext(ctx, `SELECT id, account_id, account_label, action, state, reason, detail, created_at FROM audit_log ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row storage.AuditLogRow
			if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountLabel, &row.Action, &row.State, &row.Reason, &row.Detail, &row.CreatedAt); err != nil {
				return err
			}
			if err := cw.Write(auditLogRows([]storage.AuditLogRow{row}, codebook)[0]); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

func streamDiagnosticCFCSV(ctx context.Context, db *sql.DB, zw *zip.Writer, codebook diagnosticCodebook) error {
	return streamDiagnosticCSV(zw, "cf_events.csv", []string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, func(cw *csv.Writer) error {
		rows, err := db.QueryContext(ctx, `SELECT id, account_id, egress_id, status, cf_ray, category, message, created_at FROM cf_events ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row storage.CFEvent
			if err := rows.Scan(&row.ID, &row.AccountID, &row.EgressID, &row.Status, &row.CFRay, &row.Category, &row.Message, &row.CreatedAt); err != nil {
				return err
			}
			if err := cw.Write(cfEventRows([]storage.CFEvent{row}, codebook)[0]); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

func streamDiagnosticUsageCSV(ctx context.Context, db *sql.DB, zw *zip.Writer, codebook diagnosticCodebook) error {
	header := []string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "usage_provider", "usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "estimated", "cache_miss_tokens", "cache_total_input_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "affinity_source", "route_class", "prompt_cache_key_present", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch", "raw_usage_json"}
	return streamDiagnosticCSV(zw, "usage_records.csv", header, func(cw *csv.Writer) error {
		rows, err := db.QueryContext(ctx, diagnosticUsageRecordSelectSQL()+` ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row diagnosticUsageRecord
			if err := scanDiagnosticUsageRecord(rows, &row); err != nil {
				return err
			}
			if err := cw.Write(usageRecordRows([]diagnosticUsageRecord{row}, codebook)[0]); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

func streamDiagnosticBillingHoldsCSV(ctx context.Context, db *sql.DB, zw *zip.Writer, codebook diagnosticCodebook) error {
	return streamDiagnosticCSV(zw, "billing_holds.csv", []string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status"}, func(cw *csv.Writer) error {
		rows, err := db.QueryContext(ctx, `SELECT id, route_key_hash, account_id, estimated_tokens, status, created_at, updated_at FROM billing_holds ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row diagnosticBillingHold
			if err := rows.Scan(&row.ID, &row.RouteKeyHash, &row.AccountID, &row.EstimatedTokens, &row.Status, &row.CreatedAt, &row.UpdatedAt); err != nil {
				return err
			}
			if err := cw.Write(billingHoldRows([]diagnosticBillingHold{row}, codebook)[0]); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

func buildDiagnosticsZipFiles(accounts []storage.Account, tokensByID map[string]storage.AccountToken, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding, egressProfiles []storage.EgressProfile, capabilities []storage.ModelCapability, kiroRuntimeCapabilities []storage.KiroRuntimeCapability, rateLimits []storage.AccountRateLimit, affinityBindings []storage.AffinityBinding, settings []diagnosticSetting, customProviders []storage.CustomProvider, upstreamRules []storage.UpstreamErrorRule, resetConsumptions []storage.CodexResetCreditConsumption, lifecycleStatuses []diagnosticLifecycleStatus, reauthConfigs []storage.AccountCodexReauthConfig, reauthJobs []storage.AccountCodexReauthJob, codebook diagnosticCodebook) (map[string]string, error) {
	files := map[string]string{}
	rowCounts := map[string]int{}
	addCSV := func(name string, header []string, rows [][]string) {
		files[name] = csvString(header, rows)
		rowCounts[name] = len(rows)
	}
	addJSON := func(name string, value interface{}) error {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		files[name] = string(raw) + "\n"
		rowCounts[name] = 1
		return nil
	}

	addCSV("account_map.csv", []string{"account_code", "account_id"}, accountMapRows(codebook))
	addCSV("account_auth_metadata.csv", []string{"account_code", "declared_provider", "effective_provider", "token_provider_hint", "access_token_present", "access_token_len", "access_token_type", "refresh_token_present", "refresh_token_len", "openai_api_key_present", "openai_api_key_len", "openai_api_key_type", "id_token_present", "id_token_len", "scopes", "expires_at", "last_refresh", "oauth_rate_limit_tier", "created_at", "updated_at"}, accountAuthMetadataRows(accounts, tokensByID, codebook))
	addCSV("account_model_capabilities.csv", []string{"account_code", "model_slug", "native_context_window", "native_max_context_window", "effective_context_window_percent", "auto_compact_token_limit", "visibility", "etag", "raw_model_json_hash", "source", "last_probe_at"}, modelCapabilityRows(capabilities, codebook))
	addCSV("kiro_runtime_capabilities.csv", []string{"account_code", "endpoint_hash", "model", "model_state", "thinking_state", "cache_capability", "observations", "metering_events", "cache_reported_observations", "cache_hit_observations", "consecutive_unreported", "unknown_cache_schema_json", "updated_at", "cache_point_state", "cache_reuse_state", "cache_reuse_evidence", "cache_reuse_credit_reduction_percent", "cache_reuse_probed_at"}, kiroRuntimeCapabilityRows(kiroRuntimeCapabilities, codebook))
	addCSV("account_rate_limits.csv", []string{"account_code", "provider", "model", "limiter_type", "source", "used_percent", "limit_tokens", "remaining_tokens", "limit_requests", "remaining_requests", "reset_at", "status", "raw_json", "updated_at"}, accountRateLimitRows(rateLimits, codebook))
	addCSV("affinity_bindings.csv", []string{"route_key_hash", "route_key", "source", "account_code", "provider", "model", "egress_id", "epoch", "created_at", "updated_at"}, affinityBindingRows(affinityBindings, codebook))
	addCSV("settings.csv", []string{"key", "value", "updated_at"}, settingRows(settings))
	addCSV("custom_providers.csv", []string{"id", "name", "base_url", "upstream_protocol", "enabled", "auto_discover_models", "models", "created_at", "updated_at"}, customProviderRows(customProviders))
	addCSV("upstream_error_rules.csv", []string{"id", "name", "enabled", "priority", "providers", "entrypoints", "model_patterns", "status_codes", "body_keywords", "match_mode", "account_action", "downstream_action", "response_status", "custom_message", "cooldown_seconds", "prefer_retry_after", "idle_seconds", "idle_ping_seconds", "skip_log", "description", "created_at", "updated_at"}, upstreamErrorRuleRows(upstreamRules))
	addCSV("codex_reset_credit_consumptions.csv", []string{"account_code", "seven_day_reset_at", "redeem_request_id", "status", "created_at", "updated_at"}, codexResetCreditRows(resetConsumptions, codebook))
	addCSV("account_lifecycle_status.csv", []string{"account_code", "validity_status", "subscription_tier", "subscription_expires_at", "last_health_check_at", "last_token_refresh_at", "health_check_fail_count", "summary_json", "created_at", "updated_at"}, lifecycleStatusRows(lifecycleStatuses, codebook))
	addCSV("codex_reauth_config.csv", []string{"account_code", "login_email_present", "password_configured", "otp_url_configured", "target_workspace_id", "auto_enabled", "last_status", "last_error", "created_at", "updated_at"}, codexReauthConfigRows(reauthConfigs, codebook))
	addCSV("codex_reauth_jobs.csv", []string{"id", "account_code", "status", "reason", "last_error", "created_at", "updated_at", "started_at", "finished_at"}, codexReauthJobRows(reauthJobs, codebook))
	addCSV("audit_log.csv", []string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, auditLogRows(auditRows, codebook))
	addCSV("cf_events.csv", []string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, cfEventRows(cfRows, codebook))
	addCSV("usage_records.csv", []string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "usage_provider", "usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "estimated", "cache_miss_tokens", "cache_total_input_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "affinity_source", "route_class", "prompt_cache_key_present", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch", "raw_usage_json"}, usageRecordRows(usageRows, codebook))
	addCSV("billing_holds.csv", []string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status"}, billingHoldRows(holds, codebook))
	addCSV("accounts_snapshot.csv", []string{"account_code", "group_name", "declared_provider", "effective_provider", "status", "plan_type", "is_fedramp", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, tokensByID, bindings, codebook))
	addCSV("egress_snapshot.csv", []string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
	if err := addJSON("diagnostic_summary.json", diagnosticSummary(accounts, tokensByID, auditRows, holds, bindings, rateLimits)); err != nil {
		return nil, err
	}
	rowCounts["manifest.json"] = 1
	manifest := map[string]interface{}{
		"generated_at":                       time.Now().Unix(),
		"format":                             "codex-pool-diagnostics-v2",
		"account_count":                      len(accounts),
		"current_account_count":              len(accounts),
		"historical_reference_account_count": len(codebook.byID),
		"build":                              diagnosticBuildInfo(),
		"table_time_ranges":                  diagnosticTableTimeRanges(accounts, auditRows, cfRows, usageRows, holds),
		"files":                              diagnosticFileOrder(),
		"row_counts":                         rowCounts,
		"account_redaction":                  "business files use account_code; account_map.csv contains only account_code and local account_id; email, label, and upstream identity columns are omitted",
		"account_code_format":                "ACC-0001",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = string(rawManifest) + "\n"
	return files, nil
}

func diagnosticBuildInfo() map[string]interface{} {
	out := map[string]interface{}{"version": "devel", "revision": "", "modified": false}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	if strings.TrimSpace(info.Main.Version) != "" {
		out["version"] = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			out["revision"] = setting.Value
		case "vcs.time":
			out["vcs_time"] = setting.Value
		case "vcs.modified":
			out["modified"] = setting.Value == "true"
		}
	}
	return out
}

func diagnosticTableTimeRanges(accounts []storage.Account, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold) map[string]interface{} {
	type bounds struct{ min, max int64 }
	add := func(values []int64) map[string]int64 {
		b := bounds{}
		for _, value := range values {
			if value <= 0 {
				continue
			}
			if b.min == 0 || value < b.min {
				b.min = value
			}
			if value > b.max {
				b.max = value
			}
		}
		return map[string]int64{"min_created_at": b.min, "max_created_at": b.max}
	}
	accountTimes := make([]int64, 0, len(accounts))
	for _, row := range accounts {
		accountTimes = append(accountTimes, row.CreatedAt)
	}
	auditTimes := make([]int64, 0, len(auditRows))
	for _, row := range auditRows {
		auditTimes = append(auditTimes, row.CreatedAt)
	}
	cfTimes := make([]int64, 0, len(cfRows))
	for _, row := range cfRows {
		cfTimes = append(cfTimes, row.CreatedAt)
	}
	usageTimes := make([]int64, 0, len(usageRows))
	for _, row := range usageRows {
		usageTimes = append(usageTimes, row.CreatedAt)
	}
	holdTimes := make([]int64, 0, len(holds))
	for _, row := range holds {
		holdTimes = append(holdTimes, row.CreatedAt)
	}
	return map[string]interface{}{
		"accounts_snapshot.csv": add(accountTimes),
		"audit_log.csv":         add(auditTimes),
		"cf_events.csv":         add(cfTimes),
		"usage_records.csv":     add(usageTimes),
		"billing_holds.csv":     add(holdTimes),
	}
}

func listDiagnosticAuditRows(ctx context.Context, db *sql.DB) ([]storage.AuditLogRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, account_label, action, state, reason, detail, created_at FROM audit_log ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AuditLogRow
	for rows.Next() {
		var r storage.AuditLogRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.AccountLabel, &r.Action, &r.State, &r.Reason, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listDiagnosticCFEvents(ctx context.Context, db *sql.DB) ([]storage.CFEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, egress_id, status, cf_ray, category, message, created_at FROM cf_events ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.CFEvent
	for rows.Next() {
		var e storage.CFEvent
		if err := rows.Scan(&e.ID, &e.AccountID, &e.EgressID, &e.Status, &e.CFRay, &e.Category, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func listDiagnosticUsageRecords(ctx context.Context, db *sql.DB) ([]diagnosticUsageRecord, error) {
	rows, err := db.QueryContext(ctx, diagnosticUsageRecordSelectSQL()+` ORDER BY id ASC`)
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
		out = append(out, r)
	}
	return out, rows.Err()
}

func diagnosticUsageRecordSelectSQL() string {
	return `SELECT id, account_id, route_key_hash, api_key_hash, user_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
usage_provider, usage_source, cache_read_present, cache_creation_present, compatibility_losses_json, cache_capability,
estimated, cache_miss_tokens, cache_total_input_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens,
affinity_source, prompt_cache_key_present, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, stable_prefix_bytes,
retention_effective, retention_source, claude_cache_ttl, cache_control_injected, cache_breakpoint_count,
cache_breakpoints_json, unwritten_tail_tokens, max_possible_cache_read_tokens, cache_hit_after_prewarm, singleflight_waited_requests, diagnostics_miss_reason,
latest_user_cache_control, latest_user_auto_context_cache_control, latest_user_tail_cache_control, latest_user_tool_result_cache_control, route_epoch,
raw_usage_json, created_at FROM usage_records`
}

type usageRecordScanner interface {
	Scan(dest ...interface{}) error
}

func scanDiagnosticUsageRecord(rows usageRecordScanner, r *diagnosticUsageRecord) error {
	return rows.Scan(&r.ID, &r.AccountID, &r.RouteKeyHash, &r.APIKeyHash, &r.UserID, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.UsageProvider, &r.UsageSource, &r.CacheReadPresent, &r.CacheCreationPresent, &r.CompatibilityLossesJSON, &r.CacheCapability,
		&r.Estimated, &r.CacheMissTokens, &r.CacheTotalInputTokens, &r.CacheCreation5mTokens, &r.CacheCreation1hTokens,
		&r.AffinitySource, &r.PromptCacheKeyPresent, &r.PromptCacheKeySource, &r.StablePrefixSource, &r.StablePrefixReason, &r.StablePrefixBytes,
		&r.RetentionEffective, &r.RetentionSource, &r.ClaudeCacheTTL, &r.CacheControlInjected, &r.CacheBreakpointCount,
		&r.CacheBreakpointsJSON, &r.UnwrittenTailTokens, &r.MaxPossibleCacheReadTokens, &r.CacheHitAfterPrewarm, &r.SingleflightWaitedRequests, &r.DiagnosticsMissReason,
		&r.LatestUserCacheControl, &r.LatestUserAutoContextCacheControl, &r.LatestUserTailCacheControl, &r.LatestUserToolResultCacheControl, &r.RouteEpoch,
		&r.RawUsageJSON, &r.CreatedAt)
}

func listDiagnosticBillingHolds(ctx context.Context, db *sql.DB) ([]diagnosticBillingHold, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, route_key_hash, account_id, estimated_tokens, status, created_at, updated_at FROM billing_holds ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticBillingHold
	for rows.Next() {
		var h diagnosticBillingHold
		if err := rows.Scan(&h.ID, &h.RouteKeyHash, &h.AccountID, &h.EstimatedTokens, &h.Status, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func listDiagnosticAffinityBindings(ctx context.Context, db *sql.DB) ([]storage.AffinityBinding, error) {
	rows, err := db.QueryContext(ctx, `SELECT route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at FROM affinity_bindings ORDER BY updated_at ASC, route_key_hash ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AffinityBinding
	for rows.Next() {
		var b storage.AffinityBinding
		if err := rows.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Provider, &b.Model, &b.EgressID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func listDiagnosticSettings(ctx context.Context, db *sql.DB) ([]diagnosticSetting, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value, updated_at FROM settings ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticSetting
	for rows.Next() {
		var s diagnosticSetting
		if err := rows.Scan(&s.Key, &s.Value, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func listDiagnosticCodexResetCreditConsumptions(ctx context.Context, db *sql.DB) ([]storage.CodexResetCreditConsumption, error) {
	rows, err := db.QueryContext(ctx, `SELECT account_id, seven_day_reset_at, redeem_request_id, status, created_at, updated_at FROM codex_reset_credit_consumptions ORDER BY updated_at ASC, account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.CodexResetCreditConsumption
	for rows.Next() {
		var r storage.CodexResetCreditConsumption
		if err := rows.Scan(&r.AccountID, &r.SevenDayResetAt, &r.RedeemRequestID, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listDiagnosticLifecycleStatuses(ctx context.Context, db *sql.DB) ([]diagnosticLifecycleStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT account_id, validity_status, subscription_tier, subscription_expires_at, last_health_check_at, last_token_refresh_at, health_check_fail_count, summary_json, created_at, updated_at FROM account_lifecycle_status ORDER BY account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticLifecycleStatus
	for rows.Next() {
		var r diagnosticLifecycleStatus
		if err := rows.Scan(&r.AccountID, &r.ValidityStatus, &r.SubscriptionTier, &r.SubscriptionExpiresAt, &r.LastHealthCheckAt, &r.LastTokenRefreshAt, &r.HealthCheckFailCount, &r.SummaryJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listDiagnosticCodexReauthConfigs(ctx context.Context, db *sql.DB) ([]storage.AccountCodexReauthConfig, error) {
	rows, err := db.QueryContext(ctx, `SELECT account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at FROM account_codex_reauth_config ORDER BY account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AccountCodexReauthConfig
	for rows.Next() {
		var cfg storage.AccountCodexReauthConfig
		var auto int
		var password, otp string
		if err := rows.Scan(&cfg.AccountID, &cfg.LoginEmail, &password, &otp, &cfg.TargetWorkspaceID, &auto, &cfg.LastStatus, &cfg.LastError, &cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
			return nil, err
		}
		cfg.AutoEnabled = auto != 0
		cfg.PasswordConfigured = strings.TrimSpace(password) != ""
		cfg.OTPURLConfigured = strings.TrimSpace(otp) != ""
		out = append(out, cfg)
	}
	return out, rows.Err()
}

func listDiagnosticCodexReauthJobs(ctx context.Context, db *sql.DB) ([]storage.AccountCodexReauthJob, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at FROM account_codex_reauth_jobs ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AccountCodexReauthJob
	for rows.Next() {
		var job storage.AccountCodexReauthJob
		if err := rows.Scan(&job.ID, &job.AccountID, &job.Status, &job.Reason, &job.LastError, &job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func buildDiagnosticCodebook(accounts []storage.Account, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding) diagnosticCodebook {
	identities := map[string]diagnosticAccountIdentity{}
	ensure := func(accountID string) diagnosticAccountIdentity {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			return diagnosticAccountIdentity{}
		}
		if info, ok := identities[accountID]; ok {
			return info
		}
		info := diagnosticAccountIdentity{AccountID: accountID}
		identities[accountID] = info
		return info
	}
	for _, a := range accounts {
		if strings.TrimSpace(a.ID) == "" {
			continue
		}
		identities[a.ID] = diagnosticAccountIdentity{
			AccountID:         a.ID,
			Email:             a.Email,
			Label:             a.Label,
			UpstreamAccountID: a.UpstreamAccountID,
			ChatGPTUserID:     a.ChatGPTUserID,
			Provider:          a.Provider,
			GroupName:         a.GroupName,
			Status:            a.Status,
		}
	}
	for _, row := range auditRows {
		info := ensure(row.AccountID)
		if info.AccountID != "" && info.Label == "" {
			info.Label = row.AccountLabel
			identities[info.AccountID] = info
		}
	}
	for _, row := range cfRows {
		ensure(row.AccountID)
	}
	for _, row := range usageRows {
		ensure(row.AccountID)
	}
	for _, row := range holds {
		ensure(row.AccountID)
	}
	for _, row := range bindings {
		ensure(row.AccountID)
	}
	ids := make([]string, 0, len(identities))
	for id := range identities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seenNeedle := map[string]bool{}
	replacements := []diagnosticReplacement{}
	for i, id := range ids {
		info := identities[id]
		info.Code = fmt.Sprintf("ACC-%04d", i+1)
		identities[id] = info
		for _, needle := range []string{info.AccountID, info.Email, info.Label, info.UpstreamAccountID, info.ChatGPTUserID} {
			needle = strings.TrimSpace(needle)
			if needle == "" || seenNeedle[needle] {
				continue
			}
			seenNeedle[needle] = true
			replacements = append(replacements, diagnosticReplacement{Needle: needle, Code: info.Code})
		}
	}
	sort.Slice(replacements, func(i, j int) bool {
		return len(replacements[i].Needle) > len(replacements[j].Needle)
	})
	oldnew := make([]string, 0, len(replacements)*2)
	for _, repl := range replacements {
		oldnew = append(oldnew, repl.Needle, repl.Code)
	}
	var replacer *strings.Replacer
	if len(oldnew) > 0 {
		replacer = strings.NewReplacer(oldnew...)
	}
	return diagnosticCodebook{byID: identities, replacements: replacements, replacer: replacer}
}

func (b diagnosticCodebook) code(accountID string) string {
	if info, ok := b.byID[accountID]; ok {
		return info.Code
	}
	return ""
}

func (b diagnosticCodebook) sanitize(text string) string {
	if b.replacer == nil {
		return text
	}
	return b.replacer.Replace(text)
}

func accountMapRows(codebook diagnosticCodebook) [][]string {
	ids := make([]string, 0, len(codebook.byID))
	for id := range codebook.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([][]string, 0, len(ids))
	for _, id := range ids {
		info := codebook.byID[id]
		out = append(out, []string{info.Code, info.AccountID})
	}
	return out
}

func accountIDsForDiagnostics(accounts []storage.Account) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if strings.TrimSpace(account.ID) != "" {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

func accountAuthMetadataRows(accounts []storage.Account, tokensByID map[string]storage.AccountToken, codebook diagnosticCodebook) [][]string {
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	out := make([][]string, 0, len(accounts))
	for _, account := range accounts {
		token, found := tokensByID[account.ID]
		out = append(out, []string{
			codebook.code(account.ID),
			account.Provider,
			accountprovider.EffectiveProvider(account.Provider, token, found),
			accountprovider.InferProviderFromToken(token),
			strconv.FormatBool(strings.TrimSpace(token.AccessToken) != ""),
			strconv.Itoa(len(token.AccessToken)),
			tokenSecretType(token.AccessToken),
			strconv.FormatBool(strings.TrimSpace(token.RefreshToken) != ""),
			strconv.Itoa(len(token.RefreshToken)),
			strconv.FormatBool(strings.TrimSpace(token.OpenAIAPIKey) != ""),
			strconv.Itoa(len(token.OpenAIAPIKey)),
			tokenSecretType(token.OpenAIAPIKey),
			strconv.FormatBool(strings.TrimSpace(token.IDTokenRaw) != ""),
			strconv.Itoa(len(token.IDTokenRaw)),
			token.Scopes,
			itoa64(token.ExpiresAt),
			itoa64(token.LastRefresh),
			token.OAuthRateLimitTier,
			itoa64(token.CreatedAt),
			itoa64(token.UpdatedAt),
		})
	}
	return out
}

func tokenSecretType(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "missing"
	case strings.HasPrefix(value, "sk-ant"):
		return "anthropic"
	case strings.HasPrefix(value, "sk-"):
		return "api_key"
	case strings.Count(value, ".") >= 2:
		return "jwt"
	default:
		return "opaque"
	}
}

func modelCapabilityRows(rows []storage.ModelCapability, codebook diagnosticCodebook) [][]string {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AccountID != rows[j].AccountID {
			return rows[i].AccountID < rows[j].AccountID
		}
		return rows[i].ModelSlug < rows[j].ModelSlug
	})
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			codebook.code(row.AccountID),
			row.ModelSlug,
			itoa64(row.NativeContextWindow),
			itoa64(row.NativeMaxContextWindow),
			itoa64(row.EffectiveContextWindowPercent),
			itoa64(row.AutoCompactTokenLimit),
			row.Visibility,
			row.ETag,
			row.RawModelJSONHash,
			row.Source,
			itoa64(row.LastProbeAt),
		})
	}
	return out
}

func kiroRuntimeCapabilityRows(rows []storage.KiroRuntimeCapability, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		reductionPercent := ""
		if row.CacheReuseProbedAt > 0 {
			reductionPercent = floatString(row.CacheReuseReductionPct)
		}
		out = append(out, []string{
			codebook.code(row.AccountID), row.EndpointHash, row.Model, row.ModelState, row.ThinkingState, row.CacheCapability,
			itoa64(row.Observations), itoa64(row.MeteringEvents), itoa64(row.CacheReportedObservations),
			itoa64(row.CacheHitObservations), itoa64(row.ConsecutiveUnreported),
			codebook.sanitize(row.UnknownCacheSchemaJSON), itoa64(row.UpdatedAt), row.CachePointState,
			firstNonEmpty(row.CacheReuseState, "unknown"), row.CacheReuseEvidence, reductionPercent, itoa64(row.CacheReuseProbedAt),
		})
	}
	return out
}

func accountRateLimitRows(rows []storage.AccountRateLimit, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			codebook.code(row.AccountID),
			row.Provider,
			row.Model,
			row.LimiterType,
			row.Source,
			strconv.FormatFloat(row.UsedPercent, 'f', -1, 64),
			itoa64(row.LimitTokens),
			itoa64(row.RemainingTokens),
			itoa64(row.LimitRequests),
			itoa64(row.RemainingRequests),
			itoa64(row.ResetAt),
			row.Status,
			codebook.sanitize(row.Raw),
			itoa64(row.UpdatedAt),
		})
	}
	return out
}

func affinityBindingRows(rows []storage.AffinityBinding, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{row.RouteKeyHash, codebook.sanitize(row.RouteKey), row.Source, codebook.code(row.AccountID), row.Provider, row.Model, row.EgressID, itoa64(row.Epoch), itoa64(row.CreatedAt), itoa64(row.UpdatedAt)})
	}
	return out
}

func settingRows(rows []diagnosticSetting) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		value := row.Value
		if sensitiveDiagnosticKey(row.Key) {
			value = "<redacted>"
		}
		out = append(out, []string{row.Key, value, itoa64(row.UpdatedAt)})
	}
	return out
}

func sensitiveDiagnosticKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sig := range []string{"token", "secret", "password", "cookie", "admin", "api_key", "apikey", "proxy", "private_key"} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

func customProviderRows(rows []storage.CustomProvider) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.ID,
			row.Name,
			redactURLUserinfo(row.BaseURL),
			row.UpstreamProtocol,
			strconv.FormatBool(row.Enabled),
			strconv.FormatBool(row.AutoDiscoverModels),
			strings.Join(row.Models, " "),
			itoa64(row.CreatedAt),
			itoa64(row.UpdatedAt),
		})
	}
	return out
}

func redactURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("<redacted>")
	return u.String()
}

func upstreamErrorRuleRows(rows []storage.UpstreamErrorRule) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.ID,
			row.Name,
			strconv.FormatBool(row.Enabled),
			strconv.Itoa(row.Priority),
			strings.Join(row.Providers, " "),
			strings.Join(row.Entrypoints, " "),
			strings.Join(row.ModelPatterns, " "),
			intsString(row.StatusCodes),
			strings.Join(row.BodyKeywords, " "),
			row.MatchMode,
			row.AccountAction,
			row.DownstreamAction,
			strconv.Itoa(row.ResponseStatus),
			row.CustomMessage,
			itoa64(row.CooldownSeconds),
			strconv.FormatBool(row.PreferRetryAfter),
			itoa64(row.IdleSeconds),
			itoa64(row.IdlePingSeconds),
			strconv.FormatBool(row.SkipLog),
			strconv.FormatBool(row.FilterAccountAction),
			strconv.FormatBool(row.KeywordCaseSensitive),
			row.Description,
			itoa64(row.CreatedAt),
			itoa64(row.UpdatedAt),
		})
	}
	return out
}

func intsString(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, " ")
}

func codexResetCreditRows(rows []storage.CodexResetCreditConsumption, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{codebook.code(row.AccountID), itoa64(row.SevenDayResetAt), row.RedeemRequestID, row.Status, itoa64(row.CreatedAt), itoa64(row.UpdatedAt)})
	}
	return out
}

func lifecycleStatusRows(rows []diagnosticLifecycleStatus, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			codebook.code(row.AccountID),
			row.ValidityStatus,
			row.SubscriptionTier,
			itoa64(row.SubscriptionExpiresAt),
			itoa64(row.LastHealthCheckAt),
			itoa64(row.LastTokenRefreshAt),
			itoa64(row.HealthCheckFailCount),
			codebook.sanitize(row.SummaryJSON),
			itoa64(row.CreatedAt),
			itoa64(row.UpdatedAt),
		})
	}
	return out
}

func codexReauthConfigRows(rows []storage.AccountCodexReauthConfig, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			codebook.code(row.AccountID),
			strconv.FormatBool(strings.TrimSpace(row.LoginEmail) != ""),
			strconv.FormatBool(row.PasswordConfigured),
			strconv.FormatBool(row.OTPURLConfigured),
			row.TargetWorkspaceID,
			strconv.FormatBool(row.AutoEnabled),
			row.LastStatus,
			row.LastError,
			itoa64(row.CreatedAt),
			itoa64(row.UpdatedAt),
		})
	}
	return out
}

func codexReauthJobRows(rows []storage.AccountCodexReauthJob, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{itoa64(row.ID), codebook.code(row.AccountID), row.Status, row.Reason, row.LastError, itoa64(row.CreatedAt), itoa64(row.UpdatedAt), itoa64(row.StartedAt), itoa64(row.FinishedAt)})
	}
	return out
}

func auditLogRows(rows []storage.AuditLogRow, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{itoa64(row.ID), itoa64(row.CreatedAt), codebook.code(row.AccountID), row.Action, row.State, row.Reason, codebook.sanitize(row.Detail)})
	}
	return out
}

func cfEventRows(rows []storage.CFEvent, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{itoa64(row.ID), itoa64(row.CreatedAt), codebook.code(row.AccountID), row.EgressID, strconv.Itoa(row.Status), row.CFRay, row.Category, codebook.sanitize(row.Message)})
	}
	return out
}

func usageRecordRows(rows []diagnosticUsageRecord, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			itoa64(row.ID),
			itoa64(row.CreatedAt),
			codebook.code(row.AccountID),
			row.RouteKeyHash,
			row.APIKeyHash,
			row.UserID,
			row.Model,
			itoa64(row.PromptTokens),
			itoa64(row.CompletionTokens),
			itoa64(row.TotalTokens),
			itoa64(row.CachedTokens),
			itoa64(row.CacheReadTokens),
			itoa64(row.CacheCreationTokens),
			row.UsageProvider,
			row.UsageSource,
			itoa64(row.CacheReadPresent),
			itoa64(row.CacheCreationPresent),
			codebook.sanitize(row.CompatibilityLossesJSON),
			row.CacheCapability,
			itoa64(row.Estimated),
			itoa64(row.CacheMissTokens),
			itoa64(row.CacheTotalInputTokens),
			itoa64(row.CacheCreation5mTokens),
			itoa64(row.CacheCreation1hTokens),
			row.AffinitySource,
			storage.RouteClassForAffinitySource(row.AffinitySource),
			itoa64(row.PromptCacheKeyPresent),
			row.PromptCacheKeySource,
			row.StablePrefixSource,
			row.StablePrefixReason,
			itoa64(row.StablePrefixBytes),
			row.RetentionEffective,
			row.RetentionSource,
			row.ClaudeCacheTTL,
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
			itoa64(row.RouteEpoch),
			codebook.sanitize(row.RawUsageJSON),
		})
	}
	return out
}

func billingHoldRows(rows []diagnosticBillingHold, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{codebook.sanitize(row.ID), itoa64(row.CreatedAt), itoa64(row.UpdatedAt), codebook.code(row.AccountID), row.RouteKeyHash, itoa64(row.EstimatedTokens), row.Status})
	}
	return out
}

func accountSnapshotRows(accounts []storage.Account, tokensByID map[string]storage.AccountToken, bindings []storage.AccountEgressBinding, codebook diagnosticCodebook) [][]string {
	bindingByAccount := map[string]storage.AccountEgressBinding{}
	for _, b := range bindings {
		bindingByAccount[b.AccountID] = b
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	out := make([][]string, 0, len(accounts))
	for _, a := range accounts {
		b := bindingByAccount[a.ID]
		token, found := tokensByID[a.ID]
		out = append(out, []string{
			codebook.code(a.ID),
			a.GroupName,
			a.Provider,
			accountprovider.EffectiveProvider(a.Provider, token, found),
			a.Status,
			a.PlanType,
			strconv.FormatBool(a.IsFedramp),
			itoa64(a.QuarantineUntil),
			codebook.sanitize(a.QuarantineReason),
			itoa64(a.CreatedAt),
			itoa64(a.UpdatedAt),
			b.PrimaryEgressID,
			b.StandbyEgressIDs,
			itoa64(b.CooldownUntil),
			strconv.FormatBool(b.RecheckPending),
		})
	}
	return out
}

func egressSnapshotRows(profiles []storage.EgressProfile, bindings []storage.AccountEgressBinding, codebook diagnosticCodebook) [][]string {
	bound := map[string][]string{}
	for _, b := range bindings {
		if code := codebook.code(b.AccountID); code != "" {
			bound[b.PrimaryEgressID] = append(bound[b.PrimaryEgressID], code)
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	out := make([][]string, 0, len(profiles))
	for _, p := range profiles {
		codes := bound[p.ID]
		sort.Strings(codes)
		out = append(out, []string{
			p.ID,
			p.Name,
			p.Type,
			p.Region,
			p.ExitIP,
			strconv.FormatBool(p.StreamCapable),
			p.Health,
			itoa64(p.LatencyMillis),
			itoa64(p.CFScore),
			p.LastCFRay,
			itoa64(p.CooldownUntil),
			strconv.Itoa(p.MaxConcurrency),
			itoa64(p.CreatedAt),
			itoa64(p.UpdatedAt),
			strings.Join(codes, " "),
		})
	}
	return out
}

func diagnosticSummary(accounts []storage.Account, tokensByID map[string]storage.AccountToken, auditRows []storage.AuditLogRow, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding, rateLimits []storage.AccountRateLimit) map[string]interface{} {
	now := storage.Now()
	buildWindow := func(since int64) diagnosticEventWindow {
		routing409 := map[string]int{}
		healthModels := map[string]int{}
		banEvents := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
		banAccounts := map[string]map[string]bool{"discovered": {}, "deleted": {}, "delete_failed": {}}
		for _, row := range auditRows {
			if row.CreatedAt < since {
				continue
			}
			if row.Action == "routing_unavailable" && strings.Contains(row.Detail, "status=409") {
				routing409[classifyRouting409Detail(row.Detail)]++
			}
			if row.Action == "health_test" || row.Action == "health_test_model_unsupported" {
				if model := extractDiagnosticDetailValue(row.Detail, "model"); model != "" {
					healthModels[model]++
				}
			}
			kind := ""
			if row.State == "banned" {
				kind = "discovered"
			}
			switch row.Action {
			case "ban_delete":
				kind = "deleted"
			case "ban_delete_failed":
				kind = "delete_failed"
			}
			if kind != "" {
				banEvents[kind]++
				if row.AccountID != "" {
					banAccounts[kind][row.AccountID] = true
				}
			}
		}
		unique := map[string]int{}
		for kind, ids := range banAccounts {
			unique[kind] = len(ids)
		}
		holdEvents := map[string]int{}
		for _, hold := range holds {
			if hold.CreatedAt >= since {
				holdEvents[firstNonEmpty(hold.Status, "unknown")]++
			}
		}
		return diagnosticEventWindow{
			Routing409: routing409, HealthModels: healthModels,
			Banned:       map[string]interface{}{"events": banEvents, "unique_accounts": unique},
			BillingHolds: holdEvents,
		}
	}
	lifetime := buildWindow(0)
	last24h := buildWindow(now - 24*60*60)
	freshHeld, staleHeld, expiredCount := 0, 0, 0
	oldestFreshHeldAge := int64(0)
	for _, hold := range holds {
		switch hold.Status {
		case "held":
			age := now - hold.CreatedAt
			if age <= int64(time.Hour/time.Second) {
				freshHeld++
				if age > oldestFreshHeldAge {
					oldestFreshHeldAge = age
				}
			} else {
				staleHeld++
			}
		case "expired_unsettled":
			expiredCount++
		}
	}

	bindingByAccount := map[string]storage.AccountEgressBinding{}
	for _, binding := range bindings {
		bindingByAccount[binding.AccountID] = binding
	}
	rateLimitsByAccount := map[string][]storage.AccountRateLimit{}
	for _, snap := range rateLimits {
		rateLimitsByAccount[snap.AccountID] = append(rateLimitsByAccount[snap.AccountID], snap)
	}
	type groupSummary struct {
		Active            int            `json:"active"`
		Providers         map[string]int `json:"providers"`
		Cooldown          int            `json:"cooldown"`
		RecheckPending    int            `json:"recheck_pending"`
		RateLimitCooldown int            `json:"rate_limit_cooldown"`
	}
	groups := map[string]*groupSummary{}
	for _, account := range accounts {
		g := groups[account.GroupName]
		if g == nil {
			g = &groupSummary{Providers: map[string]int{}}
			groups[account.GroupName] = g
		}
		if account.Status != "active" || account.QuarantineUntil > now {
			continue
		}
		token, found := tokensByID[account.ID]
		provider := accountprovider.EffectiveProvider(account.Provider, token, found)
		g.Active++
		g.Providers[provider]++
		if binding, ok := bindingByAccount[account.ID]; ok {
			if binding.CooldownUntil > now {
				g.Cooldown++
			}
			if binding.RecheckPending {
				g.RecheckPending++
			}
		}
		limited := false
		if _, ok := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], provider, "", now); ok {
			limited = true
		} else {
			for _, snap := range rateLimitsByAccount[account.ID] {
				if _, ok := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], provider, snap.Model, now); ok {
					limited = true
					break
				}
			}
		}
		if limited {
			g.RateLimitCooldown++
		}
	}
	return map[string]interface{}{
		// Retain the legacy top-level keys while making their event nature explicit
		// in the two windowed sections below.
		"routing_409":        lifetime.Routing409,
		"health_test_models": lifetime.HealthModels,
		"banned_accounts":    lifetime.Banned,
		"billing_holds": map[string]interface{}{
			"current_fresh_held":            freshHeld,
			"historical_stale_held":         staleHeld,
			"expired_unsettled":             expiredCount,
			"oldest_fresh_held_age_seconds": oldestFreshHeldAge,
		},
		"groups":   groups,
		"lifetime": lifetime,
		"last_24h": last24h,
		"current_state": map[string]interface{}{
			"groups":                 groups,
			"fresh_billing_holds":    freshHeld,
			"stale_historical_holds": staleHeld,
		},
	}
}

func classifyRouting409Detail(detail string) string {
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "token budget"):
		return "token_budget_exceeded"
	case strings.Contains(lower, "pending health re-check") || strings.Contains(lower, "recheck"):
		return "pending_health_recheck"
	case strings.Contains(lower, "rate-limit cooldown") || strings.Contains(lower, "cooldown"):
		return "rate_limit_cooldown"
	case strings.Contains(lower, "quarantined"):
		return "quarantined"
	default:
		return "other"
	}
}

func extractDiagnosticDetailValue(detail, key string) string {
	prefix := key + "="
	for _, field := range strings.Fields(detail) {
		if strings.HasPrefix(field, prefix) {
			return strings.Trim(strings.TrimPrefix(field, prefix), `",`)
		}
	}
	return ""
}

func csvString(header []string, rows [][]string) string {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(header)
	for _, row := range rows {
		_ = cw.Write(row)
	}
	cw.Flush()
	return buf.String()
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
