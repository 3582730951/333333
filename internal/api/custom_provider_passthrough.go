package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
)

// handleCustomProviderPassthrough relays an opaque auxiliary endpoint through
// an explicitly selected custom provider. These endpoints have no model field,
// so automatic model-based custom-provider discovery is deliberately excluded:
// only X-Pool-Provider, a downstream key policy, or an explicit user-group relay
// target may reach this handler.
func (s *Server) handleCustomProviderPassthrough(
	w http.ResponseWriter,
	r *http.Request,
	providerID string,
	policy downstreamPolicy,
) {
	s.handleCustomProviderPassthroughWithModel(w, r, providerID, policy, "", nil)
}

// handleCustomProviderPassthroughWithModel is the model-aware form used by
// standalone search. The resolved model participates in both account selection
// and diagnostics while every non-model auxiliary endpoint keeps the old path.
func (s *Server) handleCustomProviderPassthroughWithModel(
	w http.ResponseWriter,
	r *http.Request,
	providerID string,
	policy downstreamPolicy,
	routeModel string,
	raw []byte,
) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodHead:
	default:
		methodNotAllowed(w)
		return
	}

	providerID = strings.TrimSpace(providerID)
	provider, ok := s.customProviderByID(r.Context(), providerID)
	if !ok {
		s.writeCapabilityUnavailable(
			w,
			http.StatusServiceUnavailable,
			"selected custom provider is disabled or unavailable",
			[]string{"provider:" + providerID, "shared_endpoint:" + r.URL.Path},
			"enabled_custom_provider",
			"custom:"+providerID,
			"Enable the selected provider and import an active API-key account.",
		)
		return
	}
	provider, _ = storage.ResolveCustomProviderRoute(provider, r.URL.Path)
	if isStandaloneSearchPath(r.URL.Path) &&
		provider.UpstreamProtocol != storage.CustomProviderProtocolResponses {
		s.writeSearchCapabilityUnavailable(w, "custom:"+provider.ID, routeModel)
		return
	}

	body := bodySourceFromContext(r.Context())
	if body == nil && r.Body != nil {
		if raw == nil {
			value, err := readLimited(r.Body, s.cfg.MaxBodyBytes)
			if err != nil {
				writeError(w, http.StatusRequestEntityTooLarge, err)
				return
			}
			raw = value
		}
		body = bodysource.Bytes(raw)
		defer body.Close()
	}
	routeModel = strings.TrimSpace(routeModel)

	r = r.WithContext(withDownstreamKey(r.Context(), policy))

	affinity := customProviderScopedAffinity(r, provider, routing.ExtractAffinityKey(r, raw))
	resourceAffinity, resourceKind, resourceID := customProviderResourceAffinity(r, provider, r.URL.Path)
	immutableResource := false
	if resourceAffinity.Hash != "" {
		if _, err := s.store.GetAffinityBinding(r.Context(), resourceAffinity.Hash); err == nil {
			affinity = resourceAffinity
			immutableResource = true
		} else if !storage.NotFound(err) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:             policy.Group,
		Provider:          provider.ID,
		Model:             routeModel,
		EstimatedTokens:   virtual.EstimateTokensJSON(raw),
		Affinity:          affinity,
		ImmutableAffinity: immutableResource,
		SkipWait:          userGroupFallbackProbe(r.Context()),
	})
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this resource is unavailable")
			return
		}
		status, _ := noAccountHTTPStatus(err)
		s.writePublicNoAccountError(r.Context(), w, status, policy.Group, provider.ID, routeModel, err)
		return
	}
	defer lease.Release()

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response, err := s.upstream.Do(r.Context(), upstream.Request{
		Method:           r.Method,
		Provider:         provider.ID,
		BaseURL:          strings.TrimSpace(provider.BaseURL),
		TransportProfile: provider.TransportProfile,
		UpstreamProtocol: provider.UpstreamProtocol,
		PassThrough:      true,
		DownstreamPath:   customProviderPassthroughPath(provider.BaseURL, r.URL),
		Headers:          r.Header.Clone(),
		Body:             body,
		Model:            routeModel,
		Account:          lease.Account,
		Token:            token,
		Egress:           lease.Egress,
		CookieJarKey:     customProviderCookieJarKey(r, lease, provider),
		OSHint:           s.osHint(nil, lease.Egress),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		errorBody := readUpstreamErrorBody(response.Body)
		s.onUpstreamError(r.Context(), lease.Account, response.StatusCode, response.Header, errorBody)
		errorProtocol := "codex"
		if provider.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages {
			errorProtocol = "claude"
		}
		s.writeFilteredError(r.Context(), w, errorProtocol, response.StatusCode, response.Header, errorBody, nil)
		return
	}

	s.guardRateLimitForAccount(r.Context(), lease.Account, response.Header, lease.Trial)
	s.captureQuota(r.Context(), lease.Account.ID, provider.ID, routeModel, response.Header)
	if resourceAffinity.Hash != "" {
		s.persistCustomProviderResourceBinding(r.Context(), provider.ID, resourceAffinity, resourceKind, lease)
	}

	s.writeUpstreamHeaders(r.Context(), w.Header(), response.Header)
	w.Header().Set("X-Pool-Resolved-Provider", "custom:"+provider.ID)
	if resourceID == "" && r.Method == http.MethodPost &&
		strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		responseBody, readErr := s.readUpstreamResponseBody(response.Body)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, readErr)
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(responseBody, &created) == nil && strings.TrimSpace(created.ID) != "" {
			createdAffinity, _, _ := customProviderResourceAffinity(
				r, provider, "/v1/"+resourceKind+"/"+strings.TrimSpace(created.ID),
			)
			s.persistCustomProviderResourceBinding(r.Context(), provider.ID, createdAffinity, resourceKind, lease)
		}
		w.WriteHeader(response.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = w.Write(responseBody)
		}
		return
	}

	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if isEventStream(response.Header) {
		_ = streamCopy(w, response.Body)
		return
	}
	_, _ = io.Copy(w, response.Body)
}

