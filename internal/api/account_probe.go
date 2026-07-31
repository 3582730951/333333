// account_probe.go holds model-capability probing (codex/claude/custom), the
// background probe loop, and the token-refresh handlers (codex + claude). Extracted
// verbatim from server.go (no behavior change). Imports via goimports.
package api

import (
	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) probeAccountModels(ctx context.Context, account storage.Account) ([]storage.ModelCapability, error) {
	token, err := s.store.GetToken(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil {
		return nil, err
	}
	return s.probeAccountModelsWithDeps(ctx, account, token, binding, egress)
}

func (s *Server) probeAccountModelsWithDeps(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile) ([]storage.ModelCapability, error) {
	// Model discovery is free, but it still reaches the provider and must use the
	// same account-bound sidecar transport as inference. Never let a background
	// probe leak the Go stdlib fingerprint through the base proxy.
	var err error
	egress, err = s.store.ApplySidecarEgressBinding(ctx, binding, egress)
	if err != nil {
		return nil, err
	}
	switch provider := s.accountProvider(account, token); provider {
	case "claude":
		return s.probeClaudeModels(ctx, account, token, binding, egress)
	case "antigravity":
		creds, credentialsErr := s.store.GetAntigravityCredentials(ctx, account.ID)
		if credentialsErr != nil {
			return nil, credentialsErr
		}
		accessToken, refreshedCreds, refreshErr := s.ensureAntigravityToken(ctx, creds, account, egress, binding.CookieJarKey)
		if refreshErr != nil {
			return s.existingAntigravityModels(ctx, account.ID, refreshErr)
		}
		models, probeErr := s.upstream.FetchAntigravityModels(ctx, egress, binding.CookieJarKey, accessToken, refreshedCreds.ProjectID, refreshedCreds.BaseURL, refreshedCreds.UserAgent)
		if probeErr != nil {
			return s.existingAntigravityModels(ctx, account.ID, probeErr)
		}
		caps := make([]storage.ModelCapability, 0, len(models))
		for _, discovered := range models {
			contextState := capability.Context1MUnknown
			contextSource := ""
			if discovered.MaxTokens > 0 {
				contextState = capability.Context1MUnsupported
				if discovered.MaxTokens >= 1_000_000 {
					contextState = capability.Context1MSupported
				}
				contextSource = "antigravity_catalog_max_tokens"
			}
			rawHash := sha256.Sum256(discovered.RawJSON)
			caps = append(caps, storage.ModelCapability{
				AccountID: account.ID, ModelSlug: discovered.ID,
				AvailabilityState: capability.AvailabilityVerified,
				Context1MState:    contextState, Context1MSource: contextSource,
				NativeContextWindow: discovered.MaxTokens, NativeMaxContextWindow: discovered.MaxTokens,
				EffectiveContextWindowPercent: 100,
				RawModelJSONHash:              fmt.Sprintf("%x", rawHash[:]), RawModelJSON: string(discovered.RawJSON),
				Source: "antigravity_model_probe", LastProbeAt: storage.Now(),
			})
		}
		// fetchAvailableModels is not a deletion-authoritative snapshot: current
		// Antigravity responses may contain only capability hints or a partial models
		// object. Merge verified observations so a partial response cannot erase a
		// previously working account/model route.
		if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
			return nil, err
		}
		return caps, nil
	case "kiro":
		cred, err := s.store.GetKiroCredentials(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		cfg := s.effectiveKiroConfig(ctx)
		region := firstNonEmpty(cred.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1")
		endpointHash, err := kirowire.EndpointHash(cred.Endpoint, region, cfg.KiroEndpointAllowlist)
		if err != nil {
			return nil, err
		}
		capabilityKey, _ := kirowire.KiroCapabilityKey(endpointHash, region, cred.ProfileARN)
		bearer, _, refreshedCred, prepareErr := s.kiro.Prepare(ctx, account, cred, token, egress, false)
		if prepareErr != nil {
			return s.existingOrStaticKiroModels(ctx, account.ID, capabilityKey, endpointHash, prepareErr)
		}
		capabilityKey, _ = kirowire.KiroCapabilityKey(endpointHash, region, refreshedCred.ProfileARN)
		catalog, catalogErr := s.kiro.RefreshModelCatalog(ctx, account, refreshedCred, bearer, egress)
		if catalogErr != nil {
			return s.existingOrStaticKiroModels(ctx, account.ID, capabilityKey, endpointHash, catalogErr)
		}
		caps := kiroCatalogCapabilities(account.ID, catalog)
		if len(caps) == 0 {
			return s.existingOrStaticKiroModels(ctx, account.ID, capabilityKey, endpointHash, errors.New("empty Kiro catalog"))
		}
		models := make([]string, 0, len(catalog)*2)
		for _, descriptor := range catalog {
			models = append(models, descriptor.PublicID, descriptor.UpstreamID)
		}
		if err := s.store.EnsureKiroRuntimeModels(ctx, account.ID, endpointHash, models); err != nil {
			return nil, err
		}
		if err := s.store.ReplaceCapabilities(ctx, account.ID, caps); err != nil {
			return nil, err
		}
		return caps, nil
	case "codex":
		// fall through to the Codex /models probe below
	default:
		// Custom OpenAI-compatible provider (DeepSeek, …).
		custom, found, loadErr := s.store.GetCustomProvider(ctx, provider)
		if loadErr != nil {
			return nil, loadErr
		}
		if !found {
			return nil, nil
		}
		// A disabled provider is a hard network boundary for every automatic
		// discovery path (periodic sweep and post-import async probe). Preserve
		// the last capability snapshot so disabling/re-enabling a provider does
		// not destroy operator state, but do not contact its relay.
		if !custom.Enabled {
			return s.store.ListCapabilities(ctx, account.ID)
		}
		// Manual provider catalogs are seeded synchronously at import/update
		// time. When discovery is disabled, the detached post-import probe must
		// be a strict no-op: probing that same static list would race operator or
		// runtime capability evidence and replace unrelated rows.
		if !custom.AutoDiscoverModels {
			return s.store.ListCapabilities(ctx, account.ID)
		}
		if len(custom.EgressIDs) > 0 && s.scheduler != nil {
			lease, selectErr := s.scheduler.Select(ctx, scheduler.Route{
				Group:              account.GroupName,
				Provider:           provider,
				PreferredEgressIDs: custom.EgressIDs,
				RequiredAccountID:  account.ID,
				SkipWait:           true,
			})
			if selectErr != nil {
				return nil, selectErr
			}
			defer lease.Release()
			binding, egress = lease.Binding, lease.Egress
		}
		return s.probeCustomModels(ctx, account, token, binding, egress, provider)
	}
	token, err = s.ensureAgentIdentityTask(ctx, account, token, egress, binding.CookieJarKey, "")
	if err != nil {
		return s.existingOrStaticCodexModels(ctx, account.ID)
	}
	agentTaskRecovered := false
	for attempt := 0; attempt < 2; attempt++ {
		probePath := capability.ProbePath(s.cfg.ClientVersion)
		clientVersion := s.cfg.ClientVersion
		if accountprovider.UsesAPIKey("codex", token) {
			probePath = "/v1/models"
			clientVersion = ""
		}
		resp, err := s.upstream.Do(ctx, upstream.Request{
			Method:         http.MethodGet,
			DownstreamPath: probePath,
			Headers:        http.Header{},
			Account:        account,
			Token:          token,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
			// Report the same current version on the UA/`version` header as in the
			// ?client_version= query, so the probe request is internally coherent. The
			// ChatGPT /models backend gates the catalog by client_version; a stale value
			// here is exactly why the newest models never came back.
			CodexClientVersion: clientVersion,
		})
		if err != nil {
			log.Printf("codex model probe %s: %v; using static model set", account.ID, err)
			return s.existingOrStaticCodexModels(ctx, account.ID)
		}
		raw, err := upstream.DrainAndClose(resp.Body)
		if err != nil {
			log.Printf("codex model probe %s: read body: %v; using static model set", account.ID, err)
			return s.existingOrStaticCodexModels(ctx, account.ID)
		}
		if resp.StatusCode >= 400 {
			raw = redactAgentIdentityError(token, raw)
			if !agentTaskRecovered && isInvalidAgentIdentityTask(resp.StatusCode, raw, token) {
				if recovered, recoverErr := s.ensureAgentIdentityTask(ctx, account, token, egress, binding.CookieJarKey, token.AgentTaskID); recoverErr == nil {
					token = recovered
					agentTaskRecovered = true
					continue
				} else {
					log.Printf("codex model probe %s: agent task recovery failed: %v", account.ID, recoverErr)
				}
			}
			v := ban.Classify(false, resp.StatusCode, resp.Header, raw)
			if attempt == 0 && v.State == ban.AuthExpired && !accountprovider.UsesAPIKey("codex", token) && !isAgentIdentityToken(token) {
				if refreshed, rerr := s.refreshCodexToken(ctx, token); rerr == nil && refreshed.Refreshed {
					token = refreshed.Token
					continue
				} else if rerr != nil {
					log.Printf("codex model probe %s: refresh failed: %v", account.ID, rerr)
					s.handleCodexRefreshFailure(ctx, account, refreshed, rerr, "model_probe")
				}
			}
			snippet := string(raw)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			log.Printf("codex model probe %s: upstream %d (%s); using static model set", account.ID, resp.StatusCode, snippet)
			return s.existingOrStaticCodexModels(ctx, account.ID)
		}
		caps, err := capability.Parse(account.ID, raw, capability.ETagFromHeader(resp.Header))
		if err != nil {
			log.Printf("codex model probe %s: parse: %v; using static model set", account.ID, err)
			return s.existingOrStaticCodexModels(ctx, account.ID)
		}
		if err := s.store.ReplaceCapabilities(ctx, account.ID, caps); err != nil {
			return nil, err
		}
		return caps, nil
	}
	return s.existingOrStaticCodexModels(ctx, account.ID)
}

func (s *Server) existingAntigravityModels(ctx context.Context, accountID string, probeErr error) ([]storage.ModelCapability, error) {
	existing, err := s.store.ListCapabilities(ctx, accountID)
	if err == nil && len(existing) > 0 {
		log.Printf("antigravity model probe %s: %v; retaining previous account catalog", accountID, probeErr)
		return existing, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, probeErr
}

func kiroCatalogCapabilities(accountID string, catalog []storage.KiroModelDescriptor) []storage.ModelCapability {
	now := storage.Now()
	out := make([]storage.ModelCapability, 0, len(catalog))
	for _, descriptor := range catalog {
		if !descriptor.Complete || strings.TrimSpace(descriptor.PublicID) == "" {
			continue
		}
		maximum := descriptor.MaxInputTokens
		if maximum <= 0 {
			maximum = capability.KiroContextWindow(descriptor.PublicID)
		}
		contextState := capability.Context1MUnsupported
		contextSource := "kiro_live_catalog_max_input"
		if maximum >= 1_000_000 {
			contextState = capability.Context1MSupported
		} else if descriptor.MaxInputTokens <= 0 {
			contextState = capability.Context1MUnknown
			contextSource = ""
		}
		cap := storage.ModelCapability{
			AccountID:                     accountID,
			ModelSlug:                     descriptor.PublicID,
			AvailabilityState:             capability.AvailabilityVerified,
			Context1MState:                contextState,
			Context1MSource:               contextSource,
			NativeContextWindow:           capability.KiroEffectiveContextWindow(descriptor.PublicID, "", descriptor.MaxInputTokens),
			NativeMaxContextWindow:        maximum,
			EffectiveContextWindowPercent: 100,
			RawModelJSONHash:              descriptor.RawJSONHash,
			Source:                        "kiro_live_catalog",
			LastProbeAt:                   now,
		}
		out = append(out, capability.ApplyGPT56ContextContract(cap))
	}
	return out
}

func (s *Server) existingOrStaticKiroModels(ctx context.Context, accountID, capabilityKey, endpointHash string, probeErr error) ([]storage.ModelCapability, error) {
	if catalog, err := s.store.ListKiroModelCatalog(ctx, accountID, capabilityKey); err == nil && len(catalog) > 0 {
		caps := kiroCatalogCapabilities(accountID, catalog)
		if len(caps) > 0 {
			log.Printf("Kiro model catalog probe failed class=%T; retaining last-good catalog", probeErr)
			if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
				return nil, err
			}
			return caps, nil
		}
	} else if err != nil {
		return nil, err
	}
	if existing, err := s.store.ListCapabilities(ctx, accountID); err == nil && len(existing) > 0 {
		log.Printf("Kiro model catalog probe failed class=%T; retaining existing capabilities", probeErr)
		return existing, nil
	} else if err != nil {
		return nil, err
	}
	caps := capability.StaticKiroModels(accountID)
	models := make([]string, 0, len(caps))
	for _, model := range caps {
		models = append(models, model.ModelSlug)
	}
	if err := s.store.EnsureKiroRuntimeModels(ctx, accountID, endpointHash, models); err != nil {
		return nil, err
	}
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// upsertStaticCodexModels stores unverified discovery hints when the live ChatGPT
// model list is unavailable. A probe failure never bans the account.
func (s *Server) upsertStaticCodexModels(ctx context.Context, accountID string) ([]storage.ModelCapability, error) {
	caps := capability.StaticCodexModels(accountID)
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

func (s *Server) existingOrStaticCodexModels(ctx context.Context, accountID string) ([]storage.ModelCapability, error) {
	if caps, err := s.store.ListCapabilities(ctx, accountID); err == nil {
		// Preserve both runtime verification and exact model_not_found evidence.
		// Replacing a non-empty catalog with bundled hints would erase an
		// account/model-scoped rejection and make the next request retry it.
		if len(caps) > 0 {
			return caps, nil
		}
		if authoritative, authorityErr := s.store.ModelCatalogAuthoritative(ctx, accountID); authorityErr == nil && authoritative {
			return caps, nil
		}
	}
	return s.upsertStaticCodexModels(ctx, accountID)
}

// probeClaudeModels populates a Claude account's model capabilities. It first
// asks Anthropic's GET /v1/models through the account (so the advertised set
// reflects what the account can actually use), and falls back to unverified static
// discovery hints when that endpoint is unavailable. Successful live results,
// including an empty catalog, are authoritative. A probe failure never bans the
// account: capability discovery is advisory.
func (s *Server) probeClaudeModels(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile) ([]storage.ModelCapability, error) {
	var err error
	token, err = s.prepareClaudeToken(ctx, account, token, "model_probe_preflight")
	if err != nil {
		log.Printf("claude model probe %s: refresh wait: %v; using static model set", account.ID, err)
		return s.existingOrStaticClaudeModels(ctx, account, token)
	}
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         http.MethodGet,
			Provider:       "claude",
			DownstreamPath: "/v1/models",
			Headers:        http.Header{},
			Account:        account,
			Token:          t,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
		}
	}
	resp, err := s.upstream.Do(ctx, requestForToken(token))
	if err == nil {
		raw, derr := upstream.DrainAndClose(resp.Body)
		switch {
		case derr != nil:
			log.Printf("claude model probe %s: read body: %v; using static model set", account.ID, derr)
		case resp.StatusCode >= 400:
			if claudeAuthError(resp.StatusCode, resp.Header, raw) && claudeTokenCanRefresh(token) {
				if refreshed, rerr := s.forceRefreshClaudeToken(ctx, account, "auth_error"); rerr == nil {
					token = refreshed
					if retryResp, retryErr := s.upstream.Do(ctx, requestForToken(token)); retryErr == nil {
						raw, derr = upstream.DrainAndClose(retryResp.Body)
						resp = retryResp
						if derr == nil && resp.StatusCode < 400 {
							if caps, perr := capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header)); perr == nil {
								caps = capability.ApplyClaudeAccountPolicy(caps, account, token)
								if uerr := s.store.ReplaceCapabilities(ctx, account.ID, caps); uerr != nil {
									return nil, uerr
								}
								return caps, nil
							}
						}
					}
				}
			}
			snippet := string(raw)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			log.Printf("claude model probe %s: upstream %d (%s); using static model set", account.ID, resp.StatusCode, snippet)
		default:
			if caps, perr := capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header)); perr == nil {
				caps = capability.ApplyClaudeAccountPolicy(caps, account, token)
				if uerr := s.store.ReplaceCapabilities(ctx, account.ID, caps); uerr != nil {
					return nil, uerr
				}
				return caps, nil
			} else if perr != nil {
				log.Printf("claude model probe %s: parse: %v; using static model set", account.ID, perr)
			}
		}
	} else {
		log.Printf("claude model probe %s: %v; using static model set", account.ID, err)
	}
	return s.existingOrStaticClaudeModels(ctx, account, token)
}

