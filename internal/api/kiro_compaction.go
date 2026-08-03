package api

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"github.com/tidwall/sjson"
)

const kiroSummaryFallbackMaxStages = 8
const kiroSummaryFallbackOutputTokens int64 = 8192

type kiroFallbackResponseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newKiroFallbackResponseRecorder() *kiroFallbackResponseRecorder {
	return &kiroFallbackResponseRecorder{header: make(http.Header)}
}

func (w *kiroFallbackResponseRecorder) Header() http.Header { return w.header }

func (w *kiroFallbackResponseRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *kiroFallbackResponseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// prepareKiroSummaryFallback performs a bounded map/reduce using the already
// selected Kiro account. It is called only for a genuine Claude Code compaction
// request, which means the native client has already taken the first compaction
// opportunity. Intermediate requests never reach the downstream response and do
// not mutate durable goal continuity; their real metering is still recorded.
func (s *Server) prepareKiroSummaryFallback(r *http.Request, originalRaw []byte, affinity routing.AffinityKey, lease scheduler.Lease, contextErr *kirowire.ContextLengthError) ([]byte, kirowire.Conversion, int, error) {
	if contextErr == nil || !contextErr.Compaction || !kirowire.IsClaudeCodeCompactionRequest(originalRaw) {
		return nil, kirowire.Conversion{}, 0, errors.New("Kiro summary fallback requires a native Claude Code compaction request")
	}
	retryTarget := contextErr.SuggestedRetryLimit()
	working := originalRaw
	totalStages := 0
	for totalStages < kiroSummaryFallbackMaxStages {
		stageRequests, err := kirowire.BuildClaudeCodeSummaryFallbackRequests(working, retryTarget, kiroSummaryFallbackMaxStages-totalStages)
		if err != nil {
			return nil, kirowire.Conversion{}, totalStages, err
		}
		summaries := make([]string, 0, len(stageRequests))
		for index, stageRaw := range stageRequests {
			recorder := newKiroFallbackResponseRecorder()
			internalCtx := withBufferedSchedulerWait(r.Context(), recorder)
			internalRequest := r.Clone(internalCtx)
			stageAffinity := affinity
			stageAffinity.Hash = fmt.Sprintf("%s:kiro-summary:%d", affinity.Hash, totalStages+1)
			stageAffinity.Key = fmt.Sprintf("%s:kiro-summary:%d", affinity.Key, totalStages+1)
			stageAffinity.Source = "kiro_summary_stage"
			converted, convertErr := s.convertKiroRequest(internalCtx, stageRaw, stageAffinity, lease)
			if convertErr != nil {
				return nil, kirowire.Conversion{}, totalStages, fmt.Errorf("Kiro summary stage %d conversion: %w", totalStages+1, convertErr)
			}
			// Map stages are one-shot and never replayed by the client, so cache points
			// have no reuse value here. Sending the already-built no-cache body also
			// prevents a cache-capability retry from exceeding the model-call bound.
			if len(converted.BodyWithoutCachePoints) > 0 {
				converted.Body = converted.BodyWithoutCachePoints
				converted.BodyWithoutCachePoints = nil
				converted.CachePointCount = 0
				converted.CachePointBreakpoints = nil
			}
			if converted.MaxOutputTokens > kiroSummaryFallbackOutputTokens {
				boundedBody, boundErr := sjson.SetBytes(converted.Body, "additionalModelRequestFields.max_tokens", kiroSummaryFallbackOutputTokens)
				if boundErr != nil {
					return nil, kirowire.Conversion{}, totalStages, fmt.Errorf("Kiro summary stage %d output bound: %w", totalStages+1, boundErr)
				}
				converted.Body = boundedBody
				converted.MaxOutputTokens = kiroSummaryFallbackOutputTokens
			}
			converted.SummaryFallbackAttempted = true
			data, endpointHash, _ := s.doKiroAttempt(recorder, internalRequest, &converted, lease)
			totalStages++
			if data == nil {
				status := recorder.status
				if status == 0 {
					status = http.StatusBadGateway
				}
				return nil, kirowire.Conversion{}, totalStages, fmt.Errorf("Kiro summary stage %d/%d failed with status %d", index+1, len(stageRequests), status)
			}
			data.Model = firstNonEmpty(data.Model, converted.Model, lease.ResolvedModel)
			data.ToolDescriptionHashes = converted.ToolDescriptionHashes
			data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
			s.persistKiroResolvedBinding(internalCtx, affinity, lease, data.Model)
			capabilityState := s.observeKiroResponse(internalCtx, lease.Account.ID, endpointHash, converted, *data)
			s.recordKiroUsage(internalRequest, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, *data, capabilityState)
			summary := strings.TrimSpace(data.Text)
			if summary == "" {
				return nil, kirowire.Conversion{}, totalStages, fmt.Errorf("Kiro summary stage %d/%d returned empty text", index+1, len(stageRequests))
			}
			summaries = append(summaries, summary)
		}

		working, err = kirowire.BuildClaudeCodeSummaryFallbackFinal(working, summaries)
		if err != nil {
			return nil, kirowire.Conversion{}, totalStages, err
		}
		converted, convertErr := s.convertKiroRequest(r.Context(), working, affinity, lease)
		if convertErr == nil {
			converted.SummaryFallbackAttempted = true
			converted.SummaryFallbackPasses = totalStages
			log.Printf("[KIRO-COMPACTION] request_id=%s native_client_first=true fallback=kiro_summary stages=%d", requestIDFromContext(r.Context()), totalStages)
			return working, converted, totalStages, nil
		}
		var nextContextErr *kirowire.ContextLengthError
		if !errors.As(convertErr, &nextContextErr) {
			return nil, kirowire.Conversion{}, totalStages, convertErr
		}
		// A pathological map stage can itself return a very large summary. Feed the
		// ordered summaries through another bounded level rather than truncating them.
	}
	return nil, kirowire.Conversion{}, totalStages, kirowire.ErrSummaryFallbackExhausted
}

func observeKiroSummaryFallbackMetadata(w http.ResponseWriter, stages int, stream bool) {
	values := map[string]string{
		"X-MiCliProxy-Context-Status":      "compacted",
		"X-MiCliProxy-Auto-Compact":        "kiro_summary_fallback",
		"X-MiCliProxy-Compaction-Stage":    "kiro_summary_fallback",
		"X-MiCliProxy-Compaction-Order":    "claude_code_native,kiro_summary",
		"X-MiCliProxy-Kiro-Summary-Passes": strconv.Itoa(stages),
	}
	for name, value := range values {
		w.Header().Set(name, value)
		if stream {
			w.Header().Set(http.TrailerPrefix+name, value)
		}
	}
}

func annotateKiroSummaryFallbackFailure(err *kirowire.ContextLengthError, stages int) *kirowire.ContextLengthError {
	if err == nil {
		return nil
	}
	copy := *err
	copy.KiroSummaryFallbackAttempted = true
	copy.KiroSummaryFallbackPasses = stages
	return &copy
}

func recordKiroSummaryFallbackFailure(r *http.Request, stages int, err error) {
	log.Printf("[KIRO-COMPACTION] request_id=%s native_client_first=true fallback=kiro_summary result=failed stages=%d err=%v", requestIDFromContext(r.Context()), stages, err)
}
