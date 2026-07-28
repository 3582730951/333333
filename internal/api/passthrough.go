package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

// handleAnthropicPassthrough is a transparent authenticated proxy for the extra
// Anthropic endpoints that Claude Code skills / code-execution depend on but that
// the message relay never served — so they 404'd and "无法访问官方 skills":
//
//   - the Files API (/v1/files…, multipart uploads + download)
//   - /v1/skills…             (skills-2025-10-02)
//   - /v1/agents…             (managed-agents-2026-04-01)
//   - /v1/environments… , /v1/sessions…  (code-execution containers)
//
// Unlike /v1/messages these are NOT message turns: the body is opaque (a multipart
// upload, a JSON skill/agent definition, or empty for a GET/DELETE) and must be
// forwarded byte-for-byte — no cloak/virtualization, no model override, no Anthropic-
// Beta superset injection. The client's own Content-Type / Accept / Anthropic-Beta /
// Anthropic-Version ride through verbatim (PassThrough on the upstream request); we
// only select a Claude account, attach its auth + the Claude Code identity fingerprint,
// and route through the account's egress (incl. sidecar JA3 / WARP chain). The upstream
// response is forwarded with header leak-scrub but WITHOUT body scrubbing — the bytes
// are file content / resource definitions, not a model turn that could leak the virtual
// identity.
//
// Account stickiness: passthrough resources are account-scoped (a file_id only resolves
// on the account that uploaded it). ExtractAffinityKey folds in the downstream identity,
// so every passthrough call from one downstream api key pins to one account — keeping a
// file's whole lifecycle (upload → reference → delete) on a single upstream account.
func (s *Server) handleAnthropicPassthrough(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch:
	default:
		methodNotAllowed(w)
		return
	}

	body := bodySourceFromContext(r.Context())
	var raw []byte
	if body == nil && r.Body != nil {
		b, err := readLimited(r.Body, s.cfg.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		raw = b
		body = bodysource.Bytes(raw)
		defer body.Close()
	}

	// Authenticate + resolve the routing group (honors RequireDownstreamKey). The
	// force-model/effort overrides are irrelevant here and never touch the opaque body.
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	hint := normalizeProviderHintLoose(r.Header.Get("X-Pool-Provider"))
	if strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		hint = normalizeProviderHintLoose(pol.ProviderHint)
	}
	if hint == "kiro" {
		s.writeCapabilityUnavailable(w, http.StatusBadRequest, "Kiro does not support this Anthropic passthrough endpoint", []string{"kiro_unsupported_endpoint:" + r.URL.Path}, "official_claude_passthrough", "kiro", "Use provider_hint=\"claude\" for Files, Skills, Agents and related endpoints.")
		return
	}

	affinity := routing.ExtractAffinityKey(r, raw)
	resourceAffinity, resourceKind, resourceID := claudeResourceAffinity(r.URL.Path)
	immutableResource := false
	if resourceAffinity.Hash != "" {
		if _, bindingErr := s.store.GetAffinityBinding(r.Context(), resourceAffinity.Hash); bindingErr == nil {
			affinity = resourceAffinity
			immutableResource = true
		} else if !storage.NotFound(bindingErr) {
			writeError(w, http.StatusInternalServerError, bindingErr)
			return
		}
	}
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:             pol.Group,
		Provider:          "claude",
		Affinity:          affinity,
		ImmutableAffinity: immutableResource,
		SkipWait:          userGroupFallbackProbe(r.Context()),
	})
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this resource is unavailable")
			return
		}
		s.writePublicNoAccountError(r.Context(), w, http.StatusServiceUnavailable, pol.Group, "claude", "", err)
		return
	}
	leaseReleased := false
	releaseLease := func() {
		if leaseReleased {
			return
		}
		leaseReleased = true
		lease.Release()
	}
	defer releaseLease()

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	token, err = s.prepareClaudeToken(r.Context(), lease.Account, token, "passthrough_preflight")
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	osHint := s.osHint(nil, lease.Egress)
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         r.Method,
			Provider:       "claude",
			PassThrough:    true,
			DownstreamPath: pathWithQuery(r.URL.Path, r.URL.RawQuery),
			Headers:        r.Header.Clone(),
			Body:           body,
			Account:        lease.Account,
			Token:          t,
			Egress:         lease.Egress,
			CookieJarKey:   lease.Binding.CookieJarKey,
			OSHint:         osHint,
		}
	}
	resp, err := s.upstream.Do(r.Context(), requestForToken(token))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errorBody := readUpstreamErrorBody(resp.Body)
		if claudeAuthError(resp.StatusCode, resp.Header, errorBody) && claudeTokenCanRefresh(token) {
			if refreshed, rerr := s.forceRefreshClaudeToken(r.Context(), lease.Account, "auth_error"); rerr == nil {
				token = refreshed
				resp.Body.Close()
				resp, err = s.upstream.Do(r.Context(), requestForToken(token))
				if err != nil {
					writeError(w, http.StatusBadGateway, err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode < 400 {
					goto passthroughSuccess
				}
				errorBody = readUpstreamErrorBody(resp.Body)
			}
		}
		if d := cf.Detect(resp.StatusCode, resp.Header, errorBody); cf.Recordable(d) {
			s.handleCFEvent(r.Context(), lease.Account, lease.Egress, resp.StatusCode, d)
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider:   "claude",
			Entrypoint: "claude_passthrough",
			Model:      "",
			Status:     resp.StatusCode,
			Header:     resp.Header,
			Body:       errorBody,
			Streaming:  isEventStream(resp.Header),
		})
		if ruleMatched {
			s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody, decision)
		} else {
			s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
		}
		if ruleMatched {
			switch decision.Match.DownstreamAction {
			case upstreamrules.DownstreamActionIdleStream:
				if isEventStream(resp.Header) {
					resp.Body.Close()
					releaseLease()
				}
				if s.writeRuleDownstream(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, nil, decision, isEventStream(resp.Header)) {
					return
				}
			case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize:
				if s.writeRuleDownstream(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, nil, decision, isEventStream(resp.Header)) {
					return
				}
			}
		}
		// No body scrubber: the passthrough body carries no virtual identity, and
		// rewriting it (e.g. an upload-validation error echoing field bytes) is wrong.
		// writeFilteredError still neutralizes pool-internal limit/quota leak bodies.
		s.writeFilteredError(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, nil)
		return
	}