func (s *Server) upsertStaticClaudeModels(ctx context.Context, account storage.Account, token storage.AccountToken) ([]storage.ModelCapability, error) {
	caps := capability.ApplyClaudeAccountPolicy(capability.StaticClaudeModels(account.ID), account, token)
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

func (s *Server) existingOrStaticClaudeModels(ctx context.Context, account storage.Account, token storage.AccountToken) ([]storage.ModelCapability, error) {
	if caps, err := s.store.ListCapabilities(ctx, account.ID); err == nil {
		if len(caps) > 0 {
			caps = capability.ApplyClaudeAccountPolicy(caps, account, token)
			if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
				return nil, err
			}
			return caps, nil
		}
		if authoritative, authorityErr := s.store.ModelCatalogAuthoritative(ctx, account.ID); authorityErr == nil && authoritative {
			return caps, nil
		}
	}
	return s.upsertStaticClaudeModels(ctx, account, token)
}

// accountProvider resolves an account's upstream provider, preferring the explicit
// provider column and falling back to the credential-shape heuristic for legacy rows
// imported before the provider column existed.
func (s *Server) accountProvider(account storage.Account, token storage.AccountToken) string {
	return accountprovider.EffectiveProvider(account.Provider, token, true)
}

// probeCustomModels discovers a custom OpenAI-compatible provider's models for an
// account. When auto-discovery is on it GETs {base}/models (OpenAI shape, parsed by
// capability.Parse) and unions the result with the provider's manual model list; the
// newly discovered ids are also persisted back into the provider record so routing and
// the admin model-list inputs reflect them. The manual list is the fallback when
// discovery is unavailable. Capabilities are tagged source "custom:<id>" so /v1/models
// advertises them natively (never the Codex virtual-2M window). Never bans the account.
func (s *Server) probeCustomModels(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile, providerID string) ([]storage.ModelCapability, error) {
	prov, ok, err := s.store.GetCustomProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if !prov.Enabled {
		return s.store.ListCapabilities(ctx, account.ID)
	}
	seen := map[string]bool{}
	var ordered []string
	add := func(slug string) {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			return
		}
		seen[slug] = true
		ordered = append(ordered, slug)
	}
	for _, m := range prov.Models {
		add(m)
	}
	mappingSources := make([]string, 0, len(prov.ModelMappings))
	for source := range prov.ModelMappings {
		mappingSources = append(mappingSources, source)
	}
	sort.Strings(mappingSources)
	for _, source := range mappingSources {
		add(prov.ModelMappings[source])
	}
	var discovered []string
	verified := map[string]bool{}
	markVerified := func(model string) {
		verified[strings.ToLower(strings.TrimSpace(model))] = true
	}
	isVerified := func(model string) bool {
		return verified[strings.ToLower(strings.TrimSpace(model))]
	}
	if prov.AutoDiscoverModels && strings.TrimSpace(prov.BaseURL) != "" {
		headers := http.Header{}
		if prov.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
		resp, derr := s.upstream.Do(ctx, upstream.Request{
			Method:         http.MethodGet,
			Provider:       providerID,
			BaseURL:        prov.BaseURL,
			DownstreamPath: "/models",
			Headers:        headers,
			Account:        account,
			Token:          token,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
		})
		if derr != nil {
			log.Printf("custom model probe %s (%s): %v; using manual model list", account.ID, providerID, derr)
		} else {
			raw, rerr := upstream.DrainAndClose(resp.Body)
			switch {
			case rerr != nil:
				log.Printf("custom model probe %s (%s): read body: %v", account.ID, providerID, rerr)
			case resp.StatusCode >= 400:
				log.Printf("custom model probe %s (%s): upstream %d (%s)", account.ID, providerID, resp.StatusCode, bodySnippet(raw, 160))
			default:
				var caps []storage.ModelCapability
				var perr error
				if prov.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages {
					caps, perr = capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header))
				} else {
					caps, perr = capability.Parse(account.ID, raw, capability.ETagFromHeader(resp.Header))
				}
				if perr == nil {
					for _, c := range caps {
						discovered = append(discovered, c.ModelSlug)
						add(c.ModelSlug)
						markVerified(c.ModelSlug)
					}
				} else {
					log.Printf("custom model probe %s (%s, protocol=%s): parse: %v", account.ID, providerID, prov.UpstreamProtocol, perr)
				}
			}
		}
		// Many Anthropic-compatible relay stations implement /messages but not
		// /models. Fall back to the maintained Claude candidate table and verify
		// each candidate with the smallest valid non-streaming request. Stop on
		// auth, throttling, transport, or server failure so discovery never turns a
		// transient outage into a burst.
		if prov.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages && len(discovered) == 0 {
			candidates := append([]string(nil), ordered...)
			candidates = append(candidates, capability.ClaudeProbeModelTable()...)
			candidateSeen := map[string]bool{}
			for _, candidate := range candidates {
				candidate = strings.TrimSpace(candidate)
				key := strings.ToLower(candidate)
				if candidate == "" || candidateSeen[key] {
					continue
				}
				candidateSeen[key] = true
				available, stop := s.probeCustomAnthropicModelCandidate(ctx, account, token, binding, egress, prov, candidate)
				if available {
					discovered = append(discovered, candidate)
					add(candidate)
					markVerified(candidate)
				}
				if stop {
					break
				}
			}
		}
	}
	// Mapping sources are downstream-visible model aliases. Probe only each concrete
	// target above, then mirror its evidence onto the source so /v1/models advertises
	// what clients should request rather than forcing them to know relay internals.
	for _, source := range mappingSources {
		target := prov.ModelMappings[source]
		if strings.TrimSpace(source) == "*" {
			continue
		}
		add(source)
		if isVerified(target) {
			markVerified(source)
		}
	}
	// Persist newly discovered models into the provider's list so routing + the admin UI
	// reflect them (auto-discovery feeds the model list the operator can then prune).
	if len(discovered) > 0 {
		have := map[string]bool{}
		for _, m := range prov.Models {
			have[strings.TrimSpace(m)] = true
		}
		changed := false
		for _, d := range discovered {
			d = strings.TrimSpace(d)
			if d != "" && !have[d] {
				have[d] = true
				prov.Models = append(prov.Models, d)
				changed = true
			}
		}
		if changed {
			_ = s.store.UpsertCustomProvider(ctx, prov)
		}
	}
	now := storage.Now()
	caps := make([]storage.ModelCapability, 0, len(ordered))
	for _, slug := range ordered {
		availability := capability.AvailabilityUnverified
		source := "custom_manual:" + providerID
		if isVerified(slug) {
			availability = capability.AvailabilityVerified
			source = "custom_probe:" + providerID
		}
		caps = append(caps, storage.ModelCapability{
			AccountID:                     account.ID,
			ModelSlug:                     slug,
			AvailabilityState:             availability,
			EffectiveContextWindowPercent: 100,
			Visibility:                    "list",
			Source:                        source,
			LastProbeAt:                   now,
		})
	}
	if len(caps) == 0 {
		return nil, nil
	}
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// probeCustomAnthropicModelCandidate performs one explicit model reachability
// check. The bool pair is (available, stopDiscovery). Unsupported-model responses
// continue to the next table entry; infrastructure/auth/rate failures stop.
func (s *Server) probeCustomAnthropicModelCandidate(
	ctx context.Context,
	account storage.Account,
	token storage.AccountToken,
	binding storage.AccountEgressBinding,
	egress storage.EgressProfile,
	provider storage.CustomProvider,
	model string,
) (bool, bool) {
	body, _ := json.Marshal(map[string]interface{}{
		"model": model, "max_tokens": 1, "stream": false,
		"messages": []map[string]interface{}{{"role": "user", "content": "Reply OK"}},
	})
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	req := upstream.Request{
		Method: http.MethodPost, Provider: provider.ID, BaseURL: provider.BaseURL,
		TransportProfile: provider.TransportProfile, DownstreamPath: "/messages",
		Headers: headers, Account: account, Token: token, Egress: egress,
		CookieJarKey: binding.CookieJarKey, MinimalProbe: true,
	}
	req.SetBodyBytes(body)
	resp, err := s.upstream.Do(ctx, req)
	if err != nil {
		log.Printf("custom Claude model probe %s (%s/%s): %v", account.ID, provider.ID, model, err)
		return false, true
	}
	raw, readErr := upstream.DrainAndClose(resp.Body)
	if readErr != nil {
		return false, true
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if validateCustomUpstreamJSONResponse(storage.CustomProviderProtocolAnthropicMessages, raw) == nil {
			return true, false
		}
		return false, true
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= http.StatusInternalServerError:
		return false, true
	default:
		return false, false
	}
}

