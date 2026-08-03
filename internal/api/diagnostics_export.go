package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
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
	KiroCredits                       float64
	KiroCreditsPresent                int64
	BillingHoldID                     string
	RequestedModel                    string
	ResolvedModel                     string
	ModelOverrideSource               string
	RawUsageJSON                      string
	CreatedAt                         int64
}

type diagnosticBillingHold struct {
	ID              string
	RouteKeyHash    string
	AccountID       string
	EstimatedTokens int64
	Status          string
	UsageExpected   int64
	UsageRecordedAt int64
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

type diagnosticReplacementNode struct {
	children       map[byte]*diagnosticReplacementNode
	replacement    diagnosticReplacement
	hasReplacement bool
}

type diagnosticCodebook struct {
	byID         map[string]diagnosticAccountIdentity
	replacements []diagnosticReplacement
	replacement  *diagnosticReplacementNode
	aliasKey     []byte
}

type diagnosticEventWindow struct {
	Routing409   map[string]int         `json:"routing_409_events"`
	HealthModels map[string]int         `json:"health_test_events_by_model"`
	Banned       map[string]interface{} `json:"banned_accounts"`
	BillingHolds map[string]int         `json:"billing_hold_events_by_status"`
}

func diagnosticFileOrder() []string {
	return []string{
		"manifest.json",
		"diagnostic_summary.json",
		"runtime_storage.json",
		"diagnostic_events.csv",
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
}

type diagnosticExportStat struct {
	Rows       int64
	SourceRows int64
	Min        int64
	Max        int64
}

// A diagnostics bundle is a support snapshot, not a second full database
// backup. Keeping the newest rows from each append-heavy table makes export
// latency and memory/disk work predictable even after months of traffic. The
// manifest records the exact source/exported counts whenever this cap applies.
const diagnosticExportRowLimit int64 = 20_000

func (s *Server) streamDiagnosticsExport(ctx context.Context, w http.ResponseWriter) error {
	snapshot, err := s.store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		return err
	}
	snapshotOpen := true
	defer func() {
		if snapshotOpen {
			_ = snapshot.Close()
		}
	}()
	snapshotStore, err := snapshot.Store(s.store)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.cfg.BodySpoolDir, "codex-pool-diagnostics-*.zip")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err = s.writeDiagnosticsExport(ctx, temp, snapshot.ID(), snapshotStore); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = snapshot.Close(); err != nil {
		return err
	}
	snapshotOpen = false
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics-v3.zip"`)
	// Hide File.WriteTo and ResponseWriter.ReadFrom so archive delivery stays in
	// small bounded writes even when the compressed ZIP is much smaller than its
	// source tables.
	_, err = io.CopyBuffer(struct{ io.Writer }{w}, struct{ io.Reader }{temp}, make([]byte, 4<<10))
	return err
}

