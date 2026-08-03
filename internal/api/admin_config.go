// admin_config.go holds the admin console's settings/config/identity/session and
// group/audit/health-test REST handlers plus their small helpers. Extracted verbatim
// from server.go (no behavior change) to shrink the gateway file. Imports are managed
// by goimports.
package api

import (
	"bytes"
	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) adminIdentity(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := identity.For(s.identitySecret(), accountID)
	claudeVer := s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
	codexVer := s.cfg.CodexCLIVersionOrDefault(id.CodexCLIVersion)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":          accountID,
		"os_name":             id.OSName,
		"os_version":          id.OSVersion,
		"arch":                id.Arch,
		"terminal":            id.Terminal,
		"codex_user_agent":    id.CodexUserAgentVersion(codexVer),
		"claude_user_agent":   id.ClaudeUserAgentVersion(claudeVer),
		"claude_cli_version":  claudeVer,
		"codex_cli_version":   codexVer,
		"claude_node_version": s.cfg.ClaudeNodeVersionOrDefault(id.NodeVersion),
		"stainless_version":   s.cfg.ClaudeStainlessVersionOrDefault(id.StainlessPackageVersion),
		"sidecar_impersonate": s.cfg.SidecarImpersonate,
		"session_id":          id.SessionID,
		"claude_session_id":   id.ClaudeSessionID,
		"user_id":             id.UserID,
		"machine_id":          id.MachineID,
		"username":            id.Username,
		"hostname":            id.Hostname,
		"home_dir":            id.HomeDir,
		"stainless_os":        id.StainlessOS,
		"stainless_arch":      id.StainlessArch,
	})
}

// adminSettings exposes the runtime-toggleable control flags with their EFFECTIVE
// values (stored override, else config boot default) and lets the operator flip
// them from the UI without editing the config file or restarting. This is the
// "串号隔离" switch surface plus the Claude cache-injection switch.
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.effectiveSettings(r.Context()))
	case http.MethodPatch, http.MethodPost:
		var req map[string]interface{}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		updates := make(map[string]string, len(req))
		for key, value := range req {
			if !legacyAdminSettingKey(key) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unknown settings key %q", key))
				return
			}
			stored, err := boolSetting(value)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("%s: %w", key, err))
				return
			}
			updates[key] = stored
		}
		if err := s.store.SetSettings(r.Context(), updates); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s.effectiveSettings(r.Context()))
	default:
		methodNotAllowed(w)
	}
}

// adminConfig serves the registry-backed System-config page (config_fields.go). GET
// returns every runtime-editable field with its current effective value, type,
// category, effect (hot/upstream/restart) and whether a DB override is set. PATCH
// validates + persists any subset and hot-applies upstream-consumed fields via the
// upstream client's atomic config overlay — no restart for anything but the few
// bootstrap fields (which are returned read-only).
func (s *Server) adminConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows := s.settingsViewJSON(r.Context())
		placement := strings.TrimSpace(r.URL.Query().Get("placement"))
		domain := strings.TrimSpace(r.URL.Query().Get("domain"))
		if placement != "" || domain != "" {
			if placement != "" && placement != configPlacementAI && placement != configPlacementSystem && placement != configPlacementFeature {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unknown config placement %q", placement))
				return
			}
			validDomain := map[string]bool{"chatgpt": true, "claude": true, "kiro": true, "antigravity": true, "codex": true, "claude_code": true}
			if domain != "" && !validDomain[domain] {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unknown config domain %q", domain))
				return
			}
			filtered := make([]map[string]interface{}, 0, len(rows))
			for _, row := range rows {
				if placement != "" && row["placement"] != placement {
					continue
				}
				if domain != "" && row["domain"] != domain {
					continue
				}
				filtered = append(filtered, row)
			}
			rows = filtered
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPatch, http.MethodPost:
		var req map[string]interface{}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, err := s.applySettingsPatch(r.Context(), req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, s.settingsViewJSON(r.Context()))
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) effectiveSettings(ctx context.Context) map[string]interface{} {
	return map[string]interface{}{
		"conversation_isolation":                s.isolationEnabled(ctx),
		"claude_cache_control_inject":           s.cacheInjectEnabled(ctx),
		"claude_cache_mode":                     firstNonEmpty(s.settingString(ctx, "claude_cache_mode", s.cfg.ClaudeCacheMode), "stable_safe"),
		"claude_native_cache_breakpoint_inject": s.nativeCacheBreakpointInjectEnabled(ctx),
		"claude_cache_latest_tail_write":        s.claudeCacheLatestTailWriteEnabled(ctx),
		"claude_cache_prewarm_mode":             s.claudeCachePrewarmMode(ctx),
		"claude_cache_diagnostics_enabled":      s.claudeCacheDiagnosticsEnabled(ctx),
		"claude_cache_singleflight_enabled":     s.claudeCacheSingleflightEnabled(ctx),
		"claude_cache_lossless_block_split":     s.claudeCacheLosslessBlockSplitEnabled(ctx),
		"leak_scrub":                            s.leakScrubEnabled(ctx),
		"require_downstream_key":                s.flagEnabled(ctx, "require_downstream_key", s.cfg.RequireDownstreamKey),
		"claude_cache_ttl":                      s.claudeCacheTTL(ctx),
		"identity_os_source":                    firstNonEmpty(s.settingString(ctx, "identity_os_source", s.cfg.IdentityOSSource), "vps"),
		"web_search_enabled":                    s.flagEnabled(ctx, "web_search_enabled", s.cfg.WebSearchEnabled),
		"ban_detection_enabled":                 s.flagEnabled(ctx, "ban_detection_enabled", s.cfg.BanDetectionEnabled),
		"ban_auto_delete":                       s.flagEnabled(ctx, "ban_auto_delete", s.cfg.BanAutoDelete),
		"rate_limit_guard_enabled":              s.flagEnabled(ctx, "rate_limit_guard_enabled", s.cfg.RateLimitGuardEnabled),
		"max_concurrent_upstream":               s.cfg.MaxConcurrentUpstream,
		"max_concurrent_upstream_effective":     false,
		"max_concurrent_upstream_note":          "deprecated compatibility field; no process concurrency limit is enforced",
		"allow_registration":                    s.flagEnabled(ctx, "allow_registration", true),
	}
}

