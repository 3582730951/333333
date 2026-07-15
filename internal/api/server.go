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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/cfsolve"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/console"
	"codex-account-pool/internal/gopay"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/kiro"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/payment"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/reliability"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
	"codex-account-pool/internal/warp"
	"codex-account-pool/internal/web"
)

type Dependencies struct {
	Config    config.Config
	Store     *storage.Store
	Scheduler *scheduler.Scheduler
	Upstream  *upstream.Client
	Planner   *virtual.Planner
	Gopay     *gopay.Manager
	// PaymentMgr abstracts Plus upgrade payment providers (GoPay/PayPal) so automation
	// and lifecycle are decoupled from the payment implementation.
	PaymentMgr *payment.Manager
	// Warp is the multi-exit WARP CF-fallback manager (nil = WARP disabled).
	Warp *warp.Manager
	// Solver is the cf_clearance solver client (nil/disabled = no solver rung).
	Solver *cfsolve.Client
}

type Server struct {
	cfg        config.Config
	store      *storage.Store
	scheduler  *scheduler.Scheduler
	upstream   *upstream.Client
	planner    *virtual.Planner
	gopay      *gopay.Manager
	paymentMgr *payment.Manager
	warp       *warp.Manager
	solver     *cfsolve.Client
	mux        *http.ServeMux
	// oauth holds in-flight web-login (paste-back) PKCE sessions for the
	// /admin/oauth/* import flow. In-memory + TTL'd; see oauth.go.
	oauth *oauthStore
	// identitySecretCached is the resolved identity secret, computed once in
	// NewServer. ResolveSecret reads /etc/machine-id from disk on the common
	// (unconfigured) path, so resolving it per request would be a syscall on every
	// relay; the result is deterministic, so caching is behavior-identical.
	identitySecretCached []byte
	// login throttles failed end-user login attempts per client IP (multi-user portal).
	login *loginThrottle
	// clientErrors throttles browser-side error reports so the diagnostics endpoint
	// cannot amplify a frontend fault or unauthenticated request flood into log spam.
	clientErrors *clientErrorLimiter
	// relState holds per-conversation working_state for the gateway reliability layer
	// (in-memory + TTL'd + size-bounded; same pattern as oauth/login). Only written
	// when the gateway_reliability flag is on. See reliability.go.
	relState *reliability.Store
	// regHandler handles registration API requests
	regHandler       *Handler
	lifecycleHandler *LifecycleHandlers
	claudeRefresh    *claudeRefreshGates
	kiro             *kiro.Manager
	// asyncWrites carries fire-and-forget DB writes (usage rows, virtual-ledger rows)
	// off the request path so the response is not blocked on a write through the single
	// SQLite write connection. A single drainer goroutine runs them FIFO (matching the
	// 1-writer pool); see asyncwrite.go. FlushWrites drains it on shutdown.
	asyncWrites           chan func()
	usageWrites           chan telemetryWrite
	usagePending          sync.WaitGroup
	asyncWG               sync.WaitGroup
	asyncMu               sync.RWMutex
	asyncClosed           bool
	asyncBytes            int64
	billingEstimates      sync.Map
	missingLimitAudit     sync.Map // account/provider -> last emitted unix hour
	upstreamRulesMu       sync.RWMutex
	upstreamRulesCache    []storage.UpstreamErrorRule
	upstreamRulesCachedAt time.Time

	claudeCacheFlightsMu sync.Mutex
	claudeCacheFlights   map[string]chan struct{}
	claudeCacheDiagMu    sync.Mutex
	claudeCacheDiagPrev  map[string]string
	kiroCacheFlightsMu   sync.Mutex
	kiroCacheFlights     map[string]chan struct{}

	codexResetMu    sync.Mutex
	codexResetLocks map[string]*sync.Mutex
	compatMu        sync.Mutex
	compatRecent    []compatIncompatibilityRecord
	qualityMu       sync.Mutex
	qualityRunning  bool
	contextRebuilt  uint64
	contextDegraded uint64
}

