package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
	"github.com/google/uuid"
	"github.com/tidwall/sjson"
)

func (s *Server) kiroMessagesWithLease(w http.ResponseWriter, r *http.Request, raw []byte, model string, affinity routing.AffinityKey, lease scheduler.Lease, _ bool, _ map[string]bool) attemptOutcome {
	defer lease.Release()
	// Request bodies are immutable after capture. Keep the original slice for goal
	// continuity without duplicating multi-hundred-thousand-token compaction bodies;
	// fallback replaces the local raw variable with a newly allocated reduce body.
	originalRaw := raw
	streamRequest := isStreamRequest(originalRaw)
	nativeCompaction := kirowire.IsClaudeCodeCompactionRequest(originalRaw)
	if nativeCompaction {
		w.Header().Set("X-MiCliProxy-Compaction-Order", "claude_code_native,kiro_summary")
	}

	// A local context proof can start the Kiro map/reduce before the ordinary Kiro
	// request is opened. Start the heartbeat early for a genuine streaming Claude
	// Code compaction so those internal stages do not create a new idle window.
	stopKeepaliveRun := func() {}
	keepaliveActive := false
	trailersDeclared := false
	startKeepalive := func() {
		if keepaliveActive || !streamRequest {
			return
		}
		if !trailersDeclared {
			declareKiroTrailers(w)
			trailersDeclared = true
		}
		stopKeepaliveRun = startSchedulerWaitKeepalive(r.Context(), s.streamKeepAliveInterval(r.Context()))
		keepaliveActive = true
	}
	stopKeepalive := func() {
		if !keepaliveActive {
			return
		}
		stopKeepaliveRun()
		stopKeepaliveRun = func() {}
		keepaliveActive = false
	}
	if nativeCompaction {
		startKeepalive()
	}
	defer func() { stopKeepalive() }()

	converted, err := s.convertKiroRequest(r.Context(), raw, affinity, lease)
	fallbackPasses := 0
	applySummaryFallback := func(contextErr *kirowire.ContextLengthError) error {
		recoveredRaw, recovered, stages, fallbackErr := s.prepareKiroSummaryFallback(r, originalRaw, affinity, lease, contextErr)
		if fallbackErr != nil {
			fallbackPasses = stages
			recordKiroSummaryFallbackFailure(r, stages, fallbackErr)
			return fallbackErr
		}
		raw = recoveredRaw
		converted = recovered
		fallbackPasses = stages
		// Header maps must not be mutated concurrently with a heartbeat write.
		// Pause synchronously, publish both ordinary headers and declared trailers,
		// then let the caller restart the heartbeat for the final Kiro request.
		stopKeepalive()
		observeKiroSummaryFallbackMetadata(w, stages, streamRequest)
		return nil
	}
	if err != nil {
		var contextErr *kirowire.ContextLengthError
		if nativeCompaction && errors.As(err, &contextErr) {
			if fallbackErr := applySummaryFallback(contextErr); fallbackErr == nil {
				err = nil
			} else {
				stopKeepalive()
				writeKiroError(w, r, http.StatusBadRequest, annotateKiroSummaryFallbackFailure(contextErr, fallbackPasses))
				return outcomeDone
			}
		} else {
			stopKeepalive()
			writeKiroError(w, r, http.StatusBadRequest, err)
			return outcomeDone
		}
	}
	converted.SummaryFallbackEligible = converted.Compaction && fallbackPasses == 0
	converted.SummaryFallbackAttempted = fallbackPasses > 0
	converted.SummaryFallbackPasses = fallbackPasses
	stopKeepalive()
	setKiroQualityHeaders(w, converted)
	resolvedModel := firstNonEmpty(converted.Model, lease.ResolvedModel, model)
	// A streaming Kiro attempt has a silent pre-first-token window (cache-singleflight
	// wait + token refresh inside openKiroAttempt + upstream time-to-first-byte). With
	// no downstream bytes during that window a busy pool can exceed the client's
	// idle-SSE timeout ("idle timeout waiting for SSE"). Declare trailers now — before a
	// keepalive comment could commit response headers and drop the Trailer declarations —
	// then bridge the window with SSE keepalive comments. stopKeepalive is synchronous, so
	// no comment can trail the first real emitted frame. WebSearch buffers internally and
	// takes its own branch, so it is left untouched.
	if converted.WebSearch == nil && streamRequest {
		startKeepalive()
	}
	releaseFlightRaw, waitedForFlight := func() (func(), bool) {
		if converted.WebSearch != nil {
			return func() {}, false
		}
		return s.enterKiroCacheSingleflight(r.Context(), raw, affinity, lease, resolvedModel, converted.CachePointCount)
	}()
	var releaseFlightOnce sync.Once
	releaseFlight := func() { releaseFlightOnce.Do(releaseFlightRaw) }
	defer releaseFlight()
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if converted.WebSearch != nil {
		data, _, outcome := s.doKiroAttempt(w, r, &converted, lease)
		if data == nil {
			return outcome
		}
		data.Model = resolvedModel
		data.ToolDescriptionHashes = converted.ToolDescriptionHashes
		data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
		setKiroHeaders(w, data.Model, data.CompatibilityLosses, data.UsageSource)
		s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, *data, storage.KiroRuntimeCapability{})
		displayModel := downstreamKiroModel(r.Context(), data.Model)
		goalResponse := kirowire.AnthropicJSON(*data, displayModel, id)
		s.persistKiroGoalContinuity(r.Context(), r, originalRaw, goalResponse)
		if streamRequest {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(kirowire.AnthropicSSE(*data, displayModel, id))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return outcomeDone
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(goalResponse)
		return outcomeDone
	}
	if streamRequest {
		response, endpointHash, outcome := s.openKiroAttempt(w, r, &converted, lease)
		if response == nil && outcome == outcomeKiroSummaryFallback {
			releaseFlight()
			requested := requestedClaudeModelFromContext(r.Context())
			contextErr := &kirowire.ContextLengthError{
				RequestedModel: firstNonEmpty(requested.RequestedModel, converted.Model),
				KiroModel:      converted.Model, EstimatedInput: converted.EstimatedInputTokens,
				EffectiveLimit: converted.ContextWindow, ContextMode: converted.ContextMode,
				Compaction: true, UpstreamRejected: true,
			}
			if fallbackErr := applySummaryFallback(contextErr); fallbackErr != nil {
				stopKeepalive()
				writeKiroError(w, r, http.StatusBadRequest, annotateKiroSummaryFallbackFailure(contextErr, fallbackPasses))
				return outcomeDone
			}
			converted.SummaryFallbackEligible = false
			converted.SummaryFallbackAttempted = true
			converted.SummaryFallbackPasses = fallbackPasses
			setKiroQualityHeaders(w, converted)
			resolvedModel = firstNonEmpty(converted.Model, lease.ResolvedModel, model)
			startKeepalive()
			response, endpointHash, outcome = s.openKiroAttempt(w, r, &converted, lease)
		}
		releaseFlight()
		if response == nil {
			stopKeepalive()
			return outcome
		}
		for {
			// Stop the keepalive synchronously before any emitter write. The emitter
			// writes straight to w, bypassing schedulerWaitState's mutex, so a trailing
			// keepalive tick must be guaranteed gone before the first real frame.
			stopKeepalive()
			emitter := newKiroAnthropicEmitter(w, func(metering kirowire.KiroMetering, actualModel string) {
				usageSource := metering.UsageSource()
				if usageSource == kirowire.UsageSourceUnreported {
					usageSource = kirowire.UsageSourceEstimated
				}
				setKiroHeaders(w, actualModel, converted.CompatibilityLosses, usageSource)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
			}, resolvedModel, id)
			emitter.displayModel = downstreamKiroModel(r.Context(), resolvedModel)
			emitter.estimatedInputTokens = converted.EstimatedInputTokens
			data, streamErr := streamKiroResponseWithContext(r.Context(), response.Body, converted.ToolNameMap, emitter, s.responseBodyCaptureOptions(r.Context()))
			_ = response.Body.Close()
			finalizeKiroUsage(&data, converted)
			data.Model = firstNonEmpty(data.Model, resolvedModel)
			if data.UsageSource == "" {
				data.UsageSource = data.Metering.UsageSource()
			}
			data.ToolDescriptionHashes = converted.ToolDescriptionHashes
			s.persistKiroResolvedBinding(r.Context(), affinity, lease, data.Model)
			data.SingleflightWaited = waitedForFlight
			data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
			if streamErr != nil {
				normalized := normalizeKiroContextError(r.Context(), streamErr, converted)
				var contextErr *kirowire.ContextLengthError
				if !emitter.started && converted.SummaryFallbackEligible && errors.As(normalized, &contextErr) {
					startKeepalive()
					if fallbackErr := applySummaryFallback(contextErr); fallbackErr != nil {
						stopKeepalive()
						writeKiroError(w, r, http.StatusBadRequest, annotateKiroSummaryFallbackFailure(contextErr, fallbackPasses))
						return outcomeDone
					}
					converted.SummaryFallbackEligible = false
					converted.SummaryFallbackAttempted = true
					converted.SummaryFallbackPasses = fallbackPasses
					setKiroQualityHeaders(w, converted)
					resolvedModel = firstNonEmpty(converted.Model, lease.ResolvedModel, model)
					startKeepalive()
					response, endpointHash, outcome = s.openKiroAttempt(w, r, &converted, lease)
					if response == nil {
						stopKeepalive()
						return outcome
					}
					continue
				}
				if emitter.started {
					capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, data)
					setKiroTrailers(w, data.Model, data.CompatibilityLosses, data.UsageSource)
					s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, data, capabilityState)
					emitter.fail(kiroErrorCode(streamErr), streamErr)
					s.markGoalStreamRetryable(r.Context(), r, "kiro", originalRaw, "upstream_stream_error")
				} else {
					writeKiroError(w, r, http.StatusBadGateway, normalized)
				}
				return outcomeDone
			}
			capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, data)
			emitter.finish(data)
			setKiroTrailers(w, data.Model, data.CompatibilityLosses, data.UsageSource)
			s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, data, capabilityState)
			s.persistKiroGoalContinuity(r.Context(), r, originalRaw, kirowire.AnthropicJSON(data, downstreamKiroModel(r.Context(), data.Model), id))
			return outcomeDone
		}
	}

	data, endpointHash, outcome := s.doKiroAttemptOnResponse(w, r, &converted, lease, releaseFlight)
	if data == nil && outcome == outcomeKiroSummaryFallback {
		releaseFlight()
		requested := requestedClaudeModelFromContext(r.Context())
		contextErr := &kirowire.ContextLengthError{
			RequestedModel: firstNonEmpty(requested.RequestedModel, converted.Model),
			KiroModel:      converted.Model, EstimatedInput: converted.EstimatedInputTokens,
			EffectiveLimit: converted.ContextWindow, ContextMode: converted.ContextMode,
			Compaction: true, UpstreamRejected: true,
		}
		if fallbackErr := applySummaryFallback(contextErr); fallbackErr != nil {
			writeKiroError(w, r, http.StatusBadRequest, annotateKiroSummaryFallbackFailure(contextErr, fallbackPasses))
			return outcomeDone
		}
		converted.SummaryFallbackEligible = false
		converted.SummaryFallbackAttempted = true
		converted.SummaryFallbackPasses = fallbackPasses
		setKiroQualityHeaders(w, converted)
		resolvedModel = firstNonEmpty(converted.Model, lease.ResolvedModel, model)
		data, endpointHash, outcome = s.doKiroAttemptOnResponse(w, r, &converted, lease, nil)
	}
	if data == nil {
		return outcome
	}
	data.Model = firstNonEmpty(data.Model, resolvedModel)
	data.ToolDescriptionHashes = converted.ToolDescriptionHashes
	s.persistKiroResolvedBinding(r.Context(), affinity, lease, data.Model)
	data.SingleflightWaited = waitedForFlight
	data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
	capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, *data)
	setKiroHeaders(w, data.Model, data.CompatibilityLosses, data.UsageSource)
	s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, *data, capabilityState)
	goalResponse := kirowire.AnthropicJSON(*data, downstreamKiroModel(r.Context(), data.Model), id)
	s.persistKiroGoalContinuity(r.Context(), r, originalRaw, goalResponse)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(goalResponse)
	return outcomeDone
}