// adminSessions visualizes an account's namespaced session map (the "串号隔离"
// view): the conversations currently pinned to this account, the per-account
// namespaced identifier the upstream actually sees for each, the rebind/handoff
// counter (epoch — high = failover churn), and the account's quota/limit cooldown
// and quarantine status.
func (s *Server) adminSessions(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := identity.For(s.identitySecret(), accountID)
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	bindings, err := s.store.ListAffinityBindingsByAccount(r.Context(), accountID, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	isolation := s.isolationEnabled(r.Context())
	now := storage.Now()

	type sessionView struct {
		RouteKeyHash string `json:"route_key_hash"`
		Source       string `json:"source"`
		Original     string `json:"original"`
		Namespaced   string `json:"namespaced"`
		Epoch        int64  `json:"epoch"`
		UpdatedAt    int64  `json:"updated_at"`
	}
	sessions := make([]sessionView, 0, len(bindings))
	for _, b := range bindings {
		orig := correlatorValue(b.RouteKey)
		ns := orig
		if isolation && orig != "" {
			if b.Source == "prompt_cache_key" {
				ns = "cp_" + identity.DerivedKey(id.MachineID, orig)
			} else {
				ns = identity.DerivedUUID(id.MachineID, orig)
			}
		}
		sessions = append(sessions, sessionView{
			RouteKeyHash: b.RouteKeyHash,
			Source:       b.Source,
			Original:     orig,
			Namespaced:   ns,
			Epoch:        b.Epoch,
			UpdatedAt:    b.UpdatedAt,
		})
	}

	var cooldownUntil int64
	var bindingError string
	if binding, err := s.store.GetEgressBinding(r.Context(), accountID); err == nil {
		cooldownUntil = binding.CooldownUntil
	} else {
		bindingError = err.Error()
	}

	resp := map[string]interface{}{
		"account_id":           accountID,
		"isolation_enabled":    isolation,
		"machine_id":           id.MachineID,
		"session_id":           id.SessionID,
		"claude_session_id":    id.ClaudeSessionID,
		"cooldown_until":       cooldownUntil,
		"cooldown_active":      cooldownUntil > now,
		"cooldown_seconds":     maxInt64(0, cooldownUntil-now),
		"quarantine_until":     account.QuarantineUntil,
		"quarantine_active":    account.QuarantineUntil > now,
		"quarantine_reason":    account.QuarantineReason,
		"pinned_conversations": len(sessions),
		"sessions":             sessions,
	}
	if bindingError != "" {
		resp["egress_binding_error"] = bindingError
	}
	writeJSON(w, http.StatusOK, resp)
}

// correlatorValue extracts the original correlator from a route key of the form
// "source:value" (e.g. "conversation_id:abc" → "abc").
func correlatorValue(routeKey string) string {
	if i := strings.Index(routeKey, ":"); i >= 0 {
		return routeKey[i+1:]
	}
	return routeKey
}

// adminHealthTest sends a provider-correct account probe upstream and classifies
// the response (the operator "一键测试存活"). Kiro is handled by its explicit
// two-layer auth + billable inference probe; background callers continue to use
// probeAccountLiveness, whose Kiro branch remains the free UsageLimits check.
func (s *Server) adminHealthTest(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	token, err := s.store.GetToken(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if s.accountProvider(account, token) == "kiro" {
		s.adminKiroHealthTest(w, r, account, token)
		return
	}
	if accountprovider.UsesAPIKey(s.accountProvider(account, token), token) && (s.accountProvider(account, token) == "codex" || s.accountProvider(account, token) == "claude") {
		s.adminProviderAPIKeyHealthTest(w, r, account, token)
		return
	}
	res := s.probeAccountLiveness(r.Context(), account, token)
	if res.Err != nil {
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
			AccountID: accountID, AccountLabel: firstNonEmpty(account.Label, account.Email, accountID),
			Action: "health_test", State: "unreachable", Reason: res.Err.Error(),
			Detail: fmt.Sprintf("provider=%s probe_scope=%s model_checked=%v model=%s", res.Provider, res.ProbeScope, res.ModelChecked, res.Model),
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"account_id": accountID, "alive": false, "state": "unreachable", "error": res.Err.Error(),
			"ready": false, "provider": res.Provider, "model": res.Model, "probe_scope": res.ProbeScope, "model_checked": res.ModelChecked,
			"quarantined": account.QuarantineUntil > storage.Now(), "quarantine_reason": account.QuarantineReason,
		})
		return
	}
	provider := res.Provider
	model := res.Model
	v := res.Verdict
	alive := res.Alive
	deleted := false
	if v.IsBanned() {
		s.handleBannedAccount(r.Context(), account, v, res.Status, res.Body, "health_test")
		deleted = s.bannedAccountWillBeDeleted(r.Context(), account, v)
	} else if v.State == ban.PermissionDenied {
		s.recordPermissionDeniedNoQuarantine(r.Context(), account, v, res.Status, res.Body, "health_test")
	} else {
		// Include the upstream/sidecar body on any error status so the operator can see
		// WHY a probe failed (e.g. a sidecar 502 carries {"error":"..."} naming an
		// unreachable proxy or an unreplayable JA3) — otherwise an unknown/502 verdict
		// is opaque in the audit log and the UI.
		detail := fmt.Sprintf("http=%d alive=%v probe_scope=%s model_checked=%v model=%s", res.Status, alive, res.ProbeScope, res.ModelChecked, model)
		if res.Status >= 400 {
			detail += " body=" + bodySnippet(res.Body, 200)
		}
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
			AccountID:    accountID,
			AccountLabel: firstNonEmpty(account.Label, account.Email, accountID),
			Action:       "health_test",
			State:        string(v.State),
			Reason:       v.Reason,
			Detail:       detail,
		})
		// Auto-clear quarantine when the health-test succeeds (alive=true) and the
		// config enables it (DEFAULT on). This lets operators re-enable an account
		// immediately after fixing the underlying issue (re-login, scope update) by
		// simply running the health-test again, without needing a separate "clear
		// quarantine" manual step. The config gate lets strict deployments require
		// explicit admin review instead.
		if alive && s.cfg.HealthTestClearsQuarantine {
			_ = s.store.SetAccountQuarantine(r.Context(), accountID, 0, "")
		}
	}
	quarantined := false
	quarantineReason := ""
	if current, currentErr := s.store.GetAccount(r.Context(), accountID); currentErr == nil {
		quarantined = current.QuarantineUntil > storage.Now()
		quarantineReason = current.QuarantineReason
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":        accountID,
		"alive":             alive,
		"ready":             res.Ready,
		"state":             string(v.State),
		"reason":            v.Reason,
		"http_status":       res.Status,
		"provider":          provider,
		"model":             model,
		"probe_scope":       res.ProbeScope,
		"model_checked":     res.ModelChecked,
		"deleted":           deleted,
		"quarantined":       quarantined,
		"quarantine_reason": quarantineReason,
		"snippet":           bodySnippet(res.Body, 300),
	})
}