func (s *Server) adminProbeModels(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	caps, err := s.probeAccountModels(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "capabilities": caps})
}

// StartBackground launches the periodic model-capability probe sweep. It returns
// immediately; the sweep runs until ctx is cancelled (on server shutdown). When
// ModelProbeIntervalHours is 0 the background refresh is disabled (imports and the
// manual admin probe still work).
func (s *Server) StartBackground(ctx context.Context) {
	if s.regHandler != nil {
		s.regHandler.StartRuntime(ctx)
	}
	if s.teamLifecycle != nil {
		supervisor.Go(ctx, "team-lifecycle", s.teamLifecycle.Run)
	}
	// Cooldown→health-recheck loop runs independently of the model-probe sweep (it
	// must work even when model probing is disabled).
	s.startRecheckLoop(ctx)
	s.startBillingHoldExpiryLoop(ctx)
	s.startLogRetentionLoop(ctx)
	s.startDiskGuard(ctx)
	s.startDiagnosticJobLoop(ctx)
	interval := time.Duration(s.cfg.ModelProbeIntervalHours) * time.Hour
	if interval <= 0 {
		return
	}
	supervisor.Go(ctx, "model-probe", func(ctx context.Context) {
		s.probeAllAccounts(ctx)
		s.probeLoop(ctx, interval)
	})
}

