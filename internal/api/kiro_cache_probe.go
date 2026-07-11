package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

type kiroCacheProbeAttempt struct {
	InputTokens         kirowire.MeteredInt `json:"input_tokens"`
	OutputTokens        kirowire.MeteredInt `json:"output_tokens"`
	CacheReadTokens     kirowire.MeteredInt `json:"cache_read_tokens"`
	CacheCreationTokens kirowire.MeteredInt `json:"cache_creation_tokens"`
	MeteringEvents      int                 `json:"metering_events"`
	UsageSource         string              `json:"usage_source"`
}

func (s *Server) adminKiroCacheProbe(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request struct {
		Model       string `json:"model"`
		ConfirmCost bool   `json:"confirm_cost"`
	}
	if err := decodeJSONRequestBody(r.Body, &request, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !request.ConfirmCost {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "confirm_cost:true is required because this probe sends two billable Kiro requests")
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writePoolCodeError(w, http.StatusBadRequest, "verified_model_unavailable", "model is required")
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if s.accountProvider(account, storage.AccountToken{}) != "kiro" && account.Provider != "kiro" {
		writeError(w, http.StatusBadRequest, errors.New("cache probe requires a Kiro account"))
		return
	}
	credentials, err := s.store.GetKiroCredentials(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	binding, err := s.store.GetEgressBinding(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	egress, err := s.store.GetEgressProfile(r.Context(), binding.PrimaryEgressID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	cfg := s.effectiveKiroConfig(r.Context())
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, firstNonEmpty(credentials.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1"), cfg.KiroEndpointAllowlist)
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return
	}
	verified, err := s.store.VerifiedKiroModels(r.Context(), accountID, endpointHash, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resolved, ok := capability.ResolveKiroModel(request.Model, verified)
	if !ok {
		writeKiroError(w, r, http.StatusBadRequest, fmt.Errorf("%w: %s", kirowire.ErrVerifiedModelUnavailable, request.Model))
		return
	}
	// A fixed, public synthetic prefix makes both billable requests byte-identical
	// without including any account, prompt, user, or credential data. Keep it long
	// enough to cross common prompt-cache minimums; a tiny "Reply OK" request can
	// falsely look unsupported even on an endpoint that caches realistic prompts.
	stableProbePrefix := strings.Repeat("Kiro cache capability probe stable public prefix. ", 180)
	probeRaw, _ := json.Marshal(map[string]any{
		"model":      resolved,
		"system":     stableProbePrefix,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with OK."}},
		"max_tokens": 8,
		"thinking":   map[string]any{"type": "disabled"},
	})
	affinity := routing.AffinityFromKey("kiro-cache-probe-v1:"+accountID+":"+endpointHash+":"+resolved, "kiro_cache_probe")
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(probeRaw, affinity.Hash, kirowire.ConversionOptions{VerifiedModels: verified})
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return
	}
	lease := scheduler.Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: resolved}
	attempts := make([]kiroCacheProbeAttempt, 0, 2)
	var capabilityState storage.KiroRuntimeCapability
	for i := 0; i < 2; i++ {
		recorder := &probeResponseRecorder{header: http.Header{}}
		data, observedEndpointHash, _ := s.doKiroAttempt(recorder, r, converted, lease)
		if data == nil {
			message := strings.TrimSpace(recorder.body.String())
			if message == "" {
				message = "Kiro cache probe request failed"
			}
			writeError(w, firstPositive(recorder.status, http.StatusBadGateway), errors.New(message))
			return
		}
		capabilityState = s.observeKiroResponse(r.Context(), accountID, observedEndpointHash, converted, *data)
		attempts = append(attempts, kiroCacheProbeAttempt{
			InputTokens: data.Metering.InputTokens, OutputTokens: data.Metering.OutputTokens,
			CacheReadTokens: data.Metering.CacheReadTokens, CacheCreationTokens: data.Metering.CacheCreationTokens,
			MeteringEvents: data.Metering.EventCount, UsageSource: data.UsageSource,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": accountID, "model": resolved, "endpoint_hash": endpointHash,
		"egress_id": egress.ID, "session_affinity": affinity.Hash,
		"stable_request_hash": shortHash(string(converted.Body)),
		"attempts":            attempts, "capability": capabilityState,
	})
}

type probeResponseRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func (r *probeResponseRecorder) Header() http.Header    { return r.header }
func (r *probeResponseRecorder) WriteHeader(status int) { r.status = status }
func (r *probeResponseRecorder) Write(raw []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(raw)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