// livenessResult is the outcome of a single liveness probe (probeAccountLiveness).
// Alive answers "is this a valid, non-banned account" (a recoverable rate-limit
// still counts as alive); Ready answers the stricter "is it ready to serve traffic
// right now" (a clean 2xx only). The admin health-test uses Alive; the background
// recheck loop uses Ready, so it never re-admits an account that is still rate-limited.
type livenessResult struct {
	Provider     string
	Model        string
	ProbeScope   string
	ModelChecked bool
	Status       int
	Body         []byte
	Verdict      ban.Verdict
	Alive        bool
	Ready        bool
	Err          error
}

// probeAccountLiveness runs a provider-correct liveness probe ("测活") against one
// account — the same request shape live traffic uses, through the same egress — and
// classifies the result. It is the shared core of both the admin health-test endpoint
// and the background recheck loop. A setup/transport failure is returned in Err
// (treated as unreachable); otherwise Status/Body/Verdict/Alive/Ready are populated.
func (s *Server) probeAccountLiveness(ctx context.Context, account storage.Account, token storage.AccountToken) livenessResult {
	provider := s.accountProvider(account, token)
	res := livenessResult{Provider: provider, ProbeScope: "model_request", ModelChecked: true}
	if provider == "kiro" {
		res.ProbeScope = "account_auth_usage"
		res.ModelChecked = false
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		res.Err = err
		return res
	}
	egress, err := s.store.ResolvePrimaryEgressBinding(ctx, binding)
	if err != nil {
		res.Err = err
		return res
	}
	if accountprovider.UsesAPIKey(provider, token) && (provider == "codex" || provider == "claude") {
		res.ProbeScope = "account_auth_models"
		res.ModelChecked = false
		stage, caps := s.runProviderAPIKeyAuthProbe(ctx, account, token, egress)
		res.Status = stage.HTTPStatus
		res.Alive = stage.Alive
		res.Ready = stage.Alive
		if stage.ErrorCode != "" {
			res.Body, _ = json.Marshal(map[string]interface{}{"error": map[string]interface{}{"code": stage.ErrorCode}})
		}
		res.Verdict = ban.Classify(stage.Alive, stage.HTTPStatus, http.Header{}, res.Body)
		if stage.State == "unreachable" {
			res.Err = errors.New(firstNonEmpty(stage.ErrorCode, "provider API key authentication probe failed"))
		}
		if stage.Alive {
			_ = s.store.ReplaceCapabilities(ctx, account.ID, caps)
		}
		return res
	}
	if provider == "antigravity" {
		return s.probeAntigravityLiveness(ctx, account, binding, egress)
	}
	model := ""
	if provider != "kiro" {
		model = s.probeModel(ctx, account.ID, provider)
	}
	res.Model = model
	if provider == "kiro" {
		cred, kerr := s.store.GetKiroCredentials(ctx, account.ID)
		if kerr != nil {
			res.Err = kerr
			return res
		}
		s.kiro.UpdateConfig(s.effectiveKiroConfig(ctx))
		bearer, _, cred, kerr := s.kiro.Prepare(ctx, account, cred, token, egress, false)
		if kerr != nil {
			res.Err = kerr
			return res
		}
		usageResult, kerr := s.kiro.UsageLimitsProbe(ctx, account, cred, bearer, egress)
		res.Status = usageResult.StatusCode
		res.Body = usageResult.Body
		if kerr != nil {
			// Transport/setup errors have no HTTP evidence and remain unreachable.
			// A real non-2xx UsageLimits response is still returned as a classified
			// auth result so callers can report its state and status accurately.
			if usageResult.StatusCode == 0 || (usageResult.StatusCode >= 200 && usageResult.StatusCode < 300) {
				res.Err = kerr
				return res
			}
			res.Verdict = ban.Classify(false, usageResult.StatusCode, usageResult.Header, usageResult.Body)
			res.Alive = false
			res.Ready = false
			return res
		}
		if len(res.Body) == 0 {
			res.Body, _ = json.Marshal(usageResult.Limits)
		}
		usage, usageErr := parseKiroUsageLimits(usageResult.Limits)
		if usageErr != nil {
			res.Err = usageErr
			return res
		}
		res.Alive = true
		res.Ready = usage.Status != "exhausted"
		if res.Ready {
			res.Verdict = ban.Classify(true, usageResult.StatusCode, usageResult.Header, res.Body)
		} else {
			res.Verdict = ban.Verdict{State: ban.RateLimited, Reason: "kiro_usage_exhausted"}
		}
		return res
	}
	req := upstream.Request{
		Method:       http.MethodPost,
		Headers:      http.Header{},
		Account:      account,
		Token:        token,
		Egress:       egress,
		CookieJarKey: binding.CookieJarKey,
	}
	if provider == "claude" {
		// Probe liveness with a count_tokens call carrying the same Claude Code
		// identity system block as live traffic. count_tokens consumes no generation
		// quota and has a higher rate limit than a synthetic message. Its schema is
		// narrower than /v1/messages, though: current OAuth deployments reject the
		// otherwise-valid messages metadata field with "Extra inputs are not
		// permitted". Keep the identity/billing system blocks and strip only that
		// unsupported top-level field, matching the reference client's native token
		// counting path rather than turning a schema 400 into an auth failure.
		req.Provider = "claude"
		req.DownstreamPath = "/v1/messages/count_tokens"
		req.MinimalProbe = true
		base := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}]}`)
		osHint := s.osHint(base, egress)
		id := identity.ForOS(s.identitySecret(), account.ID, osHint)
		// Keep the probe byte-shaped like live traffic: cloak + (for OAuth) the
		// x-anthropic-billing-header real Claude Code sends on count_tokens too, folded
		// into the one virtualization pass exactly as messages.go does.
		probeOAuth := claudeIsOAuth(token)
		probeBillingVer := ""
		if probeOAuth {
			probeBillingVer = s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
		}
		result := cloak.VirtualizeClaudeCode(base, id, s.cfg.SensitiveWordsFor("claude"), probeOAuth, probeBillingVer)
		req.SetBodyBytes(claudeCountTokensProbeBody(result.Body))
		req.OSHint = osHint
	} else if upstream.IsCustomProvider(provider) {
		// Custom OpenAI-compatible provider: a minimal non-streaming chat completion.
		// Any 2xx — or a non-ban 4xx (bad model, rate limit) — proves the key is live.
		prov, ok := s.customProviderByID(ctx, provider)
		if !ok {
			res.Err = fmt.Errorf("custom provider %s not found", provider)
			return res
		}
		req.Provider = provider
		req.BaseURL = prov.BaseURL
		if prov.UpstreamProtocol == storage.CustomProviderProtocolResponses {
			req.DownstreamPath = "/responses"
			req.SetBodyBytes([]byte(`{"model":"` + model + `","input":[{"role":"user","content":"ping"}],"max_output_tokens":1,"stream":false}`))
		} else {
			req.DownstreamPath = "/chat/completions"
			req.SetBodyBytes([]byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`))
		}
	} else {
		// The Codex /responses backend (WHAM) is STREAMING-ONLY and hard-validates the
		// top-level fields the real Codex client always sends. THE PROBE MUST STREAM
		// (stream:true): the real client never sends a non-streaming /responses request
		// (buildCodexWebSocketCreatePayload hard-sets stream:true, and the Session-9 capture
		// shows every real call is an SSE/WS stream). A stream:false request makes the
		// backend evaluate it in stored-response mode, which conflicts with the mandatory
		// store:false for ChatGPT auth and is rejected with 400 {"detail":"Store must be set
		// to false"} EVEN WHEN store:false is already present — i.e. stream:false, not the
		// store value, is the real trigger. (This was the bug: a sidecar health-test 400'd
		// with exactly that body despite carrying store:false, so the Session-21 "add
		// store:false" fix could never resolve it.) The other two hard-validated fields:
		// a non-empty "instructions" (else 400 {"detail":"Instructions are required"}) and
		// "store":false (store:true is correct ONLY for an Azure responses endpoint, false
		// for chatgpt.com; other_codex client.rs:781). The backend validates PRESENCE/value,
		// not exact instructions content, so a short probe value suffices. (The shared
		// normalizeCodexResponsesBody in client.Do also enforces instructions+store on the
		// live relay path; this probe body is byte-correct on its own.) A healthy account
		// answers the streamed "ping" with HTTP 200 + an SSE body, which the io.ReadAll below
		// consumes (capped at 1 MiB) to classify liveness.
		//
		// Mirror the live relay's per-model client version too (codexClientVersionForModel,
		// as the gateway does at dispatch): version-gated newer models (the gpt-5.x family)
		// answer a default/older-client probe with "requires a newer version of codex"
		// instead of running, so without this the probe would surface a version error rather
		// than true liveness. applyCodexHeaders lets this per-request version win for the
		// UA + version headers; it is "" (no-op) for models that don't gate on version.
		req.CodexClientVersion = s.codexClientVersionForModel(model)
		req.DownstreamPath = "/v1/responses"
		req.SetBodyBytes(codexLivenessProbeBody(model))
	}
	if provider == "claude" {
		var perr error
		token, perr = s.prepareClaudeToken(ctx, account, token, "health_preflight")
		if perr != nil {
			res.Err = perr
			return res
		}
		req.Token = token
	} else if provider == "codex" {
		var perr error
		token, perr = s.ensureAgentIdentityTask(ctx, account, token, egress, binding.CookieJarKey, "")
		if perr != nil {
			res.Err = perr
			return res
		}
		req.Token = token
	}
	resp, err := s.upstream.Do(ctx, req)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if provider == "codex" {
		body = redactAgentIdentityError(token, body)
		if isInvalidAgentIdentityTask(resp.StatusCode, body, token) {
			if recovered, rerr := s.ensureAgentIdentityTask(ctx, account, token, egress, binding.CookieJarKey, token.AgentTaskID); rerr == nil {
				token = recovered
				req.Token = token
				resp.Body.Close()
				retryResp, retryErr := s.upstream.Do(ctx, req)
				if retryErr != nil {
					res.Err = retryErr
					return res
				}
				defer retryResp.Body.Close()
				body, _ = io.ReadAll(io.LimitReader(retryResp.Body, 1<<20))
				body = redactAgentIdentityError(token, body)
				resp = retryResp
			}
		}
	}
	if provider == "claude" && claudeAuthError(resp.StatusCode, resp.Header, body) && claudeTokenCanRefresh(token) {
		if refreshed, rerr := s.forceRefreshClaudeToken(ctx, account, "auth_error"); rerr == nil {
			req.Token = refreshed
			retryResp, retryErr := s.upstream.Do(ctx, req)
			if retryErr != nil {
				res.Err = retryErr
				return res
			}
			defer retryResp.Body.Close()
			body, _ = io.ReadAll(io.LimitReader(retryResp.Body, 1<<20))
			resp = retryResp
		}
	}
	if provider == "codex" && model != "gpt-5.5" && codexHealthModelUnsupported(resp.StatusCode, body) {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			AccountID:    account.ID,
			AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
			Action:       "health_test_model_unsupported",
			State:        "retry",
			Reason:       "model_not_supported",
			Detail:       fmt.Sprintf("provider=%s model=%s retry_model=gpt-5.5 http=%d body=%s", provider, model, resp.StatusCode, bodySnippet(body, 300)),
		})
		model = "gpt-5.5"
		res.Model = model
		req.CodexClientVersion = s.codexClientVersionForModel(model)
		req.SetBodyBytes(codexLivenessProbeBody(model))
		retryResp, retryErr := s.upstream.Do(ctx, req)
		if retryErr != nil {
			res.Err = retryErr
			return res
		}
		defer retryResp.Body.Close()
		body, _ = io.ReadAll(io.LimitReader(retryResp.Body, 1<<20))
		resp = retryResp
	}
	res.Status = resp.StatusCode
	res.Body = body
	res.Verdict = ban.Classify(resp.StatusCode < 400, resp.StatusCode, resp.Header, body)
	res.Alive = resp.StatusCode < 400 || res.Verdict.State == ban.RateLimited || res.Verdict.State == ban.Unknown
	res.Ready = resp.StatusCode < 400
	return res
}

