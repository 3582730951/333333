package api

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/virtual"
)

const standaloneSearchPath = "/v1/alpha/search"

// handleSearchPassthrough routes the stock Codex standalone-search request by
// its required model field. Unlike the other opaque auxiliary endpoints, search
// must follow the same custom-provider and two-layer user-group decision as the
// model turn that issued it.
func (s *Server) handleSearchPassthrough(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	originalRaw, err := requestBodyBytes(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	requestedModel := routing.Model(originalRaw)

	policy, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	resolvedRaw := originalRaw
	if policy.ForceModel != "" {
		resolvedRaw = setForcedModel(resolvedRaw, policy.ForceModel)
	}
	resolvedModel := routing.Model(resolvedRaw)
	r = r.WithContext(withModelDiagnostics(r.Context(), requestedModel, resolvedModel, policy.ModelOverrideSource))
	w.Header().Set("X-Pool-Requested-Model", requestedModel)
	w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
	w.Header().Set("X-Pool-Model-Override-Source", firstNonEmpty(policy.ModelOverrideSource, "none"))

	if s.dispatchUserGroupRouteCandidates(w, r, originalRaw, resolvedRaw, policy, s.handleSearchPassthrough) {
		return
	}

	userGroupProvider := ""
	if policy.UserGroupID != "" {
		routeGroup, routeProvider, routeErr := resolveUserGroupRoute(r.Context(), s.store, policy, r, resolvedRaw)
		if routeErr != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", routeErr.Error())
			return
		}
		if routeGroup != "" {
			policy.Group = routeGroup
		}
		if routeProvider != "" {
			policy.ProviderHint = routeProvider
			userGroupProvider = routeProvider
		}
	}

	hint, valid := searchProviderHint(r, policy, userGroupProvider)
	if !valid {
		s.writeCapabilityUnavailable(
			w,
			http.StatusBadRequest,
			"invalid X-Pool-Provider value",
			[]string{"standalone_web_search", "model:" + resolvedModel},
			"codex_or_responses_custom_provider",
			"standalone_search:invalid_provider",
			"Use X-Pool-Provider: codex, auto, or custom:<provider_id>.",
		)
		return
	}

	var selectedCustom storage.CustomProvider
	var selectedCustomOK bool
	switch {
	case strings.HasPrefix(hint, "custom:"):
		providerID := strings.TrimPrefix(hint, "custom:")
		provider, found := s.customProviderByID(r.Context(), providerID)
		if !found {
			s.writeCapabilityUnavailable(
				w,
				http.StatusServiceUnavailable,
				"selected custom provider is disabled or unavailable",
				[]string{"standalone_web_search", "provider:" + providerID, "model:" + resolvedModel},
				"enabled_responses_custom_provider",
				hint,
				"Enable the selected provider and import an active API-key account.",
			)
			return
		}
		selectedCustom, selectedCustomOK = customProviderSearchRoute(provider)
		if !selectedCustomOK {
			s.writeSearchCapabilityUnavailable(w, hint, resolvedModel)
			return
		}
	case hint == "auto" || hint == "":
		selectedCustom, selectedCustomOK = s.customProviderForSearchModel(
			r.Context(), resolvedModel, policy.Group, resolvedRaw,
		)
	case hint == "codex":
		// Native Codex remains the zero-configuration fallback.
	default:
		s.writeSearchCapabilityUnavailable(w, hint, resolvedModel)
		return
	}

	if selectedCustomOK {
		mappedRaw, targetModel, mapped := applyCustomProviderModelMapping(selectedCustom, resolvedRaw, resolvedModel)
		if mapped {
			resolvedRaw = mappedRaw
			resolvedModel = targetModel
			source := "custom_provider_mapping:" + selectedCustom.ID
			r = r.WithContext(withModelDiagnostics(r.Context(), requestedModel, resolvedModel, source))
			w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
			w.Header().Set("X-Pool-Model-Override-Source", source)
		}
		selectedRequest, closeRequest, buildErr := requestWithPassthroughBody(r, resolvedRaw)
		if buildErr != nil {
			writeError(w, http.StatusInternalServerError, buildErr)
			return
		}
		defer closeRequest()
		s.handleCustomProviderPassthroughWithModel(
			w, selectedRequest, selectedCustom.ID, policy, resolvedModel, resolvedRaw,
		)
		return
	}

	selectedRequest, closeRequest, buildErr := requestWithPassthroughBody(r, resolvedRaw)
	if buildErr != nil {
		writeError(w, http.StatusInternalServerError, buildErr)
		return
	}
	defer closeRequest()
	s.handleCodexPassthroughWithPolicy(w, selectedRequest, policy, resolvedModel, resolvedRaw)
}

