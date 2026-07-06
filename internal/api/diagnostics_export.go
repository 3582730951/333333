package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

type diagnosticUsageRecord struct {
	ID                     int64
	AccountID              string
	RouteKeyHash           string
	APIKeyHash             string
	UserID                 string
	Model                  string
	PromptTokens           int64
	CompletionTokens       int64
	TotalTokens            int64
	CachedTokens           int64
	CacheReadTokens        int64
	CacheCreationTokens    int64
	UsageProvider          string
	Estimated              int64
	CacheMissTokens        int64
	CacheTotalInputTokens  int64
	CacheCreation5mTokens  int64
	CacheCreation1hTokens  int64
	AffinitySource         string
	PromptCacheKeyPresent  int64
	PromptCacheKeySource   string
	StablePrefixSource     string
	StablePrefixReason     string
	StablePrefixBytes      int64
	RetentionEffective     string
	RetentionSource        string
	ClaudeCacheTTL         string
	CacheControlInjected   int64
	CacheBreakpointCount   int64
	LatestUserCacheControl int64
	RouteEpoch             int64
	RawUsageJSON           string
	CreatedAt              int64
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

func (s *Server) adminDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx := r.Context()
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	auditRows, err := listDiagnosticAuditRows(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfRows, err := listDiagnosticCFEvents(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	usageRows, err := listDiagnosticUsageRecords(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	holds, err := listDiagnosticBillingHolds(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	bindings, err := s.store.ListEgressBindings(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	egressProfiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accountIDs := accountIDsForDiagnostics(accounts)
	tokensByID, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	capabilities, err := s.store.ListCapabilities(ctx, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rateLimits, err := s.store.ListAccountRateLimits(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	affinityBindings, err := listDiagnosticAffinityBindings(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	settings, err := listDiagnosticSettings(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	customProviders, err := s.store.ListCustomProviders(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	upstreamRules, err := s.store.ListUpstreamErrorRules(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resetConsumptions, err := listDiagnosticCodexResetCreditConsumptions(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	lifecycleStatuses, err := listDiagnosticLifecycleStatuses(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reauthConfigs, err := listDiagnosticCodexReauthConfigs(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reauthJobs, err := listDiagnosticCodexReauthJobs(ctx, s.store.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	codebook := buildDiagnosticCodebook(accounts, auditRows, cfRows, usageRows, holds, bindings)
	files, err := buildDiagnosticsZipFiles(accounts, tokensByID, auditRows, cfRows, usageRows, holds, bindings, egressProfiles, capabilities, rateLimits, affinityBindings, settings, customProviders, upstreamRules, resetConsumptions, lifecycleStatuses, reauthConfigs, reauthJobs, codebook)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range diagnosticFileOrder() {
		content, ok := files[name]
		if !ok {
			continue
		}
		fw, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics-v2.zip"`)
	_, _ = w.Write(buf.Bytes())
}

func diagnosticFileOrder() []string {
	return []string{
		"manifest.json",
		"diagnostic_summary.json",
		"account_map.csv",
		"account_auth_metadata.csv",
		"account_model_capabilities.csv",
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

func buildDiagnosticsZipFiles(accounts []storage.Account, tokensByID map[string]storage.AccountToken, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding, egressProfiles []storage.EgressProfile, capabilities []storage.ModelCapability, rateLimits []storage.AccountRateLimit, affinityBindings []storage.AffinityBinding, settings []diagnosticSetting, customProviders []storage.CustomProvider, upstreamRules []storage.UpstreamErrorRule, resetConsumptions []storage.CodexResetCreditConsumption, lifecycleStatuses []diagnosticLifecycleStatus, reauthConfigs []storage.AccountCodexReauthConfig, reauthJobs []storage.AccountCodexReauthJob, codebook diagnosticCodebook) (map[string]string, error) {
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

	addCSV("account_map.csv", []string{"account_code", "account_id", "email", "label", "upstream_account_id", "chatgpt_user_id", "declared_provider", "group_name", "status"}, accountMapRows(codebook))
	addCSV("account_auth_metadata.csv", []string{"account_code", "declared_provider", "effective_provider", "token_provider_hint", "access_token_present", "access_token_len", "access_token_type", "refresh_token_present", "refresh_token_len", "openai_api_key_present", "openai_api_key_len", "openai_api_key_type", "id_token_present", "id_token_len", "scopes", "expires_at", "last_refresh", "oauth_rate_limit_tier", "created_at", "updated_at"}, accountAuthMetadataRows(accounts, tokensByID, codebook))
	addCSV("account_model_capabilities.csv", []string{"account_code", "model_slug", "native_context_window", "native_max_context_window", "effective_context_window_percent", "auto_compact_token_limit", "visibility", "etag", "raw_model_json_hash", "source", "last_probe_at"}, modelCapabilityRows(capabilities, codebook))
	addCSV("account_rate_limits.csv", []string{"account_code", "provider", "model", "limiter_type", "source", "used_percent", "limit_tokens", "remaining_tokens", "limit_requests", "remaining_requests", "reset_at", "status", "raw_json", "updated_at"}, accountRateLimitRows(rateLimits, codebook))
	addCSV("affinity_bindings.csv", []string{"route_key_hash", "route_key", "source", "account_code", "epoch", "created_at", "updated_at"}, affinityBindingRows(affinityBindings, codebook))
	addCSV("settings.csv", []string{"key", "value", "updated_at"}, settingRows(settings))
	addCSV("custom_providers.csv", []string{"id", "name", "base_url", "upstream_protocol", "enabled", "auto_discover_models", "models", "created_at", "updated_at"}, customProviderRows(customProviders))
	addCSV("upstream_error_rules.csv", []string{"id", "name", "enabled", "priority", "providers", "entrypoints", "model_patterns", "status_codes", "body_keywords", "match_mode", "account_action", "downstream_action", "response_status", "custom_message", "cooldown_seconds", "prefer_retry_after", "idle_seconds", "idle_ping_seconds", "skip_log", "description", "created_at", "updated_at"}, upstreamErrorRuleRows(upstreamRules))
	addCSV("codex_reset_credit_consumptions.csv", []string{"account_code", "seven_day_reset_at", "redeem_request_id", "status", "created_at", "updated_at"}, codexResetCreditRows(resetConsumptions, codebook))
	addCSV("account_lifecycle_status.csv", []string{"account_code", "validity_status", "subscription_tier", "subscription_expires_at", "last_health_check_at", "last_token_refresh_at", "health_check_fail_count", "summary_json", "created_at", "updated_at"}, lifecycleStatusRows(lifecycleStatuses, codebook))
	addCSV("codex_reauth_config.csv", []string{"account_code", "login_email_present", "password_configured", "otp_url_configured", "target_workspace_id", "auto_enabled", "last_status", "last_error", "created_at", "updated_at"}, codexReauthConfigRows(reauthConfigs, codebook))
	addCSV("codex_reauth_jobs.csv", []string{"id", "account_code", "status", "reason", "last_error", "created_at", "updated_at", "started_at", "finished_at"}, codexReauthJobRows(reauthJobs, codebook))
	addCSV("audit_log.csv", []string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, auditLogRows(auditRows, codebook))
	addCSV("cf_events.csv", []string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, cfEventRows(cfRows, codebook))
	addCSV("usage_records.csv", []string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "usage_provider", "estimated", "cache_miss_tokens", "cache_total_input_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "affinity_source", "route_class", "prompt_cache_key_present", "prompt_cache_key_source", "stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes", "retention_effective", "retention_source", "claude_cache_ttl", "cache_control_injected", "cache_breakpoint_count", "latest_user_cache_control", "route_epoch", "raw_usage_json"}, usageRecordRows(usageRows, codebook))
	addCSV("billing_holds.csv", []string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status"}, billingHoldRows(holds, codebook))
	addCSV("accounts_snapshot.csv", []string{"account_code", "group_name", "declared_provider", "effective_provider", "status", "plan_type", "is_fedramp", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, tokensByID, bindings, codebook))
	addCSV("egress_snapshot.csv", []string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
	if err := addJSON("diagnostic_summary.json", diagnosticSummary(accounts, tokensByID, auditRows, holds, bindings, rateLimits)); err != nil {
		return nil, err
	}
	rowCounts["manifest.json"] = 1
	manifest := map[string]interface{}{
		"generated_at":        time.Now().Unix(),
		"format":              "codex-pool-diagnostics-v2",
		"account_count":       len(codebook.byID),
		"files":               diagnosticFileOrder(),
		"row_counts":          rowCounts,
		"account_redaction":   "business files use account_code; real account identifiers are only in account_map.csv",
		"account_code_format": "ACC-0001",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = string(rawManifest) + "\n"
	return files, nil
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
usage_provider, estimated, cache_miss_tokens, cache_total_input_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens,
affinity_source, prompt_cache_key_present, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, stable_prefix_bytes,
retention_effective, retention_source, claude_cache_ttl, cache_control_injected, cache_breakpoint_count, latest_user_cache_control, route_epoch,
raw_usage_json, created_at FROM usage_records`
}

type usageRecordScanner interface {
	Scan(dest ...interface{}) error
}

func scanDiagnosticUsageRecord(rows usageRecordScanner, r *diagnosticUsageRecord) error {
	return rows.Scan(&r.ID, &r.AccountID, &r.RouteKeyHash, &r.APIKeyHash, &r.UserID, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.UsageProvider, &r.Estimated, &r.CacheMissTokens, &r.CacheTotalInputTokens, &r.CacheCreation5mTokens, &r.CacheCreation1hTokens,
		&r.AffinitySource, &r.PromptCacheKeyPresent, &r.PromptCacheKeySource, &r.StablePrefixSource, &r.StablePrefixReason, &r.StablePrefixBytes,
		&r.RetentionEffective, &r.RetentionSource, &r.ClaudeCacheTTL, &r.CacheControlInjected, &r.CacheBreakpointCount, &r.LatestUserCacheControl, &r.RouteEpoch,
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
	rows, err := db.QueryContext(ctx, `SELECT route_key_hash, route_key, source, account_id, epoch, created_at, updated_at FROM affinity_bindings ORDER BY updated_at ASC, route_key_hash ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.AffinityBinding
	for rows.Next() {
		var b storage.AffinityBinding
		if err := rows.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt); err != nil {
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
		out = append(out, []string{info.Code, info.AccountID, info.Email, info.Label, info.UpstreamAccountID, info.ChatGPTUserID, info.Provider, info.GroupName, info.Status})
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
		out = append(out, []string{row.RouteKeyHash, codebook.sanitize(row.RouteKey), row.Source, codebook.code(row.AccountID), itoa64(row.Epoch), itoa64(row.CreatedAt), itoa64(row.UpdatedAt)})
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
			itoa64(row.LatestUserCacheControl),
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
	routing409 := map[string]int{}
	healthModels := map[string]int{}
	banned := map[string]int{"discovered": 0, "deleted": 0, "delete_failed": 0}
	for _, row := range auditRows {
		if row.Action == "routing_unavailable" && strings.Contains(row.Detail, "status=409") {
			routing409[classifyRouting409Detail(row.Detail)]++
		}
		if row.Action == "health_test" || row.Action == "health_test_model_unsupported" {
			if model := extractDiagnosticDetailValue(row.Detail, "model"); model != "" {
				healthModels[model]++
			}
		}
		if row.State == "banned" {
			banned["discovered"]++
		}
		switch row.Action {
		case "ban_delete":
			banned["deleted"]++
		case "ban_delete_failed":
			banned["delete_failed"]++
		}
	}
	heldCount := 0
	expiredCount := 0
	oldestHeldAge := int64(0)
	for _, hold := range holds {
		switch hold.Status {
		case "held":
			heldCount++
			if age := now - hold.CreatedAt; age > oldestHeldAge {
				oldestHeldAge = age
			}
		case "expired_unsettled":
			expiredCount++
		}
	}

	bindingByAccount := map[string]storage.AccountEgressBinding{}
	for _, binding := range bindings {
		bindingByAccount[binding.AccountID] = binding
	}
	rateLimitByAccount := map[string]int{}
	for _, snap := range rateLimits {
		if snap.ResetAt > now {
			rateLimitByAccount[snap.AccountID]++
		}
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
		if rateLimitByAccount[account.ID] > 0 {
			g.RateLimitCooldown++
		}
	}
	return map[string]interface{}{
		"routing_409":        routing409,
		"health_test_models": healthModels,
		"banned_accounts":    banned,
		"billing_holds": map[string]interface{}{
			"held":                    heldCount,
			"expired_unsettled":       expiredCount,
			"oldest_held_age_seconds": oldestHeldAge,
		},
		"groups": groups,
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