func (s *Server) persistKiroGoalContinuity(ctx context.Context, r *http.Request, requestBody, responseBody []byte) {
	if _, err := s.persistGoalContinuity(ctx, r, "kiro", requestBody, responseBody); err != nil {
		log.Printf("[GOAL-CONTINUITY] kiro persistence degraded request_id=%s: %v", requestIDFromContext(ctx), err)
		s.auditGoalPersistenceDegraded(ctx, "kiro_terminal", err)
	}
}

func (s *Server) kiroCountTokensWithLease(w http.ResponseWriter, r *http.Request, raw []byte, affinity routing.AffinityKey, lease scheduler.Lease) attemptOutcome {
	defer lease.Release()
	w.Header().Set("X-Pool-Kiro-Thinking", "not_applicable")
	w.Header().Set("X-Pool-Kiro-Effort", "not_applicable")
	model := firstNonEmpty(lease.ResolvedModel, routing.Model(raw))
	s.persistKiroResolvedBinding(r.Context(), affinity, lease, model)
	// Count the original downstream request. Context planning must never make
	// /count_tokens look smaller by converting or dropping history first.
	count := virtual.EstimateTokensJSON(raw)
	if count < 1 {
		count = 1
	}
	setKiroHeaders(w, model, nil, kirowire.UsageSourceEstimated)
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": count, "estimated": true, "usage_source": kirowire.UsageSourceEstimated,
		"model": downstreamKiroModel(r.Context(), model),
	})
	return outcomeDone
}

