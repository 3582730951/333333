package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/usage"
)

type providerAPIKeyImportRequest struct {
	ProviderID  string
	APIKey      string
	Label       string
	GroupName   string
	EgressID    string
	ConfirmCost bool
}

const (
	providerAPIKeyInferenceProbePending       = "provider_api_key_inference_probe_pending"
	providerAPIKeyInferenceProbeFailurePrefix = "provider_api_key_inference_probe_failed:"
)

func isProviderAPIKeyInferenceQuarantine(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == providerAPIKeyInferenceProbePending || strings.HasPrefix(reason, providerAPIKeyInferenceProbeFailurePrefix)
}

type providerProbeStage struct {
	healthProbeStage
	Model string          `json:"model,omitempty"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

type providerAPIKeyImportResponse struct {
	storage.Account
	Ready            bool               `json:"ready"`
	AuthProbe        providerProbeStage `json:"auth_probe"`
	InferenceProbe   providerProbeStage `json:"inference_probe"`
	AuthMethod       string             `json:"auth_method"`
	BillingMode      string             `json:"billing_mode"`
	APIKeyPresent    bool               `json:"api_key_present"`
	Quarantined      bool               `json:"quarantined"`
	QuarantineReason string             `json:"quarantine_reason,omitempty"`
}

func (s *Server) adminImportProviderAPIKey(w http.ResponseWriter, r *http.Request, req providerAPIKeyImportRequest) {
	if !req.ConfirmCost {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "confirm_cost:true is required because API-key import runs one billable minimal inference after the free authentication check")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.ProviderID))
	key := strings.TrimSpace(req.APIKey)
	accountID := customAccountID(provider, key)
	label := strings.TrimSpace(req.Label)
	if label == "" {
		if provider == "claude" {
			label = "Anthropic API Key"
		} else {
			label = "OpenAI API Key"
		}
	}
	group := strings.TrimSpace(req.GroupName)
	if group == "" {
		group = s.cfg.DefaultGroup
	}
	account := storage.Account{ID: accountID, Label: label, GroupName: group, Provider: provider, Status: "active", PlanType: "api"}
	token := storage.AccountToken{AccountID: accountID, AuthMethod: accountprovider.AuthMethodAPIKey, AccessToken: key, OpenAIAPIKey: key, LastRefresh: storage.Now()}
	egressID, err := s.resolveImportPrimaryEgress(r.Context(), req.EgressID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	egress, err := s.store.GetEgressProfile(r.Context(), egressID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	authProbe, caps := s.runProviderAPIKeyAuthProbe(r.Context(), account, token, egress)
	s.auditProviderAPIKeyProbe(r.Context(), account, "provider_api_key_auth_probe", authProbe)
	if !authProbe.Alive {
		status := http.StatusBadGateway
		if authProbe.HTTPStatus == http.StatusUnauthorized || authProbe.HTTPStatus == http.StatusForbidden || authProbe.HTTPStatus == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, providerAPIKeyImportResponse{
			Account: account, Ready: false, AuthProbe: authProbe,
			InferenceProbe: providerProbeStage{healthProbeStage: healthProbeStage{Checked: false, Alive: false, State: "not_checked"}},
			AuthMethod:     accountprovider.AuthMethodAPIKey, BillingMode: accountprovider.BillingModePayAsYouGo, APIKeyPresent: true,
		})
		return
	}

	// Persist behind an infinite quarantine before the billable probe starts, so a
	// concurrent scheduler refresh can never route a merely authenticated account.
	// Success clears this pending quarantine; failure replaces it with the durable
	// inference-failure reason used by manual recovery.
	account.QuarantineUntil = kiroSuspensionQuarantineUntil
	account.QuarantineReason = providerAPIKeyInferenceProbePending
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bindImportedAccountPrimaryEgress(r.Context(), account.ID, egressID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.ReplaceCapabilities(r.Context(), account.ID, caps); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: account.Label, Action: "provider_api_key_import", State: "authenticated", Reason: "free_auth_probe_succeeded",
		Detail: fmt.Sprintf("provider=%s auth_method=api_key billing_mode=pay_as_you_go", provider),
	})

	inference := s.runProviderAPIKeyInferenceProbe(r.Context(), account, token, egress, caps)
	s.auditProviderAPIKeyProbe(r.Context(), account, "provider_api_key_inference_probe", inference)
	ready := inference.Alive
	if ready {
		if existing, getErr := s.store.GetAccount(r.Context(), account.ID); getErr == nil && isProviderAPIKeyInferenceQuarantine(existing.QuarantineReason) {
			_ = s.store.SetAccountQuarantine(r.Context(), account.ID, 0, "")
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label, Action: "provider_api_key_recovered", State: "active", Reason: "inference_probe_succeeded"})
		}
	} else {
		reason := providerAPIKeyInferenceProbeFailurePrefix + firstNonEmpty(inference.ErrorCode, "unknown")
		_ = s.store.SetAccountQuarantine(r.Context(), account.ID, kiroSuspensionQuarantineUntil, reason)
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label, Action: "provider_api_key_quarantined", State: "quarantined", Reason: inference.ErrorCode, Detail: "provider=" + provider})
	}
	current, _ := s.store.GetAccount(r.Context(), account.ID)
	if current.ID != "" {
		account = current
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, providerAPIKeyImportResponse{
		Account: account, Ready: ready, AuthProbe: authProbe, InferenceProbe: inference,
		AuthMethod: accountprovider.AuthMethodAPIKey, BillingMode: accountprovider.BillingModePayAsYouGo, APIKeyPresent: true,
		Quarantined: account.QuarantineUntil > storage.Now(), QuarantineReason: account.QuarantineReason,
	})
}

func (s *Server) runProviderAPIKeyAuthProbe(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile) (providerProbeStage, []storage.ModelCapability) {
	stage := providerProbeStage{healthProbeStage: healthProbeStage{Checked: true, State: "unreachable"}}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	req := upstream.Request{Method: http.MethodGet, Provider: provider, DownstreamPath: "/v1/models", Account: account, Token: token, Egress: egress, CookieJarKey: account.ID + ":" + egress.ID}
	resp, err := s.upstream.Do(ctx, req)
	if err != nil {
		stage.ErrorCode = "transport_error"
		return stage, nil
	}
	raw, readErr := upstream.DrainAndClose(resp.Body)
	stage.HTTPStatus = resp.StatusCode
	if readErr != nil {
		stage.ErrorCode = "response_read_error"
		return stage, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		stage.State = "auth_failed"
		stage.ErrorCode = providerProbeErrorCode(resp.StatusCode, raw, nil)
		return stage, nil
	}
	var caps []storage.ModelCapability
	if provider == "claude" {
		caps, err = capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header))
		if err == nil {
			caps = capability.ApplyClaudeAccountPolicy(caps, account, token)
		}
	} else {
		caps, err = capability.Parse(account.ID, raw, capability.ETagFromHeader(resp.Header))
	}
	if err != nil {
		stage.State = "invalid_model_catalog"
		stage.ErrorCode = "model_catalog_parse_error"
		return stage, nil
	}
	stage.Alive = true
	stage.State = "alive"
	return stage, caps
}

func (s *Server) runProviderAPIKeyInferenceProbe(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile, caps []storage.ModelCapability) providerProbeStage {
	stage := providerProbeStage{healthProbeStage: healthProbeStage{Checked: true, State: "unreachable"}}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	model := providerAPIKeyProbeModel(provider, caps)
	stage.Model = model
	if model == "" {
		stage.State = "inference_failed"
		stage.ErrorCode = "probe_model_unavailable"
		return stage
	}
	req := upstream.Request{Method: http.MethodPost, Provider: provider, Account: account, Token: token, Egress: egress, CookieJarKey: account.ID + ":" + egress.ID, MinimalProbe: true}
	if provider == "claude" {
		req.DownstreamPath = "/v1/messages"
		body, _ := json.Marshal(map[string]interface{}{"model": model, "max_tokens": 8, "messages": []interface{}{map[string]interface{}{"role": "user", "content": "Reply exactly OK"}}})
		req.SetBodyBytes(body)
	} else {
		req.DownstreamPath = "/v1/responses"
		body, _ := json.Marshal(map[string]interface{}{"model": model, "input": "Reply exactly OK", "max_output_tokens": 8, "stream": false})
		req.SetBodyBytes(body)
	}
	resp, err := s.upstream.Do(ctx, req)
	if err != nil {
		stage.ErrorCode = "transport_error"
		return stage
	}
	raw, readErr := upstream.DrainAndClose(resp.Body)
	stage.HTTPStatus = resp.StatusCode
	if readErr != nil {
		stage.ErrorCode = "response_read_error"
		return stage
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		stage.State = "inference_failed"
		stage.ErrorCode = providerProbeErrorCode(resp.StatusCode, raw, nil)
		return stage
	}
	parsed := usage.ParseResponse(raw)
	if parsed.Model == "" {
		parsed.Model = model
	}
	stage.Alive = true
	stage.State = "alive"
	stage.Usage = parsed.RawUsage
	_ = s.store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "", "", "", parsed.Model,
		parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens, parsed.CachedTokens, parsed.CacheReadTokens, parsed.CacheCreationTokens,
		parsed.RawUsage, storage.UsageDiagnostics{UsageProvider: provider, UsageSource: "provider_api_key_probe", CacheReadPresent: parsed.CacheReadTokens > 0, CacheCreationPresent: parsed.CacheCreationTokens > 0})
	s.verifyAccountModel(ctx, account, model, "")
	return stage
}

func providerAPIKeyProbeModel(provider string, caps []storage.ModelCapability) string {
	models := make([]string, 0, len(caps))
	for _, c := range caps {
		if c.AvailabilityState != capability.AvailabilityUnsupported && strings.TrimSpace(c.ModelSlug) != "" {
			models = append(models, c.ModelSlug)
		}
	}
	find := func(wants ...string) string {
		for _, want := range wants {
			for _, model := range models {
				if strings.EqualFold(model, want) {
					return model
				}
			}
		}
		return ""
	}
	if provider == "claude" {
		for _, family := range []string{"haiku", "sonnet", "opus"} {
			var choices []string
			for _, model := range models {
				if strings.Contains(strings.ToLower(model), family) {
					choices = append(choices, model)
				}
			}
			if len(choices) > 0 {
				sort.Strings(choices)
				return choices[len(choices)-1]
			}
		}
		return ""
	}
	if preferred := find("gpt-5-nano", "gpt-4.1-nano", "gpt-4o-mini", "gpt-5-mini"); preferred != "" {
		return preferred
	}
	sort.Strings(models)
	for _, model := range models {
		if strings.HasPrefix(strings.ToLower(model), "gpt-") {
			return model
		}
	}
	return ""
}

func providerProbeErrorCode(status int, raw []byte, err error) string {
	if err != nil {
		return "transport_error"
	}
	var root struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &root) == nil {
		if code := firstNonEmpty(root.Error.Code, root.Error.Type); code != "" {
			return code
		}
	}
	if status > 0 {
		return fmt.Sprintf("http_%d", status)
	}
	return "unknown"
}

func (s *Server) auditProviderAPIKeyProbe(ctx context.Context, account storage.Account, action string, stage providerProbeStage) {
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: account.Label, Action: action, State: stage.State, Reason: stage.ErrorCode,
		Detail: fmt.Sprintf("provider=%s auth_method=api_key http=%d alive=%v model=%s", account.Provider, stage.HTTPStatus, stage.Alive, stage.Model),
	})
}

func (s *Server) adminProviderAPIKeyHealthTest(w http.ResponseWriter, r *http.Request, account storage.Account, token storage.AccountToken) {
	confirmed, err := decodeConfirmCost(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !confirmed {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "confirm_cost:true is required because this health test runs one billable minimal inference after the free authentication check")
		return
	}
	binding, err := s.store.GetEgressBinding(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	egress, err := s.store.ResolvePrimaryEgressBinding(r.Context(), binding)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	authProbe, caps := s.runProviderAPIKeyAuthProbe(r.Context(), account, token, egress)
	s.auditProviderAPIKeyProbe(r.Context(), account, "provider_api_key_auth_probe", authProbe)
	inference := providerProbeStage{healthProbeStage: healthProbeStage{Checked: false, State: "not_checked"}}
	if authProbe.Alive {
		_ = s.store.ReplaceCapabilities(r.Context(), account.ID, caps)
		inference = s.runProviderAPIKeyInferenceProbe(r.Context(), account, token, egress, caps)
		s.auditProviderAPIKeyProbe(r.Context(), account, "provider_api_key_inference_probe", inference)
		if inference.Alive && isProviderAPIKeyInferenceQuarantine(account.QuarantineReason) {
			_ = s.store.SetAccountQuarantine(r.Context(), account.ID, 0, "")
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label, Action: "provider_api_key_recovered", State: "active", Reason: "manual_health_test"})
		} else if !inference.Alive {
			reason := providerAPIKeyInferenceProbeFailurePrefix + firstNonEmpty(inference.ErrorCode, "unknown")
			_ = s.store.SetAccountQuarantine(r.Context(), account.ID, kiroSuspensionQuarantineUntil, reason)
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label, Action: "provider_api_key_quarantined", State: "quarantined", Reason: inference.ErrorCode, Detail: "provider=" + account.Provider})
		}
	}
	current, _ := s.store.GetAccount(r.Context(), account.ID)
	if current.ID != "" {
		account = current
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, providerAPIKeyImportResponse{
		Account: account, Ready: authProbe.Alive && inference.Alive, AuthProbe: authProbe, InferenceProbe: inference,
		AuthMethod: accountprovider.AuthMethodAPIKey, BillingMode: accountprovider.BillingModePayAsYouGo, APIKeyPresent: true,
		Quarantined: account.QuarantineUntil > storage.Now(), QuarantineReason: account.QuarantineReason,
	})
}

func decodeConfirmCost(r io.Reader) (bool, error) {
	var req struct {
		ConfirmCost bool `json:"confirm_cost"`
	}
	err := decodeJSONRequestBody(r, &req, adminJSONBodyLimit)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return req.ConfirmCost, err
}
