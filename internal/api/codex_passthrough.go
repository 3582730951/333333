package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

// handleCodexPassthrough proxies the auxiliary first-party Codex surfaces used
// by the stock client when a provider identifies itself as OpenAI. Responses
// turns and model discovery have dedicated handlers; these requests are opaque:
//
//   - /v1/alpha/search
//   - /v1/images/generations and /v1/images/edits
//   - /v1/realtime/calls
//   - Codex-owned /v1/files and /v1/skills resources
//
// Keeping these routes behind the same pool credential and account scheduler is
// what lets a URL + downstream API key retain the hosted-tool surface exposed by
// an ordinary Codex account login. Multipart bodies are replayable BodySources;
// no JSON normalization or identity text rewriting is performed.
func (s *Server) handleCodexPassthrough(w http.ResponseWriter, r *http.Request) {
	policy, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	r = s.withIntelligentRoutingFallbacks(r, policy)
	s.handleCodexPassthroughWithPolicy(w, r, policy, "", nil)
}

// handleCodexPassthroughWithPolicy preserves a provider/group decision already
// made by a model-aware dispatcher. Auxiliary endpoints normally have no model,
// while standalone search supplies one for exact account capability selection.
func (s *Server) handleCodexPassthroughWithPolicy(
	w http.ResponseWriter,
	r *http.Request,
	policy downstreamPolicy,
	routeModel string,
	raw []byte,
) {
	r = r.WithContext(withPinnedEgressPolicy(r.Context(), policy.PinnedEgressNoFallback))
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch:
	default:
		methodNotAllowed(w)
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
	hint := normalizeProviderHintLoose(r.Header.Get("X-Pool-Provider"))
	if strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		hint = normalizeProviderHintLoose(policy.ProviderHint)
	}
	if hint != "" && hint != "auto" && hint != "codex" {
		s.writeCapabilityUnavailable(
			w,
			http.StatusBadRequest,
			"the selected provider does not expose this Codex endpoint",
			[]string{"codex_passthrough:" + r.URL.Path},
			"official_codex_passthrough",
			hint,
			"Route the request with provider_hint=\"codex\".",
		)
		return
	}

	affinity := routing.ExtractAffinityKey(r, raw)
	resourceAffinity, resourceKind, resourceID := codexResourceAffinity(r.URL.Path)
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
		NoEgressFallback:  policy.PinnedEgressNoFallback,
		Provider:          "codex",
		Model:             routeModel,
		Affinity:          affinity,
		ImmutableAffinity: immutableResource || policy.PinnedEgressNoFallback,
		SkipWait:          userGroupFallbackProbe(r.Context()),
	})
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this Codex resource is unavailable")
			return
		}
		s.writePublicNoAccountError(r.Context(), w, http.StatusServiceUnavailable, policy.Group, "codex", routeModel, err)
		return
	}
	defer lease.Release()

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	requestForToken := func(current storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         r.Method,
			Provider:       "codex",
			DownstreamPath: pathWithQuery(r.URL.Path, r.URL.RawQuery),
			Headers:        r.Header.Clone(),
			Body:           body,
			Model:          routeModel,
			Account:        lease.Account,
			Token:          current,
			Egress:         lease.Egress,
			CookieJarKey:   lease.Binding.CookieJarKey,
		}
	}
	response, err := s.upstream.Do(r.Context(), requestForToken(token))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		errorBody := readUpstreamErrorBody(response.Body)
		detection := cf.Detect(response.StatusCode, response.Header, errorBody)
		verdict := ban.Classify(false, response.StatusCode, response.Header, errorBody)
		if verdict.State == ban.AuthExpired && !cf.EdgeOnly(detection) &&
			!upstream.AccountUsesAPIKey(token) && !isAgentIdentityToken(token) {
			if refreshed, refreshErr := s.refreshCodexToken(r.Context(), token); refreshErr == nil && refreshed.Refreshed {
				token = refreshed.Token
				response.Body.Close()
				response, err = s.upstream.Do(r.Context(), requestForToken(token))
				if err != nil {
					writeError(w, http.StatusBadGateway, err)
					return
				}
				defer response.Body.Close()
				if response.StatusCode < http.StatusBadRequest {
					goto passthroughSuccess
				}
				errorBody = readUpstreamErrorBody(response.Body)
			} else if refreshErr != nil {
				s.handleCodexRefreshFailure(r.Context(), lease.Account, refreshed, refreshErr, "codex_passthrough")
			}
		}
		s.onUpstreamError(r.Context(), lease.Account, response.StatusCode, response.Header, errorBody)
		s.writeFilteredError(r.Context(), w, "codex", response.StatusCode, response.Header, errorBody, nil)
		return
	}

passthroughSuccess:
	s.guardRateLimitForAccount(r.Context(), lease.Account, response.Header, lease.Trial)
	s.captureQuota(r.Context(), lease.Account.ID, "codex", routeModel, response.Header)
	if resourceAffinity.Hash != "" {
		s.persistCodexResourceBinding(r.Context(), resourceAffinity, resourceKind, lease)
	}

	s.writeUpstreamHeaders(r.Context(), w.Header(), response.Header)
	w.Header().Set("X-Pool-Resolved-Provider", "codex")
	if resourceID == "" && isCodexResourceCollection(r.URL.Path) &&
		(r.Method == http.MethodPost || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json")) {
		responseBody, readErr := s.readUpstreamResponseBody(response.Body)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, readErr)
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(responseBody, &created) == nil && strings.TrimSpace(created.ID) != "" {
			createdAffinity := routing.AffinityFromKey(
				"codex_resource:"+resourceKind+":"+strings.TrimSpace(created.ID),
				"codex_resource",
			)
			s.persistCodexResourceBinding(r.Context(), createdAffinity, resourceKind, lease)
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func isCodexResourceCollection(path string) bool {
	path = strings.TrimSuffix(strings.TrimSpace(path), "/")
	return path == "/v1/files" || path == "/v1/skills"
}

func codexResourceAffinity(path string) (routing.AffinityKey, string, string) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		return routing.AffinityKey{}, "", ""
	}
	kind := strings.ToLower(strings.TrimSpace(parts[1]))
	switch kind {
	case "files", "skills":
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
	return routing.AffinityFromKey("codex_resource:"+kind+":"+id, "codex_resource"), kind, id
}

func (s *Server) persistCodexResourceBinding(
	ctx context.Context,
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
		Provider:     "codex",
		Model:        "resource:" + kind,
		EgressID:     lease.Egress.ID,
	})
}