func (s *Server) kiroChatWithLease(w http.ResponseWriter, r *http.Request, anthBody []byte, model string, affinity routing.AffinityKey, lease scheduler.Lease, _ bool, _ map[string]bool) attemptOutcome {
	defer lease.Release()
	converted, err := s.convertKiroRequest(r.Context(), anthBody, affinity, lease)
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return outcomeDone
	}
	setKiroQualityHeaders(w, converted)
	resolvedModel := firstNonEmpty(converted.Model, lease.ResolvedModel, model)
	// Bridge the silent pre-first-token window with SSE keepalive comments; see
	// kiroMessagesWithLease for the full rationale. Trailers are declared before the
	// keepalive can commit headers, and stopKeepalive is synchronous so no comment can
	// trail the first real frame written by the anthropicStreamToChatSSE goroutine.
	stopKeepalive := func() {}
	if converted.WebSearch == nil && isStreamRequest(anthBody) {
		declareKiroTrailers(w)
		stopKeepalive = startSchedulerWaitKeepalive(r.Context(), s.streamKeepAliveInterval(r.Context()))
	}
	defer stopKeepalive()
	releaseFlight, waitedForFlight := func() (func(), bool) {
		if converted.WebSearch != nil {
			return func() {}, false
		}
		return s.enterKiroCacheSingleflight(r.Context(), anthBody, affinity, lease, resolvedModel, converted.CachePointCount)
	}()
	defer releaseFlight()
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if converted.WebSearch != nil {
		data, _, outcome := s.doKiroAttempt(w, r, &converted, lease)
		if data == nil {
			return outcome
		}
		data.Model = resolvedModel
		data.ToolDescriptionHashes = converted.ToolDescriptionHashes
		data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
		setKiroHeaders(w, data.Model, data.CompatibilityLosses, data.UsageSource)
		s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, *data, storage.KiroRuntimeCapability{})
		displayModel := downstreamKiroModel(r.Context(), data.Model)
		if isStreamRequest(anthBody) {
			w.Header().Set("Content-Type", "text/event-stream")
			anthropicStreamToChatSSE(w, bytes.NewReader(kirowire.AnthropicSSE(*data, displayModel, id)), displayModel, streamrewrite.New(nil))
			return outcomeDone
		}
		body, err := prompt.AnthropicToChatCompletion(kirowire.AnthropicJSON(*data, displayModel, id), displayModel)
		if err != nil {
			writeKiroError(w, r, http.StatusBadGateway, err)
			return outcomeDone
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return outcomeDone
	}
	if isStreamRequest(anthBody) {
		response, endpointHash, outcome := s.openKiroAttempt(w, r, &converted, lease)
		releaseFlight()
		// Stop the keepalive synchronously before startChat can spawn the SSE-writing
		// goroutine, so no keepalive comment can interleave with a real frame on w.
		stopKeepalive()
		if response == nil {
			return outcome
		}
		defer response.Body.Close()
		reader, writer := io.Pipe()
		convertedDone := make(chan struct{})
		started := false
		startChat := func(metering kirowire.KiroMetering, actualModel string) {
			if started {
				return
			}
			started = true
			usageSource := metering.UsageSource()
			if usageSource == kirowire.UsageSourceUnreported {
				usageSource = kirowire.UsageSourceEstimated
			}
			setKiroHeaders(w, actualModel, converted.CompatibilityLosses, usageSource)
			w.Header().Set("Content-Type", "text/event-stream")
			go func() {
				defer supervisor.Recover("kiro-chat-stream")
				defer close(convertedDone)
				defer reader.Close()
				anthropicStreamToChatSSE(w, reader, downstreamKiroModel(r.Context(), resolvedModel), streamrewrite.New(nil))
			}()
		}
		emitter := newKiroAnthropicEmitter(writer, startChat, resolvedModel, id)
		emitter.displayModel = downstreamKiroModel(r.Context(), resolvedModel)
		emitter.estimatedInputTokens = converted.EstimatedInputTokens
		data, streamErr := streamKiroResponseWithContext(r.Context(), response.Body, converted.ToolNameMap, emitter, s.responseBodyCaptureOptions(r.Context()))
		finalizeKiroUsage(&data, converted)
		data.Model = firstNonEmpty(data.Model, resolvedModel)
		if data.UsageSource == "" {
			data.UsageSource = data.Metering.UsageSource()
		}
		data.ToolDescriptionHashes = converted.ToolDescriptionHashes
		s.persistKiroResolvedBinding(r.Context(), affinity, lease, data.Model)
		data.SingleflightWaited = waitedForFlight
		data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
		if streamErr != nil {
			if emitter.started {
				capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, data)
				setKiroTrailers(w, data.Model, data.CompatibilityLosses, data.UsageSource)
				s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, data, capabilityState)
				emitter.fail(kiroErrorCode(streamErr), streamErr)
				_ = writer.Close()
				<-convertedDone
			} else {
				_ = writer.CloseWithError(streamErr)
				writeKiroError(w, r, http.StatusBadGateway, normalizeKiroContextError(r.Context(), streamErr, converted))
			}
			return outcomeDone
		}
		capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, data)
		emitter.finish(data)
		_ = writer.Close()
		if started {
			<-convertedDone
		}
		setKiroTrailers(w, data.Model, data.CompatibilityLosses, data.UsageSource)
		s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, data, capabilityState)
		return outcomeDone
	}

	data, endpointHash, outcome := s.doKiroAttemptOnResponse(w, r, &converted, lease, releaseFlight)
	if data == nil {
		return outcome
	}
	data.Model = firstNonEmpty(data.Model, resolvedModel)
	data.ToolDescriptionHashes = converted.ToolDescriptionHashes
	s.persistKiroResolvedBinding(r.Context(), affinity, lease, data.Model)
	data.SingleflightWaited = waitedForFlight
	data.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, data.CompatibilityLosses)
	capabilityState := s.observeKiroResponse(r.Context(), lease.Account.ID, endpointHash, converted, *data)
	setKiroHeaders(w, data.Model, data.CompatibilityLosses, data.UsageSource)
	s.recordKiroUsage(r, lease.Account.ID, affinity, lease.RouteEpoch, data.Model, *data, capabilityState)
	displayModel := downstreamKiroModel(r.Context(), data.Model)
	body, err := prompt.AnthropicToChatCompletion(kirowire.AnthropicJSON(*data, displayModel, id), displayModel)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return outcomeDone
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	return outcomeDone
}

func (s *Server) convertKiroRequest(ctx context.Context, raw []byte, affinity routing.AffinityKey, lease scheduler.Lease) (kirowire.Conversion, error) {
	kiroCfg := s.effectiveKiroConfig(ctx)
	if lease.ResolvedModel != "" {
		raw = setForcedModel(raw, lease.ResolvedModel)
	}
	credentials, err := s.store.GetKiroCredentials(ctx, lease.Account.ID)
	if err != nil {
		return kirowire.Conversion{}, err
	}
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, firstNonEmpty(credentials.APIRegion, kiroCfg.KiroDefaultAPIRegion, "us-east-1"), kiroCfg.KiroEndpointAllowlist)
	if err != nil {
		return kirowire.Conversion{}, err
	}
	region := firstNonEmpty(credentials.APIRegion, kiroCfg.KiroDefaultAPIRegion, "us-east-1")
	capabilityKey, _ := kirowire.KiroCapabilityKey(endpointHash, region, credentials.ProfileARN)
	catalog, err := s.store.ListKiroModelCatalog(ctx, lease.Account.ID, capabilityKey)
	if err != nil {
		return kirowire.Conversion{}, err
	}
	// Claude-family Kiro calls require an adaptive-thinking observation before
	// they are reusable. Kiro's exact GPT models deliberately use the ordinary
	// (non-thinking) generation envelope, so their successful model observation
	// is the relevant evidence instead.
	requestedKiroModel := firstNonEmpty(lease.ResolvedModel, routing.Model(raw))
	verified, err := s.store.VerifiedKiroModels(ctx, lease.Account.ID, endpointHash, !capability.KiroSupportsGPTModel(requestedKiroModel))
	if err != nil {
		return kirowire.Conversion{}, err
	}
	capabilityModel, _ := capability.ResolveKiroModel(requestedKiroModel, verified)
	var catalogModel storage.KiroModelDescriptor
	catalogModelFound := false
	for _, descriptor := range catalog {
		if strings.EqualFold(strings.TrimSpace(descriptor.UpstreamID), strings.TrimSpace(requestedKiroModel)) {
			catalogModel, catalogModelFound = descriptor, true
			break
		}
	}
	if !catalogModelFound {
		catalogModel, catalogModelFound = capability.ResolveKiroCatalogModel(requestedKiroModel, catalog)
	}
	if catalogModelFound {
		capabilityModel = catalogModel.PublicID
		verified = append(verified, catalogModel.PublicID, catalogModel.UpstreamID)
	}
	requestedModel := requestedClaudeModelFromContext(ctx)
	compaction := kirowire.IsClaudeCodeCompactionRequest(raw)
	effectiveContextMode := requestedModel.ContextMode
	if strings.EqualFold(strings.TrimSpace(effectiveContextMode), "1m") &&
		(!catalogModelFound || catalogModel.MaxInputTokens < 1_000_000) {
		return kirowire.Conversion{}, fmt.Errorf("%w: 1m catalog capability unavailable", kirowire.ErrVerifiedModelUnavailable)
	}
	measuredWindow := int64(0)
	if capabilityModel != "" {
		storedWindow, windowErr := s.store.BestNativeWindow(ctx, lease.Account.ID, capabilityModel)
		if strings.EqualFold(strings.TrimSpace(effectiveContextMode), "1m") {
			storedWindow, windowErr = s.store.BestNativeMaxWindow(ctx, lease.Account.ID, capabilityModel)
		}
		if windowErr != nil {
			return kirowire.Conversion{}, windowErr
		}
		if storedWindow > 0 {
			measuredWindow = storedWindow
		}
	}
	contextWindow := capability.KiroEffectiveContextWindow(capabilityModel, effectiveContextMode, measuredWindow)
	if catalogModelFound && catalogModel.MaxInputTokens > 0 && catalogModel.MaxInputTokens < contextWindow {
		contextWindow = catalogModel.MaxInputTokens
	}
	cachePointsEnabled := strings.EqualFold(strings.TrimSpace(kiroCfg.KiroCacheMode), "auto")
	if cachePointsEnabled {
		capabilityState, capabilityErr := s.store.GetKiroRuntimeCapability(ctx, lease.Account.ID, endpointHash, capabilityModel)
		if capabilityErr == nil && capabilityState.CachePointState == "unsupported" {
			cachePointsEnabled = false
		} else if capabilityErr != nil && !storage.NotFound(capabilityErr) {
			return kirowire.Conversion{}, capabilityErr
		}
	}
	if cachePointsEnabled {
		// Reuse the established max-hit planner, then translate its at-most-four
		// Anthropic markers into native Kiro cachePoint objects below.
		raw = prompt.EnsureAnthropicCacheControlWithOptions(raw, prompt.AnthropicCacheControlOptions{
			Policy: "max_hit", LatestTailWrite: true, PreferRecentTurnRead: true,
		})
	}
	adaptiveSupported, adaptiveKnown := kirowire.CatalogAdaptiveThinking(catalogModel)
	maximumEffort, effortKnown := kirowire.CatalogMaximumEffort(catalogModel)
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(raw, affinity.Hash, kirowire.ConversionOptions{
		DefaultThinking:           kiroCfg.KiroDefaultThinking,
		ForceMaxQuality:           true,
		EnableCachePoints:         cachePointsEnabled,
		ContextWindow:             contextWindow,
		VerifiedModels:            verified,
		CatalogPublicModel:        catalogModel.PublicID,
		CatalogUpstreamModel:      catalogModel.UpstreamID,
		MaxOutputTokens:           catalogModel.MaxOutputTokens,
		AdaptiveThinkingKnown:     adaptiveKnown,
		AdaptiveThinkingSupported: adaptiveSupported,
		EffortKnown:               effortKnown,
		MaxThinkingEffort:         maximumEffort,
		Compaction:                compaction,
		ContextMode:               effectiveContextMode,
	})
	if err != nil {
		var contextErr *kirowire.ContextLengthError
		if errors.As(err, &contextErr) {
			contextErr.RequestedModel = firstNonEmpty(requestedModel.RequestedModel, routing.Model(raw))
			contextErr.ContextMode = effectiveContextMode
			contextErr.Compaction = compaction
		}
	}
	return converted, err
}