func (s *Server) writeDiagnosticsExport(ctx context.Context, dst io.Writer, snapshotID string, snapshotStore *storage.Store) error {
	generatedAt := time.Now().Unix()
	snapshotNow := storage.Now()
	accounts, err := snapshotStore.ListAccounts(ctx)
	if err != nil {
		return err
	}
	bindings, err := snapshotStore.ListEgressBindings(ctx)
	if err != nil {
		return err
	}
	egressProfiles, err := snapshotStore.ListEgressProfiles(ctx)
	if err != nil {
		return err
	}
	groups, err := snapshotStore.ListGroups(ctx)
	if err != nil {
		return err
	}
	// Credential metadata is selected with SQL CASE expressions only. Diagnostic
	// generation never invokes the decryption path and never holds plaintext
	// credentials in memory.
	tokensByID, err := listDiagnosticTokenMetadata(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	capabilities, err := snapshotStore.ListCapabilities(ctx, "")
	if err != nil {
		return err
	}
	kiroCapabilities, err := snapshotStore.ListKiroRuntimeCapabilities(ctx, "")
	if err != nil {
		return err
	}
	rateLimits, err := snapshotStore.ListAccountRateLimits(ctx)
	if err != nil {
		return err
	}
	codexMappings, err := snapshotStore.ListCodexSessionMappingDiagnostics(ctx)
	if err != nil {
		return err
	}
	codexInstructionSnapshots, err := snapshotStore.ListCodexInstructionSnapshotDiagnostics(ctx)
	if err != nil {
		return err
	}
	settings, err := listDiagnosticSettings(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	customProviders, err := snapshotStore.ListCustomProviders(ctx)
	if err != nil {
		return err
	}
	upstreamRules, err := snapshotStore.ListUpstreamErrorRules(ctx)
	if err != nil {
		return err
	}
	resetConsumptions, err := listDiagnosticCodexResetCreditConsumptions(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	lifecycleStatuses, err := listDiagnosticLifecycleStatuses(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	reauthConfigs, err := listDiagnosticCodexReauthConfigs(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	reauthJobs, err := listDiagnosticCodexReauthJobs(ctx, snapshotStore.ReadDB())
	if err != nil {
		return err
	}
	codebook, err := buildStreamingDiagnosticCodebook(ctx, snapshotStore.ReadDB(), accounts, bindings, s.cfg.RuntimeDiagnosticAliasKey)
	if err != nil {
		return err
	}
	summary := diagnosticSummary(accounts, tokensByID, nil, nil, bindings, rateLimits)
	summary["usage_journal"] = s.usageJournalMetrics()
	goalMetrics, err := snapshotStore.GoalContinuityMetrics(ctx)
	if err != nil {
		return err
	}
	summary["goal_continuity"] = goalMetrics
	summary["goal_policy"] = diagnosticGoalPolicy(settings, s.cfg)
	summary["routing_audit"] = s.routingAuditDiagnostics()
	codexCPA := s.codexSessionMappingStatsFromSnapshot(ctx, snapshotStore, settings)
	codexCPA["instruction_policy_groups"] = len(groups)
	namespacePrefixes := make(map[string]struct{})
	for _, mapping := range codexMappings {
		namespacePrefixes[mapping.NamespaceHMACPrefix] = struct{}{}
	}
	codexCPA["namespace_count"] = len(namespacePrefixes)
	summary["codex_cpa"] = codexCPA
	if err := applyDiagnosticAuditSummary(ctx, snapshotStore.ReadDB(), summary); err != nil {
		return err
	}
	if err := applyDiagnosticBillingHoldSummary(ctx, snapshotStore.ReadDB(), summary); err != nil {
		return err
	}
	stats, err := diagnosticLargeTableStats(ctx, snapshotStore.ReadDB(), snapshotNow)
	if err != nil {
		return err
	}

	small := map[string]string{}
	rowCounts := map[string]int64{}
	addCSV := func(name string, header []string, rows [][]string) {
		safeRows := make([][]string, 0, len(rows))
		for _, row := range rows {
			safeRows = append(safeRows, codebook.safeCSVRow(row))
		}
		small[name] = csvString(header, safeRows)
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
	addCSV("account_auth_metadata.csv", []string{"account_code", "declared_provider", "effective_provider", "auth_method", "credential_mode", "billing_mode", "credential_present", "expires_at", "last_refresh", "oauth_rate_limit_tier", "created_at", "updated_at"}, accountAuthMetadataRows(accounts, tokensByID, codebook))
	addCSV("account_model_capabilities.csv", []string{"account_code", "model_slug", "availability_state", "context_1m_state", "context_1m_source", "native_context_window", "native_max_context_window", "source", "last_probe_at"}, modelCapabilityRows(capabilities, codebook))
	addCSV("kiro_runtime_capabilities.csv", []string{"account_code", "endpoint_hash", "model", "model_state", "thinking_state", "cache_capability", "observations", "metering_events", "cache_reported_observations", "cache_hit_observations", "consecutive_unreported", "unknown_cache_schema_json", "updated_at", "cache_point_state", "cache_reuse_state", "cache_reuse_evidence", "cache_reuse_credit_reduction_percent", "cache_reuse_probed_at"}, kiroRuntimeCapabilityRows(kiroCapabilities, codebook))
	addCSV("account_rate_limits.csv", []string{"account_code", "provider", "model", "limiter_type", "source", "used_percent", "limit_tokens", "remaining_tokens", "limit_requests", "remaining_requests", "reset_at", "status", "raw_json", "updated_at"}, accountRateLimitRows(rateLimits, codebook))
	addCSV("codex_session_mappings.csv", []string{"tree_hmac_prefix", "namespace_hmac_prefix", "account_code", "egress_id", "epoch", "state", "instruction_snapshot_present", "created_at", "updated_at", "expires_at"}, codexSessionMappingDiagnosticRows(codexMappings, codebook))
	addCSV("codex_instruction_snapshots.csv", []string{"tree_hmac_prefix", "revision_hmac_prefix", "created_at", "updated_at", "expires_at"}, codexInstructionSnapshotDiagnosticRows(codexInstructionSnapshots))
	addCSV("codex_group_policy_revisions.csv", []string{"group_name", "instructions_enabled", "instruction_file_count", "policy_revision_hmac_prefix", "updated_at"}, codexGroupPolicyRevisionRows(groups, s))
	sidecarAdaptive := []upstream.SidecarAdaptiveStatus(nil)
	if s.upstream != nil {
		sidecarAdaptive = s.upstream.SidecarAdaptiveStatuses()
	}
	addCSV("sidecar_status.csv", []string{"sidecar_egress_id", "real_egress_id", "health", "profile_max_concurrency", "adaptive_limit", "inflight", "queue_depth", "recent_failures", "circuit_state", "circuit_until", "bypass_until", "cooldown_until", "bound_account_count", "created_at", "updated_at"}, sidecarStatusRows(egressProfiles, bindings, sidecarAdaptive))
	addCSV("settings.csv", []string{"key", "value", "updated_at"}, settingRows(settings))
	addCSV("custom_providers.csv", []string{"id", "name", "base_url", "upstream_protocol", "enabled", "auto_discover_models", "models", "model_mappings", "created_at", "updated_at"}, customProviderRows(customProviders))
	addCSV("upstream_error_rules.csv", []string{"id", "name", "enabled", "priority", "providers", "entrypoints", "model_patterns", "status_codes", "body_keywords", "match_mode", "account_action", "downstream_action", "response_status", "custom_message", "cooldown_seconds", "prefer_retry_after", "idle_seconds", "idle_ping_seconds", "skip_log", "description", "created_at", "updated_at"}, upstreamErrorRuleRows(upstreamRules))
	addCSV("codex_reset_credit_consumptions.csv", []string{"account_code", "seven_day_reset_at", "redeem_request_id", "status", "created_at", "updated_at"}, codexResetCreditRows(resetConsumptions, codebook))
	addCSV("account_lifecycle_status.csv", []string{"account_code", "validity_status", "subscription_tier", "subscription_expires_at", "last_health_check_at", "last_token_refresh_at", "health_check_fail_count", "summary_json", "created_at", "updated_at"}, lifecycleStatusRows(lifecycleStatuses, codebook))
	addCSV("codex_reauth_config.csv", []string{"account_code", "login_email_present", "password_configured", "otp_url_configured", "target_workspace_id", "auto_enabled", "last_status", "last_error", "created_at", "updated_at"}, codexReauthConfigRows(reauthConfigs, codebook))
	addCSV("codex_reauth_jobs.csv", []string{"id", "account_code", "status", "reason", "last_error", "created_at", "updated_at", "started_at", "finished_at"}, codexReauthJobRows(reauthJobs, codebook))
	httpRequests := s.diagnosticHTTPRequests()
	routeAttempts := s.diagnosticRouteAttempts()
	providerAttempts := s.diagnosticProviderAttempts()
	addCSV("http_requests.csv", []string{"request_id", "method", "route", "status", "request_bytes", "response_bytes", "duration_ms", "created_at"}, httpRequestRows(httpRequests))
	addCSV("route_attempts.csv", []string{"request_id", "tier", "target", "selection_type", "status_class", "fallback_target", "created_at"}, routeAttemptRows(routeAttempts))
	addCSV("provider_attempts.csv", []string{"request_id", "account_code", "provider", "phase", "status", "error_class", "body_hash", "retry_after", "created_at"}, providerAttemptRows(providerAttempts, codebook))
	addCSV("accounts_snapshot.csv", []string{"account_code", "group_name", "declared_provider", "effective_provider", "status", "plan_type", "is_fedramp", "ignore_rate_limit_controls", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "sidecar_egress_id", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, tokensByID, bindings, codebook))
	addCSV("egress_snapshot.csv", []string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
	if err := addJSON("diagnostic_summary.json", summary); err != nil {
		return err
	}
	if err := addJSON("runtime_storage.json", s.runtimeStorageDiagnostics()); err != nil {
		return err
	}
	sourceRowCounts := make(map[string]int64, len(stats))
	truncatedTables := map[string]map[string]interface{}{}
	for name, stat := range stats {
		rowCounts[name] = stat.Rows
		sourceRowCounts[name] = stat.SourceRows
		if stat.SourceRows > stat.Rows {
			truncatedTables[name] = map[string]interface{}{
				"source_rows":   stat.SourceRows,
				"exported_rows": stat.Rows,
				"omitted_rows":  stat.SourceRows - stat.Rows,
				"selection":     "most_recent",
			}
		}
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
	codexMappingTimes := make([]int64, 0, len(codexMappings))
	for _, row := range codexMappings {
		codexMappingTimes = append(codexMappingTimes, row.CreatedAt)
	}
	addRange("codex_session_mappings.csv", codexMappingTimes)
	codexSnapshotTimes := make([]int64, 0, len(codexInstructionSnapshots))
	for _, row := range codexInstructionSnapshots {
		codexSnapshotTimes = append(codexSnapshotTimes, row.CreatedAt)
	}
	addRange("codex_instruction_snapshots.csv", codexSnapshotTimes)
	groupPolicyTimes := make([]int64, 0, len(groups))
	for _, row := range groups {
		groupPolicyTimes = append(groupPolicyTimes, row.UpdatedAt)
	}
	addRange("codex_group_policy_revisions.csv", groupPolicyTimes)
	sidecarTimes := make([]int64, 0, len(egressProfiles))
	for _, row := range egressProfiles {
		if storage.IsSidecarEgress(row) {
			sidecarTimes = append(sidecarTimes, row.UpdatedAt)
		}
	}
	addRange("sidecar_status.csv", sidecarTimes)
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
	routeAttemptTimes := make([]int64, 0, len(routeAttempts))
	for _, row := range routeAttempts {
		routeAttemptTimes = append(routeAttemptTimes, row.CreatedAt)
	}
	addRange("route_attempts.csv", routeAttemptTimes)
	providerAttemptTimes := make([]int64, 0, len(providerAttempts))
	for _, row := range providerAttempts {
		providerAttemptTimes = append(providerAttemptTimes, row.CreatedAt)
	}
	addRange("provider_attempts.csv", providerAttemptTimes)
	httpRequestTimes := make([]int64, 0, len(httpRequests))
	for _, row := range httpRequests {
		httpRequestTimes = append(httpRequestTimes, row.CreatedAt)
	}
	addRange("http_requests.csv", httpRequestTimes)
	egressTimes := make([]int64, 0, len(egressProfiles))
	for _, row := range egressProfiles {
		egressTimes = append(egressTimes, row.CreatedAt)
	}
	addRange("egress_snapshot.csv", egressTimes)
	manifest := map[string]interface{}{
		"generated_at": generatedAt, "snapshot_id": snapshotID, "format": "codex-pool-diagnostics-v3",
		"account_count": len(accounts), "current_account_count": len(accounts),
		"historical_reference_account_count": len(codebook.byID),
		"files":                              diagnosticFileOrder(), "row_counts": rowCounts,
		"source_row_counts":       sourceRowCounts,
		"truncated_tables":        truncatedTables,
		"large_table_row_limit":   diagnosticExportRowLimit,
		"build":                   diagnosticBuildInfo(),
		"table_time_ranges":       timeRanges,
		"table_time_ranges_scope": "source_snapshot",
		"account_redaction":       "all entity identifiers use type-isolated stable HMAC aliases; no reverse map or alias key is included",
		"account_code_format":     "ACC-Base32(HMAC-SHA256(alias_key,domain||entity_type||raw_id)[:16])",
		"public_request_ids":      "server-generated REQ- plus 16 hexadecimal request IDs are retained for support correlation; upstream request IDs remain aliased",
	}
	if err := addJSON("manifest.json", manifest); err != nil {
		return err
	}

	zw := zip.NewWriter(dst)
	for _, name := range diagnosticFileOrder() {
		var written int64
		switch name {
		case "diagnostic_events.csv":
			written, err = streamDiagnosticEventsCSV(ctx, snapshotStore.ReadDB(), zw, codebook)
		case "audit_log.csv":
			written, err = streamDiagnosticAuditCSV(ctx, snapshotStore.ReadDB(), zw, codebook)
		case "cf_events.csv":
			written, err = streamDiagnosticCFCSV(ctx, snapshotStore.ReadDB(), zw, codebook)
		case "usage_records.csv":
			written, err = streamDiagnosticUsageCSV(ctx, snapshotStore.ReadDB(), zw, codebook)
		case "billing_holds.csv":
			written, err = streamDiagnosticBillingHoldsCSV(ctx, snapshotStore.ReadDB(), zw, codebook)
		case "affinity_bindings.csv":
			written, err = streamDiagnosticAffinityCSV(ctx, snapshotStore.ReadDB(), zw, codebook, snapshotNow)
		case "codex_upstream_attempts.csv":
			written, err = streamDiagnosticCodexUpstreamAttemptsCSV(ctx, snapshotStore.ReadDB(), zw, codebook, snapshotStore, snapshotNow)
		case "codex_upstream_attempts_daily.csv":
			written, err = streamDiagnosticCodexUpstreamAttemptsDailyCSV(ctx, snapshotStore.ReadDB(), zw, codebook, snapshotNow)
		default:
			content, ok := small[name]
			if !ok {
				continue
			}
			content = codebook.sanitize(content)
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
		if stat, streamed := stats[name]; streamed && written != stat.Rows {
			_ = zw.Close()
			return fmt.Errorf("diagnostic snapshot row count changed for %s: wrote %d expected %d", name, written, stat.Rows)
		}
	}
	return zw.Close()
}

func (s *Server) codexSessionMappingStatsFromSnapshot(ctx context.Context, snapshotStore *storage.Store, settings []diagnosticSetting) map[string]interface{} {
	values := make(map[string]string, len(settings))
	for _, setting := range settings {
		values[setting.Key] = strings.TrimSpace(setting.Value)
	}
	boolValue := func(key string, fallback bool) bool {
		value, ok := values[key]
		if !ok || value == "" {
			return fallback
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fallback
		}
		return parsed
	}
	intValue := func(key string, fallback int) int {
		value, ok := values[key]
		if !ok || value == "" {
			return fallback
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return fallback
		}
		return parsed
	}
	stateless := boolValue("codex_stateless_passthrough", s.cfg.CodexStatelessPassthrough)
	enabled := !stateless && boolValue("codex_session_mapping_enabled", s.cfg.CodexSessionMappingEnabled)
	strict := boolValue("codex_cpa_strict", s.cfg.CodexCPAStrict)
	retentionDays := intValue("codex_session_mapping_retention_days", s.cfg.CodexSessionMappingRetentionDays)
	return s.codexSessionMappingStatsConfigured(ctx, snapshotStore, enabled, stateless, strict, retentionDays)
}

func buildStreamingDiagnosticCodebook(ctx context.Context, db storage.ReadQuerier, accounts []storage.Account, bindings []storage.AccountEgressBinding, aliasKey []byte) (diagnosticCodebook, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_id, MAX(account_label) FROM (
	SELECT account_id,account_label FROM audit_log
	WHERE account_id<>'' ORDER BY id DESC LIMIT ?
) recent_audit GROUP BY account_id
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM cf_events WHERE account_id<>'' ORDER BY id DESC LIMIT ?
) recent_cf
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM usage_records WHERE account_id<>'' ORDER BY id DESC LIMIT ?
) recent_usage
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM billing_holds WHERE account_id<>'' ORDER BY created_at DESC,id DESC LIMIT ?
) recent_holds
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM affinity_bindings WHERE account_id<>'' ORDER BY updated_at DESC,route_key_hash DESC LIMIT ?
) recent_bindings
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM affinity_aliases WHERE account_id<>'' ORDER BY updated_at DESC,route_key_hash DESC LIMIT ?
) recent_aliases
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM codex_upstream_attempt WHERE account_id<>'' ORDER BY created_at DESC,id DESC LIMIT ?
) recent_attempts
UNION SELECT account_id,'' FROM (
	SELECT account_id FROM codex_upstream_attempt_daily WHERE account_id<>'' ORDER BY day_start DESC LIMIT ?
) recent_daily`,
		diagnosticExportRowLimit, diagnosticExportRowLimit,
		diagnosticExportRowLimit, diagnosticExportRowLimit,
		diagnosticExportRowLimit, diagnosticExportRowLimit,
		diagnosticExportRowLimit, diagnosticExportRowLimit,
	)
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
	return buildDiagnosticCodebookWithKey(aliasKey, accounts, historical, nil, nil, nil, bindings), nil
}

func applyDiagnosticAuditSummary(ctx context.Context, db storage.ReadQuerier, summary map[string]interface{}) error {
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

func applyDiagnosticBillingHoldSummary(ctx context.Context, db storage.ReadQuerier, summary map[string]interface{}) error {
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

func diagnosticLargeTableStats(ctx context.Context, db storage.ReadQuerier, snapshotNow int64) (map[string]diagnosticExportStat, error) {
	queries := map[string]struct {
		sql  string
		args []interface{}
	}{
		"audit_log.csv":     {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM audit_log`},
		"cf_events.csv":     {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM cf_events`},
		"usage_records.csv": {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM usage_records`},
		"billing_holds.csv": {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM billing_holds`},
		"diagnostic_events.csv": {
			sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM diagnostic_events`,
		},
		"affinity_bindings.csv": {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM (
SELECT created_at FROM affinity_aliases WHERE expires_at>?
UNION ALL
SELECT b.created_at FROM affinity_bindings b WHERE (b.expires_at=0 OR b.expires_at>?)
AND NOT EXISTS (SELECT 1 FROM affinity_aliases a WHERE a.route_key_hash=b.route_key_hash AND a.expires_at>?)
)`, args: []interface{}{snapshotNow, snapshotNow, snapshotNow}},
		"codex_upstream_attempts.csv":       {sql: `SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0) FROM codex_upstream_attempt WHERE expires_at>?`, args: []interface{}{snapshotNow}},
		"codex_upstream_attempts_daily.csv": {sql: `SELECT COUNT(*), COALESCE(MIN(first_created_at),0), COALESCE(MAX(last_created_at),0) FROM codex_upstream_attempt_daily WHERE expires_at>?`, args: []interface{}{snapshotNow}},
	}
	out := map[string]diagnosticExportStat{}
	for file, query := range queries {
		var stat diagnosticExportStat
		if err := db.QueryRowContext(ctx, query.sql, query.args...).Scan(&stat.SourceRows, &stat.Min, &stat.Max); err != nil {
			return nil, err
		}
		stat.Rows = stat.SourceRows
		if stat.Rows > diagnosticExportRowLimit {
			stat.Rows = diagnosticExportRowLimit
		}
		out[file] = stat
	}
	return out, nil
}

func streamDiagnosticCSV(zw *zip.Writer, name string, header []string, writeRows func(*csv.Writer) (int64, error)) (int64, error) {
	writer, err := zw.Create(name)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(writer)
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	rows, err := writeRows(cw)
	if err != nil {
		return rows, err
	}
	cw.Flush()
	return rows, cw.Error()
}

func streamDiagnosticEventsCSV(
	ctx context.Context,
	db storage.ReadQuerier,
	zw *zip.Writer,
	codebook diagnosticCodebook,
) (int64, error) {
	header := []string{
		"event_code", "created_at", "event_type", "severity", "entity_type",
		"entity_alias", "detail_json", "diagnostic_gap",
	}
	return streamDiagnosticCSV(zw, "diagnostic_events.csv", header, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `
SELECT id,event_type,severity,entity_type,entity_alias,detail_json,diagnostic_gap,created_at
FROM (
	SELECT id,event_type,severity,entity_type,entity_alias,detail_json,diagnostic_gap,created_at
	FROM diagnostic_events ORDER BY created_at DESC,id DESC LIMIT ?
) recent ORDER BY created_at,id`, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var event storage.DiagnosticEvent
			var gap int
			if err := rows.Scan(
				&event.ID, &event.EventType, &event.Severity, &event.EntityType,
				&event.EntityAlias, &event.DetailJSON, &gap, &event.CreatedAt,
			); err != nil {
				return written, err
			}
			event.DiagnosticGap = gap != 0
			if err := cw.Write(codebook.safeCSVRow(diagnosticEventRow(event, codebook))); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

var (
	diagnosticEventEnumRE   = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
	diagnosticEntityAliasRE = regexp.MustCompile(`^(?:ACC|EGR|GRP|USR|KEY|REQ|SES|TSK|HST|JOB|ENT)-[A-Z2-7]{26}$`)
)

func diagnosticEventRow(event storage.DiagnosticEvent, codebook diagnosticCodebook) []string {
	eventType := strings.ToLower(strings.TrimSpace(event.EventType))
	if !diagnosticEventEnumRE.MatchString(eventType) {
		eventType = "unknown"
	}
	severity := strings.ToLower(strings.TrimSpace(event.Severity))
	switch severity {
	case "debug", "info", "warning", "error", "normal", "pressure", "critical", "emergency":
	default:
		severity = "unknown"
	}
	entityType := strings.ToLower(strings.TrimSpace(event.EntityType))
	if entityType != "" && !diagnosticEventEnumRE.MatchString(entityType) {
		entityType = "unknown"
	}
	return []string{
		diagnosticAlias(codebook.aliasKey, "EVT", "diagnostic-event", event.ID),
		strconv.FormatInt(event.CreatedAt, 10),
		eventType,
		severity,
		entityType,
		diagnosticExportEntityAlias(codebook, entityType, event.EntityAlias),
		sanitizeDiagnosticEventDetail(event.DetailJSON, codebook),
		strconv.FormatBool(event.DiagnosticGap),
	}
}

func diagnosticExportEntityAlias(codebook diagnosticCodebook, entityType, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	upper := strings.ToUpper(value)
	if entityType == "request" && diagnosticPublicRequestIDRE.MatchString(upper) {
		return upper
	}
	if diagnosticEntityAliasRE.MatchString(upper) {
		return upper
	}
	prefix := map[string]string{
		"account": "ACC", "egress": "EGR", "group": "GRP", "user": "USR",
		"key": "KEY", "request": "REQ", "session": "SES", "task": "TSK",
		"host": "HST", "diagnostic_job": "JOB",
	}[entityType]
	if prefix == "" {
		prefix = "ENT"
	}
	return diagnosticAlias(codebook.aliasKey, prefix, entityType, value)
}

func sanitizeDiagnosticEventDetail(raw string, codebook diagnosticCodebook) string {
	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		value = map[string]interface{}{
			"unparsed": diagnosticAlias(codebook.aliasKey, "TEXT", "event-detail", raw),
		}
	}
	value = sanitizeDiagnosticEventValue("", value, codebook)
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"unparsed":"TEXT-INVALID"}`
	}
	return string(encoded)
}

func sanitizeDiagnosticEventValue(field string, value interface{}, codebook diagnosticCodebook) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		safe := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			safeKey := strings.ToLower(strings.TrimSpace(key))
			if !diagnosticEventEnumRE.MatchString(safeKey) {
				safeKey = strings.ToLower(diagnosticAlias(codebook.aliasKey, "FIELD", "event-field", key))
			}
			safe[safeKey] = sanitizeDiagnosticEventValue(safeKey, child, codebook)
		}
		return safe
	case []interface{}:
		safe := make([]interface{}, len(typed))
		for index := range typed {
			safe[index] = sanitizeDiagnosticEventValue(field, typed[index], codebook)
		}
		return safe
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return ""
		}
		if diagnosticEventTypedStringField(field) && diagnosticEventEnumRE.MatchString(strings.ToLower(typed)) {
			return strings.ToLower(typed)
		}
		sanitized := codebook.sanitize(typed)
		if sanitized != typed {
			return sanitized
		}
		return diagnosticAlias(codebook.aliasKey, "TEXT", "event-detail:"+field, typed)
	case json.Number, float64, bool, nil:
		return typed
	default:
		return diagnosticAlias(codebook.aliasKey, "TEXT", "event-detail:"+field, fmt.Sprint(typed))
	}
}

