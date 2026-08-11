package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

type customProviderModelTestResult struct {
	OK             bool   `json:"ok"`
	ProviderID     string `json:"provider_id"`
	RouteID        string `json:"route_id"`
	DownstreamPath string `json:"downstream_path"`
	RequestedModel string `json:"requested_model"`
	TargetModel    string `json:"target_model"`
	UpstreamPath   string `json:"upstream_path"`
	AccountID      string `json:"account_id,omitempty"`
	HTTPStatus     int    `json:"http_status"`
	LatencyMS      int64  `json:"latency_ms"`
	ErrorCode      string `json:"error_code,omitempty"`
	ResponseSample string `json:"response_sample,omitempty"`
}

// adminCustomProviderModelTest sends one minimal request through the exact provider
// URL/protocol/account/egress path used by production traffic. A Codex CLI Responses
// profile is tested as SSE because that profile is streaming-only; generic providers
// retain their native non-streaming probe.
// The administrator supplies the downstream model; configured mapping determines
// the target relay model and both are returned in the result.
func (s *Server) adminCustomProviderModelTest(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request struct {
		Model          string `json:"model"`
		DownstreamPath string `json:"downstream_path"`
	}
	if err := decodeJSONRequestBody(r.Body, &request, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		writeError(w, http.StatusBadRequest, errors.New("model is required"))
		return
	}
	provider, found, err := s.store.GetCustomProvider(r.Context(), providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	request.DownstreamPath = strings.TrimSpace(request.DownstreamPath)
	if request.DownstreamPath == "" {
		switch provider.UpstreamProtocol {
		case storage.CustomProviderProtocolResponses:
			request.DownstreamPath = storage.CustomProviderDownstreamResponses
		case storage.CustomProviderProtocolAnthropicMessages:
			request.DownstreamPath = storage.CustomProviderDownstreamMessages
		default:
			request.DownstreamPath = storage.CustomProviderDownstreamChat
		}
	}
	if _, ok := storage.NormalizeCustomProviderDownstreamPath(request.DownstreamPath); !ok {
		writeError(w, http.StatusBadRequest, errors.New("downstream_path is invalid"))
		return
	}
	provider, _ = storage.ResolveCustomProviderRoute(provider, request.DownstreamPath)
	targetModel, _ := customProviderMappedModel(provider, request.Model)
	result := customProviderModelTestResult{
		ProviderID: provider.ID, RequestedModel: request.Model, TargetModel: targetModel,
		RouteID: provider.ResolvedRouteID, DownstreamPath: provider.ResolvedDownstreamPath,
	}

	lease, token, err := s.customProviderTestLease(r, provider, targetModel)
	if err != nil {
		result.ErrorCode = "provider_account_unavailable"
		writeJSON(w, http.StatusConflict, result)
		return
	}
	defer lease.Release()
	account := lease.Account
	result.AccountID = account.ID

	headers := http.Header{}
	var body []byte
	probeOSHint := ""
	codexResponsesProbe := provider.UpstreamProtocol == storage.CustomProviderProtocolResponses &&
		provider.TransportProfile == storage.CustomProviderTransportCodexCLI
	switch provider.UpstreamProtocol {
	case storage.CustomProviderProtocolAnthropicMessages:
		result.UpstreamPath = "/messages"
		headers.Set("Anthropic-Version", "2023-06-01")
		body, probeOSHint = s.claudeCodeMinimalProbeBody(account, token, lease.Egress, targetModel, "Reply OK", 1)
	case storage.CustomProviderProtocolResponses:
		result.UpstreamPath = "/responses"
		body, _ = json.Marshal(map[string]interface{}{
			"model": targetModel, "max_output_tokens": 1, "stream": codexResponsesProbe, "input": "Reply OK",
		})
	default:
		result.UpstreamPath = "/chat/completions"
		body, _ = json.Marshal(map[string]interface{}{
			"model": targetModel, "max_tokens": 1, "stream": false,
			"messages": []map[string]interface{}{{"role": "user", "content": "Reply OK"}},
		})
	}
	probe := upstream.Request{
		Method: http.MethodPost, Provider: provider.ID, BaseURL: provider.BaseURL,
		TransportProfile: provider.TransportProfile, DownstreamPath: result.UpstreamPath,
		// The protocol decides whether the upstream sees the Claude Code client shape
		// (claudeShapedCustomCall). Pass it explicitly so the probe reports on the exact
		// header set real traffic will use, rather than one inferred from the path.
		UpstreamProtocol: provider.UpstreamProtocol,
		Headers:          headers, Account: account, Token: token, Egress: lease.Egress,
		CookieJarKey: customProviderCookieJarKey(r, lease, provider), MinimalProbe: !codexResponsesProbe,
		OSHint: probeOSHint,
	}
	probe.SetBodyBytes(body)
	started := time.Now()
	response, err := s.upstream.Do(r.Context(), probe)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.ErrorCode = "transport_error"
		s.auditCustomProviderModelTest(r, account, result)
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	raw, readErr := upstream.DrainAndClose(response.Body)
	result.HTTPStatus = response.StatusCode
	result.ResponseSample = bodySnippet(raw, 240)
	switch {
	case readErr != nil:
		result.ErrorCode = "response_read_error"
	case response.StatusCode < 200 || response.StatusCode >= 300:
		result.ErrorCode = providerProbeErrorCode(response.StatusCode, raw, nil)
	case codexResponsesProbe && validateSuccessfulCustomResponsesSSE(response.Header, raw) != nil:
		result.ErrorCode = "invalid_upstream_response"
	case !codexResponsesProbe && validateCustomUpstreamJSONResponse(provider.UpstreamProtocol, raw) != nil:
		result.ErrorCode = "invalid_upstream_response"
	default:
		result.OK = true
	}
	if result.OK {
		_ = s.store.SetModelCapabilityState(
			r.Context(), account.ID, targetModel,
			capability.AvailabilityVerified, capability.Context1MUnknown, "",
			"custom_admin_test:"+provider.ID,
		)
		if !strings.EqualFold(request.Model, targetModel) {
			_ = s.store.SetModelCapabilityState(
				r.Context(), account.ID, request.Model,
				capability.AvailabilityVerified, capability.Context1MUnknown, "",
				"custom_admin_test_mapping:"+provider.ID,
			)
		}
		if !providerHasModel(provider, targetModel) {
			provider.Models = append(provider.Models, targetModel)
			_ = s.store.UpsertCustomProvider(r.Context(), provider)
		}
		if s.scheduler != nil {
			s.scheduler.InvalidateAccountCache()
		}
	}
	s.auditCustomProviderModelTest(r, account, result)
	writeJSON(w, http.StatusOK, result)
}

func validateSuccessfulCustomResponsesSSE(header http.Header, raw []byte) error {
	if !isEventStream(header) {
		return errors.New("Codex Responses probe requires text/event-stream")
	}
	tracker := &customSSETerminalTracker{protocol: customSSEResponses}
	_, _ = tracker.Write(raw)
	tracker.finish()
	if !tracker.terminal {
		return errors.New("Codex Responses stream has no terminal event")
	}
	if !tracker.success {
		return errors.New("Codex Responses stream did not complete successfully")
	}
	return nil
}

func (s *Server) customProviderTestLease(r *http.Request, provider storage.CustomProvider, model string) (scheduler.Lease, storage.AccountToken, error) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		return scheduler.Lease{}, storage.AccountToken{}, err
	}
	for _, account := range accounts {
		if account.Status != "active" || !strings.EqualFold(strings.TrimSpace(account.Provider), provider.ID) {
			continue
		}
		lease, selectErr := s.scheduler.Select(r.Context(), scheduler.Route{
			Group:             account.GroupName,
			Provider:          provider.ID,
			RequiredAccountID: account.ID,
			Model:             model,
			SkipWait:          true,
		})
		if selectErr != nil {
			continue
		}
		token, tokenErr := s.store.GetToken(r.Context(), lease.Account.ID)
		if tokenErr != nil {
			lease.Release()
			continue
		}
		return lease, token, nil
	}
	return scheduler.Lease{}, storage.AccountToken{}, fmt.Errorf("no active account for provider %s", provider.ID)
}

func providerHasModel(provider storage.CustomProvider, model string) bool {
	for _, existing := range provider.Models {
		if strings.EqualFold(strings.TrimSpace(existing), strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

func (s *Server) auditCustomProviderModelTest(r *http.Request, account storage.Account, result customProviderModelTestResult) {
	state := "failed"
	if result.OK {
		state = "passed"
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: account.Label,
		Action: "custom_provider_model_test", State: state, Reason: result.ErrorCode,
		Detail: fmt.Sprintf("provider=%s route=%s downstream_path=%s requested_model=%s target_model=%s path=%s http=%d latency_ms=%d",
			result.ProviderID, result.RouteID, result.DownstreamPath, result.RequestedModel, result.TargetModel, result.UpstreamPath, result.HTTPStatus, result.LatencyMS),
	})
}