// probeAntigravityLiveness uses the account-scoped, non-billable model catalog.
// It exercises token refresh and the same bound outlet as live inference without
// sending a synthetic prompt or routing the built-in provider through the custom
// OpenAI-compatible adapter.
func (s *Server) probeAntigravityLiveness(ctx context.Context, account storage.Account, binding storage.AccountEgressBinding, egress storage.EgressProfile) livenessResult {
	res := livenessResult{
		Provider:     "antigravity",
		ProbeScope:   "account_auth_models",
		ModelChecked: true,
	}
	creds, err := s.store.GetAntigravityCredentials(ctx, account.ID)
	if err != nil {
		res.Err = err
		return res
	}
	accessToken, refreshedCreds, err := s.ensureAntigravityToken(ctx, creds, account, egress, binding.CookieJarKey)
	if err != nil {
		res.Err = err
		return res
	}
	models, err := s.upstream.FetchAntigravityModels(ctx, egress, binding.CookieJarKey, accessToken, refreshedCreds.ProjectID, refreshedCreds.BaseURL, refreshedCreds.UserAgent)
	if err != nil {
		res.Err = err
		return res
	}
	if len(models) > 0 {
		res.Model = models[0].ID
	}
	res.Status = http.StatusOK
	res.Body, _ = json.Marshal(map[string]interface{}{
		"model_count": len(models),
		"model":       res.Model,
	})
	res.Verdict = ban.Classify(true, res.Status, http.Header{}, res.Body)
	res.Alive = true
	res.Ready = true
	return res
}