func diagnosticEventTypedStringField(field string) bool {
	switch field {
	case "action", "component", "delivery", "error_class", "error_code", "fingerprint", "level", "mode", "operation", "phase",
		"previous_level", "reason", "resource_type", "result", "role", "route", "source", "state", "status":
		return true
	default:
		return false
	}
}

func streamDiagnosticAuditCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook) (int64, error) {
	return streamDiagnosticCSV(zw, "audit_log.csv", []string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT id,account_id,account_label,action,state,reason,detail,created_at FROM (
	SELECT id,account_id,account_label,action,state,reason,detail,created_at
	FROM audit_log ORDER BY id DESC LIMIT ?
) recent ORDER BY id`, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row storage.AuditLogRow
			if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountLabel, &row.Action, &row.State, &row.Reason, &row.Detail, &row.CreatedAt); err != nil {
				return written, err
			}
			if err := cw.Write(codebook.safeCSVRow(auditLogRows([]storage.AuditLogRow{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticCFCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook) (int64, error) {
	return streamDiagnosticCSV(zw, "cf_events.csv", []string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT id,account_id,egress_id,status,cf_ray,category,message,created_at FROM (
	SELECT id,account_id,egress_id,status,cf_ray,category,message,created_at
	FROM cf_events ORDER BY id DESC LIMIT ?
) recent ORDER BY id`, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row storage.CFEvent
			if err := rows.Scan(&row.ID, &row.AccountID, &row.EgressID, &row.Status, &row.CFRay, &row.Category, &row.Message, &row.CreatedAt); err != nil {
				return written, err
			}
			if err := cw.Write(codebook.safeCSVRow(cfEventRows([]storage.CFEvent{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticUsageCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook) (int64, error) {
	header := []string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "usage_provider", "usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "estimated", "cache_miss_tokens", "cache_total_input_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "affinity_source", "route_class", "prompt_cache_key_present", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch", "raw_usage_json", "kiro_credits", "kiro_credits_present", "billing_hold_id", "requested_model", "resolved_model", "model_override_source"}
	return streamDiagnosticCSV(zw, "usage_records.csv", header, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT * FROM (`+diagnosticUsageRecordSelectSQL()+`
 ORDER BY id DESC LIMIT ?) recent ORDER BY id`, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row diagnosticUsageRecord
			if err := scanDiagnosticUsageRecord(rows, &row); err != nil {
				return written, err
			}
			if err := cw.Write(codebook.safeCSVRow(usageRecordRows([]diagnosticUsageRecord{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticBillingHoldsCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook) (int64, error) {
	return streamDiagnosticCSV(zw, "billing_holds.csv", []string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status", "usage_expected", "usage_recorded_at"}, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT id,route_key_hash,account_id,estimated_tokens,status,usage_expected,usage_recorded_at,created_at,updated_at FROM (
	SELECT id,route_key_hash,account_id,estimated_tokens,status,usage_expected,usage_recorded_at,created_at,updated_at
	FROM billing_holds ORDER BY created_at DESC,id DESC LIMIT ?
) recent ORDER BY created_at,id`, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row diagnosticBillingHold
			if err := rows.Scan(&row.ID, &row.RouteKeyHash, &row.AccountID, &row.EstimatedTokens, &row.Status, &row.UsageExpected, &row.UsageRecordedAt, &row.CreatedAt, &row.UpdatedAt); err != nil {
				return written, err
			}
			if err := cw.Write(codebook.safeCSVRow(billingHoldRows([]diagnosticBillingHold{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticAffinityCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook, snapshotNow int64) (int64, error) {
	header := []string{"route_key_hash", "route_key", "source", "account_code", "provider", "model", "egress_id", "epoch", "created_at", "updated_at"}
	return streamDiagnosticCSV(zw, "affinity_bindings.csv", header, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at FROM (
	SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at FROM (
		SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at FROM affinity_aliases WHERE expires_at>?
		UNION ALL
		SELECT b.route_key_hash,b.route_key,b.source,b.account_id,b.provider,b.model,b.egress_id,b.epoch,b.created_at,b.updated_at FROM affinity_bindings b
		WHERE (b.expires_at=0 OR b.expires_at>?) AND NOT EXISTS (
			SELECT 1 FROM affinity_aliases a WHERE a.route_key_hash=b.route_key_hash AND a.expires_at>?
		)
	) active ORDER BY updated_at DESC,route_key_hash DESC LIMIT ?
) recent ORDER BY updated_at,route_key_hash`, snapshotNow, snapshotNow, snapshotNow, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row storage.AffinityBinding
			if err = rows.Scan(&row.RouteKeyHash, &row.RouteKey, &row.Source, &row.AccountID, &row.Provider, &row.Model, &row.EgressID, &row.Epoch, &row.CreatedAt, &row.UpdatedAt); err != nil {
				return written, err
			}
			if err = cw.Write(codebook.safeCSVRow(affinityBindingRows([]storage.AffinityBinding{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticCodexUpstreamAttemptsCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook, store *storage.Store, snapshotNow int64) (int64, error) {
	header := []string{"tree_hmac_prefix", "account_code", "egress_id", "epoch", "state", "status_code", "created_at"}
	return streamDiagnosticCSV(zw, "codex_upstream_attempts.csv", header, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT tree_id,account_id,egress_id,epoch,state,status_code,created_at FROM (
	SELECT id,tree_id,account_id,egress_id,epoch,state,status_code,created_at
	FROM codex_upstream_attempt WHERE expires_at>? ORDER BY created_at DESC,id DESC LIMIT ?
) recent ORDER BY created_at,id`, snapshotNow, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var treeID string
			var row storage.CodexUpstreamAttemptDiagnostic
			if err = rows.Scan(&treeID, &row.AccountID, &row.EgressID, &row.Epoch, &row.State, &row.StatusCode, &row.CreatedAt); err != nil {
				return written, err
			}
			row.TreeHMACPrefix = store.CodexDiagnosticTreePrefix(treeID)
			if err = cw.Write(codebook.safeCSVRow(codexUpstreamAttemptDiagnosticRows([]storage.CodexUpstreamAttemptDiagnostic{row}, codebook)[0])); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func streamDiagnosticCodexUpstreamAttemptsDailyCSV(ctx context.Context, db storage.ReadQuerier, zw *zip.Writer, codebook diagnosticCodebook, snapshotNow int64) (int64, error) {
	header := []string{"day_start", "account_code", "egress_id", "state", "status_code", "attempt_count", "first_created_at", "last_created_at"}
	return streamDiagnosticCSV(zw, "codex_upstream_attempts_daily.csv", header, func(cw *csv.Writer) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT day_start,account_id,egress_id,state,status_code,attempt_count,first_created_at,last_created_at FROM (
	SELECT day_start,account_id,egress_id,state,status_code,attempt_count,first_created_at,last_created_at
	FROM codex_upstream_attempt_daily WHERE expires_at>?
	ORDER BY day_start DESC,account_id DESC,egress_id DESC,state DESC,status_code DESC LIMIT ?
) recent ORDER BY day_start,account_id,egress_id,state,status_code`, snapshotNow, diagnosticExportRowLimit)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var written int64
		for rows.Next() {
			var row storage.CodexUpstreamAttemptDailyDiagnostic
			if err = rows.Scan(&row.DayStart, &row.AccountID, &row.EgressID, &row.State, &row.StatusCode, &row.AttemptCount, &row.FirstCreatedAt, &row.LastCreatedAt); err != nil {
				return written, err
			}
			if err = cw.Write(codebook.safeCSVRow([]string{
				strconv.FormatInt(row.DayStart, 10), codebook.code(row.AccountID), row.EgressID, row.State,
				strconv.Itoa(row.StatusCode), strconv.FormatInt(row.AttemptCount, 10), strconv.FormatInt(row.FirstCreatedAt, 10), strconv.FormatInt(row.LastCreatedAt, 10),
			})); err != nil {
				return written, err
			}
			written++
		}
		return written, rows.Err()
	})
}

func buildDiagnosticsZipFiles(accounts []storage.Account, tokensByID map[string]storage.AccountToken, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding, egressProfiles []storage.EgressProfile, capabilities []storage.ModelCapability, kiroRuntimeCapabilities []storage.KiroRuntimeCapability, rateLimits []storage.AccountRateLimit, affinityBindings []storage.AffinityBinding, settings []diagnosticSetting, customProviders []storage.CustomProvider, upstreamRules []storage.UpstreamErrorRule, resetConsumptions []storage.CodexResetCreditConsumption, lifecycleStatuses []diagnosticLifecycleStatus, reauthConfigs []storage.AccountCodexReauthConfig, reauthJobs []storage.AccountCodexReauthJob, codebook diagnosticCodebook) (map[string]string, error) {
	files := map[string]string{}
	rowCounts := map[string]int{}
	addCSV := func(name string, header []string, rows [][]string) {
		safeRows := make([][]string, 0, len(rows))
		for _, row := range rows {
			safeRows = append(safeRows, codebook.safeCSVRow(row))
		}
		files[name] = csvString(header, safeRows)
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

	addCSV("account_auth_metadata.csv", []string{"account_code", "declared_provider", "effective_provider", "auth_method", "credential_mode", "billing_mode", "credential_present", "expires_at", "last_refresh", "oauth_rate_limit_tier", "created_at", "updated_at"}, accountAuthMetadataRows(accounts, tokensByID, codebook))
	addCSV("account_model_capabilities.csv", []string{"account_code", "model_slug", "availability_state", "context_1m_state", "context_1m_source", "native_context_window", "native_max_context_window", "source", "last_probe_at"}, modelCapabilityRows(capabilities, codebook))
	addCSV("kiro_runtime_capabilities.csv", []string{"account_code", "endpoint_hash", "model", "model_state", "thinking_state", "cache_capability", "observations", "metering_events", "cache_reported_observations", "cache_hit_observations", "consecutive_unreported", "unknown_cache_schema_json", "updated_at", "cache_point_state", "cache_reuse_state", "cache_reuse_evidence", "cache_reuse_credit_reduction_percent", "cache_reuse_probed_at"}, kiroRuntimeCapabilityRows(kiroRuntimeCapabilities, codebook))
	addCSV("account_rate_limits.csv", []string{"account_code", "provider", "model", "limiter_type", "source", "used_percent", "limit_tokens", "remaining_tokens", "limit_requests", "remaining_requests", "reset_at", "status", "raw_json", "updated_at"}, accountRateLimitRows(rateLimits, codebook))
	addCSV("affinity_bindings.csv", []string{"route_key_hash", "route_key", "source", "account_code", "provider", "model", "egress_id", "epoch", "created_at", "updated_at"}, affinityBindingRows(affinityBindings, codebook))
	// This legacy in-memory helper has no Store handle. Keep the safe CPA files
	// present with headers so callers see the same bundle schema; the streaming
	// admin export above fills them from encrypted mapping tables.
	addCSV("codex_session_mappings.csv", []string{"tree_hmac_prefix", "namespace_hmac_prefix", "account_code", "egress_id", "epoch", "state", "instruction_snapshot_present", "created_at", "updated_at", "expires_at"}, nil)
	addCSV("codex_instruction_snapshots.csv", []string{"tree_hmac_prefix", "revision_hmac_prefix", "created_at", "updated_at", "expires_at"}, nil)
	addCSV("codex_upstream_attempts.csv", []string{"tree_hmac_prefix", "account_code", "egress_id", "epoch", "state", "status_code", "created_at"}, nil)
	addCSV("codex_upstream_attempts_daily.csv", []string{"day_start", "account_code", "egress_id", "state", "status_code", "attempt_count", "first_created_at", "last_created_at"}, nil)
	addCSV("http_requests.csv", []string{"request_id", "method", "route", "status", "request_bytes", "response_bytes", "duration_ms", "created_at"}, nil)
	addCSV("route_attempts.csv", []string{"request_id", "tier", "target", "selection_type", "status_class", "fallback_target", "created_at"}, nil)
	addCSV("provider_attempts.csv", []string{"request_id", "account_code", "provider", "phase", "status", "error_class", "body_hash", "retry_after", "created_at"}, nil)
	addCSV("diagnostic_events.csv", []string{"event_code", "created_at", "event_type", "severity", "entity_type", "entity_alias", "detail_json", "diagnostic_gap"}, nil)
	addCSV("codex_group_policy_revisions.csv", []string{"group_name", "instructions_enabled", "instruction_file_count", "policy_revision_hmac_prefix", "updated_at"}, nil)
	addCSV("sidecar_status.csv", []string{"sidecar_egress_id", "real_egress_id", "health", "profile_max_concurrency", "adaptive_limit", "inflight", "queue_depth", "recent_failures", "circuit_state", "circuit_until", "bypass_until", "cooldown_until", "bound_account_count", "created_at", "updated_at"}, nil)
	addCSV("settings.csv", []string{"key", "value", "updated_at"}, settingRows(settings))
	addCSV("custom_providers.csv", []string{"id", "name", "base_url", "upstream_protocol", "enabled", "auto_discover_models", "models", "model_mappings", "created_at", "updated_at"}, customProviderRows(customProviders))
	addCSV("upstream_error_rules.csv", []string{"id", "name", "enabled", "priority", "providers", "entrypoints", "model_patterns", "status_codes", "body_keywords", "match_mode", "account_action", "downstream_action", "response_status", "custom_message", "cooldown_seconds", "prefer_retry_after", "idle_seconds", "idle_ping_seconds", "skip_log", "description", "created_at", "updated_at"}, upstreamErrorRuleRows(upstreamRules))
	addCSV("codex_reset_credit_consumptions.csv", []string{"account_code", "seven_day_reset_at", "redeem_request_id", "status", "created_at", "updated_at"}, codexResetCreditRows(resetConsumptions, codebook))
	addCSV("account_lifecycle_status.csv", []string{"account_code", "validity_status", "subscription_tier", "subscription_expires_at", "last_health_check_at", "last_token_refresh_at", "health_check_fail_count", "summary_json", "created_at", "updated_at"}, lifecycleStatusRows(lifecycleStatuses, codebook))
	addCSV("codex_reauth_config.csv", []string{"account_code", "login_email_present", "password_configured", "otp_url_configured", "target_workspace_id", "auto_enabled", "last_status", "last_error", "created_at", "updated_at"}, codexReauthConfigRows(reauthConfigs, codebook))
	addCSV("codex_reauth_jobs.csv", []string{"id", "account_code", "status", "reason", "last_error", "created_at", "updated_at", "started_at", "finished_at"}, codexReauthJobRows(reauthJobs, codebook))
	addCSV("audit_log.csv", []string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, auditLogRows(auditRows, codebook))
	addCSV("cf_events.csv", []string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, cfEventRows(cfRows, codebook))
	addCSV("usage_records.csv", []string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "usage_provider", "usage_source", "cache_read_present", "cache_creation_present", "compatibility_losses_json", "cache_capability", "estimated", "cache_miss_tokens", "cache_total_input_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "affinity_source", "route_class", "prompt_cache_key_present", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "cache_breakpoints_json", "unwritten_tail_tokens", "max_possible_cache_read_tokens", "cache_hit_after_prewarm", "singleflight_waited_requests", "diagnostics_miss_reason", "latest_user_cache_control", "latest_user_auto_context_cache_control", "latest_user_tail_cache_control", "latest_user_tool_result_cache_control", "route_epoch", "raw_usage_json", "kiro_credits", "kiro_credits_present", "billing_hold_id", "requested_model", "resolved_model", "model_override_source"}, usageRecordRows(usageRows, codebook))
	addCSV("billing_holds.csv", []string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status", "usage_expected", "usage_recorded_at"}, billingHoldRows(holds, codebook))
	addCSV("accounts_snapshot.csv", []string{"account_code", "group_name", "declared_provider", "effective_provider", "status", "plan_type", "is_fedramp", "ignore_rate_limit_controls", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "sidecar_egress_id", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, tokensByID, bindings, codebook))
	addCSV("egress_snapshot.csv", []string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
	if err := addJSON("diagnostic_summary.json", diagnosticSummary(accounts, tokensByID, auditRows, holds, bindings, rateLimits)); err != nil {
		return nil, err
	}
	if err := addJSON("runtime_storage.json", map[string]interface{}{
		"budget": bodysource.BudgetSnapshot{}, "filesystem": bodysource.DiskReserverSnapshot{},
		"rejection_counts": map[string]int64{},
	}); err != nil {
		return nil, err
	}
	rowCounts["manifest.json"] = 1
	manifest := map[string]interface{}{
		"generated_at":                       time.Now().Unix(),
		"format":                             "codex-pool-diagnostics-v3",
		"account_count":                      len(accounts),
		"current_account_count":              len(accounts),
		"historical_reference_account_count": len(codebook.byID),
		"build":                              diagnosticBuildInfo(),
		"table_time_ranges":                  diagnosticTableTimeRanges(accounts, auditRows, cfRows, usageRows, holds),
		"files":                              diagnosticFileOrder(),
		"row_counts":                         rowCounts,
		"account_redaction":                  "all entity identifiers use type-isolated stable HMAC aliases; no reverse map or alias key is included",
		"account_code_format":                "ACC-Base32(HMAC-SHA256(alias_key,domain||entity_type||raw_id)[:16])",
		"public_request_ids":                 "server-generated REQ- plus 16 hexadecimal request IDs are retained for support correlation; upstream request IDs remain aliased",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = string(rawManifest) + "\n"
	for name, content := range files {
		files[name] = codebook.sanitize(content)
	}
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

func listDiagnosticAuditRows(ctx context.Context, db storage.ReadQuerier) ([]storage.AuditLogRow, error) {
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

func listDiagnosticCFEvents(ctx context.Context, db storage.ReadQuerier) ([]storage.CFEvent, error) {
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

func listDiagnosticUsageRecords(ctx context.Context, db storage.ReadQuerier) ([]diagnosticUsageRecord, error) {
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
raw_usage_json, created_at, kiro_credits, kiro_credits_present, billing_hold_id, requested_model, resolved_model, model_override_source FROM usage_records`
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
		&r.RawUsageJSON, &r.CreatedAt, &r.KiroCredits, &r.KiroCreditsPresent, &r.BillingHoldID, &r.RequestedModel, &r.ResolvedModel, &r.ModelOverrideSource)
}

func listDiagnosticBillingHolds(ctx context.Context, db storage.ReadQuerier) ([]diagnosticBillingHold, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, route_key_hash, account_id, estimated_tokens, status, usage_expected, usage_recorded_at, created_at, updated_at FROM billing_holds ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticBillingHold
	for rows.Next() {
		var h diagnosticBillingHold
		if err := rows.Scan(&h.ID, &h.RouteKeyHash, &h.AccountID, &h.EstimatedTokens, &h.Status, &h.UsageExpected, &h.UsageRecordedAt, &h.CreatedAt, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func listDiagnosticAffinityBindings(ctx context.Context, db storage.ReadQuerier) ([]storage.AffinityBinding, error) {
	now := storage.Now()
	rows, err := db.QueryContext(ctx, `SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at FROM (
	SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at FROM affinity_aliases WHERE expires_at>?
	UNION ALL
	SELECT b.route_key_hash,b.route_key,b.source,b.account_id,b.provider,b.model,b.egress_id,b.epoch,b.created_at,b.updated_at FROM affinity_bindings b
	WHERE (b.expires_at=0 OR b.expires_at>?) AND NOT EXISTS (
		SELECT 1 FROM affinity_aliases a WHERE a.route_key_hash=b.route_key_hash AND a.expires_at>?
	)
	) ORDER BY updated_at ASC,route_key_hash ASC`, now, now, now)
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

func listDiagnosticSettings(ctx context.Context, db storage.ReadQuerier) ([]diagnosticSetting, error) {
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

// diagnosticGoalPolicy exports the effective, non-secret Goal controls even when
// they came from startup config and therefore have no row in settings.csv. Support
// bundles previously showed thousands of storage_budget events without exposing
// the active limit, making a full diagnostic impossible. Values are resolved from
// the snapshot's runtime overrides, so the export stays internally consistent and
// has no effect on downstream CLI requests.
func diagnosticGoalPolicy(rows []diagnosticSetting, cfg config.Config) map[string]interface{} {
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Key] = strings.TrimSpace(row.Value)
	}
	sources := map[string]string{}
	raw := func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok && value != ""
	}
	boolean := func(key string, fallback bool) bool {
		if value, ok := raw(key); ok {
			switch strings.ToLower(value) {
			case "1", "true", "on", "yes":
				sources[key] = "runtime_setting"
				return true
			case "0", "false", "off", "no":
				sources[key] = "runtime_setting"
				return false
			}
		}
		sources[key] = "bootstrap_config"
		return fallback
	}
	integer := func(key string, fallback int) int {
		if value, ok := raw(key); ok {
			if parsed, err := strconv.Atoi(value); err == nil {
				sources[key] = "runtime_setting"
				return parsed
			}
		}
		sources[key] = "bootstrap_config"
		return fallback
	}
	floating := func(key string, fallback float64) float64 {
		if value, ok := raw(key); ok {
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				sources[key] = "runtime_setting"
				return parsed
			}
		}
		sources[key] = "bootstrap_config"
		return fallback
	}

	retentionDays := integer("goal_retention_days", cfg.GoalRetentionDays)
	if retentionDays <= 0 {
		retentionDays = 7
	}
	storageMaxMB := integer("goal_storage_max_mb", cfg.GoalStorageMaxMB)
	if storageMaxMB <= 0 {
		storageMaxMB = 256
	}
	chunkRatio := floating("goal_compression_chunk_ratio", cfg.GoalCompressionChunkRatio)
	if chunkRatio <= 0 || chunkRatio > 1 {
		chunkRatio = 0.70
	}
	compressionStages := integer("goal_compression_max_stages", cfg.GoalCompressionMaxStages)
	if compressionStages <= 0 {
		compressionStages = 16
	}
	compressionConcurrency := integer("goal_compression_concurrency", cfg.GoalCompressionConcurrency)
	if compressionConcurrency <= 0 {
		compressionConcurrency = 1
	}
	if compressionConcurrency > 32 {
		compressionConcurrency = 32
	}
	leaseSeconds := integer("goal_lease_seconds", cfg.GoalLeaseSeconds)
	if leaseSeconds <= 0 {
		leaseSeconds = 90
	}
	heartbeatSeconds := integer("goal_heartbeat_seconds", cfg.GoalHeartbeatSeconds)
	if heartbeatSeconds <= 0 {
		heartbeatSeconds = 15
	}
	if heartbeatSeconds >= leaseSeconds {
		heartbeatSeconds = leaseSeconds / 2
	}
	if heartbeatSeconds <= 0 {
		heartbeatSeconds = 15
	}
	storageMaxBytes := int64(storageMaxMB) << 20
	storageTargetBytes, storageReserveBytes := goalStorageMaintenanceTarget(storageMaxBytes)

	return map[string]interface{}{
		"continuity_enabled":                boolean("goal_continuity_enabled", cfg.GoalContinuityEnabled),
		"legacy_journal_dual_write":         boolean("goal_legacy_journal_dual_write", cfg.GoalLegacyJournalDualWrite),
		"retention_days":                    retentionDays,
		"storage_max_mb":                    storageMaxMB,
		"storage_max_bytes":                 storageMaxBytes,
		"storage_maintenance_target_bytes":  storageTargetBytes,
		"storage_maintenance_reserve_bytes": storageReserveBytes,
		"compression_chunk_ratio":           chunkRatio,
		"compression_max_stages":            compressionStages,
		"compression_concurrency":           compressionConcurrency,
		"lease_seconds":                     leaseSeconds,
		"heartbeat_seconds":                 heartbeatSeconds,
		"sources":                           sources,
	}
}

func listDiagnosticTokenMetadata(ctx context.Context, db storage.ReadQuerier) (map[string]storage.AccountToken, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_id,auth_method,credential_mode,
 CASE WHEN access_token<>'' THEN 1 ELSE 0 END,
 CASE WHEN refresh_token<>'' THEN 1 ELSE 0 END,
 CASE WHEN openai_api_key<>'' THEN 1 ELSE 0 END,
 CASE WHEN id_token_raw<>'' THEN 1 ELSE 0 END,
 CASE WHEN agent_runtime_id<>'' THEN 1 ELSE 0 END,
 CASE WHEN agent_private_key<>'' THEN 1 ELSE 0 END,
 CASE WHEN agent_task_id<>'' THEN 1 ELSE 0 END,
 last_refresh,expires_at,oauth_rate_limit_tier,created_at,updated_at
FROM account_auth_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storage.AccountToken{}
	for rows.Next() {
		var (
			token                                                 storage.AccountToken
			access, refresh, apiKey, idToken, runtime, privateKey int
			task                                                  int
		)
		if err := rows.Scan(
			&token.AccountID, &token.AuthMethod, &token.CredentialMode,
			&access, &refresh, &apiKey, &idToken, &runtime, &privateKey, &task,
			&token.LastRefresh, &token.ExpiresAt, &token.OAuthRateLimitTier,
			&token.CreatedAt, &token.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if access != 0 {
			token.AccessToken = "configured"
		}
		if refresh != 0 {
			token.RefreshToken = "configured"
		}
		if apiKey != 0 {
			token.OpenAIAPIKey = "configured"
		}
		if idToken != 0 {
			token.IDTokenRaw = "configured"
		}
		if runtime != 0 {
			token.AgentRuntimeID = "configured"
		}
		if privateKey != 0 {
			token.AgentPrivateKey = "configured"
		}
		if task != 0 {
			token.AgentTaskID = "configured"
		}
		out[token.AccountID] = token
	}
	return out, rows.Err()
}

func listDiagnosticCodexResetCreditConsumptions(ctx context.Context, db storage.ReadQuerier) ([]storage.CodexResetCreditConsumption, error) {
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

func listDiagnosticLifecycleStatuses(ctx context.Context, db storage.ReadQuerier) ([]diagnosticLifecycleStatus, error) {
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

func listDiagnosticCodexReauthConfigs(ctx context.Context, db storage.ReadQuerier) ([]storage.AccountCodexReauthConfig, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_id,
 CASE WHEN login_email<>'' THEN 1 ELSE 0 END,
 CASE WHEN encrypted_password<>'' THEN 1 ELSE 0 END,
 CASE WHEN encrypted_otp_url<>'' THEN 1 ELSE 0 END,
 target_workspace_id,auto_enabled,last_status,last_error,created_at,updated_at
FROM account_codex_reauth_config ORDER BY account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AccountCodexReauthConfig
	for rows.Next() {
		var cfg storage.AccountCodexReauthConfig
		var auto, loginEmail, password, otp int
		if err := rows.Scan(&cfg.AccountID, &loginEmail, &password, &otp, &cfg.TargetWorkspaceID, &auto, &cfg.LastStatus, &cfg.LastError, &cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
			return nil, err
		}
		cfg.AutoEnabled = auto != 0
		if loginEmail != 0 {
			cfg.LoginEmail = "configured"
		}
		cfg.PasswordConfigured = password != 0
		cfg.OTPURLConfigured = otp != 0
		out = append(out, cfg)
	}
	return out, rows.Err()
}

func listDiagnosticCodexReauthJobs(ctx context.Context, db storage.ReadQuerier) ([]storage.AccountCodexReauthJob, error) {
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
	return buildDiagnosticCodebookWithKey([]byte("diagnostic-test-alias-key-v3"), accounts, auditRows, cfRows, usageRows, holds, bindings)
}

func buildDiagnosticCodebookWithKey(aliasKey []byte, accounts []storage.Account, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding) diagnosticCodebook {
	if len(aliasKey) == 0 {
		// Production startup always provisions an independent persistent alias key.
		// This fallback is deterministic only for legacy in-memory test helpers.
		aliasKey = []byte("diagnostic-test-alias-key-v3")
	}
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
	for _, id := range ids {
		info := identities[id]
		info.Code = diagnosticAlias(aliasKey, "ACC", "account", id)
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
	var replacementRoot *diagnosticReplacementNode
	if len(replacements) > 0 {
		replacementRoot = &diagnosticReplacementNode{children: map[byte]*diagnosticReplacementNode{}}
	}
	for _, repl := range replacements {
		node := replacementRoot
		for index := 0; index < len(repl.Needle); index++ {
			next := node.children[repl.Needle[index]]
			if next == nil {
				next = &diagnosticReplacementNode{children: map[byte]*diagnosticReplacementNode{}}
				node.children[repl.Needle[index]] = next
			}
			node = next
		}
		node.replacement = repl
		node.hasReplacement = true
	}
	return diagnosticCodebook{byID: identities, replacements: replacements, replacement: replacementRoot, aliasKey: append([]byte(nil), aliasKey...)}
}

var diagnosticBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func diagnosticAlias(aliasKey []byte, prefix, entityType, rawID string) string {
	mac := hmac.New(sha256.New, aliasKey)
	_, _ = mac.Write([]byte("codex-pool-diagnostic-alias-v3\x00"))
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(entityType))))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(rawID))
	sum := mac.Sum(nil)
	return strings.ToUpper(strings.TrimSpace(prefix)) + "-" + diagnosticBase32.EncodeToString(sum[:16])
}

func (b diagnosticCodebook) code(accountID string) string {
	if info, ok := b.byID[accountID]; ok {
		return info.Code
	}
	return ""
}

func (b diagnosticCodebook) sanitize(text string) string {
	text = b.replaceIdentities(text)
	text = diagnosticPrivateKeyRE.ReplaceAllString(text, "[REDACTED-CREDENTIAL]")
	text = diagnosticBearerRE.ReplaceAllString(text, "Bearer [REDACTED-CREDENTIAL]")
	text = diagnosticJWTRE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "TOKEN", "credential", value)
	})
	text = diagnosticSecretPrefixRE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "TOKEN", "credential", value)
	})
	text = diagnosticRequestIDRE.ReplaceAllStringFunc(text, func(value string) string {
		if diagnosticPublicRequestIDRE.MatchString(value) {
			return strings.ToUpper(value)
		}
		if diagnosticStableAliasRE.MatchString(strings.ToUpper(value)) {
			return value
		}
		return diagnosticAlias(b.aliasKey, "REQ", "request", value)
	})
	text = diagnosticEmailRE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "EMAIL", "email", strings.ToLower(value))
	})
	text = diagnosticURLRE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "URL", "url", value)
	})
	text = diagnosticIPv4RE.ReplaceAllStringFunc(text, func(value string) string {
		if net.ParseIP(value) == nil {
			return value
		}
		return diagnosticAlias(b.aliasKey, "IP", "ip", value)
	})
	text = diagnosticIPv6RE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "IP", "ip", strings.ToLower(value))
	})
	text = diagnosticWindowsPathRE.ReplaceAllStringFunc(text, func(value string) string {
		return diagnosticAlias(b.aliasKey, "PATH", "path", value)
	})
	text = diagnosticUnixPathRE.ReplaceAllStringFunc(text, func(value string) string {
		prefix := ""
		path := value
		if !strings.HasPrefix(value, "/") {
			prefix, path = value[:1], value[1:]
		}
		return prefix + diagnosticAlias(b.aliasKey, "PATH", "path", path)
	})
	text = diagnosticHighEntropyRE.ReplaceAllStringFunc(text, func(value string) string {
		if !diagnosticHighEntropyCandidate(value) {
			return value
		}
		return diagnosticAlias(b.aliasKey, "TOKEN", "unknown-string", value)
	})
	return text
}