// StopRegistrationJobs cancels registrar subprocesses and waits within the
// caller's shutdown deadline. It is separate from async telemetry flushing so
// account/provider transactions settle before the database is closed.
func (s *Server) StopRegistrationJobs(ctx context.Context) error {
	if s == nil || s.regHandler == nil {
		return nil
	}
	return s.regHandler.StopRuntime(ctx)
}

func (s *Server) probeLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeAllAccounts(ctx)
		}
	}
}

// probeAllAccounts re-probes every active account's upstream model list, spreading
// the probes out (small stagger) to avoid a thundering herd against the upstream
// and the shared egress. Every blocking point selects on ctx.Done() so the sweep
// never blocks shutdown.
func (s *Server) probeAllAccounts(ctx context.Context) {
	if s.diskGuardPausesBackground() {
		return
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		log.Printf("model probe sweep: list accounts: %v", err)
		return
	}
	active := activeModelProbeAccounts(accounts)
	if len(active) == 0 {
		log.Printf("model probe sweep: probed=0 failed=0")
		return
	}
	accountIDs := accountIDsFromAccounts(active)
	tokens, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("model probe sweep: list tokens: %v", err)
		return
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("model probe sweep: list egress bindings: %v", err)
		return
	}
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		log.Printf("model probe sweep: list egress profiles: %v", err)
		return
	}
	profilesByID := modelProbeEgressProfilesByID(profiles)
	const stagger = 3 * time.Second
	probed, failed := 0, 0
	for _, account := range active {
		if ctx.Err() != nil {
			return
		}
		token, ok := tokens[account.ID]
		if !ok {
			failed++
			continue
		}
		binding, ok := bindings[account.ID]
		if !ok {
			failed++
			continue
		}
		egress, ok := profilesByID[binding.PrimaryEgressID]
		if !ok {
			failed++
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout())
		_, perr := s.probeAccountModelsWithDeps(cctx, account, token, binding, egress)
		cancel()
		if perr != nil {
			failed++
		} else {
			probed++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stagger):
		}
	}
	log.Printf("model probe sweep: probed=%d failed=%d", probed, failed)
}