// probeModel returns a provider-correct model slug to use for a liveness probe.
func (s *Server) probeModel(ctx context.Context, accountID, provider string) string {
	if provider == "claude" {
		return "claude-sonnet-4-5"
	}
	if provider == "kiro" {
		// Kiro liveness uses UsageLimits and does not execute a model request.
		return ""
	}
	if upstream.IsCustomProvider(provider) {
		if prov, ok := s.customProviderByID(ctx, provider); ok && len(prov.Models) > 0 {
			return prov.Models[0]
		}
		return provider
	}
	if configured := strings.TrimSpace(s.settingString(ctx, "codex_install_model", s.cfg.CodexInstallModel)); codexHealthProbeModelAllowed(configured) {
		return configured
	}
	if caps, _ := s.store.ListCapabilities(ctx, accountID); len(caps) > 0 {
		for _, cap := range caps {
			if codexHealthProbeModelAllowed(cap.ModelSlug) && strings.HasPrefix(strings.TrimSpace(cap.ModelSlug), "gpt-5") {
				return strings.TrimSpace(cap.ModelSlug)
			}
		}
		for _, cap := range caps {
			if codexHealthProbeModelAllowed(cap.ModelSlug) {
				return strings.TrimSpace(cap.ModelSlug)
			}
		}
	}
	return "gpt-5.6-sol"
}

