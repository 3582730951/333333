package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
)

type kiroCacheProbeAttempt struct {
	InputTokens           kirowire.MeteredInt   `json:"input_tokens"`
	OutputTokens          kirowire.MeteredInt   `json:"output_tokens"`
	CacheReadTokens       kirowire.MeteredInt   `json:"cache_read_tokens"`
	CacheCreationTokens   kirowire.MeteredInt   `json:"cache_creation_tokens"`
	CacheTotalInputTokens kirowire.MeteredInt   `json:"cache_total_input_tokens"`
	TotalTokens           kirowire.MeteredInt   `json:"total_tokens"`
	Credits               kirowire.MeteredFloat `json:"credits"`
	MetadataEvents        int                   `json:"metadata_events"`
	MeteringEvents        int                   `json:"metering_events"`
	UsageSource           string                `json:"usage_source"`
	CachePointState       string                `json:"cache_point_state"`
	ElapsedMillis         int64                 `json:"elapsed_millis"`
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
	egress, err := s.store.ResolvePrimaryEgressBinding(r.Context(), binding)
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
	verified, err := s.store.VerifiedKiroModels(r.Context(), accountID, endpointHash, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resolved, ok := capability.ResolveKiroModel(request.Model, verified)
	if !ok {
		writeKiroError(w, r, http.StatusBadRequest, fmt.Errorf("%w: %s", kirowire.ErrVerifiedModelUnavailable, request.Model))
		return
	}
	// A per-run public synthetic prefix makes the two attempts byte-identical while
	// preventing a previous probe run from warming attempt one. Without the nonce,
	// repeated probes eventually become read/read and can no longer prove the
	// required write/read transition. Keep it long enough to cross common prompt-
	// cache minimums; no account, prompt, user, or credential data is included.
	probeRunID := uuid.NewString()
	stableProbePrefix := "Kiro cache capability probe run " + probeRunID + ". " + strings.Repeat("Kiro cache capability probe stable public prefix. ", 900)
	probeRaw, _ := json.Marshal(map[string]any{
		"model": resolved,
		"system": []any{map[string]any{
			"type": "text", "text": stableProbePrefix,
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages":      []any{map[string]any{"role": "user", "content": "Reply with OK."}},
		"max_tokens":    8,
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "max"},
	})
	affinity := routing.AffinityFromKey("kiro-cache-probe-v2:"+accountID+":"+endpointHash+":"+resolved+":"+probeRunID, "kiro_cache_probe")
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(probeRaw, affinity.Hash, kirowire.ConversionOptions{ForceMaxQuality: true, EnableCachePoints: true, VerifiedModels: verified})
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return
	}
	lease := scheduler.Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: resolved}
	attempts := make([]kiroCacheProbeAttempt, 0, 2)
	var capabilityState storage.KiroRuntimeCapability
	for i := 0; i < 2; i++ {
		recorder := &probeResponseRecorder{header: http.Header{}}
		startedAt := time.Now()
		data, observedEndpointHash, _ := s.doKiroAttempt(recorder, r, &converted, lease)
		elapsed := time.Since(startedAt).Milliseconds()
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
			CacheTotalInputTokens: data.Metering.TotalInputTokens, TotalTokens: data.Metering.TotalTokens,
			Credits: data.Metering.Credits, MetadataEvents: data.Metering.MetadataEventCount,
			MeteringEvents: data.Metering.MeteringEventCount, UsageSource: data.UsageSource,
			CachePointState: capabilityState.CachePointState, ElapsedMillis: elapsed,
		})
	}
	cacheVerified := len(attempts) == 2 && attempts[0].CacheCreationTokens.Present && attempts[0].CacheCreationTokens.Value > 0 &&
		attempts[1].CacheReadTokens.Present && attempts[1].CacheReadTokens.Value > 0
	creditReductionPercent, creditReuseObserved := kiroCacheProbeCreditEvidence(attempts)
	cacheEvidence := "none"
	if creditReuseObserved {
		cacheEvidence = "credits_reduction"
	}
	if cacheVerified {
		cacheEvidence = "token_metadata"
	}
	cacheReuseState := "not_observed"
	if cacheVerified || creditReuseObserved {
		cacheReuseState = "verified"
	}
	if err := s.store.SetKiroCacheReuseProbe(r.Context(), accountID, endpointHash, resolved, cacheReuseState, cacheEvidence, creditReductionPercent, time.Now().Unix()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if persisted, err := s.store.GetKiroRuntimeCapability(r.Context(), accountID, endpointHash, resolved); err == nil {
		capabilityState = persisted
		cacheReuseState = firstNonEmpty(persisted.CacheReuseState, cacheReuseState)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": accountID, "model": resolved, "endpoint_hash": endpointHash,
		"egress_id": egress.ID, "session_affinity": affinity.Hash,
		"stable_request_hash": shortHash(string(converted.Body)),
		"thinking":            "adaptive", "effort": converted.ThinkingEffort, "max_output_tokens": converted.MaxOutputTokens,
		"attempts": attempts, "capability": capabilityState,
		"cache_verified": cacheVerified, "cache_reuse_observed": cacheVerified || creditReuseObserved, "cache_reuse_state": cacheReuseState,
		"cache_evidence": cacheEvidence, "credit_reduction_percent": creditReductionPercent,
		"success_criteria": "token metadata: first write > 0 and second read > 0; credits-only endpoints: second request costs at least 10% less",
	})
}

func kiroCacheProbeCreditEvidence(attempts []kiroCacheProbeAttempt) (float64, bool) {
	if len(attempts) != 2 || !attempts[0].Credits.Present || !attempts[1].Credits.Present || attempts[0].Credits.Value <= 0 {
		return 0, false
	}
	reduction := (attempts[0].Credits.Value - attempts[1].Credits.Value) / attempts[0].Credits.Value
	percent := math.Round(reduction*10000) / 100
	return percent, reduction >= 0.10
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