func NewServer(dep Dependencies) *Server {
	s := &Server{
		cfg:        dep.Config,
		store:      dep.Store,
		scheduler:  dep.Scheduler,
		upstream:   dep.Upstream,
		planner:    dep.Planner,
		gopay:      dep.Gopay,
		paymentMgr: dep.PaymentMgr,
		warp:       dep.Warp,
		solver:     dep.Solver,
		mux:        http.NewServeMux(),
		oauth:      newOAuthStore(oauthSessionTTL),
		login:      newLoginThrottle(),
		clientErrors: newClientErrorLimiter(
			clientErrorLogLimit,
			clientErrorLogWindow,
			clientErrorLogMaxClients,
		),
		relState:            reliability.NewStore(relStateTTL, relStateMax),
		regHandler:          NewHandler(dep.Store, dep.Upstream, dep.Config.DefaultRegisterMethod, dep.Config.RegistrationConcurrency, &dep.Config), // builds live provider Manager from provider_settings
		lifecycleHandler:    newServerLifecycleHandlers(dep.Store),
		claudeRefresh:       newClaudeRefreshGates(),
		kiro:                kiro.NewManager(dep.Store, dep.Upstream, dep.Config),
		claudeCacheFlights:  map[string]chan struct{}{},
		claudeCacheDiagPrev: map[string]string{},
		kiroCacheFlights:    map[string]chan struct{}{},
		codexResetLocks:     map[string]*sync.Mutex{},
	}
	// Resolve the identity secret once (it can read host files on the unconfigured
	// path); s.identitySecret() returns this cached value on the hot path.
	s.identitySecretCached = identity.ResolveSecret([]byte(dep.Config.IdentitySecret))
	s.startAsyncWriter()
	// Apply any persisted runtime overrides for the upstream-consumed fingerprint /
	// identity fields to the upstream client at boot, so admin settings survive a
	// restart (the request-time getters already overlay the rest live).
	if s.upstream != nil {
		s.upstream.UpdateConfig(s.effectiveUpstreamConfig(context.Background()))
	}
	if s.scheduler != nil {
		s.scheduler.UpdateConfig(s.effectiveSchedulerConfig(context.Background()))
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/v1/responses", s.handleGatewayPost)
	s.mux.HandleFunc("/v1/responses/compact", s.handleGatewayPost)
	s.mux.HandleFunc("/v1/chat/completions", s.handleGatewayPost)
	// Local gateway identity API - returns virtual identity for client-side rewriting
	s.mux.HandleFunc("/v1/gateway/identity", s.handleGatewayIdentity)
	// Gateway download and install script
	s.mux.HandleFunc("/download/gateway", s.handleDownloadGateway)
	s.mux.HandleFunc("/install-gateway.sh", s.handleGatewayInstallScript)
	// One-shot Codex CLI auto-config: `curl <pool>/file/<api_key> | bash` writes
	// ~/.codex/config.toml pointing codex at this pool (requirement #3). Both the
	// bare and trailing-slash forms are registered (Go mux treats them distinctly).
	s.mux.HandleFunc("/file", s.handleCodexConfigScript)
	s.mux.HandleFunc("/file/", s.handleCodexConfigScript)
	// Native Anthropic relay (Claude Pro/Max OAuth or API key). Provider-aware
	// account selection + request/response virtualization live in messages.go.
	s.mux.HandleFunc("/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/v1/messages/count_tokens", s.handleMessages)
	// Shared Files/Skills endpoints exist in multiple official client ecosystems.
	// Dispatch them by provider headers/key hints instead of always treating them as
	// Claude passthrough.
	for _, p := range []string{"/v1/files", "/v1/files/", "/v1/skills", "/v1/skills/"} {
		s.mux.HandleFunc(p, s.handleSharedEndpoint)
	}
	// Claude-only extra surfaces for skills / code-execution beyond the message turn.
	for _, p := range []string{"/v1/agents", "/v1/agents/", "/v1/environments", "/v1/environments/", "/v1/sessions", "/v1/sessions/"} {
		s.mux.HandleFunc(p, s.handleAnthropicPassthrough)
	}
	// End-user portal authentication (multi-user). Cookie sessions; coexists with the
	// legacy admin_token. See userauth.go.
	s.mux.HandleFunc("/auth/register", s.handleAuthRegister)
	s.mux.HandleFunc("/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("/auth/me", s.handleAuthMe)
	s.mux.HandleFunc("/client/errors", s.handleClientError)

	// Automation-only account pool import. Requires a dedicated poolimp_ key and never
	// exposes listing/export/inference/admin surfaces.
	s.mux.HandleFunc("/api/account-pool/import", s.accountPoolImport)

	// Registration automation APIs
	s.mux.HandleFunc("/admin/register/providers/test", s.handleProviderTest)
	s.mux.HandleFunc("/admin/automation/policies", s.handleAutomationPolicies)
	s.mux.HandleFunc("/admin/automation/stats", s.handleAutomationStats)

	// End-user self-service (session-authenticated, owner-scoped). See userportal.go.
	s.mux.HandleFunc("/user/api-keys", s.handleUserKeys)
	s.mux.HandleFunc("/user/api-keys/", s.handleUserKeyAction)
	s.mux.HandleFunc("/user/usage", s.handleUserUsage)
	s.mux.HandleFunc("/user/usage/timeseries", s.handleUserUsageTimeseries)
	s.mux.HandleFunc("/user/profile", s.handleUserProfile)
	s.mux.HandleFunc("/admin/accounts", s.adminAccounts)
	s.mux.HandleFunc("/admin/accounts/summary", s.adminAccountsSummary)
	s.mux.HandleFunc("/admin/accounts/export", s.adminAccountsExport)
	s.mux.HandleFunc("/admin/accounts/import-auth-json", s.adminImportAuthJSON)
	s.mux.HandleFunc("/admin/accounts/import-token", s.adminImportToken)
	s.mux.HandleFunc("/admin/accounts/import-cookie", s.adminImportCookie)
	s.mux.HandleFunc("/admin/accounts/import-key", s.adminImportKey)
	s.mux.HandleFunc("/admin/accounts/import-kiro-json", s.adminImportKiroJSON)
	// Bulk-reassign accounts to a group (exact path; takes precedence over the
	// /admin/accounts/ subtree handler). Single reassign is /admin/accounts/<id>/group.
	s.mux.HandleFunc("/admin/accounts/assign-group", s.adminAccountsAssignGroup)
	// Web-login (paste-back) OAuth import for Codex + Claude (see oauth.go).
	s.mux.HandleFunc("/admin/oauth/start", s.adminOAuthStart)
	s.mux.HandleFunc("/admin/oauth/complete", s.adminOAuthComplete)
	s.mux.HandleFunc("/admin/accounts/", s.adminAccountAction)
	s.mux.HandleFunc("/admin/egress-profiles", s.adminEgressProfiles)
	s.mux.HandleFunc("/admin/egress-profiles/", s.adminEgressProfileAction)
	s.mux.HandleFunc("/admin/providers", s.adminProviders)

	// Lifecycle management API routes
	s.mux.HandleFunc("/admin/lifecycle/services", s.handleLifecycleServices)
	s.mux.HandleFunc("/admin/lifecycle/tasks", s.handleLifecycleTasks)
	s.mux.HandleFunc("/admin/lifecycle/tasks/", s.handleLifecycleTaskAction)

	s.mux.HandleFunc("/admin/providers/", s.adminProviderAction)
	s.mux.HandleFunc("/admin/moderation", s.adminModeration)
	s.mux.HandleFunc("/admin/moderation/translate", s.adminModerationTranslate)
	s.mux.HandleFunc("/admin/cf-events", s.adminCFEvents)
	s.mux.HandleFunc("/admin/usage", s.adminUsage)
	s.mux.HandleFunc("/admin/usage/window", s.adminUsageWindowEndpoint)
	s.mux.HandleFunc("/admin/model-quality", s.adminModelQuality)
	s.mux.HandleFunc("/admin/model-quality/run", s.adminModelQualityRun)
	s.mux.HandleFunc("/admin/model-quality/reset", s.adminModelQualityReset)
	s.mux.HandleFunc("/admin/usage/cache", s.adminUsageCache)
	s.mux.HandleFunc("/admin/usage/cache/reset", s.adminUsageCacheReset)
	s.mux.HandleFunc("/admin/usage/timeseries", s.adminUsageTimeseries)
	s.mux.HandleFunc("/admin/usage/by-model", s.adminUsageByModel)
	s.mux.HandleFunc("/admin/compat/skills", s.adminSkillsCompatDoctor)
	s.mux.HandleFunc("/admin/quota", s.adminQuota)
	// Host + registration-task resource metrics (CPU/mem/disk + node/Chrome/Xvfb RSS).
	s.mux.HandleFunc("/admin/system", s.adminSystem)
	s.mux.HandleFunc("/admin/settings", s.adminSettings)
	// /admin/config is the registry-backed System-config surface (requirement #1):
	// GET returns every runtime-editable field with its effective value + metadata;
	// PATCH validates and persists any subset, hot-applying upstream-consumed fields.
	s.mux.HandleFunc("/admin/config", s.adminConfig)
	s.mux.HandleFunc("/admin/egress-pools", s.adminEgressPools)
	s.mux.HandleFunc("/admin/egress-pools/", s.adminEgressPoolAction)
	s.mux.HandleFunc("/admin/export/logs", s.adminDiagnosticsExport)
	s.mux.HandleFunc("/admin/logs", s.adminLogRecords)
	s.mux.HandleFunc("/admin/audit", s.adminAudit)
	s.mux.HandleFunc("/admin/groups", s.adminGroups)
	s.mux.HandleFunc("/admin/groups/", s.adminGroupAction)
	s.mux.HandleFunc("/admin/model-instructions", s.adminModelInstructions)
	s.mux.HandleFunc("/admin/tenants", s.adminTenants)
	s.mux.HandleFunc("/admin/users", s.adminUsers)
	s.mux.HandleFunc("/admin/users/", s.adminUserAction)
	s.mux.HandleFunc("/admin/projects", s.adminProjects)
	s.mux.HandleFunc("/admin/api-keys", s.adminAPIKeys)
	s.mux.HandleFunc("/admin/api-keys/", s.adminAPIKeyAction)
	// Thinking (deep reasoning) configuration APIs
	s.mux.HandleFunc("/admin/thinking", s.handleThinkingConfig)
	s.mux.HandleFunc("/admin/thinking/preview", s.handlePreviewThinking)
	s.mux.HandleFunc("/admin/gopay", s.adminGopay)
	s.mux.HandleFunc("/admin/gopay/subscribe", s.adminGopaySubscribe)
	s.mux.HandleFunc("/admin/gopay/otp", s.adminGopayOTP)
	s.mux.HandleFunc("/admin/virtual-context/sweep", s.adminVirtualSweep)

	// Registration API routes
	s.mux.HandleFunc("/admin/register/batch", s.handleRegisterBatch)
	s.mux.HandleFunc("/admin/register/jobs", s.handleRegisterJobs)
	s.mux.HandleFunc("/admin/register/job/status", s.handleJobStatus)
	s.mux.HandleFunc("/admin/register/job/events", s.handleJobEvents)
	s.mux.HandleFunc("/admin/register/job/", s.handleJobAction)
	s.mux.HandleFunc("/admin/register/providers", s.handleProviderSettings)
	s.mux.HandleFunc("/admin/register/providers/options", func(w http.ResponseWriter, r *http.Request) {
		if s.regHandler != nil {
			s.regHandler.HandleProviderOptions(w, r)
		} else {
			writeError(w, http.StatusNotImplemented, errors.New("registration not initialized"))
		}
	})
	s.mux.HandleFunc("/admin/register/stats", s.handleRegisterStats)
	// Phone-country catalog (ISO/dial/中英文名) for the registration page's searchable
	// country Select. Static, embedded from phone_countries.json.
	s.mux.HandleFunc("/admin/register/countries", s.adminRegisterCountries)
	// Daily registration statistics by SMS provider + country (success-rate aggregation
	// accumulated locally from registration_records, for the stats-driven selection UI).
	s.mux.HandleFunc("/admin/register/stats/daily", s.adminRegisterStatsDaily)
	// Node registration engine credentials (hero-sms / mail / residential proxy), stored
	// as the node_registrar_config setting — configurable entirely from the web UI so an
	// operator never needs to SSH the VPS to edit the registrar's config files.
	s.mux.HandleFunc("/admin/register/node-config", s.adminNodeRegistrarConfig)
	// Readiness self-check: reports whether "deploy → auto-fill the pool" is actually
	// configured to run (refill policy, providers, pool deficit, blockers).
	s.mux.HandleFunc("/admin/register/readiness", s.handleRegisterReadiness)
	// Unified settings center (SettingsV2 page): aggregates config registry, registrar
	// credentials, automation policies, lifecycle defaults, logging & memory knobs.
	s.mux.HandleFunc("/admin/settings-center", s.handleSettingsCenter)
	s.mux.HandleFunc("/admin/settings-center/apply-template", s.handleSettingsCenterTemplate)
	s.mux.HandleFunc("/admin/export/cache-hits", s.adminCacheHitsExport)
	s.mux.HandleFunc("/admin/upstream-error-rules/test", s.adminUpstreamErrorRulesTest)
	s.mux.HandleFunc("/admin/upstream-error-rules/model-options", s.adminUpstreamErrorRuleModelOptions)
	s.mux.HandleFunc("/admin/upstream-error-rules", s.adminUpstreamErrorRules)
	s.mux.HandleFunc("/admin/upstream-error-rules/", s.adminUpstreamErrorRuleAction)

	// Embedded admin UI (catch-all; more specific API routes above take precedence).
	s.mux.Handle("/console/", console.Handler())
	// The new SPA console is now the primary UI: root redirects to it. The legacy
	// vanilla-JS UI stays reachable at /legacy/ as a fallback (not deleted).
	s.mux.Handle("/legacy/", http.StripPrefix("/legacy", web.Handler()))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]interface{}{"ok": true, "time": storage.Now()}
	if s.scheduler != nil {
		body["scheduler"] = s.scheduler.Metrics()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if isPoolImportKeyPlain(downstreamBearer(r)) {
		writeError(w, http.StatusForbidden, errors.New("pool import key cannot query models"))
		return
	}
	caps, err := s.store.ListCapabilities(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if hint, ok := s.modelsProviderHint(r); !ok {
		writeError(w, http.StatusBadRequest, errors.New("provider hint must be auto, codex, claude, or kiro"))
		return
	} else if hint != "auto" {
		accounts, listErr := s.store.ListAccounts(r.Context())
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, listErr)
			return
		}
		allowed := map[string]bool{}
		for _, account := range accounts {
			if strings.EqualFold(strings.TrimSpace(account.Provider), hint) {
				allowed[account.ID] = true
			}
		}
		filtered := caps[:0]
		for _, c := range caps {
			if allowed[c.AccountID] {
				filtered = append(filtered, c)
			}
		}
		caps = filtered
	}
	// Content-negotiate by client family: an Anthropic client (Claude Code) needs the
	// native Anthropic /v1/models schema or its model picker / "auto" selection breaks;
	// every other client keeps the OpenAI-shaped list. Detection keys off headers only
	// Anthropic clients send (anthropic-version / anthropic-beta / x-api-key).
	if isAnthropicClient(r) {
		body, etag, err := capability.BuildAnthropicModelsResponse(caps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)
		_, _ = w.Write(body)
		return
	}
	cfg := s.cfg
	if group, err := s.store.GetGroup(r.Context(), cfg.DefaultGroup); err == nil {
		cfg.Virtual2MEnabled = cfg.Virtual2MEnabled && group.Virtual2MEnabled
	}
	body, etag, err := capability.BuildModelsResponse(caps, cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(body)
}

func (s *Server) modelsProviderHint(r *http.Request) (string, bool) {
	if raw := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); raw != "" {
		return normalizeProviderHint(raw)
	}
	if plain := downstreamBearer(r); plain != "" {
		if key, found, _ := s.store.LookupAPIKey(r.Context(), hashAPIKey(plain)); found {
			hint := normalizeProviderHintLoose(key.ProviderHint)
			if hint == "auto" || hint == "codex" || hint == "claude" || hint == "kiro" {
				return hint, true
			}
			return "", false
		}
	}
	return "auto", true
}

func (s *Server) handleGatewayPost(w http.ResponseWriter, r *http.Request) {
	if isResponsesWebSocketUpgrade(r) {
		s.handleGatewayWebSocket(w, r)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	raw, err := readLimited(r.Body, s.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	r = r.WithContext(withSchedulerWait(r.Context(), w, isStreamRequest(raw), "openai"))
	// Authenticate the downstream api key (if any) and resolve its routing group +
	// forced model/effort policy. The forced model is applied to the body BEFORE
	// affinity/capability routing so the request lands on an account that has the
	// forced model and the upstream actually receives it.
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	// Carry the matched key/user identity in the context so usage is attributed to the
	// owning portal user (their console reads /user/usage).
	r = r.WithContext(withDownstreamKey(r.Context(), pol))
	// Gateway reliability model routing (opt-in): when the layer is on and neither the
	// key nor the group forced a model, apply the configured reliability model BEFORE
	// routing so account selection lands on an account that actually has it. Empty
	// config (the default) = never override the downstream's model.
	if pol.ForceModel == "" && s.reliabilityEnabled(r.Context()) {
		if m := strings.TrimSpace(s.reliabilityModel(r.Context())); m != "" {
			pol.ForceModel = m
		}
	}
	if pol.ForceModel != "" {
		raw = setForcedModel(raw, pol.ForceModel)
	}
	// Compliance: sanitize the conversation history (prior assistant turns) before
	// forwarding upstream. Zero-cost when moderation is off or no keyword matches; the
	// live streamed reply is never touched.
	if r.URL.Path == "/v1/chat/completions" {
		raw = s.moderateHistory(r.Context(), raw, "chat")
	} else {
		raw = s.moderateHistory(r.Context(), raw, "responses")
	}
	path := r.URL.Path
	isChat := path == "/v1/chat/completions"
	isCompact := strings.Contains(path, "/responses/compact")
	affinityGroup := pol.Group
	if affinityGroup == "" {
		affinityGroup = s.cfg.DefaultGroup
	}
	model := routing.Model(raw)

	// OpenAI-compatible requests targeting a Claude model are transparently
	// relayed to the Anthropic upstream (format-converted both ways) instead of
	// Codex; everything else continues down the Codex/Responses path.
	if isChat && isClaudeModel(model) {
		s.handleChatViaClaude(w, r, raw, model, pol)
		return
	}

	// A model served by a custom OpenAI-compatible provider (DeepSeek, …) is routed to
	// that provider's generic adapter, converting both the chat-completions entrypoint
	// (near-passthrough) and the Codex /v1/responses entrypoint (Responses ↔ chat).
	if prov, ok := s.customProviderForModel(r.Context(), model); ok {
		if isChat {
			s.handleChatViaCustom(w, r, raw, model, pol.Group, prov)
		} else {
			s.handleResponsesViaCustom(w, r, raw, model, pol.Group, prov)
		}
		return
	}
	if !isChat {
		raw = ensureEncryptedReasoningInclude(raw)
	}

	compiledModelInstructions := ""
	if group, err := s.store.GetGroup(r.Context(), affinityGroup); err == nil && group.ModelInstructionsEnabled {
		compiled, _, err := s.compileGroupModelInstructions(r.Context(), group)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		compiledModelInstructions = compiled
		if !isChat {
			raw = setResponsesInstructions(raw, compiledModelInstructions)
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	affinity := codexSelectionAffinity(r, raw, routing.ExtractAffinityKey(r, raw), affinityGroup)
	// movable: the request carries its full input and so can be re-sent to a fresh
	// account losslessly. This is the failover gate. It is broader than !strict: a
	// strict-sticky turn (tool_result/function_call_output, kept on one account for
	// prompt-cache warmth) is still movable, and MUST be allowed to fail over on an
	// error — pinning it was the cause of "429 leaks downstream, no auto-switch". Only
	// genuine server-side-state turns (previous_response_id / x-codex-turn-state) are
	// non-movable, because a fresh account cannot continue from state it never created.
	movable := !routing.HasServerSideState(path, r, raw)
	if !movable {
		if _, bindErr := s.store.GetAffinityBinding(r.Context(), affinity.Hash); bindErr != nil {
			if rebuilt, ok := s.journalReplayBody(r.Context(), raw); ok {
				raw = rebuilt
				movable = true
				w.Header().Set("X-MiCliProxy-Context-Status", "rebuilt")
				atomic.AddUint64(&s.contextRebuilt, 1)
			}
		}
	}
	if !movable {
		if affinity.Hash == "" {
			writePoolCodeError(w, http.StatusConflict, "state_binding_missing", "request depends on server-side state but no persisted session binding exists")
			return
		}
		if _, bindingErr := s.store.GetAffinityBinding(r.Context(), affinity.Hash); bindingErr != nil {
			if storage.NotFound(bindingErr) {
				raw = degradedResponsesReplay(raw)
				movable = true
				w.Header().Set("X-MiCliProxy-Context-Status", "degraded")
				atomic.AddUint64(&s.contextDegraded, 1)
				log.Printf("[CONTEXT-DEGRADED] request_id=%s previous response journal/binding unavailable", requestIDFromContext(r.Context()))
			} else {
				writeError(w, http.StatusInternalServerError, bindingErr)
			}
			if !movable {
				return
			}
		}
	}

	// Transparent seamless failover. A movable request (no per-account server-side
	// state) carries its full input, so if the chosen account is rate-limited /
	// region-blocked / auth-stale / banned we can move it to a fresh account
	// losslessly — the downstream sees a single successful response and never learns a
	// switch happened, and the new account derives ALL of its own session identifiers,
	// so it can never inherit the prior account's flagged session (the cross-account-401
	// "串号" leak). This INCLUDES strict-sticky turns (tool_result/function_call_output):
	// they stay pinned for cache warmth in steady state, but on an error they fail over
	// rather than leak it. Genuine stateful turns (previous_response_id /
	// x-codex-turn-state) are non-movable and surface the error instead, because a fresh
	// account has no such state to continue from.
	attempts := 1
	_, stateReplayAvailable := s.journalReplayBody(r.Context(), raw)
	if s.flagEnabled(r.Context(), "seamless_failover", s.cfg.SeamlessFailover) && (movable || stateReplayAvailable) {
		if attempts = s.settingInt(r.Context(), "failover_max_attempts", s.cfg.FailoverMaxAttempts); attempts < 1 {
			attempts = 1
		}
	}
	// Accounts this request has already tried and failed on. Passed to the scheduler
	// each attempt so a just-failed account is never re-selected within the same
	// request — not even via its sticky binding (the conversation rebinds to the
	// fresh account instead). codexAttempt adds the leased account here before it
	// returns outcomeRetry.
	exclude := map[string]bool{}
	current := codexRetryRequest{Raw: raw, Header: r.Header.Clone()}
	for attempt := 0; attempt < attempts; attempt++ {
		headerReq := r.Clone(r.Context())
		headerReq.Header = current.Header
		currentStrict := routing.IsStrictSticky(path, headerReq, current.Raw)
		currentMovable := !routing.HasServerSideState(path, headerReq, current.Raw)
		currentModel := routing.Model(current.Raw)
		if currentModel == "" {
			currentModel = model
		}
		currentAffinity := codexSelectionAffinity(headerReq, current.Raw, routing.ExtractAffinityKey(headerReq, current.Raw), affinityGroup)
		if currentAffinity.Hash == "" {
			currentAffinity = affinity
		}
		result := s.codexAttempt(w, r, current.Raw, current.Header, current.Prepared, isChat && !current.Prepared, isCompact, currentAffinity, currentStrict, currentMovable, currentModel, pol.Group, pol.ForceEffort, compiledModelInstructions, attempt < attempts-1, exclude)
		if result.Outcome != outcomeRetry {
			return
		}
		if len(result.Retry.Raw) > 0 {
			current = result.Retry
		}
	}
	writePoolCodeError(w, http.StatusBadGateway, "retry_exhausted", "upstream retry limit exhausted")
}

type attemptOutcome int

const (
	outcomeDone  attemptOutcome = iota // request finished (success, or terminal error already written)
	outcomeRetry                       // recoverable error on a self-contained request — retry on a fresh account
)

type codexRetryRequest struct {
	Raw      []byte
	Header   http.Header
	Prepared bool
}

type codexAttemptResult struct {
	Outcome attemptOutcome
	Retry   codexRetryRequest
}

// shouldInjectCodexHostedWebSearch keeps hosted search for classic Responses and
// API-key upstreams. Only a request already carrying the official Lite input
// envelope must skip it, because Lite rejects hosted web_search tools.
func shouldInjectCodexHostedWebSearch(model string, token storage.AccountToken, body []byte) bool {
	return upstream.AccountUsesAPIKey(token) || !capability.CodexUsesResponsesLite(model) || !upstream.CodexRequestUsesResponsesLite(body)
}

// codexAttempt serves one account's attempt at a Codex/Responses request. It
// returns outcomeRetry when the caller should transparently retry on a fresh
// account (a recoverable error on a self-contained, non-strict request) and
// outcomeDone otherwise. Every per-account value — the virtual identity, the
// upstream headers, the namespaced conversation correlators, the prompt-cache key
// — is derived fresh inside this call, so a retry on account B can never reuse
// account A's session identifiers.
//
// exclude carries the accounts this request already failed on; the leased account is
// added to it before any outcomeRetry so the next attempt selects a different one.
func (s *Server) codexAttempt(w http.ResponseWriter, r *http.Request, raw []byte, baseHeader http.Header, prepared bool, isChat, isCompact bool, affinity routing.AffinityKey, strict, movable bool, model, routeGroup, forceEffort, compiledModelInstructions string, allowRetry bool, exclude map[string]bool) codexAttemptResult {
	path := r.URL.Path
	includeChatStreamUsage := isChat && chatStreamUsageRequested(raw)
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:             routeGroup,
		Provider:          "codex",
		Affinity:          affinity,
		Strict:            strict,
		ServerSideState:   !movable,
		ImmutableAffinity: !movable,
		Movable:           movable,
		Model:             model,
		EstimatedTokens:   virtual.EstimateTokensJSON(raw),
		Compaction:        routing.IsCompaction(path, raw),
		Exclude:           exclude,
		OnWait:            schedulerWaitCallback(r.Context()),
	})
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this session is unavailable")
			return codexAttemptResult{Outcome: outcomeDone}
		}
		if schedulerWaitTerminal(r.Context(), "The model is temporarily unavailable. Please retry shortly.") {
			return codexAttemptResult{Outcome: outcomeDone}
		}
		status, _ := noAccountHTTPStatus(err)
		s.writePublicNoAccountError(r.Context(), w, status, routeGroup, "", model, err)
		return codexAttemptResult{Outcome: outcomeDone}
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
	// retry marks the leased account as failed-for-this-request (so the next attempt
	// excludes it) and signals the caller to fail over to a fresh account.
	retry := func() codexAttemptResult {
		if exclude != nil {
			exclude[lease.Account.ID] = true
		}
		return codexAttemptResult{Outcome: outcomeRetry}
	}
	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	// Start with the raw body. Every downstream transform (system-prompt inject,
	// chat→responses conversion, Virtualize) returns its own freshly-allocated
	// slice via json.Marshal, so the initial clone is unnecessary — it would
	// only be overwritten. The rare unmarshal-failure fallback path returns the
	// original slice, which is read-only from this point on.
	body := raw
	promptCacheKeySource := "none"
	if routing.PromptCacheKey(body) != "" {
		promptCacheKeySource = "downstream"
	}
	retentionEffective := routing.JSONStringField(body, "prompt_cache_retention")
	retentionSource := "unsupported_current_codex"
	if retentionEffective != "" {
		retentionSource = "downstream_unsupported"
	}
	group, err := s.store.GetGroup(r.Context(), lease.Account.GroupName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	if !prepared {
		if compiledModelInstructions == "" && prompt.ShouldRewrite(group.SystemPrompt, isCompact, group.SystemPromptApplyToCompaction) {
			if isChat {
				body, _, err = prompt.InjectChatSystemPrompt(body, group.SystemPrompt)
			} else {
				body, _, err = prompt.InjectResponsesSystemPrompt(body, group.SystemPrompt)
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
		}
		if isChat {
			body, err = prompt.ChatCompletionToResponses(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if !isStreamRequest(raw) {
				body = forceResponsesStream(body)
			}
			path = "/v1/responses"
		}
		if compiledModelInstructions != "" {
			body = setResponsesInstructions(body, compiledModelInstructions)
		}
	}

	// Gateway reliability layer (opt-in, default off): inject the developer rules +
	// per-turn <gateway_request> envelope, classify task/risk, accumulate
	// working_state, and derive the risk-based reasoning-effort floor. Skipped for
	// compaction turns (a summarization pass should not carry the envelope). relTurn
	// stays inactive when the flag is off, so the response guard below is a no-op too.
	var relTurn reliabilityTurn
	effectiveEffort := forceEffort
	if !prepared && !isCompact && s.reliabilityEnabled(r.Context()) {
		body, relTurn = s.applyReliabilityRequest(r.Context(), body, affinity)
		if s.reliabilityEffortFloorEnabled(r.Context()) {
			// Floor RAISES effort to the risk minimum but never lowers a stronger
			// operator-forced effort — a high-risk task can't be downgraded downstream.
			effectiveEffort = reliability.MaxEffort(forceEffort, relTurn.EffortFloor)
		}
	}

	// Per-key/group forced reasoning effort, combined with the reliability risk floor
	// above: applied to the responses-shaped body (after any chat→responses
	// conversion) so it covers both entry paths.
	if !prepared && effectiveEffort != "" {
		body = applyForcedReasoningResponses(body, effectiveEffort)
	}

	// 联网搜索 (AT path): ensure a web_search tool is present so the account
	// session serves it without API-key/org verification. Skipped for compact.
	if !prepared && !isCompact && shouldInjectCodexHostedWebSearch(model, token, body) &&
		s.flagEnabled(r.Context(), "web_search_enabled", s.cfg.WebSearchEnabled) {
		if injected, werr := prompt.EnsureResponsesWebSearchTool(body, s.cfg.WebSearchToolType); werr == nil {
			body = injected
		}
	}

	// Current codex-rs (0.144.x) has no prompt_cache_retention field on HTTP or
	// Responses-over-WebSocket. The upstream choke point strips any legacy value;
	// do not inject one here or report an unsupported 24h policy as effective. Cache
	// reuse is driven by the supported prompt_cache_key + stable account affinity.
	if !prepared {
		if updated, normalized := normalizeOfficialCodexPromptCacheKey(r, body, model); normalized {
			body = updated
			promptCacheKeySource = "official_codex_stable_prefix"
		}
	}
	if !prepared && routing.PromptCacheKey(body) == "" {
		if prefixHash := automaticPromptCachePrefixHash(body); prefixHash != "" {
			updated := ensureResponsesPromptCacheKey(body, automaticPromptCacheKey(model, prefixHash))
			if routing.PromptCacheKey(updated) != "" {
				body = updated
				promptCacheKeySource = "auto_stable_prefix"
			}
		}
	}
	logicalUsageDiag := codexRequestUsageDiagnostics(body, affinity, promptCacheKeySource, retentionEffective, retentionSource)

	// Virtual2M is intentionally disabled: the gateway no longer records/replays a
	// virtual context ledger and never trims Responses input. Official Codex
	// compaction is forwarded unchanged to the upstream.
	tryRebuildStateful := func(reason string) (codexAttemptResult, bool) {
		if movable {
			return codexAttemptResult{}, false
		}
		rebuilt, ok := s.journalReplayBody(r.Context(), raw)
		if !ok {
			return codexAttemptResult{}, false
		}
		exclude[lease.Account.ID] = true
		w.Header().Set("X-MiCliProxy-Context-Status", "rebuilt")
		atomic.AddUint64(&s.contextRebuilt, 1)
		log.Printf("[CONTEXT-REBUILT] request_id=%s account=%s reason=%s", requestIDFromContext(r.Context()), lease.Account.ID, reason)
		return codexAttemptResult{Outcome: outcomeRetry, Retry: codexRetryRequest{Raw: rebuilt, Header: baseHeader.Clone()}}, true
	}

	// Per-account conversation isolation ("串号隔离", default on, runtime-toggleable):
	// namespace the forwarded conversation-correlation identifiers so a rate-limited
	// / risk-flagged session on one account can never contaminate another account
	// that later serves the same conversation. Also resolves the (optionally
	// OS-hinted) identity used to build the upstream headers.
	osHint := s.osHint(raw, lease.Egress)
	id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
	hdr := baseHeader.Clone()
	if s.isolationEnabled(r.Context()) {
		body = isolateCodexConversation(hdr, body, id)
	}

	// Optional Codex sensitive-word scrub (off by default → raw fast path). Per
	// project policy the Codex request's working directory and paths are never
	// rewritten; only operator sensitive words are replaced, in the request body
	// and — via the same matcher — the response stream. An empty matcher (scrub
	// off / no words) is a zero-cost pass-through.
	codexScrubber := streamrewrite.New(nil)
	if s.flagEnabled(r.Context(), "codex_identity_scrub", s.cfg.CodexIdentityScrub) {
		scrub := cloak.ScrubSensitive(body, s.cfg.SensitiveWordsFor("codex"))
		body = scrub.Body
		codexScrubber = scrub.Scrubber
	}

	codexClientVersion := s.codexClientVersionForModel(model)
	codexUseWebSocket := forceCodexResponsesWebSocket(r.Context()) || s.codexResponsesWebSocketForModel(model, isChat, isCompact, body)
	// A sidecar-bound account presents the real Codex JA3 only on the HTTP/SSE path
	// (postViaSidecar replays it); the WS dialer cannot, so it would dial with Go-stdlib
	// TLS and throw away the fingerprint the sidecar binding exists for. Prefer the SSE
	// path for those accounts (the version-gated models still work over SSE). A forced
	// WS request (the explicit responses-WS upgrade) is honored regardless.
	if codexUseWebSocket && s.flagEnabled(r.Context(), "codex_prefer_sidecar_ja3_over_ws", s.cfg.CodexPreferSidecarJA3OverWS) && !forceCodexResponsesWebSocket(r.Context()) &&
		strings.EqualFold(strings.TrimSpace(lease.Egress.Type), "curl_cffi_sidecar") {
		codexUseWebSocket = false
	}
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:                  http.MethodPost,
			DownstreamPath:          pathWithQuery(path, r.URL.RawQuery),
			Headers:                 hdr,
			Body:                    body,
			Account:                 lease.Account,
			Token:                   t,
			Egress:                  lease.Egress,
			CookieJarKey:            lease.Binding.CookieJarKey,
			OSHint:                  osHint,
			CodexClientVersion:      codexClientVersion,
			CodexResponsesWebSocket: codexUseWebSocket,
			CodexWebSocketSession:   codexResponsesWebSocketSession(r.Context()),
		}
	}

	holdID := s.createBillingHold(affinity.Hash, lease.Account.ID, virtual.EstimateTokensJSON(body))
	// Backstop: settle-if-held on return so a cancelled/streaming disconnect can't leak
	// the hold; an explicit settle below always wins (WHERE status='held' no longer matches).
	defer func() { _ = s.settleBillingHoldIfHeld(r.Context(), holdID, "abandoned") }()
	// Session 33: Carry the billing hold id in the request context so deferred
	// usage recording (recordUsage / recordParsedUsage) can fall back to
	// estimated_tokens when the response body lacks countable usage data.
	withCodexUsageContext := func() {
		r = r.WithContext(withUsageDiagnostics(withBillingHold(r.Context(), holdID), codexRetentionDiagnosticsForTransport(logicalUsageDiag, codexUseWebSocket)))
	}
	withCodexUsageContext()
	resp, finalEgress, err := s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		if allowRetry && movable {
			return retry() // transport error — a fresh account/egress may succeed
		}
		if rebuilt, ok := tryRebuildStateful("transport_error"); ok {
			return rebuilt
		}
		writeError(w, http.StatusBadGateway, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	codexResetRetried := false

	if resp.StatusCode >= 400 {
		errorBody := readUpstreamErrorBody(resp.Body)
		detection := cf.Detect(resp.StatusCode, resp.Header, errorBody)
		v := ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
		if v.State == ban.AuthExpired && !cf.EdgeOnly(detection) {
			if refreshed, rerr := s.refreshCodexToken(r.Context(), token); rerr == nil && refreshed.Refreshed {
				token = refreshed.Token
				_ = resp.Body.Close()
				resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "failed_after_refresh")
					if allowRetry && movable {
						return retry()
					}
					if rebuilt, ok := tryRebuildStateful("transport_after_refresh"); ok {
						return rebuilt
					}
					writeError(w, http.StatusBadGateway, err)
					return codexAttemptResult{Outcome: outcomeDone}
				}
				if resp.StatusCode < 400 {
					goto codexSuccess
				}
				errorBody = readUpstreamErrorBody(resp.Body)
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			} else if rerr != nil {
				log.Printf("codex auth refresh %s: %v", lease.Account.ID, rerr)
				s.handleCodexRefreshFailure(r.Context(), lease.Account, refreshed, rerr, "gateway")
			}
		}
		if codexUseWebSocket && v.State == ban.PermissionDenied && !forceCodexResponsesWebSocket(r.Context()) {
			originalStatus := resp.StatusCode
			originalHeader := resp.Header.Clone()
			originalBody := append([]byte(nil), errorBody...)
			_ = resp.Body.Close()
			fallbackReq := requestForToken(token)
			fallbackReq.CodexResponsesWebSocket = false
			resp, finalEgress, err = s.doWithCFRetry(r.Context(), fallbackReq, lease, strict)
			if err != nil {
				log.Printf("codex websocket permission fallback %s: %v", lease.Account.ID, err)
				resp = &upstream.Response{
					StatusCode: originalStatus,
					Header:     originalHeader,
					Body:       io.NopCloser(bytes.NewReader(originalBody)),
				}
				errorBody = originalBody
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			} else if resp.StatusCode < 400 {
				codexUseWebSocket = false
				withCodexUsageContext()
				goto codexSuccess
			} else {
				errorBody = readUpstreamErrorBody(resp.Body)
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			}
		}
		if codexClientVersion == "" && codexRequiresNewerVersion(errorBody) {
			codexClientVersion = s.cfg.ClientVersion
			if !isChat && !isCompact && isStreamRequest(body) {
				codexUseWebSocket = true
			}
			withCodexUsageContext()
			_ = resp.Body.Close()
			resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
			if err != nil {
				_ = s.settleBillingHold(r.Context(), holdID, "failed_after_version_retry")
				if allowRetry && movable {
					return retry()
				}
				if rebuilt, ok := tryRebuildStateful("transport_after_version_retry"); ok {
					return rebuilt
				}
				writeError(w, http.StatusBadGateway, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if resp.StatusCode < 400 {
				goto codexSuccess
			}
			errorBody = readUpstreamErrorBody(resp.Body)
			detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
			v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
		}
		if !codexResetRetried && codexResetTriggerAllowed(resp.StatusCode, errorBody) &&
			s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, resp.StatusCode, resp.Header, errorBody, "http_error") {
			codexResetRetried = true
			if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
				token = latest
			}
			_ = resp.Body.Close()
			resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
			if err != nil {
				log.Printf("codex reset-credit retry %s: %v", lease.Account.ID, err)
			} else if resp.StatusCode < 400 {
				goto codexSuccess
			} else {
				errorBody = readUpstreamErrorBody(resp.Body)
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			}
		}
		if cf.Recordable(detection) {
			s.handleCFEvent(r.Context(), lease.Account, finalEgress, resp.StatusCode, detection)
		}
		entrypoint := "responses"
		if isChat {
			entrypoint = "chat_completions"
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider:   "codex",
			Entrypoint: entrypoint,
			Model:      model,
			Status:     resp.StatusCode,
			Header:     resp.Header,
			Body:       errorBody,
			Streaming:  isStreamRequest(raw),
		})
		if cf.EdgeOnly(detection) {
			_ = s.store.BenchBindingForRecheck(r.Context(), lease.Account.ID, storage.Now()+60)
			v = ban.Verdict{State: ban.RegionBlocked, Reason: "cloudflare_edge"}
		} else if ruleMatched {
			v = s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody, decision)
		} else {
			v = s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
		}
		_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
		if ruleMatched {
			switch decision.Match.DownstreamAction {
			case upstreamrules.DownstreamActionFailover:
				if allowRetry && movable {
					return retry()
				}
			case upstreamrules.DownstreamActionIdleStream:
				if isStreamRequest(raw) {
					if resp.Body != nil {
						_ = resp.Body.Close()
						resp.Body = nil
					}
					releaseLease()
				}
				if s.writeRuleDownstream(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber, decision, isStreamRequest(raw)) {
					return codexAttemptResult{Outcome: outcomeDone}
				}
			case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize:
				if s.writeRuleDownstream(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber, decision, isStreamRequest(raw)) {
					return codexAttemptResult{Outcome: outcomeDone}
				}
			}
		}
		canFailover := allowRetry && movable
		if canFailover && retryableForFailover(v, resp.StatusCode) {
			if v.State == ban.PermissionDenied && !s.hasCodexFailoverCandidate(r.Context(), routeGroup, lease.Account.ID) {
				s.writeFilteredError(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			// Recoverable error → move to a fresh account. The downstream never sees this.
			return retry()
		}
		if allowRetry && !movable && retryableForFailover(v, resp.StatusCode) {
			if rebuilt, ok := tryRebuildStateful(fmt.Sprintf("http_%d", resp.StatusCode)); ok {
				return rebuilt
			}
		}
		s.writeFilteredError(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber)
		return codexAttemptResult{Outcome: outcomeDone}
	}
codexSuccess:
	normalizeCodexStreamContentType(resp.Header, isStreamRequest(body))
	resetHeaderExhaustion := false
	if !codexResetRetried && exhaustedCooldown(resp.Header, storage.Now()) > 0 {
		resetHeaderExhaustion = s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, resp.StatusCode, resp.Header, nil, "success_header_exhaustion")
	}
	if !resetHeaderExhaustion {
		s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
	}
	s.captureQuota(r.Context(), lease.Account.ID, "codex", model, resp.Header)

	// Diagnostic logging for usage tracking
	log.Printf("[CODEX-PATH] account=%s, egress=%s, path=%s, isChat=%v, reqStream=%v, respStream=%v, status=%d, ct=%q",
		lease.Account.ID, finalEgress.Type, r.URL.Path, isChat, isStreamRequest(raw),
		isEventStream(resp.Header), resp.StatusCode, resp.Header.Get("Content-Type"))

	if isChat && !isStreamRequest(raw) {
		responseBody, err := s.readUpstreamResponseBody(resp.Body)
		if err != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "failed_response_too_large")
			writeError(w, http.StatusBadGateway, err)
			return codexAttemptResult{Outcome: outcomeDone}
		}
		// Session 33: Detect rate-limit/quota-exhausted signals in a 200 response
		// body (ChatGPT backend-api may return usage_limit_exceeded with HTTP 200
		// instead of 429). When detected, cool the account and fail over to a fresh
		// one so the downstream never sees the error.
		if cd := usageLimitCooldown(200, responseBody); cd > 0 {
			if !codexResetRetried && codexResetTriggerAllowed(200, responseBody) &&
				s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, 200, resp.Header, responseBody, "soft_200_body") {
				codexResetRetried = true
				if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
					token = latest
				}
				_ = resp.Body.Close()
				retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
				if retryErr == nil && retryResp.StatusCode < 400 {
					resp = retryResp
					finalEgress = retryEgress
					goto codexSuccess
				}
				if retryResp != nil && retryResp.Body != nil {
					_ = retryResp.Body.Close()
				}
				if retryErr != nil {
					log.Printf("codex soft-200 reset-credit retry %s: %v", lease.Account.ID, retryErr)
				}
			}
			s.benchOnLimit(r.Context(), lease.Account.ID, 200, resp.Header, responseBody)
			_ = s.settleBillingHold(r.Context(), holdID, "rate_limited_in_200_body")
			// Rate limit in 200 body — treat like a 429 for failover purposes.
			if allowRetry && movable {
				return retry()
			}
			if rebuilt, ok := tryRebuildStateful("soft_200_rate_limit"); ok {
				return rebuilt
			}
			if s.leakScrubEnabled(r.Context()) {
				if nb, changed := leakfilter.NeutralizeResponsesJSON(responseBody); changed {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write(nb)
					return codexAttemptResult{Outcome: outcomeDone}
				}
			}
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("account rate limited"))
			return codexAttemptResult{Outcome: outcomeDone}
		}
		// Reliability repair (opt-in, default off): if the answer fabricated a
		// tool/test/command result, re-ask ONCE on the same account with the repair
		// instruction appended, then bill + serve the repaired answer (so usage reflects
		// the final response). A failed re-ask falls through to the deterministic
		// downgrade below.
		if relTurn.Active && s.reliabilityRepairEnabled(r.Context()) {
			if findings := s.reliabilityFindings(r.Context(), responseBody, relTurn); len(findings) > 0 {
				repairReq := requestForToken(token)
				repairReq.Body = appendDeveloperTurn(body, s.reliabilityEnvelopeRole(r.Context()), reliability.RepairInstruction(findings))
				if r2, _, e2 := s.doWithCFRetry(r.Context(), repairReq, lease, strict); e2 == nil && r2 != nil {
					if r2.StatusCode < 400 {
						if b2, e := s.readUpstreamResponseBody(r2.Body); e == nil && len(b2) > 0 {
							responseBody = b2
						} else if e != nil {
							log.Printf("codex reliability repair response rejected: %v", e)
						}
					}
					_ = r2.Body.Close()
				}
			}
		}
		if responseJSON := codexSSEToResponseJSON(responseBody); len(responseJSON) > 0 {
			responseBody = responseJSON
		}
		s.persistCodexStateBindings(r.Context(), affinity, body, responseBody, lease, finalEgress, model)
		s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
		_ = s.settleBillingHold(r.Context(), holdID, "settled")
		// A soft 200 "failed" response carrying limit/quota/switch-model state must
		// not reach the downstream (envelope-only check; never inspects content).
		if s.leakScrubEnabled(r.Context()) {
			if nb, changed := leakfilter.NeutralizeResponsesJSON(responseBody); changed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(nb)
				return codexAttemptResult{Outcome: outcomeDone}
			}
		}
		chatBody, err := prompt.ResponsesToChatCompletion(responseBody, resp.Header.Get("x-request-id"), model)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return codexAttemptResult{Outcome: outcomeDone}
		}
		// Output guard: deterministically downgrade a response that claims unverified
		// test/command/file results (no-op when reliability/guard is off).
		chatBody = s.reliabilityGuardChatBody(r.Context(), responseBody, chatBody, relTurn)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(codexScrubber.ReplaceAll(chatBody))
		return codexAttemptResult{Outcome: outcomeDone}
	}

	if isEventStream(resp.Header) {
		// Hold back only a bounded early prefix so a retryable failure frame can still
		// fail over before any downstream bytes are committed. As soon as real content
		// appears (or the 64KiB/8-frame probe budget is reached), stream live. The old
		// full-response capture destroyed TTFT and manufactured 503s once a successful
		// stream exceeded its memory/disk capture cap. This probe runs before both the
		// native Responses and Chat Completions adapters. Downstream-visible safety
		// progress also releases the probe so a long safety check can be kept alive;
		// once either adapter writes HTTP 200, transparent failover is no longer possible.
		prefix, streamFailure, retryableStream, probeErr := probeEarlyCodexSSEFailure(resp.Body)
		if probeErr != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "stream_probe_failed")
			if allowRetry && movable {
				return retry()
			}
			if rebuilt, ok := tryRebuildStateful("stream_capture_error"); ok {
				return rebuilt
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return codexAttemptResult{Outcome: outcomeDone}
		}
		if retryableStream {
			failureHeader := resp.Header.Clone()
			for name, values := range streamFailure.Header {
				failureHeader.Del(name)
				for _, value := range values {
					failureHeader.Add(name, value)
				}
			}
			failureStatus := streamFailure.StatusCode
			failureBody := streamFailure.Body
			entrypoint := "responses"
			if isChat {
				entrypoint = "chat_completions"
			}
			decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
				Provider:   "codex",
				Entrypoint: entrypoint,
				Model:      model,
				Status:     failureStatus,
				Header:     failureHeader,
				Body:       failureBody,
				Streaming:  true,
			})
			shouldRetry := !ruleMatched || decision.Match.DownstreamAction == upstreamrules.DownstreamActionBuiltin || decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover

			// A reset credit is an in-place recovery on the same account and therefore
			// precedes cooldown/failover. Explicit pass/custom/neutralize/idle rules do
			// not consume credits behind the operator's back.
			if shouldRetry {
				// The WebSocket-to-SSE pipe is unbuffered. Closing before another request
				// prevents its terminal [DONE] write from retaining the WS request mutex.
				_ = resp.Body.Close()
			}
			if shouldRetry && !codexResetRetried && codexResetTriggerAllowed(failureStatus, failureBody) &&
				s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, failureStatus, failureHeader, failureBody, "stream_retryable_limit") {
				codexResetRetried = true
				if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
					token = latest
				}
				retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
				if retryErr == nil && retryResp.StatusCode < 400 {
					resp = retryResp
					finalEgress = retryEgress
					goto codexSuccess
				}
				if retryResp != nil && retryResp.Body != nil {
					_ = retryResp.Body.Close()
				}
				if retryErr != nil {
					log.Printf("codex stream reset-credit retry %s: %v", lease.Account.ID, retryErr)
				}
			}
			if ruleMatched {
				s.applyRuleAccountAction(r.Context(), lease.Account, failureStatus, failureHeader, failureBody, decision)
			} else {
				s.onUpstreamError(r.Context(), lease.Account, failureStatus, failureHeader, failureBody)
			}
			_ = s.settleBillingHold(r.Context(), holdID, "stream_retryable_error_retry")
			if ruleMatched {
				switch decision.Match.DownstreamAction {
				case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize, upstreamrules.DownstreamActionIdleStream:
					_ = resp.Body.Close()
					if s.writeRuleDownstream(r.Context(), w, "codex", failureStatus, failureHeader, failureBody, codexScrubber, decision, true) {
						return codexAttemptResult{Outcome: outcomeDone}
					}
				case upstreamrules.DownstreamActionFailover:
					_ = resp.Body.Close()
					if allowRetry && movable {
						return retry()
					}
					s.writeFilteredError(r.Context(), w, "codex", failureStatus, failureHeader, failureBody, codexScrubber)
					return codexAttemptResult{Outcome: outcomeDone}
				}
			}
			_ = resp.Body.Close()
			if allowRetry && movable {
				return retry()
			}
			if rebuilt, ok := tryRebuildStateful("stream_response_failed"); ok {
				return rebuilt
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return codexAttemptResult{Outcome: outcomeDone}
		}
		streamBody := io.MultiReader(bytes.NewReader(prefix), resp.Body)
		if isChat {
			// Third-party OpenAI client streaming against a GPT model: convert the
			// upstream Responses SSE into chat.completion.chunk frames it can parse
			// (incl. streamed tool calls), teeing through a usage scanner so streamed
			// chat traffic records tokens like the native path.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			uscan := usage.NewStreamScanner("codex")
			responsesStreamToChatSSE(w, io.TeeReader(streamBody, uscan), model, includeChatStreamUsage, codexScrubber)
			if parsed, ok := uscan.Parsed(); ok {
				s.recordParsedUsage(r.Context(), lease.Account.ID, affinity.Hash, parsed)
			}
			_ = s.settleBillingHold(r.Context(), holdID, "settled_streaming")
			return codexAttemptResult{Outcome: outcomeDone}
		}
		streamRecorder := newCodexStreamLedgerRecorder()
		recordingStream := io.TeeReader(streamBody, streamRecorder)
		s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		streamCtx := r.Context()
		if rf := s.responseRuleFilter(streamCtx, "codex", func() string {
			if isChat {
				return "chat_completions"
			}
			return "responses"
		}(), model, resp.StatusCode); rf != nil {
			if rf.Rule != nil && rf.Rule.FilterAccountAction {
				_ = s.applyRuleAccountAction(streamCtx, lease.Account, resp.StatusCode, resp.Header, nil, upstreamErrorRuleDecision{Rule: *rf.Rule, Match: upstreamrules.MatchResult{Rule: *rf.Rule, AccountAction: rf.Rule.AccountAction, DownstreamAction: rf.Rule.DownstreamAction}})
			}
			streamCtx = withResponseRuleFilter(streamCtx, rf)
		}
		commitWriter := newTerminalCommitWriter(w, func() error {
			completed := streamRecorder.ResponseJSON()
			if len(completed) == 0 {
				log.Printf("[CONTEXT-JOURNAL] recorder empty request_id=%s response_id=%s", requestIDFromContext(r.Context()), streamRecorder.id)
				return errors.New("completed response missing from context journal recorder")
			}
			err := s.persistContextJournal(r.Context(), body, completed, affinity.Hash, lease.Account.ID)
			if err != nil {
				log.Printf("[CONTEXT-JOURNAL] terminal commit failed request_id=%s: %v", requestIDFromContext(r.Context()), err)
			}
			return err
		})
		streamErr := s.streamSSE(streamCtx, commitWriter, recordingStream, codexScrubber, "codex", lease.Account.ID, affinity.Hash)
		if closeErr := commitWriter.Close(); streamErr == nil {
			streamErr = closeErr
		}
		s.persistCodexBindingAliases(r.Context(), affinity, streamRecorder.id, streamRecorder.model, lease, finalEgress, model)
		if streamErr != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "stream_interrupted_compensated")
		} else {
			_ = s.settleBillingHold(r.Context(), holdID, "settled_streaming")
		}
		return codexAttemptResult{Outcome: outcomeDone}
	}
	responseBody, err := s.readUpstreamResponseBody(resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	// Session 33: Success-path body scan for rate-limit signals in 200 response.
	// Same logic as the isChat non-streaming path above: when the upstream returns
	// a soft limit error in a 200 body, cool the account and fail over.
	if cd := usageLimitCooldown(200, responseBody); cd > 0 {
		if !codexResetRetried && codexResetTriggerAllowed(200, responseBody) &&
			s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, 200, resp.Header, responseBody, "soft_200_body") {
			codexResetRetried = true
			if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
				token = latest
			}
			_ = resp.Body.Close()
			retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, strict)
			if retryErr == nil && retryResp.StatusCode < 400 {
				resp = retryResp
				finalEgress = retryEgress
				goto codexSuccess
			}
			if retryResp != nil && retryResp.Body != nil {
				_ = retryResp.Body.Close()
			}
			if retryErr != nil {
				log.Printf("codex raw soft-200 reset-credit retry %s: %v", lease.Account.ID, retryErr)
			}
		}
		s.benchOnLimit(r.Context(), lease.Account.ID, 200, resp.Header, responseBody)
		_ = s.settleBillingHold(r.Context(), holdID, "rate_limited_in_200_body")
		// Honor failover here too: a soft rate-limit in a 200 body is the same condition
		// as a 429, so a movable request should fail over rather than surface it.
		if allowRetry && movable {
			return retry()
		}
		if rebuilt, ok := tryRebuildStateful("soft_200_rate_limit"); ok {
			return rebuilt
		}
		if s.leakScrubEnabled(r.Context()) {
			if nb, changed := leakfilter.NeutralizeResponsesJSON(responseBody); changed {
				s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(nb)
				return codexAttemptResult{Outcome: outcomeDone}
			}
		}
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("account rate limited"))
		return codexAttemptResult{Outcome: outcomeDone}
	}
	// Non-stream raw responses: scrub a soft 200 limit/quota failure before forwarding.
	if s.leakScrubEnabled(r.Context()) {
		if nb, changed := leakfilter.NeutralizeResponsesJSON(responseBody); changed {
			s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(nb)
			return codexAttemptResult{Outcome: outcomeDone}
		}
	}
	s.persistCodexStateBindings(r.Context(), affinity, body, responseBody, lease, finalEgress, model)
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
	_ = s.settleBillingHold(r.Context(), holdID, "settled")
	s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Output guard: deterministically downgrade a non-streaming Responses answer that
	// claims unverified test/command/file results (no-op when reliability/guard is off).
	responseBody = s.reliabilityGuardResponsesBody(r.Context(), responseBody, relTurn)
	if rf := s.responseRuleFilter(r.Context(), "codex", func() string {
		if isChat {
			return "chat_completions"
		}
		return "responses"
	}(), model, resp.StatusCode); rf != nil {
		responseBody = filterRuleJSON(responseBody, rf)
	}
	_, _ = w.Write(codexScrubber.ReplaceAll(responseBody))
	return codexAttemptResult{Outcome: outcomeDone}
}