func (s *Server) doKiroAttempt(w http.ResponseWriter, r *http.Request, converted *kirowire.Conversion, lease scheduler.Lease) (*kirowire.ResponseData, string, attemptOutcome) {
	return s.doKiroAttemptOnResponse(w, r, converted, lease, nil)
}

func (s *Server) doKiroAttemptOnResponse(w http.ResponseWriter, r *http.Request, converted *kirowire.Conversion, lease scheduler.Lease, onResponse func()) (*kirowire.ResponseData, string, attemptOutcome) {
	response, endpointHash, outcome := s.openKiroAttempt(w, r, converted, lease)
	if response == nil {
		return nil, endpointHash, outcome
	}
	if onResponse != nil {
		onResponse()
	}
	var data kirowire.ResponseData
	var err error
	if converted.WebSearch != nil {
		var raw []byte
		raw, err = readLimited(response.Body, s.cfg.MaxBodyBytes)
		if err == nil {
			data, err = kirowire.DecodeWebSearchResponse(raw, *converted.WebSearch, converted.EstimatedInputTokens)
		}
	} else {
		data, err = kirowire.DecodeResponseWithOptions(r.Context(), response.Body, converted.ToolNameMap, s.responseBodyCaptureOptions(r.Context()))
	}
	response.Body.Close()
	if err != nil {
		normalized := normalizeKiroContextError(r.Context(), err, *converted)
		if converted.SummaryFallbackEligible && errors.Is(normalized, kirowire.ErrContextTooLong) {
			return nil, endpointHash, outcomeKiroSummaryFallback
		}
		writeKiroError(w, r, http.StatusBadGateway, normalized)
		return nil, endpointHash, outcomeDone
	}
	finalizeKiroUsage(&data, *converted)
	if converted.WebSearch == nil && strings.TrimSpace(data.Text) == "" && len(data.Tools) == 0 {
		// generateAssistantResponse is a non-idempotent, metered POST. An empty
		// successful response may already have consumed credits, so transparently
		// replaying it can double-charge and create a request burst.
		writePoolCodeError(w, http.StatusBadGateway, "kiro_empty_response", "Kiro completed without text or tool output")
		return nil, endpointHash, outcomeDone
	}
	return &data, endpointHash, outcomeDone
}

func finalizeKiroUsage(data *kirowire.ResponseData, converted kirowire.Conversion) {
	if data == nil {
		return
	}
	data.CachePointCount = converted.CachePointCount
	data.CachePointBreakpoints = append([]kirowire.KiroCachePointBreakpoint(nil), converted.CachePointBreakpoints...)
	if data.Metering.UsageSource() == kirowire.UsageSourceUpstream {
		data.UsageSource = kirowire.UsageSourceUpstream
		return
	}
	if data.UsageSource == kirowire.UsageSourceEstimated && data.InputTokens > 0 {
		return
	}
	data.InputTokens = maxKiroInt64(1, converted.EstimatedInputTokens)
	outputBytes := len(data.Text) + len(data.Thinking)
	for _, tool := range data.Tools {
		outputBytes += len(tool.Name) + len(tool.Input)
	}
	data.OutputTokens = maxKiroInt64(1, int64((outputBytes+3)/4))
	data.UsageSource = kirowire.UsageSourceEstimated
}

func maxKiroInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func downstreamKiroModel(ctx context.Context, fallback string) string {
	requested := requestedClaudeModelFromContext(ctx)
	return firstNonEmpty(requested.RequestedModel, fallback)
}

func normalizeKiroContextError(ctx context.Context, err error, converted kirowire.Conversion) error {
	if !errors.Is(err, kirowire.ErrContextTooLong) {
		return err
	}
	requested := requestedClaudeModelFromContext(ctx)
	return &kirowire.ContextLengthError{
		RequestedModel: firstNonEmpty(requested.RequestedModel, converted.Model),
		KiroModel:      converted.Model, EstimatedInput: converted.EstimatedInputTokens,
		EffectiveLimit: converted.ContextWindow, ContextMode: converted.ContextMode, Compaction: converted.Compaction,
		UpstreamRejected: true, KiroSummaryFallbackAttempted: converted.SummaryFallbackAttempted,
	}
}

func minKiroInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (s *Server) openKiroAttempt(w http.ResponseWriter, r *http.Request, converted *kirowire.Conversion, lease scheduler.Lease) (*upstream.Response, string, attemptOutcome) {
	// Claude-family Kiro calls require the native max-quality envelope. Kiro's
	// GPT-5.6 models intentionally omit that Claude-only envelope and use their
	// upstream defaults, so they are exempt from this invariant.
	if converted.WebSearch == nil && !capability.KiroSupportsGPTModel(converted.Model) && (!converted.ThinkingEnabled || converted.ThinkingEffort != "max" || converted.MaxOutputTokens <= 0) {
		writeKiroError(w, r, http.StatusInternalServerError, errors.New("mandatory Kiro adaptive thinking/max-effort invariant was not applied"))
		return nil, "", outcomeDone
	}
	kiroCfg := s.effectiveKiroConfig(r.Context())
	s.kiro.UpdateConfig(kiroCfg)
	credentials, err := s.store.GetKiroCredentials(r.Context(), lease.Account.ID)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return nil, "", outcomeDone
	}
	region := firstNonEmpty(credentials.APIRegion, kiroCfg.KiroDefaultAPIRegion, "us-east-1")
	target, err := kirowire.GenerateAssistantResponseEndpoint(credentials.Endpoint, region, kiroCfg.KiroEndpointAllowlist)
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return nil, "", outcomeDone
	}
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, region, kiroCfg.KiroEndpointAllowlist)
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return nil, "", outcomeDone
	}
	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return nil, endpointHash, outcomeDone
	}
	bearer, token, credentials, err := s.kiro.Prepare(r.Context(), lease.Account, credentials, token, lease.Egress, false)
	if err != nil {
		if errors.Is(err, kirowire.ErrInvalidGrant) {
			s.scheduler.InvalidateAccountCache()
		}
		s.kiroAttemptError(w, r, lease, 0, nil, []byte(err.Error()), false, nil, err)
		return nil, endpointHash, outcomeDone
	}
	body := converted.Body
	fallbackBody := converted.BodyWithoutCachePoints
	if credentials.ProfileARN != "" {
		if updated, updateErr := sjson.SetBytes(body, "profileArn", credentials.ProfileARN); updateErr == nil {
			body = updated
		}
		if len(fallbackBody) > 0 {
			if updated, updateErr := sjson.SetBytes(fallbackBody, "profileArn", credentials.ProfileARN); updateErr == nil {
				fallbackBody = updated
			}
		}
	}
	requestHeaders := kirowire.Headers(kiroCfg, credentials, bearer, true)
	if converted.WebSearch != nil {
		target = strings.TrimSuffix(strings.TrimRight(target, "/"), "/generateAssistantResponse") + "/mcp"
		body, _ = json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": converted.WebSearch.ToolUseID, "method": "tools/call",
			"params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": converted.WebSearch.Query}},
		})
		requestHeaders.Set("accept", "application/json")
	} else {
		kirowire.ApplyOperationHeaders(requestHeaders, kirowire.OperationGenerateAssistantResponse)
	}
	send := func(accessToken string, requestBody []byte) (*upstream.Response, error) {
		headers := requestHeaders.Clone()
		headers.Set("authorization", "Bearer "+accessToken)
		return s.upstream.DoRawSource(r.Context(), lease.Egress, http.MethodPost, target, headers, bodysource.Bytes(requestBody), lease.Binding.CookieJarKey)
	}
	response, err := send(bearer, body)
	if err != nil {
		s.kiroAttemptError(w, r, lease, 0, nil, nil, false, nil, err)
		return nil, endpointHash, outcomeDone
	}
	if response.StatusCode == http.StatusUnauthorized {
		header := response.Header
		rawError := readUpstreamErrorBody(response.Body)
		response.Body.Close()
		if credentials.AuthMethod != "api_key" && kiroAccessTokenExpired(rawError) {
			refreshed, _, updated, refreshErr := s.kiro.Prepare(r.Context(), lease.Account, credentials, token, lease.Egress, true)
			if refreshErr != nil {
				// Refresh failures are surfaced as an availability failure without
				// classifying the already-expired access token as an account ban.
				s.kiroAttemptError(w, r, lease, http.StatusServiceUnavailable, header, rawError, false, nil, refreshErr)
				return nil, endpointHash, outcomeDone
			}
			credentials = updated
			bearer = refreshed
			response, err = send(bearer, body)
			if err != nil {
				s.kiroAttemptError(w, r, lease, 0, nil, nil, false, nil, err)
				return nil, endpointHash, outcomeDone
			}
		} else {
			// generateAssistantResponse is metered and non-idempotent. Preserve the
			// first error body and return it without replaying a generic 401/API-key
			// rejection or permanently invalidating an otherwise recoverable account.
			s.kiroAttemptError(w, r, lease, http.StatusServiceUnavailable, header, rawError, false, nil, nil)
			return nil, endpointHash, outcomeDone
		}
	}
	activeBody := body
	activeFallbackBody := fallbackBody
	cacheFallbackUsed := false
	for response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		header := response.Header
		rawError := readUpstreamErrorBody(response.Body)
		response.Body.Close()

		if kirowire.ContentLengthExceeded(rawError) {
			// Preserve the exact downstream prompt and requested output budget.
			// Ordinary turns first return the structured signal that activates the
			// native Claude Code compactor. A genuine compaction retry may enter the
			// one bounded Kiro map/reduce fallback, but only before any model content
			// has been emitted and never by silently dropping history.
			if converted.SummaryFallbackEligible {
				return nil, endpointHash, outcomeKiroSummaryFallback
			}
			requested := requestedClaudeModelFromContext(r.Context())
			writeKiroError(w, r, http.StatusBadRequest, &kirowire.ContextLengthError{
				RequestedModel: firstNonEmpty(requested.RequestedModel, converted.Model),
				KiroModel:      converted.Model, EstimatedInput: converted.EstimatedInputTokens,
				EffectiveLimit: converted.ContextWindow, ContextMode: converted.ContextMode, Compaction: converted.Compaction,
				UpstreamRejected: true, KiroSummaryFallbackAttempted: converted.SummaryFallbackAttempted,
			})
			return nil, endpointHash, outcomeDone
		}

		if (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && converted.CachePointCount > 0 && len(activeFallbackBody) > 0 && kirowire.CachePointRejected(rawError) {
			// Unknown endpoint/model capabilities are optimistic. Retry only when the
			// validation response actually identifies cachePoint; an unrelated 400 must
			// never poison the account/model capability state.
			cacheFallbackUsed = true
			activeBody = activeFallbackBody
			activeFallbackBody = nil
			converted.Body = activeBody
			converted.BodyWithoutCachePoints = nil
			converted.CachePointCount = 0
			converted.CachePointBreakpoints = nil
			converted.CompatibilityLosses = mergeCompatibilityLosses(converted.CompatibilityLosses, []string{kirowire.LossCacheControlNotForwarded})
			response, err = send(bearer, activeBody)
			if err != nil {
				s.kiroAttemptError(w, r, lease, 0, nil, nil, false, nil, err)
				return nil, endpointHash, outcomeDone
			}
			continue
		}

		downstreamStatus := status
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			downstreamStatus = http.StatusServiceUnavailable
		}
		s.kiroAttemptError(w, r, lease, downstreamStatus, header, rawError, false, nil, nil)
		return nil, endpointHash, outcomeDone
	}
	if cacheFallbackUsed {
		if stateErr := s.store.SetKiroCachePointState(r.Context(), lease.Account.ID, endpointHash, converted.Model, "unsupported"); stateErr != nil {
			log.Printf("[KIRO-CAPABILITY] account=%s model=%s cachePoint fallback persistence failed: %v", lease.Account.ID, converted.Model, stateErr)
		}
	} else if converted.CachePointCount > 0 {
		if stateErr := s.store.SetKiroCachePointState(r.Context(), lease.Account.ID, endpointHash, converted.Model, "verified"); stateErr != nil {
			log.Printf("[KIRO-CAPABILITY] account=%s model=%s cachePoint verification persistence failed: %v", lease.Account.ID, converted.Model, stateErr)
		}
	}
	return response, endpointHash, outcomeDone
}

func streamKiroResponse(reader io.Reader, names map[string]string, emitter *kiroAnthropicEmitter, captureOptions ...bodysource.CaptureOptions) (kirowire.ResponseData, error) {
	return streamKiroResponseWithContext(context.Background(), reader, names, emitter, captureOptions...)
}