passthroughSuccess:

	s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, "claude", "", resp.Header)
	if resourceAffinity.Hash != "" {
		s.persistClaudeResourceBinding(r.Context(), resourceAffinity, resourceKind, lease)
	}

	s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
	w.Header().Set("X-Pool-Resolved-Provider", "claude")
	if isEventStream(resp.Header) {
		w.WriteHeader(resp.StatusCode)
		_ = streamCopy(w, resp.Body)
		return
	}
	if resourceID == "" && (r.Method == http.MethodPost || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json")) {
		responseBody, readErr := s.readUpstreamResponseBody(resp.Body)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, readErr)
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(responseBody, &created) == nil && strings.TrimSpace(created.ID) != "" {
			createdAffinity := routing.AffinityFromKey("claude_resource:"+resourceKind+":"+strings.TrimSpace(created.ID), "claude_resource")
			s.persistClaudeResourceBinding(r.Context(), createdAffinity, resourceKind, lease)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func claudeResourceAffinity(path string) (routing.AffinityKey, string, string) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
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
	return routing.AffinityFromKey("claude_resource:"+kind+":"+id, "claude_resource"), kind, id
}

func (s *Server) persistClaudeResourceBinding(ctx context.Context, affinity routing.AffinityKey, kind string, lease scheduler.Lease) {
	if affinity.Hash == "" {
		return
	}
	_ = s.scheduler.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source,
		AccountID: lease.Account.ID, Provider: "claude", Model: "resource:" + kind, EgressID: lease.Egress.ID,
	})
}