func (b diagnosticCodebook) replaceIdentities(text string) string {
	if b.replacement == nil || text == "" {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	changed := false
	for index := 0; index < len(text); {
		node := b.replacement
		var match diagnosticReplacement
		matchEnd := index
		for cursor := index; cursor < len(text); cursor++ {
			node = node.children[text[cursor]]
			if node == nil {
				break
			}
			if node.hasReplacement && diagnosticIdentityBoundaries(text, index, cursor+1, node.replacement.Needle) {
				match = node.replacement
				matchEnd = cursor + 1
			}
		}
		if matchEnd > index {
			out.WriteString(match.Code)
			index = matchEnd
			changed = true
			continue
		}
		out.WriteByte(text[index])
		index++
	}
	if !changed {
		return text
	}
	return out.String()
}

func diagnosticIdentityBoundaries(text string, start, end int, needle string) bool {
	first, _ := utf8.DecodeRuneInString(needle)
	if diagnosticIdentityWordRune(first) && start > 0 {
		previous, _ := utf8.DecodeLastRuneInString(text[:start])
		if diagnosticIdentityWordRune(previous) {
			return false
		}
	}
	last, _ := utf8.DecodeLastRuneInString(needle)
	if diagnosticIdentityWordRune(last) && end < len(text) {
		next, _ := utf8.DecodeRuneInString(text[end:])
		if diagnosticIdentityWordRune(next) {
			return false
		}
	}
	return true
}

func diagnosticIdentityWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsNumber(value)
}

