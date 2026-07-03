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
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

type diagnosticUsageRecord struct {
	ID                  int64
	AccountID           string
	RouteKeyHash        string
	APIKeyHash          string
	UserID              string
	Model               string
	PromptTokens        int64
	CompletionTokens    int64
	TotalTokens         int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	RawUsageJSON        string
	CreatedAt           int64
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
	codebook := buildDiagnosticCodebook(accounts, auditRows, cfRows, usageRows, holds, bindings)
	files, err := buildDiagnosticsZipFiles(accounts, auditRows, cfRows, usageRows, holds, bindings, egressProfiles, codebook)
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
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-diagnostics.zip"`)
	_, _ = w.Write(buf.Bytes())
}

func diagnosticFileOrder() []string {
	return []string{
		"manifest.json",
		"account_map.csv",
		"audit_log.csv",
		"cf_events.csv",
		"usage_records.csv",
		"billing_holds.csv",
		"accounts_snapshot.csv",
		"egress_snapshot.csv",
	}
}

func buildDiagnosticsZipFiles(accounts []storage.Account, auditRows []storage.AuditLogRow, cfRows []storage.CFEvent, usageRows []diagnosticUsageRecord, holds []diagnosticBillingHold, bindings []storage.AccountEgressBinding, egressProfiles []storage.EgressProfile, codebook diagnosticCodebook) (map[string]string, error) {
	files := map[string]string{}
	manifest := map[string]interface{}{
		"generated_at":        time.Now().Unix(),
		"format":              "codex-pool-diagnostics-v1",
		"account_count":       len(codebook.byID),
		"files":               diagnosticFileOrder(),
		"account_redaction":   "business files use account_code; real account identifiers are only in account_map.csv",
		"account_code_format": "ACC-0001",
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = string(rawManifest) + "\n"
	files["account_map.csv"] = csvString([]string{"account_code", "account_id", "email", "label", "upstream_account_id", "chatgpt_user_id", "provider", "group_name", "status"}, accountMapRows(codebook))
	files["audit_log.csv"] = csvString([]string{"id", "created_at", "account_code", "action", "state", "reason", "detail"}, auditLogRows(auditRows, codebook))
	files["cf_events.csv"] = csvString([]string{"id", "created_at", "account_code", "egress_id", "status", "cf_ray", "category", "message"}, cfEventRows(cfRows, codebook))
	files["usage_records.csv"] = csvString([]string{"id", "created_at", "account_code", "route_key_hash", "api_key_hash", "user_id", "model", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "raw_usage_json"}, usageRecordRows(usageRows, codebook))
	files["billing_holds.csv"] = csvString([]string{"id", "created_at", "updated_at", "account_code", "route_key_hash", "estimated_tokens", "status"}, billingHoldRows(holds, codebook))
	files["accounts_snapshot.csv"] = csvString([]string{"account_code", "group_name", "provider", "status", "plan_type", "is_fedramp", "quarantine_until", "quarantine_reason", "created_at", "updated_at", "primary_egress_id", "standby_egress_ids", "cooldown_until", "recheck_pending"}, accountSnapshotRows(accounts, bindings, codebook))
	files["egress_snapshot.csv"] = csvString([]string{"egress_id", "name", "type", "region", "exit_ip", "stream_capable", "health", "latency_millis", "cf_score", "last_cf_ray", "cooldown_until", "max_concurrency", "created_at", "updated_at", "bound_account_codes"}, egressSnapshotRows(egressProfiles, bindings, codebook))
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
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, route_key_hash, api_key_hash, user_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, raw_usage_json, created_at FROM usage_records ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagnosticUsageRecord
	for rows.Next() {
		var r diagnosticUsageRecord
		if err := rows.Scan(&r.ID, &r.AccountID, &r.RouteKeyHash, &r.APIKeyHash, &r.UserID, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens, &r.RawUsageJSON, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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

func accountSnapshotRows(accounts []storage.Account, bindings []storage.AccountEgressBinding, codebook diagnosticCodebook) [][]string {
	bindingByAccount := map[string]storage.AccountEgressBinding{}
	for _, b := range bindings {
		bindingByAccount[b.AccountID] = b
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	out := make([][]string, 0, len(accounts))
	for _, a := range accounts {
		b := bindingByAccount[a.ID]
		out = append(out, []string{
			codebook.code(a.ID),
			a.GroupName,
			a.Provider,
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