func streamKiroResponseWithContext(ctx context.Context, reader io.Reader, names map[string]string, emitter *kiroAnthropicEmitter, captureOptions ...bodysource.CaptureOptions) (kirowire.ResponseData, error) {
	decoder := kirowire.NewDecoder()
	var options bodysource.CaptureOptions
	if len(captureOptions) > 0 {
		options = captureOptions[0]
	}
	processor := kirowire.NewResponseProcessorWithOptions(ctx, names, options)
	defer processor.Close()
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			frames, err := decoder.Feed(buffer[:n])
			if err != nil {
				return processor.Data(), err
			}
			for _, frame := range frames {
				deltas, err := processor.ProcessFrame(frame)
				if err != nil {
					return processor.Data(), err
				}
				if model := processor.Model(); model != "" {
					emitter.model = model
				}
				for _, delta := range deltas {
					emitter.delta(delta, processor.Metering())
				}
			}
		}
		if readErr == io.EOF {
			if err := decoder.Finish(); err != nil {
				return processor.Data(), err
			}
			break
		}
		if readErr != nil {
			return processor.Data(), readErr
		}
	}
	deltas, err := processor.Finish()
	for _, delta := range deltas {
		emitter.delta(delta, processor.Metering())
	}
	return processor.Data(), err
}

type kiroAnthropicEmitter struct {
	w                    io.Writer
	onStart              func(kirowire.KiroMetering, string)
	flusher              interface{ Flush() }
	model                string
	displayModel         string
	id                   string
	started              bool
	blockOpen            bool
	blockKind            string
	blockToolID          string
	index                int
	estimatedInputTokens int64
}

func newKiroAnthropicEmitter(writer io.Writer, onStart func(kirowire.KiroMetering, string), model, id string) *kiroAnthropicEmitter {
	emitter := &kiroAnthropicEmitter{w: writer, onStart: onStart, model: model, displayModel: model, id: id}
	if flusher, ok := writer.(interface{ Flush() }); ok {
		emitter.flusher = flusher
	}
	return emitter
}

func (e *kiroAnthropicEmitter) emit(event string, value any) {
	raw, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, raw)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *kiroAnthropicEmitter) start(metering kirowire.KiroMetering) {
	if e.started {
		return
	}
	e.started = true
	if e.onStart != nil {
		e.onStart(metering, e.model)
	}
	inputTokens := metering.InputTokens.Value
	if !metering.InputTokens.Present {
		inputTokens = maxKiroInt64(1, e.estimatedInputTokens)
	}
	usage := map[string]any{"input_tokens": inputTokens, "output_tokens": 0}
	addKiroCacheUsage(usage, metering)
	e.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": e.id, "type": "message", "role": "assistant", "model": e.displayModel, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage,
	}})
}

func (e *kiroAnthropicEmitter) closeBlock() {
	if !e.blockOpen {
		return
	}
	e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.index})
	e.index++
	e.blockOpen = false
	e.blockKind = ""
	e.blockToolID = ""
}

func (e *kiroAnthropicEmitter) delta(delta kirowire.ResponseDelta, metering kirowire.KiroMetering) {
	e.start(metering)
	switch delta.Kind {
	case "thinking", "text":
		if e.blockOpen && e.blockKind != delta.Kind {
			e.closeBlock()
		}
		if !e.blockOpen {
			e.blockOpen = true
			e.blockKind = delta.Kind
			block := map[string]any{"type": delta.Kind}
			block[delta.Kind] = ""
			e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": e.index, "content_block": block})
		}
		deltaType := "text_delta"
		payload := map[string]any{"type": deltaType, "text": delta.Text}
		if delta.Kind == "thinking" {
			payload = map[string]any{"type": "thinking_delta", "thinking": delta.Text}
		}
		e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.index, "delta": payload})
	case "tool":
		if e.blockOpen && (e.blockKind != "tool" || e.blockToolID != delta.ToolID) {
			e.closeBlock()
		}
		if !e.blockOpen {
			e.blockOpen = true
			e.blockKind = "tool"
			e.blockToolID = delta.ToolID
			e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": e.index, "content_block": map[string]any{
				"type": "tool_use", "id": delta.ToolID, "name": delta.ToolName, "input": map[string]any{},
			}})
		}
		if delta.ToolInput != "" {
			e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.index, "delta": map[string]any{
				"type": "input_json_delta", "partial_json": delta.ToolInput,
			}})
		}
	}
}

func (e *kiroAnthropicEmitter) finish(data kirowire.ResponseData) {
	e.start(data.Metering)
	e.closeBlock()
	// metadataEvent commonly arrives after content has already started. Repeat the
	// authoritative input value in the final delta so streaming clients can replace
	// the provisional message_start estimate without delaying the first token.
	usage := map[string]any{"input_tokens": data.InputTokens, "output_tokens": data.OutputTokens}
	addKiroCacheUsage(usage, data.Metering)
	if data.WebSearch != nil {
		usage["server_tool_use"] = map[string]any{"web_search_requests": 1}
	}
	e.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": data.StopReason, "stop_sequence": nil}, "usage": usage})
	e.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (e *kiroAnthropicEmitter) fail(code string, err error) {
	e.start(kirowire.KiroMetering{})
	e.closeBlock()
	e.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": code, "message": err.Error()}})
	e.emit("message_stop", map[string]any{"type": "message_stop"})
}

func addKiroCacheUsage(usage map[string]any, metering kirowire.KiroMetering) {
	if metering.CacheReadTokens.Present {
		usage["cache_read_input_tokens"] = metering.CacheReadTokens.Value
	}
	if metering.CacheCreationTokens.Present {
		usage["cache_creation_input_tokens"] = metering.CacheCreationTokens.Value
	}
}

func setKiroHeaders(w http.ResponseWriter, model string, losses []string, usageSource string) {
	w.Header().Set("X-Pool-Resolved-Provider", "kiro")
	w.Header().Set("X-Pool-Resolved-Model", model)
	if len(losses) == 0 {
		w.Header().Set("X-Pool-Kiro-Compatibility", "none")
	} else {
		w.Header().Set("X-Pool-Kiro-Compatibility", strings.Join(mergeCompatibilityLosses(losses), ","))
	}
	if usageSource == "" {
		usageSource = kirowire.UsageSourceUnreported
	}
	w.Header().Set("X-Pool-Usage-Source", usageSource)
}

func setKiroQualityHeaders(w http.ResponseWriter, conversion kirowire.Conversion) {
	if conversion.WebSearch != nil {
		w.Header().Set("X-Pool-Kiro-Thinking", "not_applicable")
		w.Header().Set("X-Pool-Kiro-Effort", "not_applicable")
		return
	}
	if conversion.ThinkingEnabled {
		w.Header().Set("X-Pool-Kiro-Thinking", "adaptive")
	} else {
		w.Header().Set("X-Pool-Kiro-Thinking", "disabled")
	}
	w.Header().Set("X-Pool-Kiro-Effort", firstNonEmpty(conversion.ThinkingEffort, "unspecified"))
	if conversion.MaxOutputTokens > 0 {
		w.Header().Set("X-Pool-Kiro-Max-Output-Tokens", fmt.Sprintf("%d", conversion.MaxOutputTokens))
	}
	if conversion.ContextWindow > 0 {
		w.Header().Set("X-Pool-Kiro-Context-Window", fmt.Sprintf("%d", conversion.ContextWindow))
	}
	if conversion.EstimatedInputTokens > 0 {
		w.Header().Set("X-Pool-Kiro-Estimated-Input-Tokens", fmt.Sprintf("%d", conversion.EstimatedInputTokens))
	}
	if conversion.HistoryMessagesDropped > 0 {
		w.Header().Set("X-Pool-Kiro-History-Messages-Dropped", fmt.Sprintf("%d", conversion.HistoryMessagesDropped))
	}
}

func declareKiroTrailers(w http.ResponseWriter) {
	w.Header().Add("Trailer", "X-Pool-Resolved-Model")
	w.Header().Add("Trailer", "X-Pool-Kiro-Compatibility")
	w.Header().Add("Trailer", "X-Pool-Usage-Source")
	for _, name := range []string{
		"X-MiCliProxy-Context-Status", "X-MiCliProxy-Context-Limit",
		"X-MiCliProxy-Context-Limit-Source", "X-MiCliProxy-Context-Retry-Target",
		"X-MiCliProxy-Context-Estimated-Input", "X-MiCliProxy-Kiro-Model",
		"X-MiCliProxy-Auto-Compact", "X-MiCliProxy-Compaction-Stage",
		"X-MiCliProxy-Compaction-Order", "X-MiCliProxy-Kiro-Summary-Passes",
	} {
		w.Header().Add("Trailer", name)
	}
}

func setKiroTrailers(w http.ResponseWriter, model string, losses []string, usageSource string) {
	compatibility := "none"
	if merged := mergeCompatibilityLosses(losses); len(merged) > 0 {
		compatibility = strings.Join(merged, ",")
	}
	if usageSource == "" {
		usageSource = kirowire.UsageSourceUnreported
	}
	w.Header().Set(http.TrailerPrefix+"X-Pool-Resolved-Model", model)
	w.Header().Set(http.TrailerPrefix+"X-Pool-Kiro-Compatibility", compatibility)
	w.Header().Set(http.TrailerPrefix+"X-Pool-Usage-Source", usageSource)
}

func mergeCompatibilityLosses(sets ...[]string) []string {
	seen := map[string]bool{}
	for _, values := range sets {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				seen[strings.TrimSpace(value)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func writeKiroError(w http.ResponseWriter, r *http.Request, status int, err error) {
	var contextErr *kirowire.ContextLengthError
	if errors.As(err, &contextErr) {
		contextErr = kiroContextErrorForRequest(r, contextErr)
		message := contextErr.Error()
		metadata := kiroContextErrorMetadata(contextErr)
		if schedulerWaitContextLengthTerminal(r.Context(), message, metadata) {
			return
		}
		for name, value := range metadata {
			w.Header().Set(name, value)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{
			"message": message, "type": "invalid_request_error", "code": "context_length_exceeded",
		}})
		return
	}
	if schedulerWaitTerminal(r.Context(), err.Error()) {
		return
	}
	code := kiroErrorCode(err)
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": err.Error(), "type": "codex_pool_error", "code": code,
	}})
}