func diagnosticHighEntropyCandidate(value string) bool {
	if len(value) < 40 || diagnosticStableAliasRE.MatchString(strings.ToUpper(value)) {
		return false
	}
	lowerSnakeCase := true
	underscoreCount := 0
	counts := map[rune]int{}
	runeCount := 0
	for _, character := range value {
		runeCount++
		counts[character]++
		if character == '_' {
			underscoreCount++
		}
		if (character < 'a' || character > 'z') && character != '_' {
			lowerSnakeCase = false
		}
	}
	// Long schema fields and enum values are identifiers, not secrets. The old
	// regexp classified any 40-byte snake_case value as high entropy and
	// destroyed otherwise useful diagnostic state.
	if lowerSnakeCase && underscoreCount >= 2 {
		return false
	}
	if runeCount == 0 {
		return false
	}
	var entropy float64
	for _, count := range counts {
		probability := float64(count) / float64(runeCount)
		entropy -= probability * (math.Log(probability) / math.Ln2)
	}
	return entropy >= 3.5
}

func diagnosticContainsHighEntropy(value string) bool {
	for _, candidate := range diagnosticHighEntropyRE.FindAllString(value, -1) {
		if diagnosticHighEntropyCandidate(candidate) {
			return true
		}
	}
	return false
}