// customProviderPassthroughPath removes the leading /v1 only when the provider
// base URL already ends in /v1. This preserves arbitrary relay prefixes while
// preventing the common ".../v1/v1/files" failure. RawQuery is carried through
// untouched so beta flags, pagination and resource filters retain their meaning.
func customProviderPassthroughPath(baseURL string, requestURL *url.URL) string {
	if requestURL == nil {
		return "/"
	}
	requestPath := requestURL.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}
	if parsed, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		basePath := strings.TrimRight(strings.ToLower(parsed.EscapedPath()), "/")
		lowerRequestPath := strings.ToLower(requestPath)
		if strings.HasSuffix(basePath, "/v1") &&
			(lowerRequestPath == "/v1" || strings.HasPrefix(lowerRequestPath, "/v1/")) {
			requestPath = requestPath[len("/v1"):]
			if requestPath == "" {
				requestPath = "/"
			}
		}
	}
	return pathWithQuery(requestPath, requestURL.RawQuery)
}

func customProviderResourceAffinity(r *http.Request, provider storage.CustomProvider, path string) (routing.AffinityKey, string, string) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "v1") {
		return routing.AffinityKey{}, "", ""
	}
	kind := strings.ToLower(strings.TrimSpace(parts[1]))
	switch kind {
	case "files", "skills", "agents", "environments", "sessions":
	default:
		return routing.AffinityKey{}, "", ""
	}
	if len(parts) < 3 {
		return routing.AffinityKey{}, kind, ""
	}
	id := strings.TrimSpace(parts[2])
	if id == "" {
		return routing.AffinityKey{}, kind, ""
	}
	return routing.AffinityFromKey(
		"custom_resource:"+strings.TrimSpace(provider.ID)+":"+customProviderRouteScope(provider)+":"+
			customProviderDownstreamScope(r)+":"+kind+":"+id,
		"custom_resource",
	), kind, id
}

func (s *Server) persistCustomProviderResourceBinding(
	ctx context.Context,
	providerID string,
	affinity routing.AffinityKey,
	kind string,
	lease scheduler.Lease,
) {
	if affinity.Hash == "" {
		return
	}
	_ = s.scheduler.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: affinity.Hash,
		RouteKey:     affinity.Key,
		Source:       affinity.Source,
		AccountID:    lease.Account.ID,
		Provider:     providerID,
		Model:        "resource:" + kind,
		EgressID:     lease.Egress.ID,
	})
}