func isStandaloneSearchPath(path string) bool {
	return strings.TrimSuffix(strings.TrimSpace(path), "/") == standaloneSearchPath
}

func searchProviderHint(r *http.Request, policy downstreamPolicy, userGroupProvider string) (string, bool) {
	if strings.TrimSpace(userGroupProvider) != "" {
		return normalizeProviderHint(userGroupProvider)
	}
	if explicit := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); explicit != "" {
		return normalizeProviderHint(explicit)
	}
	return normalizeProviderHintLoose(policy.ProviderHint), true
}

// customProviderSearchRoute applies the path-specific route before checking the
// wire capability. Standalone search is an opaque Responses-provider extension;
// chat-completions and Anthropic routes have no lossless adapter for its DTOs.
func customProviderSearchRoute(provider storage.CustomProvider) (storage.CustomProvider, bool) {
	provider, _ = storage.ResolveCustomProviderRoute(provider, standaloneSearchPath)
	return provider, provider.UpstreamProtocol == storage.CustomProviderProtocolResponses
}

func (s *Server) customProviderForSearchModel(
	ctx context.Context,
	model, group string,
	body []byte,
) (storage.CustomProvider, bool) {
	candidates := s.customProvidersForModel(ctx, model)
	// An explicit path route is positive operator evidence that the provider
	// implements this proprietary extension. Keep wildcard passthrough ahead of
	// an inferred legacy Responses endpoint, without changing order within a tier.
	sort.SliceStable(candidates, func(i, j int) bool {
		return customProviderSearchRoutePriority(candidates[i]) > customProviderSearchRoutePriority(candidates[j])
	})
	for _, candidate := range candidates {
		provider, supported := customProviderSearchRoute(candidate)
		if !supported {
			continue
		}
		targetModel, mapped := customProviderMappedModel(provider, model)
		if !mapped {
			targetModel = model
		}
		lease, err := s.scheduler.Select(ctx, scheduler.Route{
			Group:              group,
			Provider:           provider.ID,
			PreferredEgressIDs: provider.EgressIDs,
			Model:              targetModel,
			EstimatedTokens:    virtual.EstimateTokensJSON(body),
			SkipWait:           true,
		})
		if err != nil {
			continue
		}
		lease.Release()
		return provider, true
	}
	return storage.CustomProvider{}, false
}

func customProviderSearchRoutePriority(provider storage.CustomProvider) int {
	wildcard := false
	for _, route := range provider.Routes {
		downstreamPath, ok := storage.NormalizeCustomProviderDownstreamPath(route.DownstreamPath)
		if !ok {
			continue
		}
		if downstreamPath == standaloneSearchPath {
			return 2
		}
		if downstreamPath == storage.CustomProviderDownstreamWildcard {
			wildcard = true
		}
	}
	if wildcard {
		return 1
	}
	return 0
}

func (s *Server) writeSearchCapabilityUnavailable(w http.ResponseWriter, actual, model string) {
	s.writeCapabilityUnavailable(
		w,
		http.StatusBadRequest,
		"the selected provider does not expose standalone web search",
		[]string{"standalone_web_search", "model:" + strings.TrimSpace(model)},
		"codex_or_responses_custom_provider",
		actual,
		"Select Codex or a custom provider whose effective search route uses the Responses protocol.",
	)
}

func requestWithPassthroughBody(r *http.Request, raw []byte) (*http.Request, func(), error) {
	source := bodysource.Bytes(raw)
	body, err := source.Open()
	if err != nil {
		_ = source.Close()
		return nil, func() {}, err
	}
	selected := r.Clone(contextWithBodySource(r.Context(), source))
	selected.Body = body
	selected.GetBody = source.Open
	selected.ContentLength = int64(len(raw))
	closeRequest := func() {
		_ = body.Close()
		_ = source.Close()
	}
	return selected, closeRequest, nil
}