var (
	diagnosticPrivateKeyRE      = regexp.MustCompile(`(?is)-----BEGIN[ A-Z0-9_-]{0,40}PRIVATE KEY-----.*?-----END[ A-Z0-9_-]{0,40}PRIVATE KEY-----`)
	diagnosticBearerRE          = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`)
	diagnosticJWTRE             = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	diagnosticSecretPrefixRE    = regexp.MustCompile(`(?i)\b(?:sk|rk|ghp|github_pat|xox[baprs]|ya29|AIza)[_-][A-Za-z0-9._~+/=-]{12,}`)
	diagnosticRequestIDRE       = regexp.MustCompile(`(?i)\breq[_-][A-Za-z0-9][A-Za-z0-9_-]{11,}\b`)
	diagnosticPublicRequestIDRE = regexp.MustCompile(`(?i)^REQ-[A-F0-9]{16}$`)
	diagnosticEmailRE           = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	diagnosticURLRE             = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s,"'<>]+`)
	diagnosticIPv4RE            = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	diagnosticWindowsPathRE     = regexp.MustCompile(`(?i)\b[A-Z]:\\(?:[^\\\s,"'<>]+\\)*[^\\\s,"'<>]*`)
	diagnosticUnixPathRE        = regexp.MustCompile(`(?:^|[\s="'(])(/[A-Za-z0-9._~+-]+(?:/[A-Za-z0-9._~+:-]+)+)`)
	diagnosticHighEntropyRE     = regexp.MustCompile(`[A-Za-z0-9_+/=-]{40,}`)
	diagnosticStableAliasRE     = regexp.MustCompile(`^(?:ACC|EGR|GRP|USR|KEY|REQ|SES|TSK|HST|JOB|ENT|EVT|EMAIL|URL|IP|PATH|TOKEN|TEXT|FIELD)-[A-Z2-7]{26}$`)
)