func codexLivenessProbeBody(model string) []byte {
	return []byte(`{"model":"` + model + `","instructions":"You are a coding agent.","store":false,"input":[{"role":"user","content":"ping"}],"stream":true}`)
}

func claudeCountTokensProbeBody(body []byte) []byte {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	if _, present := root["metadata"]; !present {
		return body
	}
	delete(root, "metadata")
	normalized, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return normalized
}

func codexHealthProbeModelAllowed(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if strings.HasSuffix(model, "-codex") {
		return false
	}
	return true
}

func codexHealthModelUnsupported(status int, body []byte) bool {
	if status < 400 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, sig := range []string{
		"model not supported",
		"unsupported model",
		"does not support this model",
		"doesn't support this model",
		"not supported for this account",
	} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// adminAudit returns the audit log (account lifecycle + automated ban actions).
func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	var (
		rows []storage.AuditLogRow
		err  error
	)
	if accountID != "" {
		rows, err = s.store.ListAuditLogForAccount(r.Context(), accountID, limit)
	} else {
		rows, err = s.store.ListAuditLog(r.Context(), limit)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []storage.AuditLogRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func legacyAdminSettingKey(key string) bool {
	switch key {
	case "conversation_isolation", "claude_cache_control_inject", "leak_scrub", "allow_registration":
		return true
	default:
		return false
	}
}

func boolSetting(v interface{}) (string, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return "1", nil
		}
		return "0", nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "on", "yes":
			return "1", nil
		case "0", "false", "off", "no", "":
			return "0", nil
		}
	}
	return "", fmt.Errorf("expected boolean")
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) adminGroups(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		groups, err := s.store.ListGroups(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		names := make([]string, 0, len(groups))
		for _, group := range groups {
			names = append(names, group.Name)
		}
		counts, err := s.store.CountAccountsByGroups(r.Context(), names)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		type groupView struct {
			storage.Group
			AccountCount           int    `json:"account_count"`
			ActiveAccountCount     int    `json:"active_account_count"`
			ModelInstructionsError string `json:"model_instructions_error,omitempty"`
		}
		out := make([]groupView, 0, len(groups))
		for _, group := range groups {
			c := counts[group.Name]
			view := groupView{Group: group, AccountCount: c.AccountCount, ActiveAccountCount: c.ActiveAccountCount}
			if group.ModelInstructionsEnabled {
				if _, _, err := s.compileGroupModelInstructions(r.Context(), group); err != nil {
					view.ModelInstructionsError = err.Error()
				}
			}
			out = append(out, view)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		// Create a new group. Body: {name, [system_prompt, force_model, force_effort,
		// default_egress_id, virtual_2m_enabled, ...]} — same field set as PATCH.
		raw, err := readLimited(r.Body, s.cfg.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		var nameReq struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &nameReq)
		name := strings.TrimSpace(nameReq.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, errors.New("group name required"))
			return
		}
		if _, err := s.store.GetGroup(r.Context(), name); err == nil {
			writeError(w, http.StatusConflict, fmt.Errorf("group %q already exists", name))
			return
		}
		g := storage.Group{Name: name, PromptMode: "prepend", Virtual2MEnabled: false, SystemPromptApplyToCompaction: true}
		if err := applyGroupFieldsFromBody(&g, bytes.NewReader(raw)); err != nil {
			writeGroupFieldError(w, err)
			return
		}
		if err := s.validateOrderedEgressIDs(r, g.EgressIDs); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeGroupEgressDeprecationWarning(w, raw)
		if err := s.store.CreateGroup(r.Context(), g); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if stored, err := s.store.GetGroup(r.Context(), g.Name); err == nil {
			g = stored
		}
		writeJSON(w, http.StatusOK, g)
	default:
		methodNotAllowed(w)
	}
}

// applyGroupFieldsFromBody decodes a partial group update (pointer fields = only the
// provided keys change) from the request body and applies it onto g. Shared by the
// cyber back-compat handler, group create, and the generic PATCH /admin/groups/<name>.
var errInvalidGroupPolicy = errors.New("account pool groups cannot contain user policy")