func kiroContextErrorForRequest(r *http.Request, contextErr *kirowire.ContextLengthError) *kirowire.ContextLengthError {
	if contextErr == nil || r == nil || r.URL == nil || r.URL.Path != "/v1/responses" {
		return contextErr
	}
	model := firstNonEmpty(contextErr.RequestedModel, contextErr.KiroModel)
	if !capability.KiroSupportsGPTModel(model) && !capability.KiroSupportsGPTModel(contextErr.KiroModel) {
		return contextErr
	}
	copy := *contextErr
	copy.NativeCompactionClient = "codex"
	return &copy
}

func kiroContextErrorMetadata(contextErr *kirowire.ContextLengthError) map[string]string {
	contextStatus := "compact_required"
	if contextErr.Compaction {
		contextStatus = "compact_failed"
	}
	limitSource := "local_planner"
	if contextErr.UpstreamRejected {
		limitSource = "upstream_unreported"
	}
	autoCompact := "client_retry"
	compactionStage := "claude_code_native"
	compactionOrder := "claude_code_native,kiro_summary"
	if strings.EqualFold(strings.TrimSpace(contextErr.NativeCompactionClient), "codex") {
		compactionStage = "codex_native"
		compactionOrder = "codex_native,kiro_retry"
	}
	if contextErr.KiroSummaryFallbackAttempted {
		autoCompact = "kiro_summary_failed"
		compactionStage = "kiro_summary_fallback"
		compactionOrder = "claude_code_native,kiro_summary"
	}
	metadata := map[string]string{
		"X-MiCliProxy-Context-Status":          contextStatus,
		"X-MiCliProxy-Context-Limit":           fmt.Sprintf("%d", contextErr.EffectiveLimit),
		"X-MiCliProxy-Context-Limit-Source":    limitSource,
		"X-MiCliProxy-Context-Retry-Target":    fmt.Sprintf("%d", contextErr.SuggestedRetryLimit()),
		"X-MiCliProxy-Context-Estimated-Input": fmt.Sprintf("%d", contextErr.EstimatedInput),
		"X-MiCliProxy-Kiro-Model":              contextErr.KiroModel,
		"X-MiCliProxy-Auto-Compact":            autoCompact,
		"X-MiCliProxy-Compaction-Stage":        compactionStage,
		"X-MiCliProxy-Compaction-Order":        compactionOrder,
	}
	if contextErr.KiroSummaryFallbackAttempted {
		metadata["X-MiCliProxy-Kiro-Summary-Passes"] = fmt.Sprintf("%d", contextErr.KiroSummaryFallbackPasses)
	}
	return metadata
}

func kiroAccessTokenExpired(body []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(body)))
	if text == "" {
		return false
	}
	compactJSON := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(text)
	for _, marker := range []string{
		`"code":"expired_token"`,
		`"code":"token_expired"`,
		`"error":"expired_token"`,
		`"error":"token_expired"`,
	} {
		if strings.Contains(compactJSON, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"access token has expired",
		"access token expired",
		"token has expired",
		"token is expired",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func writePoolCodeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message, "type": "codex_pool_error", "code": code,
	}})
}

func kiroErrorCode(err error) string {
	switch {
	case errors.Is(err, kirowire.ErrReasoningUnavailable):
		return "kiro_reasoning_unavailable"
	case errors.Is(err, kirowire.ErrInvalidToolInput):
		return "kiro_invalid_tool_input"
	case errors.Is(err, kirowire.ErrVerifiedModelUnavailable):
		return "verified_model_unavailable"
	case errors.Is(err, kirowire.ErrEndpointNotAllowed):
		return "kiro_endpoint_not_allowed"
	case errors.Is(err, kirowire.ErrContextTooLong):
		return "context_length_exceeded"
	case errors.Is(err, kirowire.ErrAccountSuspended):
		return "kiro_account_suspended"
	default:
		return "kiro_upstream_error"
	}
}

func (s *Server) effectiveKiroConfig(ctx context.Context) config.Config {
	cfg := s.cfg
	cfg.KiroVersion = s.settingString(ctx, "kiro_version", cfg.KiroVersion)
	cfg.KiroNodeVersion = s.settingString(ctx, "kiro_node_version", cfg.KiroNodeVersion)
	cfg.KiroDefaultAuthRegion = s.settingString(ctx, "kiro_default_auth_region", cfg.KiroDefaultAuthRegion)
	cfg.KiroDefaultAPIRegion = s.settingString(ctx, "kiro_default_api_region", cfg.KiroDefaultAPIRegion)
	// Mandatory quality invariant: legacy config/settings values cannot turn Kiro
	// thinking off. Request conversion additionally enforces native max-quality fields.
	cfg.KiroDefaultThinking = true
	cfg.KiroCacheMode = s.settingString(ctx, "kiro_cache_mode", cfg.KiroCacheMode)
	cfg.KiroEndpointAllowlist = s.settingCSV(ctx, "kiro_endpoint_allowlist", cfg.KiroEndpointAllowlist)
	cfg.KiroCacheUnreportedThreshold = s.settingInt(ctx, "kiro_cache_unreported_threshold", cfg.KiroCacheUnreportedThreshold)
	if cfg.KiroCacheUnreportedThreshold <= 0 {
		cfg.KiroCacheUnreportedThreshold = config.DefaultKiroCacheUnreportedThreshold
	}
	return cfg
}

func (s *Server) kiroAttemptError(w http.ResponseWriter, r *http.Request, lease scheduler.Lease, status int, header http.Header, body []byte, _ bool, _ map[string]bool, cause error) attemptOutcome {
	transient := status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status >= 500
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		transient = true
	}
	verdict := ban.Classify(false, status, header, body)
	if verdict.IsBanned() {
		s.handleBannedAccount(r.Context(), lease.Account, verdict, status, body, "upstream_error")
	} else if transient {
		verdict = s.onUpstreamError(r.Context(), lease.Account, status, header, body)
	}
	if verdict.Reason == kiroSuspensionQuarantineReason {
		writeKiroError(w, r, http.StatusServiceUnavailable, kirowire.ErrAccountSuspended)
		return outcomeDone
	}
	if cause != nil {
		if schedulerWaitTerminal(r.Context(), cause.Error()) {
			return outcomeDone
		}
		writeKiroError(w, r, http.StatusBadGateway, cause)
		return outcomeDone
	}
	code := status
	if code == 0 {
		code = http.StatusBadGateway
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(code)
	}
	if schedulerWaitTerminal(r.Context(), message) {
		return outcomeDone
	}
	if (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) && header != nil {
		if seconds := retryAfterSeconds(header, storage.Now()); seconds > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
	}
	writeKiroError(w, r, code, errors.New(message))
	return outcomeDone
}

