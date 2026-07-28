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

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
)

const kiroHealthProbeScope = "account_auth_usage+model_request"

type healthProbeStage struct {
	Checked    bool   `json:"checked,omitempty"`
	Alive      bool   `json:"alive"`
	State      string `json:"state"`
	HTTPStatus int    `json:"http_status"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type kiroInferenceProbeResult struct {
	healthProbeStage
	Model string
	Body  []byte
	Data  *kirowire.ResponseData
	Err   error
}

func (s *Server) adminKiroHealthTest(w http.ResponseWriter, r *http.Request, account storage.Account, token storage.AccountToken) {
	var request struct {
		ConfirmCost bool `json:"confirm_cost"`
	}
	if err := decodeJSONRequestBody(r.Body, &request, adminJSONBodyLimit); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !request.ConfirmCost {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "confirm_cost:true is required because the Kiro health test sends one billable model request after the free authentication check")
		return
	}

	authResult := s.probeAccountLiveness(r.Context(), account, token)
	authProbe := healthProbeStage{State: string(authResult.Verdict.State), HTTPStatus: authResult.Status}
	if authResult.Err != nil {
		authProbe.State = "unreachable"
	} else {
		authProbe.Alive = authResult.Ready
	}
	if authProbe.State == "" {
		authProbe.State = "unknown"
	}
	s.recordKiroHealthAuthAudit(r.Context(), account, authResult, authProbe)

	// Only a clean UsageLimits response authorizes the paid stage. A rate limit or
	// ambiguous non-2xx may still indicate a real credential, but it is not a
	// successful authentication preflight and must not spend credits.
	if !authResult.Ready {
		if authResult.Verdict.IsBanned() {
			s.handleBannedAccount(r.Context(), account, authResult.Verdict, authResult.Status, authResult.Body, "health_test_auth")
		} else if authResult.Verdict.State == ban.PermissionDenied {
			s.recordPermissionDeniedNoQuarantine(r.Context(), account, authResult.Verdict, authResult.Status, authResult.Body, "health_test_auth")
		}
		s.writeKiroHealthTestResult(w, r.Context(), account.ID, authProbe, authResult.Verdict.Reason, kiroInferenceProbeResult{
			healthProbeStage: healthProbeStage{Checked: false, Alive: false, State: "not_checked"},
		}, authResult.Err)
		return
	}

	inference := s.runKiroInferenceHealthProbe(r, account)
	if inference.Alive {
		s.recoverKiroAfterHealthProbe(r.Context(), account, authResult.Status, inference)
	}
	s.writeKiroHealthTestResult(w, r.Context(), account.ID, authProbe, authResult.Verdict.Reason, inference, inference.Err)
}

func (s *Server) recordKiroHealthAuthAudit(ctx context.Context, account storage.Account, result livenessResult, stage healthProbeStage) {
	reason := result.Verdict.Reason
	if result.Err != nil {
		reason = result.Err.Error()
	}
	detail := fmt.Sprintf("provider=kiro stage=auth http=%d alive=%v probe_scope=%s model_checked=false", result.Status, stage.Alive, kiroHealthProbeScope)
	if result.Status >= 400 {
		detail += " body=" + bodySnippet(result.Body, 600)
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action: "health_test", State: stage.State, Reason: reason, Detail: detail,
	})
}

func (s *Server) runKiroInferenceHealthProbe(r *http.Request, account storage.Account) kiroInferenceProbeResult {
	result := kiroInferenceProbeResult{healthProbeStage: healthProbeStage{Checked: false, State: "unreachable"}}
	ctx := r.Context()
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	egress, err := s.store.ResolvePrimaryEgressBinding(ctx, binding)
	if err != nil {
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	credentials, err := s.store.GetKiroCredentials(ctx, account.ID)
	if err != nil {
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	cfg := s.effectiveKiroConfig(ctx)
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, firstNonEmpty(credentials.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1"), cfg.KiroEndpointAllowlist)
	if err != nil {
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	verified, err := s.store.VerifiedKiroModels(ctx, account.ID, endpointHash, true)
	if err != nil {
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	model, ok := kiroHealthProbeModel(verified, account.PlanType)
	if !ok {
		result.State = string(ban.Unknown)
		result.ErrorCode = "verified_model_unavailable"
		result.Err = fmt.Errorf("%w for Kiro health probe", kirowire.ErrVerifiedModelUnavailable)
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	result.Model = model

	probeRaw, _ := json.Marshal(map[string]any{
		"model":         model,
		"messages":      []any{map[string]any{"role": "user", "content": "Reply exactly OK"}},
		"max_tokens":    8,
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "max"},
	})
	// A random, in-memory conversation key prevents different probe runs from
	// sharing upstream conversation state. It is never persisted as scheduler or
	// database affinity.
	probeConversation := "kiro-health-probe:" + uuid.NewString()
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(probeRaw, probeConversation, kirowire.ConversionOptions{
		DefaultThinking: true, ForceMaxQuality: true, EnableCachePoints: false, VerifiedModels: verified,
	})
	if err != nil {
		result.State = string(ban.Unknown)
		result.ErrorCode = kiroErrorCode(err)
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}

	lease := scheduler.Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: model}
	recorder := &probeResponseRecorder{header: http.Header{}}
	probeRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://kiro.internal/health-probe", nil)
	result.Checked = true
	response, observedEndpointHash, _ := s.openKiroAttempt(recorder, probeRequest, &converted, lease)
	if response == nil {
		result.HTTPStatus = firstPositive(recorder.status, http.StatusBadGateway)
		result.Body = []byte(recorder.body.String())
		result.ErrorCode = kiroProbeErrorCode(result.Body)
		if result.ErrorCode == "kiro_account_suspended" {
			result.State = string(ban.Banned)
			result.Err = kirowire.ErrAccountSuspended
		} else {
			verdict := ban.Classify(false, result.HTTPStatus, recorder.header, result.Body)
			result.State = string(verdict.State)
			if result.State == "" {
				result.State = string(ban.Unknown)
			}
			result.Err = errors.New(firstNonEmpty(strings.TrimSpace(recorder.body.String()), http.StatusText(result.HTTPStatus)))
		}
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	data, err := kirowire.DecodeResponse(response.Body, converted.ToolNameMap)
	if err != nil {
		result.HTTPStatus = http.StatusBadGateway
		result.State = string(ban.Unknown)
		result.ErrorCode = kiroErrorCode(err)
		result.Err = err
		s.recordKiroInferenceProbeAudit(ctx, account, result)
		return result
	}
	finalizeKiroUsage(&data, converted)
	data.Model = firstNonEmpty(data.Model, converted.Model, model)
	data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
	capabilityState := s.observeKiroResponse(ctx, account.ID, observedEndpointHash, converted, data)
	// Empty route hash + a diagnostic affinity source records metering without
	// creating a scheduler/session binding.
	s.recordKiroUsage(probeRequest, account.ID, routing.AffinityKey{Source: "kiro_health_probe"}, 0, data.Model, data, capabilityState)
	result.Alive = true
	result.State = string(ban.Alive)
	result.Data = &data
	s.recordKiroInferenceProbeAudit(ctx, account, result)
	return result
}

func kiroHealthProbeModel(verified []string, plan string) (string, bool) {
	candidates := make([]string, 0, len(verified))
	seen := map[string]bool{}
	for _, model := range verified {
		canonical, ok := capability.KiroCanonicalModel(model)
		if !ok || seen[canonical] || !capability.KiroSupportsAdaptiveThinking(canonical) || !capability.KiroPlanAllowsBootstrap(plan, canonical) {
			continue
		}
		seen[canonical] = true
		candidates = append(candidates, canonical)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := kiroHealthModelCostTier(candidates[i]), kiroHealthModelCostTier(candidates[j])
		if left != right {
			return left < right
		}
		return candidates[i] < candidates[j]
	})
	if len(candidates) > 0 {
		return candidates[0], true
	}
	const fallback = "claude-sonnet-4.6"
	if capability.KiroSupportsAdaptiveThinking(fallback) && capability.KiroPlanAllowsBootstrap(plan, fallback) {
		return fallback, true
	}
	return "", false
}

func kiroHealthModelCostTier(model string) int {
	switch {
	case strings.Contains(model, "-sonnet-"):
		return 0
	case strings.Contains(model, "-opus-"):
		return 1
	default:
		return 2
	}
}

func kiroProbeErrorCode(body []byte) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		return strings.TrimSpace(envelope.Error.Code)
	}
	return ""
}

func (s *Server) recordKiroInferenceProbeAudit(ctx context.Context, account storage.Account, result kiroInferenceProbeResult) {
	reason := result.ErrorCode
	if reason == "" && result.Err != nil {
		reason = result.Err.Error()
	}
	detail := fmt.Sprintf("provider=kiro checked=%v http=%d model=%s probe_scope=%s", result.Checked, result.HTTPStatus, result.Model, kiroHealthProbeScope)
	if result.Data != nil {
		credits := "unreported"
		if result.Data.Metering.Credits.Present {
			credits = fmt.Sprintf("%g", result.Data.Metering.Credits.Value)
		}
		detail += fmt.Sprintf(" usage_source=%s input_tokens=%d output_tokens=%d credits=%s output=%s", result.Data.UsageSource, result.Data.InputTokens, result.Data.OutputTokens, credits, bodySnippet([]byte(result.Data.Text), 120))
	} else if len(result.Body) > 0 {
		detail += " body=" + bodySnippet(result.Body, 1000)
	} else if result.Err != nil {
		detail += " error=" + result.Err.Error()
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action: "kiro_inference_probe", State: result.State, Reason: reason, Detail: detail,
	})
}

func (s *Server) recoverKiroAfterHealthProbe(ctx context.Context, account storage.Account, authStatus int, inference kiroInferenceProbeResult) {
	wasSuspended := isKiroSuspensionQuarantine(account)
	shouldClear := wasSuspended || s.cfg.HealthTestClearsQuarantine
	if shouldClear {
		_ = s.store.SetAccountQuarantine(ctx, account.ID, 0, "")
	}
	if wasSuspended || account.Status == "invalid" {
		_ = s.store.SetAccountStatus(ctx, account.ID, "active")
	}
	_ = s.store.ClearBindingRecheck(ctx, account.ID)
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	if wasSuspended {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			AccountID: account.ID, AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
			Action: "kiro_suspension_recovered", State: "active", Reason: "auth_and_inference_probe_succeeded",
			Detail: fmt.Sprintf("auth_http=%d inference_http=%d model=%s probe_scope=%s", authStatus, inference.HTTPStatus, inference.Model, kiroHealthProbeScope),
		})
	}
}

func (s *Server) writeKiroHealthTestResult(w http.ResponseWriter, ctx context.Context, accountID string, auth healthProbeStage, authReason string, inference kiroInferenceProbeResult, probeErr error) {
	state := inference.State
	reason := inference.ErrorCode
	httpStatus := inference.HTTPStatus
	model := inference.Model
	modelChecked := inference.Checked
	body := inference.Body
	ready := auth.Alive && inference.Checked && inference.Alive
	if !inference.Checked && !auth.Alive {
		state = auth.State
		reason = authReason
		httpStatus = auth.HTTPStatus
		model = ""
		modelChecked = false
	}
	if probeErr != nil && reason == "" {
		reason = probeErr.Error()
	}
	if state == "" {
		state = string(ban.Unknown)
	}
	quarantined := false
	quarantineReason := ""
	if current, err := s.store.GetAccount(ctx, accountID); err == nil {
		quarantined = current.QuarantineUntil > storage.Now()
		quarantineReason = current.QuarantineReason
	}
	response := map[string]any{
		"account_id": accountID,
		"alive":      ready, "ready": ready, "state": state, "reason": reason, "http_status": httpStatus,
		"provider": "kiro", "model": model, "probe_scope": kiroHealthProbeScope, "model_checked": modelChecked,
		"deleted": false, "quarantined": quarantined, "quarantine_reason": quarantineReason,
		"auth_probe": map[string]any{"alive": auth.Alive, "state": auth.State, "http_status": auth.HTTPStatus},
		"inference_probe": map[string]any{
			"checked": inference.Checked, "alive": inference.Alive, "state": inference.State,
			"http_status": inference.HTTPStatus, "error_code": inference.ErrorCode,
		},
		"snippet": bodySnippet(body, 300),
	}
	if probeErr != nil {
		response["error"] = probeErr.Error()
	}
	writeJSON(w, http.StatusOK, response)
}