func diagnosticSafeCSVCell(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	default:
		return value
	}
}

func (b diagnosticCodebook) safeCSVRow(row []string) []string {
	out := make([]string, len(row))
	for index, value := range row {
		out[index] = diagnosticSafeCSVCell(b.sanitize(value))
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
		provider := accountprovider.EffectiveProvider(account.Provider, token, found)
		out = append(out, []string{
			codebook.code(account.ID),
			account.Provider,
			provider,
			accountprovider.EffectiveAuthMethod(provider, token),
			token.CredentialMode,
			accountprovider.BillingMode(provider, token),
			strconv.FormatBool(strings.TrimSpace(accountprovider.Credential(provider, token)) != "" || accountprovider.IsAgentIdentity(token)),
			itoa64(token.ExpiresAt),
			itoa64(token.LastRefresh),
			token.OAuthRateLimitTier,
			itoa64(token.CreatedAt),
			itoa64(token.UpdatedAt),
		})
	}
	return out
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
			row.AvailabilityState,
			row.Context1MState,
			row.Context1MSource,
			itoa64(row.NativeContextWindow),
			itoa64(row.NativeMaxContextWindow),
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

func codexSessionMappingDiagnosticRows(rows []storage.CodexSessionMappingDiagnostic, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.TreeHMACPrefix,
			row.NamespaceHMACPrefix,
			codebook.code(row.AccountID),
			row.EgressID,
			itoa64(row.Epoch),
			row.State,
			strconv.FormatBool(row.SnapshotPresent),
			itoa64(row.CreatedAt),
			itoa64(row.UpdatedAt),
			itoa64(row.ExpiresAt),
		})
	}
	return out
}