func (s *Server) observeKiroResponse(ctx context.Context, accountID, endpointHash string, conversion kirowire.Conversion, data kirowire.ResponseData) storage.KiroRuntimeCapability {
	unknownSchema, _ := json.Marshal(data.Metering.UnknownCacheFields)
	cfg := s.effectiveKiroConfig(ctx)
	observedModel := firstNonEmpty(data.Model, conversion.Model)
	state, err := s.store.ObserveKiroCapability(ctx, accountID, endpointHash, observedModel, storage.KiroCapabilityObservation{
		ModelSucceeded:         true,
		ThinkingRequested:      conversion.ThinkingEnabled,
		MeteringEvents:         data.Metering.EventCount,
		CacheReadPresent:       data.Metering.CacheReadTokens.Present,
		CacheReadTokens:        data.Metering.CacheReadTokens.Value,
		CacheCreationPresent:   data.Metering.CacheCreationTokens.Present,
		CacheCreationTokens:    data.Metering.CacheCreationTokens.Value,
		ExplicitlyUnsupported:  data.Metering.ExplicitUnsupported,
		UnknownCacheSchemaJSON: string(unknownSchema),
		UnreportedThreshold:    cfg.KiroCacheUnreportedThreshold,
	})
	if err != nil {
		log.Printf("[KIRO-CAPABILITY] account=%s model=%s observation failed: %v", accountID, observedModel, err)
		return state
	}
	contextState, contextSource := capability.Context1MUnknown, ""
	if strings.EqualFold(strings.TrimSpace(conversion.ContextMode), "1m") {
		contextState, contextSource = capability.Context1MSupported, "runtime_inference"
	}
	if capErr := s.store.SetModelCapabilityState(ctx, accountID, observedModel, capability.AvailabilityVerified, contextState, contextSource, "kiro_runtime_inference"); capErr != nil {
		log.Printf("[KIRO-CAPABILITY] account=%s model=%s model catalog promotion failed: %v", accountID, observedModel, capErr)
	}
	return state
}

func (s *Server) persistKiroResolvedBinding(ctx context.Context, affinity routing.AffinityKey, lease scheduler.Lease, model string) {
	if affinity.Hash == "" || strings.TrimSpace(model) == "" {
		return
	}
	bound, err := s.store.GetAffinityBinding(ctx, affinity.Hash)
	if err == nil && bound.AccountID == lease.Account.ID && bound.Provider == "kiro" && bound.Model == model && bound.EgressID == lease.Egress.ID {
		return
	}
	if err != nil && !storage.NotFound(err) {
		return
	}
	_ = s.scheduler.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source,
		AccountID: lease.Account.ID, Provider: "kiro", Model: model, EgressID: lease.Egress.ID,
	})
}

func (s *Server) recordKiroUsage(r *http.Request, accountID string, affinity routing.AffinityKey, routeEpoch int64, model string, data kirowire.ResponseData, capabilityState storage.KiroRuntimeCapability) {
	rawUsage := map[string]any{
		"input_tokens": data.InputTokens, "output_tokens": data.OutputTokens,
		"usage_source":           data.UsageSource,
		"event_count":            data.Metering.EventCount,
		"metadata_event_count":   data.Metering.MetadataEventCount,
		"metering_event_count":   data.Metering.MeteringEventCount,
		"input_tokens_present":   data.Metering.InputTokens.Present,
		"output_tokens_present":  data.Metering.OutputTokens.Present,
		"cache_read_present":     data.Metering.CacheReadTokens.Present,
		"cache_creation_present": data.Metering.CacheCreationTokens.Present,
		"total_tokens_present":   data.Metering.TotalTokens.Present,
		"credits_present":        data.Metering.Credits.Present,
	}
	if data.Metering.TotalInputTokens.Present {
		rawUsage["cache_total_input_tokens"] = data.Metering.TotalInputTokens.Value
	}
	if data.Metering.TotalTokens.Present {
		rawUsage["upstream_total_tokens"] = data.Metering.TotalTokens.Value
	}
	if data.Metering.Credits.Present {
		rawUsage["kiro_credits"] = data.Metering.Credits.Value
	}
	if capabilityState.CachePointState != "" {
		rawUsage["cache_point_state"] = capabilityState.CachePointState
	}
	if len(data.ToolDescriptionHashes) > 0 {
		rawUsage["tool_description_hashes"] = data.ToolDescriptionHashes
	}
	if data.Metering.CacheReadTokens.Present {
		rawUsage["cache_read_input_tokens"] = data.CacheReadTokens
	}
	if data.Metering.CacheCreationTokens.Present {
		rawUsage["cache_creation_input_tokens"] = data.CacheCreationTokens
	}
	if data.UsageSource == kirowire.UsageSourceEstimated {
		rawUsage["estimated"] = true
	}
	if affinity.Source == "kiro_health_probe" {
		rawUsage["probe_kind"] = "kiro_health_probe"
	}
	raw, _ := json.Marshal(rawUsage)
	lossesJSON, _ := json.Marshal(data.CompatibilityLosses)
	breakpointsJSON, _ := json.Marshal(data.CachePointBreakpoints)
	keyHash, userID := downstreamFromCtx(r.Context())
	diagnostics := storage.UsageDiagnostics{
		UsageEventID:            firstNonEmpty(usageEventIDFromContext(r.Context()), requestIDFromContext(r.Context())),
		UsageProvider:           "kiro",
		UsageSource:             data.UsageSource,
		CacheReadPresent:        data.Metering.CacheReadTokens.Present,
		CacheCreationPresent:    data.Metering.CacheCreationTokens.Present,
		CompatibilityLossesJSON: string(lossesJSON),
		CacheCapability:         capabilityState.CacheCapability,
		Estimated:               data.UsageSource == kirowire.UsageSourceEstimated,
		KiroCredits:             data.Metering.Credits.Value,
		KiroCreditsPresent:      data.Metering.Credits.Present,
		CacheMissTokens:         data.InputTokens,
		CacheTotalInputTokens:   data.Metering.TotalInputTokens.Value,
		AffinitySource:          affinity.Source,
		RouteEpoch:              routeEpoch,
		CacheControlInjected:    data.CachePointCount > 0,
		CacheBreakpointCount:    data.CachePointCount,
		CacheBreakpointsJSON:    string(breakpointsJSON),
		SingleflightWaitedRequests: func() int64 {
			if data.SingleflightWaited {
				return 1
			}
			return 0
		}(),
	}
	modelDiag := modelDiagnosticsFromCtx(r.Context())
	diagnostics.BillingHoldID = holdIDFromCtx(r.Context())
	diagnostics.RequestedModel, diagnostics.ResolvedModel, diagnostics.ModelOverrideSource = modelDiag.Requested, firstNonEmpty(modelDiag.Resolved, model), modelDiag.Source
	totalInput := data.InputTokens
	if data.Metering.TotalInputTokens.Present {
		totalInput = data.Metering.TotalInputTokens.Value
	}
	s.enqueueUsage(storage.UsageRecordWrite{AccountID: accountID, RouteKeyHash: affinity.Hash, APIKeyHash: keyHash, UserID: userID, Model: model,
		Prompt: data.InputTokens, Completion: data.OutputTokens, Total: totalInput + data.OutputTokens, Cached: data.CacheReadTokens,
		CacheRead: data.CacheReadTokens, CacheCreation: data.CacheCreationTokens, Raw: raw, Diagnostics: diagnostics})
}