func applyGroupFieldsFromBody(g *storage.Group, body io.Reader) error {
	var req struct {
		SystemPrompt                        *string                       `json:"system_prompt"`
		PromptMode                          *string                       `json:"prompt_mode"`
		SystemPromptApplyToCompaction       *bool                         `json:"system_prompt_apply_to_compaction"`
		ModelInstructionsEnabled            *bool                         `json:"model_instructions_enabled"`
		ModelInstructionsFiles              []string                      `json:"model_instructions_files"`
		SuperInstructEnabled                *bool                         `json:"super_instruct_enabled"`
		SuperInstructSkillIDs               []string                      `json:"super_instruct_skill_ids"`
		SuperInstructProfiles               storage.SuperInstructProfiles `json:"super_instruct_profiles"`
		SuperInstructResponseRewriteEnabled *bool                         `json:"super_instruct_response_rewrite_enabled"`
		SuperInstructMemoryEnabled          *bool                         `json:"super_instruct_memory_enabled"`
		SuperInstructMonitorEnabled         *bool                         `json:"super_instruct_monitor_enabled"`
		ForceModel                          *string                       `json:"force_model"`
		ForceEffort                         *string                       `json:"force_effort"`
		DefaultEgressID                     *string                       `json:"default_egress_id"`
		EgressIDs                           *[]string                     `json:"egress_ids"`
		Virtual2MEnabled                    *bool                         `json:"virtual_2m_enabled"`
	}
	if err := decodeJSONRequestBody(body, &req, adminJSONBodyLimit); err != nil {
		return err
	}
	if (req.SystemPrompt != nil && strings.TrimSpace(*req.SystemPrompt) != "") ||
		(req.ModelInstructionsEnabled != nil && *req.ModelInstructionsEnabled) ||
		(req.ModelInstructionsFiles != nil && len(req.ModelInstructionsFiles) > 0) ||
		(req.SuperInstructEnabled != nil && *req.SuperInstructEnabled) ||
		(req.SuperInstructSkillIDs != nil && len(req.SuperInstructSkillIDs) > 0) ||
		(req.SuperInstructProfiles != nil && len(req.SuperInstructProfiles) > 0) ||
		(req.SuperInstructResponseRewriteEnabled != nil && *req.SuperInstructResponseRewriteEnabled) ||
		(req.SuperInstructMemoryEnabled != nil && *req.SuperInstructMemoryEnabled) ||
		(req.SuperInstructMonitorEnabled != nil && *req.SuperInstructMonitorEnabled) ||
		(req.ForceModel != nil && strings.TrimSpace(*req.ForceModel) != "") ||
		(req.ForceEffort != nil && strings.TrimSpace(*req.ForceEffort) != "") ||
		(req.Virtual2MEnabled != nil && *req.Virtual2MEnabled) {
		return fmt.Errorf("%w: configure prompts, instructions, Super-Instruct, model, and effort on a user group", errInvalidGroupPolicy)
	}
	if req.SystemPrompt != nil {
		g.SystemPrompt = *req.SystemPrompt
	}
	if req.PromptMode != nil {
		g.PromptMode = *req.PromptMode
	}
	if req.SystemPromptApplyToCompaction != nil {
		g.SystemPromptApplyToCompaction = *req.SystemPromptApplyToCompaction
	}
	if req.ModelInstructionsEnabled != nil {
		g.ModelInstructionsEnabled = *req.ModelInstructionsEnabled
	}
	if req.ModelInstructionsFiles != nil {
		files, err := normalizeModelInstructionFileNames(req.ModelInstructionsFiles)
		if err != nil {
			return err
		}
		g.ModelInstructionsFiles = files
	}
	if req.ForceModel != nil {
		g.ForceModel = strings.TrimSpace(*req.ForceModel)
	}
	if req.ForceEffort != nil {
		g.ForceEffort = normalizeEffort(*req.ForceEffort)
	}
	if req.DefaultEgressID != nil {
		g.DefaultEgressID = strings.TrimSpace(*req.DefaultEgressID)
		g.EgressIDs = []string{g.DefaultEgressID}
	}
	if req.EgressIDs != nil {
		g.EgressIDs = *req.EgressIDs
	}
	return nil
}

func writeGroupFieldError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInvalidGroupPolicy) {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_group_policy", err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func writeGroupEgressDeprecationWarning(w http.ResponseWriter, raw []byte) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields["default_egress_id"] != nil && fields["egress_ids"] != nil {
		w.Header().Add("Warning", `299 codex-pool "default_egress_id is deprecated; egress_ids was used"`)
	}
}