func (s *Server) persistCodexStateBindings(ctx context.Context, affinity routing.AffinityKey, requestBody, responseBody []byte, lease scheduler.Lease, egress storage.EgressProfile, requestedModel string) {
	var response struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(responseBody, &response)
	s.persistCodexBindingAliases(ctx, affinity, response.ID, response.Model, lease, egress, requestedModel)
	s.persistContextJournal(ctx, requestBody, responseBody, affinity.Hash, lease.Account.ID)
}

func (s *Server) persistCodexBindingAliases(ctx context.Context, affinity routing.AffinityKey, responseID, actualModel string, lease scheduler.Lease, egress storage.EgressProfile, requestedModel string) {
	model := firstNonEmpty(actualModel, lease.ResolvedModel, requestedModel)
	egressID := firstNonEmpty(egress.ID, lease.Egress.ID)
	upsert := func(key routing.AffinityKey) {
		if key.Hash == "" {
			return
		}
		_ = s.store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
			RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source,
			AccountID: lease.Account.ID, Provider: "codex", Model: model, EgressID: egressID,
		})
	}
	upsert(affinity)
	upsert(routing.ResponseAffinityKey(responseID))
}

func (s *Server) doWithCFRetry(ctx context.Context, req upstream.Request, lease scheduler.Lease, strict bool) (*upstream.Response, storage.EgressProfile, error) {
	resp, err := s.upstream.Do(ctx, req)
	if err != nil {
		return nil, req.Egress, err
	}
	if resp.StatusCode < 400 {
		return resp, req.Egress, nil
	}
	body, err := upstream.DrainAndClose(resp.Body)
	if err != nil {
		return nil, req.Egress, err
	}
	detection := cf.Detect(resp.StatusCode, resp.Header, body)
	if !detection.Matched {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	if cf.EdgeOnly(detection) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	if cf.Recordable(detection) {
		s.handleCFEvent(ctx, req.Account, req.Egress, resp.StatusCode, detection)
	}
	if strict {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	// Re-read the binding so a WARP exit just assigned by handleCFEvent is visible to
	// the standby retry below — this is what reroutes the request through WARP in the
	// SAME turn instead of only on the next one.
	standbys := lease.Binding.StandbyIDs()
	if updated, err := s.store.GetEgressBinding(ctx, req.Account.ID); err == nil {
		standbys = updated.StandbyIDs()
	}
	for _, standbyID := range standbys {
		standby, err := s.store.GetEgressProfile(ctx, standbyID)
		if err != nil || !scheduler.EgressHealthy(standby, storage.Now()) {
			continue
		}
		retryReq := req
		retryReq.Egress = standby
		retryResp, err := s.upstream.Do(ctx, retryReq)
		if err != nil {
			continue
		}
		return retryResp, standby, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, req.Egress, nil
}

// handleCFEvent is the single reaction point for a confirmed Cloudflare event. It
// records the event (the existing StormBreaker cooldown/quarantine ladder) and then
// applies the WARP ladder:
//
//   - CF on a normal egress (direct/proxy/sidecar): assign the account a WARP exit as
//     a standby (≤3 accounts/exit). Because scheduler.selectEgress serves a cooled
//     binding via its standby, the account keeps working through WARP during the
//     cooldown instead of dropping out of the pool — the fix for "动不动就冷却".
//   - CF on a WARP exit itself: the exit's IP is flagged. Repair it in the background
//     (solver first, then re-register for a fresh IP) so the live request fails fast
//     to a fresh account while the exit is restored for subsequent traffic.
func (s *Server) handleCFEvent(ctx context.Context, account storage.Account, egress storage.EgressProfile, status int, detection cf.Detection) {
	_ = (cf.StormBreaker{Store: s.store}).Record(ctx, account.ID, egress.ID, status, detection)
	if s.warp == nil || !s.warp.Enabled() {
		return
	}
	if s.warp.IsWarpExit(egress.ID) {
		go func() {
			defer supervisor.Recover("warp-exit-recovery")
			s.recoverWarpExit(context.Background(), account, egress)
		}()
		return
	}
	if _, err := s.warp.AssignCFAccount(ctx, account.ID); err != nil {
		log.Printf("warp: assign account %s on CF: %v", account.ID, err)
	}
}

// recoverWarpExit repairs a CF-blocked WARP exit: try the cf_clearance solver through
// that exit first; if it is disabled or fails, re-register the exit's wgcf profile for
// a fresh WARP IP. Runs in the background (off the request path).
func (s *Server) recoverWarpExit(ctx context.Context, account storage.Account, egress storage.EgressProfile) {
	if s.solver != nil && s.solver.Enabled() {
		if err := s.solveAndInject(ctx, account, egress); err == nil {
			log.Printf("warp: solved cf_clearance for exit %s (account %s)", egress.ID, account.ID)
			return
		} else {
			log.Printf("warp: solver failed for exit %s: %v; re-registering exit", egress.ID, err)
		}
	}
	if s.warp != nil {
		if err := s.warp.ReregisterExit(ctx, egress.ID); err != nil {
			log.Printf("warp: reregister exit %s failed: %v", egress.ID, err)
		}
	}
}

// solveAndInject asks the solver to clear Cloudflare for the account's upstream host
// THROUGH the exit's proxy, then persists the cf_clearance via the existing injected-
// cookie plumbing (DB record + Go jar + sidecar store) and clears the binding cooldown
// so the account retries with the clearance. cf_clearance is only valid replayed with
// the same UA + exit IP, which is why the solver UA + exit IP are stored alongside it.
func (s *Server) solveAndInject(ctx context.Context, account storage.Account, egress storage.EgressProfile) error {
	token, err := s.store.GetToken(ctx, account.ID)
	if err != nil {
		return err
	}
	host := s.cfHostForProvider(scheduler.ProviderFromToken(token))
	if host == "" {
		return errors.New("no upstream host for cf solve")
	}
	proxyURL := firstNonEmpty(egress.ChainProxy, egress.Endpoint)
	sol, err := s.solver.Solve(ctx, "https://"+host+"/", proxyURL)
	if err != nil {
		return err
	}
	upstreamHost := "https://" + host
	fallbackKey := account.ID + ":" + egress.ID
	_ = s.store.UpsertInjectedCookie(ctx, storage.InjectedCookie{
		AccountID:    account.ID,
		EgressID:     egress.ID,
		UpstreamHost: upstreamHost,
		CookieHeader: sol.CookieHeader,
		UserAgent:    sol.UserAgent,
		ExitIP:       egress.ExitIP,
	})
	_ = s.upstream.ImportCookies(account.ID, egress.ID, upstreamHost, fallbackKey, sol.CookieHeader)
	if sc := strings.TrimSpace(s.cfg.DefaultSidecarEndpoint); sc != "" {
		_ = s.upstream.SeedSidecarCookies(ctx, sc, account.ID, egress.ID, upstreamHost, fallbackKey, upstream.CookieMapFromHeader(sol.CookieHeader))
	}
	_ = s.store.SetBindingCooldown(ctx, account.ID, 0)
	s.scheduler.NotifyStateChanged()
	return nil
}

// cfHostForProvider returns the upstream host whose Cloudflare wall a provider sits
// behind (chatgpt.com for Codex; api.anthropic.com for Claude), derived from config.
func (s *Server) cfHostForProvider(provider string) string {
	base := s.cfg.UpstreamBaseURL
	if provider == "claude" {
		base = firstNonEmpty(s.cfg.ClaudeUpstreamBaseURL, "https://api.anthropic.com")
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return ""
}

func (s *Server) recordUsage(ctx context.Context, accountID, routeHash string, body []byte) {
	parsed := usage.ParseResponse(body)
	if len(parsed.RawUsage) == 0 {
		// Session 33: When the upstream response body does not contain countable
		// usage (e.g. a soft rate-limit error returned as HTTP 200), fall back to
		// the billing hold's estimated_tokens so the usage overview is not empty.
		holdID := holdIDFromCtx(ctx)
		if holdID != "" {
			estimate := s.billingHoldEstimate(holdID)
			if estimate == 0 {
				if hold, err := s.store.GetBillingHold(ctx, holdID); err == nil {
					estimate = hold.EstimatedTokens
				}
			}
			if estimate > 0 {
				log.Printf("[USAGE-WARN] account=%s: no usage in body (len=%d), using billing_hold estimate=%d",
					accountID, len(body), estimate)
				parsed = usage.Parsed{
					Model:        "unknown",
					TotalTokens:  estimate,
					PromptTokens: estimate / 2,
					RawUsage:     json.RawMessage(`{"estimated":true}`),
				}
			} else {
				log.Printf("[USAGE-WARN] account=%s: no usage in response body (len=%d) and no billing_hold fallback",
					accountID, len(body))
				return
			}
		} else {
			log.Printf("[USAGE-WARN] account=%s: no usage in response body (len=%d)", accountID, len(body))
			return
		}
	}
	keyHash, userID := downstreamFromCtx(ctx)
	diag := usageDiagnosticsFromCtx(ctx)
	if diag.UsageEventID == "" {
		diag.UsageEventID = requestIDFromContext(ctx)
	}
	diag.CacheMissTokens = parsed.CacheMissTokens
	diag.CacheTotalInputTokens = parsed.CacheTotalInputTokens
	diag.CacheCreation5mTokens = parsed.CacheCreation5mTokens
	diag.CacheCreation1hTokens = parsed.CacheCreation1hTokens
	if diag.CachePrewarmAttempted && parsed.CacheReadTokens > 0 {
		diag.CacheHitAfterPrewarm = true
	}
	// Defer the insert off the request path. Usage rows are read only by the admin
	// dashboards (never by request processing), so eventual recording is correct; the
	// closure captures only the small parsed usage + identity, and uses a detached
	// context because the request's is cancelled on return.
	s.enqueueUsage(storage.UsageRecordWrite{AccountID: accountID, RouteKeyHash: routeHash, APIKeyHash: keyHash, UserID: userID, Model: parsed.Model,
		Prompt: parsed.PromptTokens, Completion: parsed.CompletionTokens, Total: parsed.TotalTokens, Cached: parsed.CachedTokens,
		CacheRead: parsed.CacheReadTokens, CacheCreation: parsed.CacheCreationTokens, Raw: parsed.RawUsage, Diagnostics: diag})
}

func (s *Server) hasCodexFailoverCandidate(ctx context.Context, groupName, excludeAccountID string) bool {
	if groupName == "" {
		groupName = s.cfg.DefaultGroup
	}
	accounts, err := s.store.ListActiveAccountsByGroup(ctx, groupName)
	if err != nil {
		return false
	}
	now := storage.Now()
	candidateIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.ID == excludeAccountID || account.QuarantineUntil > now {
			continue
		}
		candidateIDs = append(candidateIDs, account.ID)
	}
	tokens, err := s.store.ListTokensByAccountIDs(ctx, candidateIDs)
	if err != nil {
		return false
	}
	for _, accountID := range candidateIDs {
		token, ok := tokens[accountID]
		if ok && scheduler.ProviderFromToken(token) == "codex" {
			return true
		}
	}
	return false
}

// recordParsedUsage stores an already-parsed usage figure (the streaming path, where
// usage is extracted incrementally from the SSE frames rather than a buffered body).
func (s *Server) recordParsedUsage(ctx context.Context, accountID, routeHash string, parsed usage.Parsed) {
	if parsed.TotalTokens == 0 && parsed.PromptTokens == 0 && parsed.CompletionTokens == 0 {
		// Session 33: When the stream scanner cannot extract countable tokens
		// (e.g. the upstream terminated with an error instead of a normal
		// completion), fall back to the billing hold's estimated_tokens.
		holdID := holdIDFromCtx(ctx)
		if holdID != "" {
			estimate := s.billingHoldEstimate(holdID)
			if estimate == 0 {
				if hold, err := s.store.GetBillingHold(ctx, holdID); err == nil {
					estimate = hold.EstimatedTokens
				}
			}
			if estimate > 0 {
				log.Printf("[USAGE-WARN] account=%s: stream usage all zero, using billing_hold estimate=%d",
					accountID, estimate)
				parsed = usage.Parsed{
					Model:        parsed.Model,
					TotalTokens:  estimate,
					PromptTokens: estimate / 2,
					RawUsage:     json.RawMessage(`{"estimated":true}`),
				}
			} else {
				log.Printf("[USAGE-WARN] account=%s: all tokens are zero, model=%s, raw=%s",
					accountID, parsed.Model, string(parsed.RawUsage))
				return
			}
		} else {
			log.Printf("[USAGE-WARN] account=%s: all tokens are zero, model=%s, raw=%s",
				accountID, parsed.Model, string(parsed.RawUsage))
			return
		}
	}
	keyHash, userID := downstreamFromCtx(ctx)
	diag := usageDiagnosticsFromCtx(ctx)
	if diag.UsageEventID == "" {
		diag.UsageEventID = requestIDFromContext(ctx)
	}
	diag.CacheMissTokens = parsed.CacheMissTokens
	diag.CacheTotalInputTokens = parsed.CacheTotalInputTokens
	diag.CacheCreation5mTokens = parsed.CacheCreation5mTokens
	diag.CacheCreation1hTokens = parsed.CacheCreation1hTokens
	if diag.CachePrewarmAttempted && parsed.CacheReadTokens > 0 {
		diag.CacheHitAfterPrewarm = true
	}
	s.enqueueUsage(storage.UsageRecordWrite{AccountID: accountID, RouteKeyHash: routeHash, APIKeyHash: keyHash, UserID: userID, Model: parsed.Model,
		Prompt: parsed.PromptTokens, Completion: parsed.CompletionTokens, Total: parsed.TotalTokens, Cached: parsed.CachedTokens,
		CacheRead: parsed.CacheReadTokens, CacheCreation: parsed.CacheCreationTokens, Raw: parsed.RawUsage, Diagnostics: diag})
}