func activeModelProbeAccounts(accounts []storage.Account) []storage.Account {
	active := make([]storage.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == "active" {
			active = append(active, account)
		}
	}
	return active
}

func accountIDsFromAccounts(accounts []storage.Account) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

func modelProbeEgressProfilesByID(profiles []storage.EgressProfile) map[string]storage.EgressProfile {
	out := make(map[string]storage.EgressProfile, len(profiles))
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) == "" {
			continue
		}
		out[profile.ID] = profile
	}
	return out
}

func (s *Server) adminRefresh(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	token, err := s.store.GetToken(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	provider := s.accountProvider(account, token)
	if accountprovider.UsesAPIKey(provider, token) {
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": false, "reason": "api_key credentials are static"})
		return
	}
	switch {
	case provider == "claude":
		s.refreshClaude(w, r, token)
		return
	case provider == "antigravity":
		cred, err := s.store.GetAntigravityCredentials(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		if strings.TrimSpace(cred.RefreshToken) == "" {
			writeError(w, http.StatusBadGateway, errors.New("antigravity refresh token is missing"))
			return
		}
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		egress, err := s.store.ResolvePrimaryEgressBinding(r.Context(), binding)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		refreshed, err := s.upstream.RefreshAntigravityToken(r.Context(), egress, binding.CookieJarKey, cred.RefreshToken, &s.cfg)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		if strings.TrimSpace(refreshed.AccessToken) == "" {
			writeError(w, http.StatusBadGateway, errors.New("antigravity token refresh returned no access_token"))
			return
		}
		cred.AccessToken = refreshed.AccessToken
		if strings.TrimSpace(refreshed.RefreshToken) != "" {
			cred.RefreshToken = refreshed.RefreshToken
		}
		cred.ExpiresAt = time.Now().Unix() + refreshed.ExpiresIn
		if err := s.store.UpsertAntigravityCredentials(r.Context(), cred); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": true, "method": "antigravity_oauth"})
		return
	case provider == "kiro":
		cred, err := s.store.GetKiroCredentials(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		egress, err := s.store.ResolvePrimaryEgressBinding(r.Context(), binding)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.kiro.UpdateConfig(s.effectiveKiroConfig(r.Context()))
		if _, _, _, err = s.kiro.Prepare(r.Context(), account, cred, token, egress, true); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": true, "method": "kiro"})
		return
	case upstream.IsCustomProvider(provider):
		// Custom OpenAI-compatible providers authenticate with a static API key — there
		// is nothing to refresh.
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": false, "reason": "custom provider uses a static API key"})
		return
	}
	refreshed, err := s.refreshCodexToken(r.Context(), token)
	if err != nil {
		s.handleCodexRefreshFailure(r.Context(), account, refreshed, err, "admin_refresh")
		if refreshed.StatusCode >= 400 {
			writeRaw(w, refreshed.StatusCode, refreshed.Header, refreshed.Body)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !refreshed.Refreshed {
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": false, "reason": refreshed.Reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": true, "method": refreshed.Method})
}

type codexRefreshResult struct {
	Token               storage.AccountToken
	Refreshed           bool
	Method              string
	Reason              string
	TerminalAuthFailure bool
	StatusCode          int
	Header              http.Header
	Body                []byte
}

func (s *Server) refreshCodexToken(ctx context.Context, token storage.AccountToken) (codexRefreshResult, error) {
	result := codexRefreshResult{Token: token}
	if accountprovider.IsAgentIdentity(token) {
		result.Reason = "agent identity uses signed task assertions, not OAuth token refresh"
		return result, nil
	}
	tokenURL := firstNonEmpty(s.cfg.OAuthTokenURL, s.cfg.CodexOAuthTokenURL)
	if tokenURL == "" || token.RefreshToken == "" {
		// Cookie-imported "AT" accounts (no refresh_token) refresh by re-minting
		// the access token from the stored chatgpt.com session cookie.
		if cookie, _ := s.store.GetSessionCookie(ctx, token.AccountID); cookie != "" {
			if reminted, ferr := s.remintCodexTokenFromSessionCookie(ctx, token, cookie, false); ferr == nil {
				result.Token = reminted
				result.Refreshed = true
				result.Method = "session_cookie"
				return result, nil
			} else {
				result.Reason = "session cookie re-mint failed"
				s.enqueueCodexReauthIfEligible(ctx, token.AccountID, result.Reason)
				return result, ferr
			}
		}
		token.LastRefresh = storage.Now()
		_ = s.store.UpdateToken(ctx, token)
		result.Token = token
		result.Reason = "no refresh_token and no session cookie to re-mint from"
		s.enqueueCodexReauthIfEligible(ctx, token.AccountID, result.Reason)
		return result, nil
	}
	// OAuth refresh (web-login or auth.json accounts). OpenAI requires client_id +
	// scope alongside the refresh_token grant; older configs that only set
	// OAuthTokenURL still work (it takes precedence over the CodexOAuth default).
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", token.RefreshToken)
	form.Set("client_id", s.cfg.CodexOAuthClientID)
	form.Set("scope", firstNonEmpty(s.cfg.CodexOAuthScope, config.DefaultCodexOAuthScope))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgent)
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		result.StatusCode = resp.StatusCode
		result.Header = resp.Header.Clone()
		result.Body = raw
		result.Reason, result.TerminalAuthFailure = codexRefreshFailureReason(resp.StatusCode, raw)
		if result.TerminalAuthFailure {
			if cookie, _ := s.store.GetSessionCookie(ctx, token.AccountID); cookie != "" {
				if reminted, ferr := s.remintCodexTokenFromSessionCookie(ctx, token, cookie, true); ferr == nil {
					result.Token = reminted
					result.Refreshed = true
					result.Method = "session_cookie"
					return result, nil
				}
			}
			s.enqueueCodexReauthIfEligible(ctx, token.AccountID, result.Reason)
		}
		return result, fmt.Errorf("openai token refresh failed (%d): %s", resp.StatusCode, bodySnippet(raw, 300))
	}
	var refreshed map[string]interface{}
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		return result, err
	}
	if access, ok := refreshed["access_token"].(string); ok && access != "" {
		token.AccessToken = access
	} else {
		return result, errors.New("openai oauth response had no access_token")
	}
	if refresh, ok := refreshed["refresh_token"].(string); ok && refresh != "" {
		token.RefreshToken = refresh
	}
	token.LastRefresh = storage.Now()
	if err := s.store.UpdateToken(ctx, token); err != nil {
		return result, err
	}
	result.Token = token
	result.Refreshed = true
	result.Method = "openai_oauth"
	return result, nil
}

func (s *Server) remintCodexTokenFromSessionCookie(ctx context.Context, token storage.AccountToken, cookie string, discardRefreshToken bool) (storage.AccountToken, error) {
	accessToken, err := fetchChatGPTSessionToken(ctx, cookie)
	if err != nil {
		return token, err
	}
	account, err := s.store.GetAccount(ctx, token.AccountID)
	if err != nil {
		return token, err
	}
	parsed, err := authparse.ParseAccessToken(accessToken, account.UpstreamAccountID)
	if err != nil {
		return token, fmt.Errorf("ChatGPT session returned unusable accessToken: %w", err)
	}
	token.AccessToken = parsed.AccessToken
	token.IDTokenRaw = parsed.IDTokenRaw
	token.ExpiresAt = parsed.ExpiresAt
	token.LastRefresh = storage.Now()
	token.CredentialMode = authparse.CredentialModeChatGPTAuthTokens
	token.AuthMethod = accountprovider.AuthMethodAccessToken
	if discardRefreshToken {
		token.RefreshToken = ""
	}
	if err := s.store.UpdateToken(ctx, token); err != nil {
		return token, err
	}
	return token, nil
}

func codexRefreshFailureReason(status int, body []byte) (string, bool) {
	hay := strings.ToLower(string(body))
	for _, sig := range []string{
		"refresh_token_invalidated",
		"refresh token has been invalidated",
		"refresh_token_expired",
		"refresh_token_reused",
		"invalid_grant",
		"invalid_scope",
		"insufficient_scope",
		"please try signing in again",
	} {
		if strings.Contains(hay, sig) {
			return sig, true
		}
	}
	if (status == http.StatusBadRequest || status == http.StatusUnauthorized) &&
		strings.Contains(hay, "refresh") &&
		(strings.Contains(hay, "invalid") || strings.Contains(hay, "revoked")) {
		return "invalid_refresh_token", true
	}
	return "http_" + strconv.Itoa(status), false
}

func (s *Server) handleCodexRefreshFailure(ctx context.Context, account storage.Account, result codexRefreshResult, err error, source string) {
	reason := firstNonEmpty(result.Reason, "refresh_failed")
	if result.TerminalAuthFailure {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			AccountID:    account.ID,
			AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
			Action:       "auth_quarantine",
			State:        string(ban.AuthExpired),
			Reason:       reason,
			Detail:       fmt.Sprintf("source=%s http=%d error=%v body=%s", source, result.StatusCode, err, bodySnippet(result.Body, 600)),
		})
		_ = s.store.SetAccountQuarantine(ctx, account.ID, storage.Now()+int64((30*24*time.Hour)/time.Second), "auth refresh failed: "+reason+"; re-login required")
		return
	}
	// Non-terminal (transient) refresh failure: bench-for-recheck so the account is
	// pulled from the pool and only restored once a probe (which re-attempts the
	// refresh) confirms it recovered.
	_ = s.store.BenchBindingForRecheck(ctx, account.ID, storage.Now()+int64((5*time.Minute)/time.Second))
}

// refreshClaude refreshes a Claude OAuth (sk-ant-oat) access token via the
// Anthropic OAuth token endpoint. API keys (sk-ant-api) are static, so there is
// nothing to refresh for them.
func (s *Server) refreshClaude(w http.ResponseWriter, r *http.Request, token storage.AccountToken) {
	account, err := s.store.GetAccount(r.Context(), token.AccountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !claudeTokenCanRefresh(token) {
		token.LastRefresh = storage.Now()
		_ = s.store.UpdateToken(r.Context(), token)
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": token.AccountID, "refreshed": false, "reason": "api key or missing refresh_token; nothing to refresh"})
		return
	}
	if _, err := s.forceRefreshClaudeToken(r.Context(), account, "admin_refresh"); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": token.AccountID, "refreshed": true, "method": "anthropic_oauth"})
}