// adminGroupAction handles the /admin/groups/<name> subtree. PATCH/DELETE are the
// active group-management surface. The assign-egress subroute is retained for legacy
// clients and now updates the same ordered group egress list as PATCH; it never copies
// that list into account records. The egress-policy subroute remains registration-only.
func (s *Server) adminGroupAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if len(parts) == 2 {
		switch parts[1] {
		case "assign-egress":
			s.adminGroupAssignEgress(w, r, name)
		case "egress-policy":
			s.adminGroupEgressPolicy(w, r, name)
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodPatch:
		group, err := s.store.GetGroup(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Errorf("group %q not found", name))
			return
		}
		raw, readErr := readLimited(r.Body, s.cfg.MaxBodyBytes)
		if readErr != nil {
			writeError(w, http.StatusRequestEntityTooLarge, readErr)
			return
		}
		if err := applyGroupFieldsFromBody(&group, bytes.NewReader(raw)); err != nil {
			writeGroupFieldError(w, err)
			return
		}
		if err := s.validateOrderedEgressIDs(r, group.EgressIDs); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeGroupEgressDeprecationWarning(w, raw)
		if err := s.store.UpdateGroup(r.Context(), group); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if stored, err := s.store.GetGroup(r.Context(), name); err == nil {
			group = stored
		}
		writeJSON(w, http.StatusOK, group)
	case http.MethodDelete:
		if name == s.cfg.DefaultGroup {
			writeError(w, http.StatusBadRequest, fmt.Errorf("cannot delete the default group %q", name))
			return
		}
		if n, err := s.store.CountAccountsByGroup(r.Context(), name); err == nil && n > 0 {
			writeError(w, http.StatusConflict, fmt.Errorf("group %q still has %d account(s); reassign them first", name, n))
			return
		}
		if err := s.store.DeleteGroup(r.Context(), name); err != nil {
			if errors.Is(err, storage.ErrTargetInUse) {
				writePoolCodeError(w, http.StatusConflict, "target_in_use", err.Error())
				return
			}
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"group": name, "deleted": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminGroupEgressPolicy(w http.ResponseWriter, r *http.Request, groupName string) {
	if _, err := s.store.GetGroup(r.Context(), groupName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("group %q not found", groupName))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		policy, err := s.store.GetGroupEgressPolicy(r.Context(), groupName)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusOK, storage.GroupEgressPolicy{GroupName: groupName, AssignmentStrategy: "sticky_least_used"})
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, policy)
	case http.MethodPost:
		var policy storage.GroupEgressPolicy
		if err := decodeJSONRequestBody(r.Body, &policy, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		policy.GroupName = groupName
		policy.RegistrationPoolID = strings.TrimSpace(policy.RegistrationPoolID)
		policy.RuntimePoolID = strings.TrimSpace(policy.RuntimePoolID)
		if policy.RuntimePoolID != "" {
			writeError(w, http.StatusBadRequest, errors.New("runtime egress pools are no longer supported; use per-account egress bindings"))
			return
		}
		if policy.RegistrationPoolID != "" {
			if _, err := s.getRegistrationEgressPool(r.Context(), policy.RegistrationPoolID); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		if err := s.store.UpsertGroupEgressPolicy(r.Context(), policy); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		got, err := s.store.GetGroupEgressPolicy(r.Context(), groupName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, got)
	default:
		methodNotAllowed(w)
	}
}

// adminGroupAssignEgress is a legacy compatibility endpoint for clients that still send
// primary_egress_id + standby_egress_ids. It updates the account-pool group's ordered
// outlet list only. Accounts inherit that list dynamically at selection time, so no
// account egress binding is copied or rewritten here.
func (s *Server) adminGroupAssignEgress(w http.ResponseWriter, r *http.Request, groupName string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		PrimaryEgressID string   `json:"primary_egress_id"`
		StandbyEgressID []string `json:"standby_egress_ids"`
		SetGroupDefault bool     `json:"set_group_default"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	primary := strings.TrimSpace(req.PrimaryEgressID)
	// Validate referenced egresses exist before touching any binding.
	for _, id := range append([]string{}, append(req.StandbyEgressID, primary)...) {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if _, err := s.store.GetEgressProfile(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("egress %q not found", id))
			return
		}
	}
	standby := make([]string, 0, len(req.StandbyEgressID))
	seen := map[string]bool{}
	if primary != "" {
		seen[primary] = true
	}
	for _, id := range req.StandbyEgressID {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			standby = append(standby, id)
		}
	}
	group, err := s.store.GetGroup(r.Context(), groupName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	group.EgressIDs = append([]string{}, standby...)
	if primary != "" {
		group.EgressIDs = append([]string{primary}, standby...)
	}
	if err := s.store.UpdateGroup(r.Context(), group); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accountCount, err := s.store.CountAccountsByGroup(r.Context(), groupName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Add("Warning", `299 codex-pool "assign-egress no longer rewrites accounts; the group egress order is inherited dynamically"`)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"group":               groupName,
		"accounts_updated":    0,
		"accounts_inheriting": accountCount,
		"primary_egress":      primary,
		"standby_egress":      standby,
		"egress_ids":          group.EgressIDs,
		"set_group_default":   true,
		"deprecated_request":  req.SetGroupDefault,
	})
}

func (s *Server) adminVirtualSweep(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		RouteKeyHash string `json:"route_key_hash"`
		Limit        int    `json:"limit"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.RouteKeyHash == "" {
		writeError(w, http.StatusBadRequest, errors.New("route_key_hash required"))
		return
	}
	items, err := s.store.ListVirtualLedger(r.Context(), req.RouteKeyHash, req.Limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"route_key_hash": req.RouteKeyHash,
		"items":          items,
		"reasoning":      "same-model exhaustive ledger sweep; caller controls model/reasoning on replay",
	})
}

func (s *Server) adminAllowed(w http.ResponseWriter, r *http.Request) bool {
	// A logged-in user session takes precedence: admins pass, a non-admin is rejected
	// (so a normal portal user can never reach /admin/*). Session-authenticated unsafe
	// methods must also carry the double-submit CSRF token; the admin_token (Bearer,
	// non-ambient) path below is exempt.
	if u, ok := s.currentUser(r); ok {
		if u.Role != "admin" {
			writeError(w, http.StatusForbidden, errors.New("admin role required"))
			return false
		}
		if !s.csrfOK(r) {
			writeError(w, http.StatusForbidden, errors.New("invalid or missing CSRF token"))
			return false
		}
		return true
	}
	if s.cfg.AdminToken == "" {
		if downstreamAPIKeyAttempt(r) {
			writeError(w, http.StatusForbidden, errors.New("admin role required"))
			return false
		}
		// No admin token configured and no session: open bootstrap until the portal has
		// an admin, after which anonymous /admin is locked down (must log in as admin).
		if s.hasAdminUser(r.Context()) {
			writeError(w, http.StatusUnauthorized, errors.New("admin login required"))
			return false
		}
		return true
	}
	token := adminBearerToken(r)
	// Constant-time compare so a remote attacker can't time-probe the admin token
	// byte-by-byte. (An empty AdminToken is handled by the bootstrap branch above, so
	// both operands are non-empty secrets here.)
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) == 1 {
		return true
	}
	if downstreamAPIKeyAttempt(r) {
		writeError(w, http.StatusForbidden, errors.New("admin role required"))
		return false
	}
	writeError(w, http.StatusUnauthorized, errors.New("admin token required"))
	return false
}

func adminBearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func downstreamAPIKeyAttempt(r *http.Request) bool {
	for _, plain := range []string{
		strings.TrimSpace(r.Header.Get("x-api-key")),
		strings.TrimSpace(r.Header.Get("X-Downstream-Key")),
		adminBearerToken(r),
	} {
		if strings.HasPrefix(plain, "cap_") || strings.HasPrefix(plain, "poolimp_") {
			return true
		}
	}
	return false
}