func codexInstructionSnapshotDiagnosticRows(rows []storage.CodexInstructionSnapshotDiagnostic) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{row.TreeHMACPrefix, row.RevisionPrefix, itoa64(row.CreatedAt), itoa64(row.UpdatedAt), itoa64(row.ExpiresAt)})
	}
	return out
}

func codexUpstreamAttemptDiagnosticRows(rows []storage.CodexUpstreamAttemptDiagnostic, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.TreeHMACPrefix,
			codebook.code(row.AccountID),
			row.EgressID,
			itoa64(row.Epoch),
			row.State,
			strconv.Itoa(row.StatusCode),
			itoa64(row.CreatedAt),
		})
	}
	return out
}

func codexGroupPolicyRevisionRows(groups []storage.Group, s *Server) [][]string {
	rows := append([]storage.Group(nil), groups...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	out := make([][]string, 0, len(rows))
	for _, group := range rows {
		revision := s.codexGroupInstructionPolicyRevision(group)
		if len(revision) > 16 {
			revision = revision[:16]
		}
		out = append(out, []string{
			group.Name,
			strconv.FormatBool(group.ModelInstructionsEnabled),
			strconv.Itoa(len(group.ModelInstructionsFiles)),
			revision,
			itoa64(group.UpdatedAt),
		})
	}
	return out
}

// sidecarStatusRows deliberately reports only durable profile state. It contains
// no endpoint, proxy URL, cookie key, raw error body, or client identifier; the
// egress id is already the safe operator-facing reference used throughout the
// diagnostic bundle.
func sidecarStatusRows(profiles []storage.EgressProfile, bindings []storage.AccountEgressBinding, adaptive []upstream.SidecarAdaptiveStatus) [][]string {
	bound := map[string]int{}
	for _, binding := range bindings {
		if id := strings.TrimSpace(binding.SidecarEgressID); id != "" {
			bound[id]++
		}
	}
	profilesByID := map[string]storage.EgressProfile{}
	for _, profile := range profiles {
		profilesByID[profile.ID] = profile
	}
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if storage.IsSidecarEgress(profile) {
			ids = append(ids, profile.ID)
		}
	}
	sort.Strings(ids)
	out := make([][]string, 0, len(ids)+len(adaptive))
	for _, id := range ids {
		profile := profilesByID[id]
		if !storage.IsSidecarEgress(profile) {
			continue
		}
		out = append(out, []string{
			profile.ID,
			"",
			profile.Health,
			strconv.Itoa(profile.MaxConcurrency),
			"", "", "", "", "", "", "",
			itoa64(profile.CooldownUntil),
			strconv.Itoa(bound[profile.ID]),
			itoa64(profile.CreatedAt),
			itoa64(profile.UpdatedAt),
		})
	}
	for _, status := range adaptive {
		profile := profilesByID[status.SidecarEgressID]
		out = append(out, []string{
			status.SidecarEgressID,
			status.RealEgressID,
			profile.Health,
			strconv.Itoa(profile.MaxConcurrency),
			strconv.Itoa(status.Limit),
			strconv.Itoa(status.Inflight),
			strconv.Itoa(status.QueueDepth),
			strconv.Itoa(status.RecentFailures),
			status.CircuitState,
			itoa64(status.CircuitUntil),
			itoa64(status.BypassUntil),
			itoa64(profile.CooldownUntil),
			strconv.Itoa(bound[status.SidecarEgressID]),
			itoa64(profile.CreatedAt),
			itoa64(status.UpdatedAt),
		})
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
	for _, sig := range []string{"token", "secret", "password", "cookie", "admin", "api_key", "apikey", "proxy", "private_key", "dsn", "redis_url", "database_url"} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

func customProviderRows(rows []storage.CustomProvider) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		modelMappings, _ := json.Marshal(row.ModelMappings)
		out = append(out, []string{
			row.ID,
			row.Name,
			redactURLUserinfo(row.BaseURL),
			row.UpstreamProtocol,
			strconv.FormatBool(row.Enabled),
			strconv.FormatBool(row.AutoDiscoverModels),
			strings.Join(row.Models, " "),
			string(modelMappings),
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
			floatString(row.KiroCredits),
			itoa64(row.KiroCreditsPresent),
			codebook.sanitize(row.BillingHoldID),
			row.RequestedModel,
			row.ResolvedModel,
			row.ModelOverrideSource,
		})
	}
	return out
}

func billingHoldRows(rows []diagnosticBillingHold, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{codebook.sanitize(row.ID), itoa64(row.CreatedAt), itoa64(row.UpdatedAt), codebook.code(row.AccountID), row.RouteKeyHash, itoa64(row.EstimatedTokens), row.Status, itoa64(row.UsageExpected), itoa64(row.UsageRecordedAt)})
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
			strconv.FormatBool(a.IgnoreRateLimitControls),
			itoa64(a.QuarantineUntil),
			codebook.sanitize(a.QuarantineReason),
			itoa64(a.CreatedAt),
			itoa64(a.UpdatedAt),
			b.PrimaryEgressID,
			b.StandbyEgressIDs,
			b.SidecarEgressID,
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
			if sidecarID := strings.TrimSpace(b.SidecarEgressID); sidecarID != "" && sidecarID != b.PrimaryEgressID {
				bound[sidecarID] = append(bound[sidecarID], code)
			}
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
		safe := make([]string, len(row))
		for index, value := range row {
			safe[index] = diagnosticSafeCSVCell(value)
		}
		_ = cw.Write(safe)
	}
	cw.Flush()
	return buf.String()
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
