package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/bodysource"
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
	"codex-account-pool/internal/registration"
	"codex-account-pool/internal/reliability"
	"codex-account-pool/internal/responsefilter"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/turbo_gpt_register"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/usagejournal"
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
	cfg       config.Config
	store     *storage.Store
	scheduler *scheduler.Scheduler
	upstream  *upstream.Client
	// upstreamDo is the single Codex/Responses call boundary. Production binds it
	// to upstream.Client.Do during construction; focused fault-injection tests replace
	// it to verify that a broken transport contract cannot panic an HTTP/WS request.
	upstreamDo func(context.Context, upstream.Request) (*upstream.Response, error)
	planner    *virtual.Planner
	gopay      *gopay.Manager
	paymentMgr *payment.Manager
	warp       *warp.Manager
	solver     *cfsolve.Client
	mux        *http.ServeMux
	bodyBudget *bodysource.Budget
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
	regHandler *Handler
	// emailReg orchestrates email-based ChatGPT registration
	emailReg         *registration.EmailRegOrchestrator
	turboGPTRegister *turbo_gpt_register.Orchestrator
	lifecycleHandler *LifecycleHandlers
	claudeRefresh    *claudeRefreshGates
	kiro             *kiro.Manager
	// asyncWrites carries fire-and-forget DB writes (usage rows, virtual-ledger rows)
	// off the request path so the response is not blocked on a write through the single
	// SQLite write connection. A single drainer goroutine runs them FIFO (matching the
	// 1-writer pool); see asyncwrite.go. FlushWrites drains it on shutdown.
	asyncWrites           chan func()
	usageWrites           chan telemetryWrite
	usageJournal          *usagejournal.Journal
	usageJournalStop      chan struct{}
	usageJournalWake      chan struct{}
	usageJournalCommitMu  sync.Mutex
	usageEnqueueMu        sync.Mutex
	usageJournalAcked     atomic.Uint64
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
	codexCacheFlightsMu  sync.Mutex
	codexCacheFlights    map[string]chan struct{}
	claudeCacheDiagMu    sync.Mutex
	claudeCacheDiagPrev  map[string]string
	kiroCacheFlightsMu   sync.Mutex
	kiroCacheFlights     map[string]chan struct{}
	codexSessionGatesMu  sync.Mutex
	codexSessionGates    map[string]*codexSessionGate

	codexResetMu         sync.Mutex
	codexResetLocks      map[string]*sync.Mutex
	compatMu             sync.Mutex
	compatRecent         []compatIncompatibilityRecord
	qualityMu            sync.Mutex
	qualityRunning       bool
	goalCompactionMu     sync.Mutex
	goalCompactionQueued map[string]bool
	goalCompactionQueue  chan string
	goalCompactionCtx    context.Context
	goalCompactionCancel context.CancelFunc
	goalCompactionTimers sync.WaitGroup
	contextRebuilt       uint64
	contextDegraded      uint64
	// Codex CPA-v2 aggregate-only observability. Values intentionally carry no
	// downstream/upstream ids or prompt bodies.
	codexMappingBindingsCreated uint64
	codexMappingEpochRotations  uint64
	codexMappingFreshRoots      uint64
	codexMappingUnidentified    uint64
	codexMappingAmbiguous       uint64
	codexNativeContinues        uint64
	codexEOFCompensations       uint64
	diskGuard                   atomic.Value // DiskGuardSnapshot
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
		bodyBudget: bodysource.NewBudget(dep.Config.EffectiveBodyMemoryBudgetBytes(), dep.Config.BodySpoolMaxBytes),
		oauth:      newOAuthStore(oauthSessionTTL),
		login:      newLoginThrottle(),
		clientErrors: newClientErrorLimiter(
			clientErrorLogLimit,
			clientErrorLogWindow,
			clientErrorLogMaxClients,
		),
		relState:   reliability.NewStore(relStateTTL, relStateMax),
		regHandler: NewHandler(dep.Store, dep.Upstream, dep.Config.DefaultRegisterMethod, dep.Config.RegistrationConcurrency, &dep.Config), // builds live provider Manager from provider_settings
		emailReg:   newEmailRegOrchestrator(dep.Store, &dep.Config),
		turboGPTRegister: turbo_gpt_register.New(dep.Store, turbo_gpt_register.NodeExecutor{
			NodePath: "node", ScriptPath: "services/turbo_gpt_register/index.js",
		}, turbo_gpt_register.Options{
			MaxConcurrent: dep.Config.RegistrationConcurrency,
			PhaseTimeout:  time.Duration(dep.Config.RegistrationTimeout) * time.Second,
		}),
		lifecycleHandler:    newServerLifecycleHandlers(dep.Store),
		claudeRefresh:       newClaudeRefreshGates(),
		kiro:                kiro.NewManager(dep.Store, dep.Upstream, dep.Config),
		claudeCacheFlights:  map[string]chan struct{}{},
		codexCacheFlights:   map[string]chan struct{}{},
		claudeCacheDiagPrev: map[string]string{},
		kiroCacheFlights:    map[string]chan struct{}{},
		codexSessionGates:   map[string]*codexSessionGate{},
		codexResetLocks:     map[string]*sync.Mutex{},
	}
	// Resolve the identity secret once (it can read host files on the unconfigured
	// path); s.identitySecret() returns this cached value on the hot path.
	s.identitySecretCached = identity.ResolveSecret([]byte(dep.Config.IdentitySecret))
	if dep.Config.UsageJournalEnabled && dep.Store != nil && !dep.Store.InMemory() {
		journalDir, err := usageJournalDirectory(dep.Config, dep.Store.Path())
		if err != nil {
			panic(fmt.Sprintf("usage journal path: %v", err))
		}
		s.usageJournal, err = usagejournal.Open(journalDir, dep.Config.UsageJournalSegmentBytes)
		if err != nil {
			panic(fmt.Sprintf("open usage journal: %v", err))
		}
		s.usageJournalStop = make(chan struct{})
		s.usageJournalWake = make(chan struct{}, 1)
		if err = s.replayUsageJournal(context.Background()); err != nil {
			_ = s.usageJournal.Close()
			panic(fmt.Sprintf("replay usage journal: %v", err))
		}
		if snapshot, snapshotErr := s.usageJournal.Snapshot(); snapshotErr == nil {
			s.usageJournalAcked.Store(snapshot.AckedSequence)
		}
	}
	s.startAsyncWriter()
	s.startGoalCompactionWorkers()
	// Apply any persisted runtime overrides for the upstream-consumed fingerprint /
	// identity fields to the upstream client at boot, so admin settings survive a
	// restart (the request-time getters already overlay the rest live).
	if s.upstream != nil {
		s.upstreamDo = s.upstream.Do
		s.upstream.UpdateConfig(s.effectiveUpstreamConfig(context.Background()))
	}
	if s.scheduler != nil {
		s.scheduler.UpdateConfig(s.effectiveSchedulerConfig(context.Background()))
	}
	s.routes()
	return s
}

func usageJournalDirectory(cfg config.Config, databasePath string) (string, error) {
	if configured := strings.TrimSpace(cfg.UsageJournalDir); configured != "" {
		return filepath.Clean(configured), nil
	}
	lowerPath := strings.ToLower(strings.TrimSpace(databasePath))
	if strings.HasPrefix(lowerPath, "postgres://") || strings.HasPrefix(lowerPath, "postgresql://") {
		base, err := os.UserCacheDir()
		if err != nil || strings.TrimSpace(base) == "" {
			base = os.TempDir()
		}
		nodeHash := sha256.Sum256([]byte(strings.TrimSpace(cfg.NodeID)))
		return filepath.Join(base, "codex-account-pool", "usage-journal", fmt.Sprintf("node-%x", nodeHash[:8])), nil
	}
	raw := strings.TrimSpace(strings.SplitN(databasePath, "?", 2)[0])
	if strings.HasPrefix(raw, "file:") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		raw = parsed.Path
		if raw == "" {
			raw = parsed.Opaque
		}
	}
	if raw == "" || raw == ":memory:" {
		return "", errors.New("file-backed database path is required")
	}
	return filepath.Join(filepath.Dir(raw), "."+filepath.Base(raw)+".usage-journal"), nil
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
	s.mux.HandleFunc("/user/models", s.handleUserModels)
	s.mux.HandleFunc("/user/profile", s.handleUserProfile)
	s.mux.HandleFunc("/admin/accounts", s.adminAccounts)
	s.mux.HandleFunc("/admin/models", s.handleAdminModels)
	s.mux.HandleFunc("/admin/accounts/summary", s.adminAccountsSummary)
	s.mux.HandleFunc("/admin/accounts/export", s.adminAccountsExport)
	s.mux.HandleFunc("/admin/accounts/import-auth-json", s.adminImportAuthJSON)
	s.mux.HandleFunc("/admin/accounts/import-token", s.adminImportToken)
	s.mux.HandleFunc("/admin/accounts/import-cookie", s.adminImportCookie)
	s.mux.HandleFunc("/admin/accounts/import-key", s.adminImportKey)
	s.mux.HandleFunc("/admin/accounts/import-kiro-json", s.adminImportKiroJSON)
	s.mux.HandleFunc("/admin/accounts/import-kiro-api-key", s.adminImportKiroAPIKey)
	// Bulk-reassign accounts to a group (exact path; takes precedence over the
	// /admin/accounts/ subtree handler). Single reassign is /admin/accounts/<id>/group.
	s.mux.HandleFunc("/admin/accounts/assign-group", s.adminAccountsAssignGroup)
	// Web-login (paste-back) OAuth import for Codex + Claude (see oauth.go).
	s.mux.HandleFunc("/admin/oauth/start", s.adminOAuthStart)
	s.mux.HandleFunc("/admin/oauth/complete", s.adminOAuthComplete)
	s.mux.HandleFunc("/admin/accounts/", s.adminAccountAction)
	s.mux.HandleFunc("/admin/egress-profiles", s.adminEgressProfiles)
	s.mux.HandleFunc("/admin/egress-profiles/", s.adminEgressProfileAction)
	// A2 fidelity-diff: verify in-process vs sidecar JA3/JA4/Akamai against a reflector.
	s.mux.HandleFunc("/admin/egress-fingerprint-check", s.adminEgressFingerprintCheck)
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
	s.mux.HandleFunc("/admin/usage/dashboard", s.adminUsageDashboard)
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
	s.mux.HandleFunc("/admin/context-journal", s.adminContextJournal)
	s.mux.HandleFunc("/admin/goals", s.adminGoals)
	s.mux.HandleFunc("/admin/goals/", s.adminGoalAction)
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
	s.mux.HandleFunc("/admin/user-groups", s.adminUserGroups)
	s.mux.HandleFunc("/admin/user-groups/", s.adminUserGroupsAction)
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
	// Email-based registration (ChatGPT protocol registration via Outlook/IMAP OTP).
	s.mux.HandleFunc("/admin/register/email/start", s.handleEmailRegStart)
	s.mux.HandleFunc("/admin/register/email/jobs", s.handleEmailRegJobs)
	s.mux.HandleFunc("/admin/register/email/job/status", s.handleEmailRegJobStatus)
	s.mux.HandleFunc("/admin/register/email/job/events", s.handleEmailRegJobEvents)
	s.mux.HandleFunc("/admin/register/email/job/events/sse", s.handleEmailRegJobEventsSSE)
	s.mux.HandleFunc("/admin/register/email/job/", s.handleEmailRegJobAction)
	s.mux.HandleFunc("/admin/register/email/config", s.handleEmailRegConfig)
	// Turbo GPT phased browser registrar (durable jobs + encrypted OAuth results).
	s.mux.HandleFunc("/admin/turbo-gpt-register/jobs", s.adminTurboGPTRegisterJobs)
	s.mux.HandleFunc("/admin/turbo-gpt-register/jobs/", s.adminTurboGPTRegisterJobAction)
	s.mux.HandleFunc("/admin/turbo-gpt-register/config", s.adminTurboGPTRegisterConfig)
	// Email account pool management (Outlook/Hotmail accounts for registration).
	s.mux.HandleFunc("/admin/email-pool/import", s.adminEmailPoolImport)
	s.mux.HandleFunc("/admin/email-pool", s.adminEmailPool)
	s.mux.HandleFunc("/admin/email-pool/", s.adminEmailPoolAction)
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
	body["body_storage"] = s.bodyBudgetSnapshot()
	body["usage_journal"] = s.usageJournalMetrics()
	body["codex_session_mapping"] = s.codexSessionMappingStats(r.Context())
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
	group := s.cfg.DefaultGroup
	userGroupID := ""
	if plain := downstreamBearer(r); plain != "" {
		if key, found, _ := s.store.LookupAPIKey(r.Context(), hashAPIKey(plain)); found {
			if strings.TrimSpace(key.GroupName) != "" {
				group = strings.TrimSpace(key.GroupName)
			}
			userGroupID = strings.TrimSpace(key.UserGroupID)
		}
	}
	scopes, err := s.modelsRoutableCapabilityScopes(r.Context(), group, userGroupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if hint, ok := s.modelsProviderHint(r); !ok {
		writeError(w, http.StatusBadRequest, errors.New("provider hint must be auto, codex, claude, kiro, antigravity, or custom:<id>"))
		return
	} else if hint != "auto" {
		providers, providerErr := s.modelsAccountProviders(r.Context())
		if providerErr != nil {
			writeError(w, http.StatusInternalServerError, providerErr)
			return
		}
		for i := range scopes {
			scopes[i] = filterModelsCapabilitiesByProvider(scopes[i], providers, hint)
		}
	}
	// Content-negotiate by client family: an Anthropic client (Claude Code) needs the
	// native Anthropic /v1/models schema or its model picker / "auto" selection breaks;
	// every other client keeps the OpenAI-shaped list. Detection keys off headers only
	// Anthropic clients send (anthropic-version / anthropic-beta / x-api-key).
	// The official Codex client identifies its catalog request with the client_version
	// query key and expects {"models":[ModelInfo,...]}. Check this first because wrapper
	// processes can retain Anthropic-family headers while invoking Codex.
	if _, codexClient := r.URL.Query()["client_version"]; codexClient {
		body, etag, err := capability.BuildCodexModelsResponseForScopes(scopes)
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
	if isAnthropicClient(r) {
		body, etag, err := capability.BuildAnthropicModelsResponseForScopes(scopes)
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
	body, etag, err := capability.BuildModelsResponseForScopes(scopes, cfg)
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

// modelsRoutableCapabilities mirrors the two-layer request router for model
// discovery. Account-pool targets contribute their own inventory, while a model
// provider target keeps the API key's base group and narrows that inventory to
// the selected provider. The union is built before a request-level provider hint
// is applied, so a hint can only reduce the user group's visible route surface.
func (s *Server) modelsRoutableCapabilities(ctx context.Context, baseGroup, userGroupID string) ([]storage.ModelCapability, error) {
	scopes, err := s.modelsRoutableCapabilityScopes(ctx, baseGroup, userGroupID)
	if err != nil {
		return nil, err
	}
	out := make([]storage.ModelCapability, 0)
	seen := make(map[string]struct{})
	for _, caps := range scopes {
		for _, c := range caps {
			key := c.AccountID + "\x00" + c.ModelSlug
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Server) modelsRoutableCapabilityScopes(ctx context.Context, baseGroup, userGroupID string) ([][]storage.ModelCapability, error) {
	if strings.TrimSpace(userGroupID) == "" {
		caps, err := s.store.ListRoutableCapabilities(ctx, baseGroup)
		return [][]storage.ModelCapability{caps}, err
	}
	group, found, err := s.store.GetUserGroup(ctx, userGroupID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("user group %s not found", userGroupID)
	}
	// Legacy user groups created before target rows were mandatory retain the
	// router's base-group fallback instead of presenting an empty catalog.
	if len(group.Targets) == 0 {
		caps, listErr := s.store.ListRoutableCapabilities(ctx, baseGroup)
		return [][]storage.ModelCapability{caps}, listErr
	}

	var baseCaps []storage.ModelCapability
	baseLoaded := false
	var providers map[string]string
	scopes := make([][]storage.ModelCapability, 0, len(group.Targets))

	for _, target := range group.Targets {
		routeGroup, routeProvider, routeErr := targetRefToRoute(target)
		if routeErr != nil {
			return nil, routeErr
		}
		if routeGroup != "" {
			caps, listErr := s.store.ListRoutableCapabilities(ctx, routeGroup)
			if listErr != nil {
				return nil, listErr
			}
			scopes = append(scopes, caps)
			continue
		}
		if routeProvider == "" {
			continue
		}
		if !baseLoaded {
			baseCaps, err = s.store.ListRoutableCapabilities(ctx, baseGroup)
			if err != nil {
				return nil, err
			}
			baseLoaded = true
		}
		if providers == nil {
			providers, err = s.modelsAccountProviders(ctx)
			if err != nil {
				return nil, err
			}
		}
		scopes = append(scopes, filterModelsCapabilitiesByProvider(baseCaps, providers, routeProvider))
	}
	return scopes, nil
}

func (s *Server) modelsAccountProviders(ctx context.Context) (map[string]string, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ResolveAccountProviders(ctx, accounts)
}

func filterModelsCapabilitiesByProvider(caps []storage.ModelCapability, providers map[string]string, provider string) []storage.ModelCapability {
	provider = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provider)), "custom:")
	filtered := make([]storage.ModelCapability, 0, len(caps))
	for _, c := range caps {
		if strings.EqualFold(strings.TrimSpace(providers[c.AccountID]), provider) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func (s *Server) modelsProviderHint(r *http.Request) (string, bool) {
	if raw := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); raw != "" {
		return normalizeProviderHint(raw)
	}
	if plain := downstreamBearer(r); plain != "" {
		if key, found, _ := s.store.LookupAPIKey(r.Context(), hashAPIKey(plain)); found {
			return normalizeProviderHint(key.ProviderHint)
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
	if timeout := s.cfg.RequestTimeout(); timeout > 0 {
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > timeout {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
	}
	raw, err := requestBodyBytes(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	originalRaw := raw
	var capturedMeta *bodysource.BodyMeta
	if meta, ok := bodyMetaFromContext(r.Context()); ok && meta.Size == int64(len(raw)) {
		capturedMeta = &meta
	}
	r = r.WithContext(withSchedulerWait(r.Context(), w, streamRequestWithMeta(raw, capturedMeta), schedulerWaitProtocol(r.URL.Path)))
	requestedModel := modelWithMeta(raw, capturedMeta)
	// Authenticate the downstream api key (if any) and resolve its routing group +
	// forced model/effort policy. The forced model is applied to the body BEFORE
	// affinity/capability routing so the request lands on an account that has the
	// forced model and the upstream actually receives it.
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	r, ok = s.attachUserGroupPolicy(w, r, pol)
	if !ok {
		return
	}
	// Carry the matched key/user identity in the context so usage is attributed to the
	// owning portal user (their console reads /user/usage).
	r = r.WithContext(withDownstreamKey(r.Context(), pol))
	// Keep the exact downstream aliases and turn body available to the durable Codex
	// recovery journal.  Native CPA remains the steady-state fast path; these values
	// are consulted only if its bound account disappears or the upstream confirms
	// that a previous_response_id has been lost.
	r = r.WithContext(withGoalIdentityAliases(r.Context(), goalAliasesWithMeta(r, raw, "codex", capturedMeta)))
	r = r.WithContext(withGoalOriginalBody(r.Context(), raw))
	// This is populated only after provider routing below confirms the request is
	// headed to native Codex. Claude and custom providers keep their own continuity
	// semantics and must not create Codex aliases merely by sharing this gateway.
	var codexMapping *codexSessionMapping
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
	requestPolicy := requestUserGroupPolicy(r.Context())
	if prompt.ShouldRewrite(requestPolicy.SystemPrompt, compactionRequestWithMeta(r.URL.Path, raw, bodyMetaForView(capturedMeta, originalRaw, raw)), requestPolicy.SystemPromptApplyToCompaction) {
		if r.URL.Path == "/v1/chat/completions" {
			raw, _, err = prompt.InjectChatSystemPrompt(raw, requestPolicy.SystemPrompt)
		} else {
			raw, _, err = prompt.InjectResponsesSystemPrompt(raw, requestPolicy.SystemPrompt)
		}
		if err != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_user_group_prompt", err.Error())
			return
		}
	}
	resolvedPolicyModel := modelWithMeta(raw, bodyMetaForView(capturedMeta, originalRaw, raw))
	r = r.WithContext(withModelDiagnostics(r.Context(), requestedModel, resolvedPolicyModel, pol.ModelOverrideSource))
	w.Header().Set("X-Pool-Requested-Model", requestedModel)
	w.Header().Set("X-Pool-Resolved-Model", resolvedPolicyModel)
	w.Header().Set("X-Pool-Model-Override-Source", firstNonEmpty(pol.ModelOverrideSource, "none"))
	if s.dispatchUserGroupRouteCandidates(w, r, originalRaw, raw, pol, s.handleGatewayPost) {
		return
	}
	path := r.URL.Path
	isChat := path == "/v1/chat/completions"
	// Compaction is signaled either by the dedicated endpoint or by the native
	// trigger carried in a Responses body. The same classification drives routing,
	// request shaping, and the CPA window-generation increment after terminal
	// success, so the three cannot disagree.
	isCompact := compactionRequestWithMeta(path, raw, bodyMetaForView(capturedMeta, originalRaw, raw))
	affinityGroup := pol.Group
	if affinityGroup == "" {
		affinityGroup = s.cfg.DefaultGroup
	}
	model := modelWithMeta(raw, bodyMetaForView(capturedMeta, originalRaw, raw))
	userGroupProvider := ""
	if pol.UserGroupID != "" {
		routeGroup, routeProvider, routeErr := resolveUserGroupRoute(r.Context(), s.store, pol, r, raw)
		if routeErr != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", routeErr.Error())
			return
		}
		if routeGroup != "" {
			pol.Group = routeGroup
			affinityGroup = routeGroup
		}
		if routeProvider != "" {
			pol.ProviderHint = routeProvider
			userGroupProvider = routeProvider
		}
	}

	// Antigravity currently implements the native Anthropic Messages contract,
	// not the OpenAI Chat Completions wire protocol.  Reject an explicit route at
	// this dispatch boundary for every model family (notably gemini-*); otherwise a
	// non-Claude slug falls through to the Codex adapter and produces a misleading
	// account/model error after using the wrong protocol.
	if isChat && (userGroupProvider == "antigravity" || effectiveGatewayProviderHint(r, pol) == "antigravity") {
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"provider antigravity does not expose the OpenAI Chat Completions protocol",
			[]string{"openai_chat_completions", "model:" + model},
			"native_antigravity_messages",
			"antigravity",
			"Send this model through POST /v1/messages, or select a provider route that advertises /v1/chat/completions.")
		return
	}

	// OpenAI-compatible requests targeting a Claude model are transparently
	// relayed to the Anthropic upstream (format-converted both ways) instead of
	// Codex; everything else continues down the Codex/Responses path.
	if isChat && (userGroupProvider == "claude" || (userGroupProvider == "" && isClaudeModel(model))) {
		raw, err = s.applyModelInstructionsForEntrypoint(r.Context(), requestUserGroupPolicy(r.Context()), model, path, raw)
		if err != nil {
			writeCodexInstructionConfigurationError(w, err)
			return
		}
		raw = s.moderateHistory(r.Context(), raw, "chat")
		s.handleChatViaClaude(w, r, raw, model, pol)
		return
	}

	// A model served by a custom OpenAI-compatible provider (DeepSeek, …) is routed to
	// that provider's generic adapter, converting both the chat-completions entrypoint
	// (near-passthrough) and the Codex /v1/responses entrypoint (Responses ↔ chat).
	var selectedCustom storage.CustomProvider
	var selectedCustomOK bool
	if strings.HasPrefix(userGroupProvider, "custom:") {
		selectedCustom, selectedCustomOK = s.customProviderByID(r.Context(), strings.TrimPrefix(userGroupProvider, "custom:"))
	} else if userGroupProvider == "" {
		selectedCustom, selectedCustomOK = s.customProviderForModel(r.Context(), model)
	}
	if selectedCustomOK {
		prov := selectedCustom
		raw, err = s.applyModelInstructionsForEntrypoint(r.Context(), requestUserGroupPolicy(r.Context()), model, path, raw)
		if err != nil {
			writeCodexInstructionConfigurationError(w, err)
			return
		}
		if isChat {
			raw = s.moderateHistory(r.Context(), raw, "chat")
		} else {
			raw = s.moderateHistory(r.Context(), raw, "responses")
		}
		if isChat {
			s.handleChatViaCustom(w, r, raw, model, pol.Group, prov)
		} else {
			s.handleResponsesViaCustom(w, r, raw, model, pol.Group, prov)
		}
		return
	}
	// `ultra` is a Codex CLI semantic, not a generic Responses wire value. Sol and
	// Terra may use it (the upstream boundary serializes max); Luna and unknown
	// models must be rejected before CPA mapping, scheduling, or tool/session work.
	if effort := responsesReasoningEffortWithMeta(raw, bodyMetaForView(capturedMeta, originalRaw, raw)); unsupportedCodexReasoningEffort(model, effort) {
		writeCodexReasoningEffortUnsupported(w, model, effort)
		return
	}
	// In auto mode, exact Kiro GPT-5.6 models may join the current group's Codex
	// fair pool when pressure exceeds 50%, or when fewer than two Codex GPT
	// accounts are available while pressure remains below 50%. This happens before
	// native Codex CPA mapping because Kiro has no compatible
	// previous_response_id/session state.
	kiroRaw := raw
	if !serverSideStateWithMeta(path, r, raw, bodyMetaForView(capturedMeta, originalRaw, raw)) {
		kiroRaw, err = s.applyModelInstructionsForEntrypoint(r.Context(), requestUserGroupPolicy(r.Context()), model, path, raw)
		if err != nil {
			writeCodexInstructionConfigurationError(w, err)
			return
		}
	}
	if s.tryServeAutoKiroGPT(w, r, kiroRaw, bodyMetaForView(capturedMeta, originalRaw, kiroRaw), model, affinityGroup, isChat, isCompact, pol) {
		return
	}
	// CPA-style stateless passthrough (default). Make every native Codex turn
	// self-contained so ANY account can serve it and seamless failover is lossless:
	// strip the two server-side-state signals the client carries — previous_response_id
	// (body) and x-codex-turn-state (header). degradedResponsesReplay removes both from
	// the body AND rewrites any now-orphaned tool-call outputs into plain context, so the
	// stripped turn stays a valid Responses request rather than 400-ing on a missing
	// tool call. `store` is deliberately left untouched: upstream normalization already
	// forces the exact per-upstream real-client value, which is better fingerprint
	// fidelity than a blanket store:false. With no state signals the request is `movable`
	// below, the session-mapping engine reports disabled, and the 409/400 the client used
	// to see cannot arise. Chat completions carry their history in `messages` and never
	// hold server-side state, so they are unaffected.
	if !isChat && s.codexStatelessPassthrough(r.Context()) && serverSideStateWithMeta(path, r, raw, bodyMetaForView(capturedMeta, originalRaw, raw)) {
		if codexResponsesWebSocketUsesHTTPSFallback(r.Context()) {
			// response.append carries only the current delta. After its persistent
			// upstream WebSocket has failed, simply deleting previous_response_id
			// would silently discard the earlier turns (and can orphan tool outputs).
			// Rebuild the durable goal/journal into one self-contained HTTP request,
			// then keep all later turns on the stateless HTTPS bridge.
			contextError := leakfilter.ResponsesContextErrorPreviousResponseNotFound
			retry, mode, recovered := s.recoverResponsesContext(r.Context(), raw, r.Header, contextError)
			unpaired := false
			if recovered && mode == "rebuilt" {
				unpaired = responsesHasUnpairedToolOutput(retry.Raw, leakfilter.ResponsesContextErrorNone)
			} else if recovered {
				unpaired = responsesHasUnpairedToolOutput(raw, contextError)
			}
			if !recovered || unpaired {
				code := "codex_context_epoch_retired"
				if unpaired {
					code = codexMappingErrorCode(errCodexToolContextUnrecoverable)
				}
				s.auditCodexMappingFailure(r.Context(), code)
				s.writeCodexSessionMappingError(w, isStreamRequest(raw), code)
				return
			}
			raw = retry.Raw
			r = r.Clone(r.Context())
			r.Header = retry.Header
			w.Header().Set("X-MiCliProxy-Context-Status", mode)
			w.Header().Set("X-MiCliProxy-Codex-Passthrough", "https-fallback-"+mode)
			if mode == "rebuilt" {
				atomic.AddUint64(&s.contextRebuilt, 1)
			} else {
				atomic.AddUint64(&s.contextDegraded, 1)
			}
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
				Action: "codex_context_migrated", State: "recovered", Reason: "websocket_https_fallback",
				Detail: "stateless_http_replay",
			})
		} else {
			raw = degradedResponsesReplay(raw)
			if r.Header.Get("X-Codex-Turn-State") != "" {
				r = r.Clone(r.Context())
				r.Header.Del("X-Codex-Turn-State")
			}
			w.Header().Set("X-MiCliProxy-Codex-Passthrough", "stateless")
		}
	}
	// Native Codex context is owned by the upstream Responses session during normal
	// operation. Resolve exact downstream aliases first; only a missing bound account
	// or confirmed upstream context loss activates the encrypted durable replay path.
	if s.codexSessionMappingEnabled(r.Context()) {
		var identityMeta *bodysource.BodyMeta
		identityMeta = bodyMetaForView(capturedMeta, originalRaw, raw)
		downstreamIdentity := codexDownstreamSessionIdentityWithMeta(r.Header, raw, identityMeta)
		r = r.WithContext(withCodexDownstreamIdentity(r.Context(), raw, downstreamIdentity))
		s.codexMappingContextHeader(w)
		releaseSessionGate, gateErr := s.acquireCodexSessionGate(r.Context(), codexSessionGateKey(pol, r, raw))
		if gateErr != nil {
			return
		}
		defer releaseSessionGate()
		var mappingErr error
		freshRootAfterContextLoss := false
		contextRecoveryStatus := ""
		codexMapping, mappingErr = s.resolveCodexSessionMapping(r.Context(), r, raw, pol)
		// A prior process/version may already have retired this epoch after an
		// upstream context loss. Prefer the durable checkpoint replay on the first
		// following request so the client does not need to manually restart its
		// task. The legacy fresh-root reset remains only when no checkpoint exists.
		if mappingErr != nil && s.codexCPAStrict(r.Context()) && errors.Is(mappingErr, storage.ErrCodexSessionEpochRetired) {
			if migration, recovered, recoveryErr := s.recoverCodexSessionMapping(r.Context(), r, raw, r.Header, pol, codexMapping, leakfilter.ResponsesContextErrorPreviousResponseNotFound, "retired_upstream_context"); recovered {
				raw = migration.Retry.Raw
				r = r.Clone(r.Context())
				r.Header = migration.Retry.Header
				codexMapping, mappingErr = migration.Mapping, nil
				freshRootAfterContextLoss = true
				contextRecoveryStatus = migration.Mode
				if migration.Mode == "rebuilt" {
					atomic.AddUint64(&s.contextRebuilt, 1)
				} else {
					atomic.AddUint64(&s.contextDegraded, 1)
				}
				atomic.AddUint64(&s.codexMappingFreshRoots, 1)
				_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
					Action: "codex_context_migrated", State: "recovered", Reason: "retired_upstream_context", Detail: "durable_replay_new_epoch",
				})
			} else if errors.Is(recoveryErr, errCodexToolContextUnrecoverable) {
				code := codexMappingErrorCode(recoveryErr)
				s.auditCodexMappingFailure(r.Context(), code)
				s.writeCodexSessionMappingError(w, isStreamRequest(raw), code)
				return
			} else if recoveryErr != nil {
				log.Printf("[CODEX-SESSION-MAPPING] retired context migration request_id=%s: %v", requestIDFromContext(r.Context()), recoveryErr)
			}
			if mappingErr != nil {
				retiredIdentity := codexDownstreamSessionIdentityForRequest(r, raw)
				if resetBody, resetHeader, reset := codexRetiredEpochFreshRootRequest(raw, r.Header); reset {
					raw = resetBody
					r = r.Clone(r.Context())
					r.Header = resetHeader
					codexMapping, mappingErr = s.resolveCodexSessionMapping(r.Context(), r, raw, pol)
					if mappingErr == nil {
						codexMapping.retainRetiredEpochHierarchy(retiredIdentity)
						freshRootAfterContextLoss = true
						atomic.AddUint64(&s.codexMappingFreshRoots, 1)
						_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
							Action: "codex_context_new_root", State: "recovered",
							Reason: "retired_upstream_context", Detail: "no_history_replay",
						})
					}
				}
			}
		}
		if mappingErr != nil {
			code := codexMappingErrorCode(mappingErr)
			if code == "codex_session_mapping_unidentified" && bodyHasClientToolResult(raw) {
				code = "codex_tool_context_unrecoverable"
			}
			// An unidentified first request without a real session identity is valid:
			// it receives a one-shot identity and is bound only after success. Every
			// stateful request, ambiguity, and retired epoch is explicit downstream.
			if codexDownstreamSessionIdentityForRequest(r, raw).stateful() || code != "codex_session_mapping_unidentified" {
				s.auditCodexMappingFailure(r.Context(), code)
				s.writeCodexSessionMappingError(w, isStreamRequest(raw), code)
				return
			}
		}
		if freshRootAfterContextLoss {
			w.Header().Set("X-MiCliProxy-Context-Status", firstNonEmpty(contextRecoveryStatus, "new_root_after_context_loss"))
		}
		r = r.WithContext(withCodexSessionMapping(r.Context(), codexMapping))
		if s.codexCPAStrict(r.Context()) {
			r = r.WithContext(withCodexStrictCPA(r.Context()))
		}
	}
	// Strict CPA sends Responses context and client tool results directly to the
	// original upstream session. In that mode even a moderation rewrite would make
	// the submitted tool payload differ from what the client paired with its call.
	if !codexStrictCPAFromContext(r.Context()) {
		if isChat {
			raw = s.moderateHistory(r.Context(), raw, "chat")
		} else {
			raw = s.moderateHistory(r.Context(), raw, "responses")
		}
	}
	// Native CPA turns must preserve the submitted Responses payload.  The
	// compatibility fields below are useful for ordinary stateless traffic, but
	// adding them beside a previous_response_id or a client tool result creates a
	// different upstream turn rather than an exact native resume.
	strictNativeCPA := codexStrictCPAFromContext(r.Context()) && !isChat
	if !strictNativeCPA && unsupportedCodexReasoningEffort(model, pol.ForceEffort) {
		writeCodexReasoningEffortUnsupported(w, model, normalizeEffort(pol.ForceEffort))
		return
	}
	if !isChat && !strictNativeCPA && !isCompact {
		raw = ensureEncryptedReasoningInclude(raw)
	}

	group := requestUserGroupPolicy(r.Context())
	instructionPlan, err := s.codexInstructionPlan(r.Context(), group, codexMapping, strictNativeCPA, model)
	if err != nil {
		// A configured instruction file is part of the administrator's session
		// policy. Report missing/empty files before any account lease or upstream
		// attempt; existing strict trees instead load their durable snapshot and do
		// not touch changed/deleted files at all.
		writeCodexInstructionConfigurationError(w, err)
		return
	}
	if codexMapping != nil {
		codexMapping.setInstructionPlan(instructionPlan)
	}
	if strictNativeCPA {
		w.Header().Set("X-MiCliProxy-CPA-Instructions", instructionPlan.Source)
	}
	// Responses requests are shaped once at the entrance. Chat requests are
	// converted to Responses later in codexAttempt, where the same immutable plan
	// is applied exactly once after conversion.
	if !isChat && instructionPlan.applies() {
		raw = setResponsesInstructions(raw, instructionPlan.Instructions)
	}

	affinity := codexSelectionAffinity(r, raw, affinityWithMeta(r, raw, bodyMetaForView(capturedMeta, originalRaw, raw)), affinityGroup)
	currentHeader := r.Header.Clone()
	// movable: the request carries its full input and so can be re-sent to a fresh
	// account losslessly. This is the failover gate. It is broader than !strict: a
	// strict-sticky turn (tool_result/function_call_output, kept on one account for
	// prompt-cache warmth) is still movable, and MUST be allowed to fail over on an
	// error — pinning it was the cause of "429 leaks downstream, no auto-switch". Only
	// genuine server-side-state turns (previous_response_id / x-codex-turn-state) are
	// non-movable in the normal failover loop. A separate, one-shot durable replay
	// handles a disappeared binding or a confirmed upstream context loss.
	movable := !serverSideStateWithMeta(path, r, raw, bodyMetaForView(capturedMeta, originalRaw, raw))
	if s.codexSessionMappingEnabled(r.Context()) && !movable && (codexMapping == nil || codexMapping.binding == nil) {
		// resolveCodexSessionMapping should have caught this already; keep a strict
		// defensive boundary in case a middleware supplied an unusual body shape.
		s.auditCodexMappingFailure(r.Context(), "codex_session_mapping_unidentified")
		s.writeCodexSessionMappingError(w, isStreamRequest(raw), "codex_session_mapping_unidentified")
		return
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
	// x-codex-turn-state) stay on their native session here; only the dedicated
	// durable-replay recovery below may replace a lost state epoch.
	// Accounts this request has already tried and failed on. Passed to the scheduler
	// each attempt so a just-failed account is never re-selected within the same
	// request — not even via its sticky binding (the conversation rebinds to the
	// fresh account instead). codexAttempt adds the leased account here before it
	// returns outcomeRetry.
	exclude := map[string]bool{}
	attempts := s.codexFailoverAttempts(r.Context(), movable, pol.Group, model, exclude)
	current := codexRetryRequest{Raw: raw, Header: currentHeader}
	modelCapabilityRejected := false
	attemptsRemaining := attempts
	contextRecoveryAttempted := false
	for attemptsRemaining > 0 {
		attemptsRemaining--
		headerReq := r.Clone(r.Context())
		headerReq.Header = current.Header
		attemptSource, attemptMeta := codexBodySourceForAttempt(r.Context(), originalRaw, current.Raw)
		currentStrict := strictStickyWithMeta(path, headerReq, current.Raw, attemptMeta)
		currentMovable := !serverSideStateWithMeta(path, headerReq, current.Raw, attemptMeta)
		currentModel := modelWithMeta(current.Raw, attemptMeta)
		if currentModel == "" {
			currentModel = model
		}
		currentAffinity := codexSelectionAffinity(headerReq, current.Raw, affinityWithMeta(headerReq, current.Raw, attemptMeta), affinityGroup)
		if currentAffinity.Hash == "" {
			currentAffinity = affinity
		}
		result := s.codexAttempt(w, r, current.Raw, attemptSource, attemptMeta, current.Header, current.Prepared, isChat && !current.Prepared, isCompact, currentAffinity, currentStrict, currentMovable, currentModel, pol.Group, pol.ForceEffort, instructionPlan, attemptsRemaining > 0, exclude)
		if result.Outcome == outcomeModelRetry {
			modelCapabilityRejected = true
		}
		if result.Outcome == outcomeRetry && result.WaitForCapacity && attemptsRemaining == 0 {
			// Distinct-account retries are bounded, but quota recovery is an
			// admission wait rather than another eager upstream retry. Give the
			// scheduler one more pass so it can select another healthy account or
			// remain in its cancellation-aware FIFO until a cooled account has
			// passed recheck.
			attemptsRemaining = 1
		}
		if result.Outcome == outcomeContextRecovery {
			// A strict CPA turn normally stays on its original account.  When that
			// account is gone, or its upstream context has been confirmed missing,
			// rebuild the encrypted durable history into one fresh root instead of
			// returning a permanent 409/previous_response_not_found to the client.
			if !contextRecoveryAttempted {
				contextRecoveryAttempted = true
				migration, recovered, recoveryErr := s.recoverCodexSessionMapping(r.Context(), r, current.Raw, current.Header, pol, codexSessionMappingFromContext(r.Context()), result.RecoveryContextError, result.RecoveryReason)
				if errors.Is(recoveryErr, errCodexToolContextUnrecoverable) {
					code := codexMappingErrorCode(recoveryErr)
					s.auditCodexMappingFailure(r.Context(), code)
					s.writeCodexSessionMappingError(w, isStreamRequest(current.Raw), code)
					return
				}
				if recoveryErr != nil {
					log.Printf("[CODEX-SESSION-MAPPING] context migration request_id=%s: %v", requestIDFromContext(r.Context()), recoveryErr)
				}
				if recovered {
					codexMapping = migration.Mapping
					freshPlan, planErr := s.codexInstructionPlan(r.Context(), group, codexMapping, strictNativeCPA, routing.Model(migration.Retry.Raw))
					if planErr != nil {
						writeCodexInstructionConfigurationError(w, planErr)
						return
					}
					instructionPlan = freshPlan
					codexMapping.setInstructionPlan(instructionPlan)
					current = migration.Retry
					current.Prepared = false
					if !isChat && instructionPlan.applies() {
						current.Raw = setResponsesInstructions(current.Raw, instructionPlan.Instructions)
					}
					r = r.WithContext(withCodexSessionMapping(r.Context(), codexMapping))
					if s.codexCPAStrict(r.Context()) {
						r = r.WithContext(withCodexStrictCPA(r.Context()))
					}
					w.Header().Set("X-MiCliProxy-Context-Status", migration.Mode)
					if migration.Mode == "rebuilt" {
						atomic.AddUint64(&s.contextRebuilt, 1)
					} else {
						atomic.AddUint64(&s.contextDegraded, 1)
					}
					atomic.AddUint64(&s.codexMappingFreshRoots, 1)
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
						Action: "codex_context_migrated", State: "recovered", Reason: firstNonEmpty(result.RecoveryReason, "upstream_context_unavailable"),
						Detail: "durable_replay_new_epoch",
					})
					// The recovered turn is now self-contained. Give it the ordinary
					// failover budget rather than a single no-retry attempt: the first
					// replacement account may be exhausted too while a later one is
					// healthy. The exclusion set still prevents revisiting any account
					// already failed by this downstream turn.
					attemptsRemaining = s.codexFailoverAttempts(r.Context(), true, pol.Group, routing.Model(current.Raw), exclude)
					continue
				}
			}
			if result.RecoveryBoundUnavailable {
				writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this session is unavailable")
				return
			}
			writeRaw(w, result.RecoveryStatus, result.RecoveryHeader, result.RecoveryBody)
			return
		}
		if result.Outcome != outcomeRetry && result.Outcome != outcomeModelRetry {
			return
		}
		if len(result.Retry.Raw) > 0 {
			current = result.Retry
		}
	}
	if modelCapabilityRejected && s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, false, pol.Group, "codex", model, "") {
		return
	}
	writePoolCodeError(w, http.StatusBadGateway, "retry_exhausted", "upstream retry limit exhausted")
}

func (s *Server) codexFailoverAttempts(ctx context.Context, movable bool, group, model string, exclude map[string]bool) int {
	attempts := 1
	if s.flagEnabled(ctx, "seamless_failover", s.cfg.SeamlessFailover) && movable {
		if attempts = s.settingInt(ctx, "failover_max_attempts", s.cfg.FailoverMaxAttempts); attempts < 1 {
			attempts = 1
		}
	}
	if !movable {
		return attempts
	}
	candidates, err := s.scheduler.EligibleCandidateCount(ctx, scheduler.Route{Group: group, Provider: "codex", Model: model, Exclude: exclude})
	if err == nil {
		if candidates == 0 {
			return 1
		}
		if attempts > candidates {
			attempts = candidates
		}
		// A self-contained request gets one replacement when the pool actually has
		// one, even if an older deployment saved failover_max_attempts=1.
		if candidates > 1 && attempts < 2 {
			attempts = 2
		}
	}
	return attempts
}

type attemptOutcome int

const (
	outcomeDone            attemptOutcome = iota // request finished (success, or terminal error already written)
	outcomeRetry                                 // recoverable error on a self-contained request — retry on a fresh account
	outcomeModelRetry                            // account-scoped model_not_found — retry, then require manual fallback
	outcomeContextRecovery                       // strict CPA needs a fresh, durable replay epoch
)

type codexRetryRequest struct {
	Raw      []byte
	Header   http.Header
	Prepared bool
}

type codexAttemptResult struct {
	Outcome         attemptOutcome
	WaitForCapacity bool
	Retry           codexRetryRequest
	// Recovery* is populated only with outcomeContextRecovery. The outer gateway
	// owns the mapping epoch transition so it can recompile instruction policy and
	// install the fresh mapping before issuing the replay.
	RecoveryContextError     leakfilter.ResponsesContextErrorKind
	RecoveryReason           string
	RecoveryStatus           int
	RecoveryHeader           http.Header
	RecoveryBody             []byte
	RecoveryBoundUnavailable bool
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
func codexBodySourceForAttempt(ctx context.Context, original, current []byte) (bodysource.BodySource, *bodysource.BodyMeta) {
	source := bodySourceFromContext(ctx)
	meta, ok := bodyMetaFromContext(ctx)
	if source == nil || !ok || meta.Size != source.Size() || !sameBodyBytes(original, current) || int64(len(current)) != source.Size() {
		return nil, nil
	}
	return source, &meta
}

func sameBodyBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	return len(left) == 0 || &left[0] == &right[0] || bytes.Equal(left, right)
}

func (s *Server) codexAttempt(w http.ResponseWriter, r *http.Request, raw []byte, replaySource bodysource.BodySource, replayMeta *bodysource.BodyMeta, baseHeader http.Header, prepared bool, isChat, isCompact bool, affinity routing.AffinityKey, strict, movable bool, model, routeGroup, forceEffort string, instructionPlan *CodexInstructionPlan, allowRetry bool, exclude map[string]bool) codexAttemptResult {
	path := r.URL.Path
	sourceRaw := raw
	includeChatStreamUsage := isChat && chatStreamUsageRequested(raw)
	// Decode the "stream" flag once for this attempt: isStreamRequest full-parses
	// the (potentially multi-MB) body, and it was previously called up to 7× on the
	// same unchanged `raw` within this function. `raw` is a stable parameter here.
	streamReq := streamRequestWithMeta(raw, replayMeta)
	waitHeartbeat := schedulerWaitCallback(r.Context())
	var waitAuditOnce sync.Once
	onSchedulerWait := func(reason string, waited time.Duration) {
		waitAuditOnce.Do(func() {
			auditModel := strings.TrimSpace(model)
			if len(auditModel) > 128 {
				auditModel = auditModel[:128]
			}
			_ = s.store.InsertAuditLog(context.WithoutCancel(r.Context()), storage.AuditLogRow{
				Action: "codex_scheduler_wait", State: "queued", Reason: strings.TrimSpace(reason),
				Detail: fmt.Sprintf("model=%q waited_ms=%d", auditModel, waited.Milliseconds()),
			})
		})
		if waitHeartbeat != nil {
			waitHeartbeat(reason, waited)
		}
	}
	route := scheduler.Route{
		Group:             routeGroup,
		Provider:          "codex",
		Affinity:          affinity,
		Strict:            strict,
		ServerSideState:   !movable,
		ImmutableAffinity: !movable,
		Movable:           movable,
		Model:             model,
		EstimatedTokens:   estimatedTokensWithMeta(raw, replayMeta),
		Compaction:        compactionRequestWithMeta(path, raw, replayMeta),
		Exclude:           exclude,
		OnWait:            onSchedulerWait,
		SkipWait:          userGroupFallbackProbe(r.Context()),
	}
	route = codexMappingRequiredRoute(codexSessionMappingFromContext(r.Context()), route)
	lease, err := s.scheduler.Select(r.Context(), route)
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			if mapping := codexSessionMappingFromContext(r.Context()); mapping != nil && mapping.enabled && mapping.binding != nil {
				return codexAttemptResult{
					Outcome:                  outcomeContextRecovery,
					RecoveryReason:           "bound_account_unavailable",
					RecoveryStatus:           http.StatusConflict,
					RecoveryBoundUnavailable: true,
				}
			}
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this session is unavailable")
			return codexAttemptResult{Outcome: outcomeDone}
		}
		if s.handleCapabilitySelectionError(r.Context(), w, err, false, routeGroup, "codex", model, "") {
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
	resolvedModel := firstNonEmpty(lease.ResolvedModel, model)
	if resolvedModel != model {
		raw = setForcedModel(raw, resolvedModel)
		model = resolvedModel
	}
	w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
	modelDiag := modelDiagnosticsFromCtx(r.Context())
	r = r.WithContext(withModelDiagnostics(r.Context(), modelDiag.Requested, resolvedModel, modelDiag.Source))
	// retry marks the leased account as failed-for-this-request (so the next attempt
	// excludes it) and signals the caller to fail over to a fresh account.
	retry := func() codexAttemptResult {
		if exclude != nil {
			exclude[lease.Account.ID] = true
		}
		return codexAttemptResult{Outcome: outcomeRetry}
	}
	retryAfterCapacity := func() codexAttemptResult {
		// A persisted quota cooldown is itself the exclusion. Keeping the account
		// in the request-local exclusion set would make it permanently invisible
		// after recheck succeeds, so let the scheduler observe its transient state
		// and queue until it is genuinely eligible again.
		if exclude != nil {
			delete(exclude, lease.Account.ID)
		}
		s.scheduler.InvalidateAccountCache()
		return codexAttemptResult{Outcome: outcomeRetry, WaitForCapacity: true}
	}
	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	token, err = s.ensureAgentIdentityTask(r.Context(), lease.Account, token, lease.Egress, lease.Binding.CookieJarKey, "")
	if err != nil {
		log.Printf("agent identity task prepare %s: %v", lease.Account.ID, err)
		if allowRetry && movable {
			return retry()
		}
		writeError(w, http.StatusBadGateway, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	// Start with the raw body. Every downstream transform (system-prompt inject,
	// chat→responses conversion, Virtualize) returns its own freshly-allocated
	// slice via json.Marshal, so the initial clone is unnecessary — it would
	// only be overwritten. The rare unmarshal-failure fallback path returns the
	// original slice, which is read-only from this point on.
	body := raw
	strictNativeCPA := codexStrictCPAFromContext(r.Context()) && !isChat
	promptCacheKeySource := "none"
	if promptCacheKeyWithMeta(body, replayMeta) != "" {
		promptCacheKeySource = "downstream"
	}
	retentionEffective := topLevelStringWithMeta(body, replayMeta, "prompt_cache_retention")
	retentionSource := "unsupported_current_codex"
	if retentionEffective != "" {
		retentionSource = "downstream_unsupported"
	}
	group := requestUserGroupPolicy(r.Context())
	if !prepared {
		if !strictNativeCPA && !instructionPlan.applies() && prompt.ShouldRewrite(group.SystemPrompt, isCompact, group.SystemPromptApplyToCompaction) {
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
			if !streamReq {
				body = forceResponsesStream(body)
			}
			path = "/v1/responses"
		}
		if !strictNativeCPA && isChat && instructionPlan.applies() {
			body = setResponsesInstructions(body, instructionPlan.Instructions)
		}
	}

	// Gateway reliability layer (opt-in, default off): inject the developer rules +
	// per-turn <gateway_request> envelope, classify task/risk, accumulate
	// working_state, and derive the risk-based reasoning-effort floor. Skipped for
	// compaction turns (a summarization pass should not carry the envelope). relTurn
	// stays inactive when the flag is off, so the response guard below is a no-op too.
	var relTurn reliabilityTurn
	effectiveEffort := forceEffort
	if !prepared && !strictNativeCPA && !isCompact && s.reliabilityEnabled(r.Context()) {
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
	if !prepared && !strictNativeCPA && effectiveEffort != "" {
		body = applyForcedReasoningResponses(body, effectiveEffort)
	}

	// 联网搜索 (AT path): ensure a web_search tool is present so the account
	// session serves it without API-key/org verification. Skipped for compact.
	if !prepared && !strictNativeCPA && !isCompact && shouldInjectCodexHostedWebSearch(model, token, body) &&
		s.flagEnabled(r.Context(), "web_search_enabled", s.cfg.WebSearchEnabled) {
		if injected, werr := prompt.EnsureResponsesWebSearchTool(body, s.cfg.WebSearchToolType); werr == nil {
			body = injected
		}
	}

	// Current codex-rs (0.144.x) has no prompt_cache_retention field on HTTP or
	// Responses-over-WebSocket. The upstream choke point strips any legacy value;
	// do not inject one here or report an unsupported 24h policy as effective. Cache
	// reuse is driven by the supported prompt_cache_key + stable account affinity.
	if !prepared && !strictNativeCPA {
		if updated, normalized := normalizeOfficialCodexPromptCacheKey(r, body, model); normalized {
			body = updated
			promptCacheKeySource = "official_codex_stable_prefix"
		}
	}
	currentMeta := bodyMetaForView(replayMeta, sourceRaw, body)
	if !prepared && !strictNativeCPA && promptCacheKeyWithMeta(body, currentMeta) == "" {
		if prefixHash := automaticPromptCachePrefixHash(body); prefixHash != "" {
			updated := ensureResponsesPromptCacheKey(body, automaticPromptCacheKey(model, prefixHash))
			if routing.PromptCacheKey(updated) != "" {
				body = updated
				promptCacheKeySource = "auto_stable_prefix"
			}
		}
	}
	var usageMeta *bodysource.BodyMeta
	if replayMeta != nil && len(body) == len(sourceRaw) && (len(body) == 0 || &body[0] == &sourceRaw[0]) {
		usageMeta = replayMeta
	}
	logicalUsageDiag := codexRequestUsageDiagnostics(body, usageMeta, affinity, promptCacheKeySource, retentionEffective, retentionSource)

	// Resolve the persistent identity only after the exact account and egress are
	// selected. A transport/auth retry below reuses this snapshot and therefore the
	// same turn_id; a separately issued native EOF continue allocates its own turn.
	osHint := s.osHint(raw, lease.Egress)
	mapping := codexSessionMappingFromContext(r.Context())
	codexIdentity, identityErr := mapping.identitySnapshot(s.identitySecret(), lease, osHint)
	if identityErr != nil {
		writePoolCodeError(w, http.StatusConflict, codexMappingErrorCode(identityErr), "Codex session identity is no longer available")
		return codexAttemptResult{Outcome: outcomeDone}
	}
	// The mapping snapshot includes an account+egress-bound virtual device. Once it
	// exists, a Cloudflare standby retry must not silently move only the egress and
	// leave the durable identity describing a different device boundary.
	mappingTransportStrict := strict || (mapping != nil && mapping.enabled)

	// Per-account conversation isolation ("串号隔离", default on, runtime-toggleable):
	// namespace the forwarded conversation-correlation identifiers so a rate-limited
	// / risk-flagged session on one account can never contaminate another account
	// that later serves the same conversation. Also resolves the (optionally
	// OS-hinted) identity used to build the upstream headers.
	hdr := baseHeader.Clone()
	if s.isolationEnabled(r.Context()) && !codexStrictCPAFromContext(r.Context()) {
		id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
		body = isolateCodexConversation(hdr, body, id)
	}

	// Optional Codex sensitive-word scrub (off by default → raw fast path). Per
	// project policy the Codex request's working directory and paths are never
	// rewritten; only operator sensitive words are replaced, in the request body
	// and — via the same matcher — the response stream. An empty matcher (scrub
	// off / no words) is a zero-cost pass-through.
	codexScrubber := streamrewrite.New(nil)
	if s.flagEnabled(r.Context(), "codex_identity_scrub", s.cfg.CodexIdentityScrub) && !codexStrictCPAFromContext(r.Context()) {
		scrub := cloak.ScrubSensitive(body, s.cfg.SensitiveWordsFor("codex"))
		body = scrub.Body
		codexScrubber = scrub.Scrubber
	}

	codexClientVersion := s.codexClientVersionForModel(model)
	webSocketSession := codexResponsesWebSocketSession(r.Context())
	codexUseWebSocket := forceCodexResponsesWebSocket(r.Context()) || !isChat && !isCompact && streamReq && capability.CodexPrefersWebSocket(model)
	if webSocketSession != nil && webSocketSession.UseHTTPSFallback() {
		codexUseWebSocket = false
	}
	// A preferred Responses WebSocket opened for an ordinary HTTP request is a
	// one-shot upstream connection. Some Codex backend response ids are scoped to
	// that connection, so a later HTTP continuation would necessarily open a new
	// socket and lose its previous_response_id despite retaining the account,
	// egress and virtual device. Strict CPA therefore uses HTTP/SSE for the whole
	// HTTP-originated tree. A real downstream WebSocket carries the session object
	// below and still reuses its matching upstream connection across turns.
	if strictNativeCPA && webSocketSession == nil {
		codexUseWebSocket = false
	}
	// A sidecar-bound account presents the real Codex JA3 only on the HTTP/SSE path
	// (postViaSidecar replays it); the WS dialer cannot, so it would dial with Go-stdlib
	// TLS and throw away the fingerprint the sidecar binding exists for. Prefer the SSE
	// path for those accounts (the version-gated models still work over SSE). A
	// downstream Responses WebSocket can bridge this SSE stream without coupling
	// the two transport legs.
	if codexUseWebSocket && s.flagEnabled(r.Context(), "codex_prefer_sidecar_ja3_over_ws", s.cfg.CodexPreferSidecarJA3OverWS) &&
		strings.EqualFold(strings.TrimSpace(lease.Egress.Type), "curl_cffi_sidecar") {
		codexUseWebSocket = false
	}
	if forceCodexResponsesWebSocket(r.Context()) && webSocketSession != nil && !codexUseWebSocket {
		webSocketSession.MarkHTTPSFallback()
	}
	requestForTokenWithIdentity := func(t storage.AccountToken, requestBody []byte, requestHeaders http.Header, requestIdentity *upstream.CodexIdentitySnapshot) upstream.Request {
		requestOSHint := osHint
		if requestIdentity != nil {
			// A strict mapped tree keeps the root-elected device profile even when
			// a later tool result happens to contain a different OS marker.
			requestOSHint = requestIdentity.DeviceOSHint
		}
		return upstream.Request{
			Method:                  http.MethodPost,
			DownstreamPath:          pathWithQuery(path, r.URL.RawQuery),
			Headers:                 requestHeaders,
			Body:                    bodysource.Bytes(requestBody),
			Account:                 lease.Account,
			Token:                   t,
			Egress:                  lease.Egress,
			CookieJarKey:            lease.Binding.CookieJarKey,
			OSHint:                  requestOSHint,
			CodexClientVersion:      codexClientVersion,
			CodexResponsesWebSocket: codexUseWebSocket,
			CodexWebSocketSession:   webSocketSession,
			CodexIdentity:           requestIdentity,
		}
	}
	forwardSource, forwardMeta := bodysource.BodySource(nil), (*bodysource.BodyMeta)(nil)
	if replaySource != nil && sameBodyBytes(body, sourceRaw) {
		forwardSource, forwardMeta = replaySource, replayMeta
	}
	requestForToken := func(t storage.AccountToken) upstream.Request {
		req := requestForTokenWithIdentity(t, body, hdr, codexIdentity)
		if forwardSource != nil {
			req.Body, req.BodyMeta = forwardSource, forwardMeta
		}
		return req
	}
	// Explicit administrator rules are a downstream policy overlay, including on
	// strict CPA turns.  Strict CPA still guarantees that the request sent upstream
	// is the native session/tool payload; it must not silently suppress the
	// administrator's configured response policy after an upstream result arrives.
	//
	// A stateful previous_response_id stays on its native account during ordinary
	// rule-driven failover. A different account is used only by the dedicated
	// durable-replay recovery after the binding vanished or the upstream proved that
	// its state is missing. A self-contained turn can transparently retry as usual.
	applyCodexRule := func(decision upstreamErrorRuleDecision, status int, header http.Header, errorBody []byte, streaming bool) (codexAttemptResult, bool) {
		switch decision.Match.DownstreamAction {
		case upstreamrules.DownstreamActionFailover:
			if allowRetry && movable {
				return retry(), true
			}
			if strictNativeCPA && !movable {
				// Never translate an administrator failover preference into a false
				// claim that the upstream context epoch is invalid. The caller below
				// has already attempted the only safe recovery (same binding); fall
				// through to the rule's normal visible/builtin terminal behavior.
				return codexAttemptResult{}, false
			}
		case upstreamrules.DownstreamActionPass,
			upstreamrules.DownstreamActionCustomError,
			upstreamrules.DownstreamActionNeutralize,
			upstreamrules.DownstreamActionIdleStream,
			upstreamrules.DownstreamActionHeartbeatFinish:
			if decision.Match.DownstreamAction == upstreamrules.DownstreamActionIdleStream && streaming {
				// The rule owns the downstream keepalive from here on; do not keep
				// the upstream account lease occupied for its configured idle window.
				releaseLease()
			}
			if s.writeRuleDownstream(r.Context(), w, "codex", status, header, errorBody, codexScrubber, decision, streaming) {
				return codexAttemptResult{Outcome: outcomeDone}, true
			}
		}
		return codexAttemptResult{}, false
	}

	logicalUsageDiag.RouteEpoch = lease.RouteEpoch
	releaseCacheFlight, waitedForCacheFlight := s.enterCodexCacheSingleflight(r.Context(), s.codexCacheSingleflightEnabled(r.Context()), lease.Account.ID, resolvedModel, body, affinity)
	if waitedForCacheFlight {
		logicalUsageDiag.SingleflightWaitedRequests = 1
	}
	defer releaseCacheFlight()
	holdID := s.createBillingHold(r.Context(), affinity.Hash, lease.Account.ID, lease.RouteEpoch, estimatedTokensWithMeta(body, forwardMeta))
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
	var webSocketFallbackAuditOnce sync.Once
	errWebSocketFallbackContextRecovery := errors.New("websocket continuation requires durable HTTPS recovery")
	webSocketFallbackRequiresContextRecovery := func() bool {
		if !strictNativeCPA || mapping == nil || !responsesRecoveryEligible(body, hdr) {
			return false
		}
		mapping.mu.Lock()
		defer mapping.mu.Unlock()
		return mapping.binding != nil
	}
	webSocketFallbackContextResult := func() codexAttemptResult {
		_ = s.settleBillingHold(r.Context(), holdID, "websocket_fallback_context_recovering")
		return codexAttemptResult{
			Outcome:              outcomeContextRecovery,
			RecoveryContextError: leakfilter.ResponsesContextErrorPreviousResponseNotFound,
			RecoveryReason:       "websocket_https_fallback",
			RecoveryStatus:       http.StatusConflict,
			RecoveryHeader:       http.Header{"Content-Type": []string{"application/json"}},
			RecoveryBody:         []byte(`{"error":{"type":"codex_context_unavailable","message":"The upstream conversation transport changed."}}`),
		}
	}
	fallbackToHTTPS := func(reason string) (*upstream.Response, storage.EgressProfile, error) {
		codexUseWebSocket = false
		if webSocketSession != nil {
			webSocketSession.MarkHTTPSFallback()
		}
		withCodexUsageContext()
		webSocketFallbackAuditOnce.Do(func() {
			_ = s.store.InsertAuditLog(context.WithoutCancel(r.Context()), storage.AuditLogRow{
				AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
				Action: "codex_upstream_websocket_fallback", State: "attempted", Reason: reason,
				Detail: "same_account_same_egress_https_sse_bridge",
			})
		})
		// A response id created on the retired WebSocket is not a portable HTTP
		// parameter. Rotate through the durable checkpoint before issuing any HTTPS
		// request, rather than deliberately eliciting a 400 and reacting afterward.
		if webSocketFallbackRequiresContextRecovery() {
			s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "https_fallback_context_recovery", 0)
			return nil, lease.Egress, errWebSocketFallbackContextRecovery
		}
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "https_fallback_attempted", 0)
		fallbackRequest := requestForToken(token)
		fallbackRequest.CodexResponsesWebSocket = false
		fallbackResponse, fallbackEgress, fallbackErr := s.doWithCFRetry(r.Context(), fallbackRequest, lease, mappingTransportStrict)
		if fallbackErr != nil {
			s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "https_fallback_transport_error", 0)
			return nil, fallbackEgress, fallbackErr
		}
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, fallbackEgress, "https_fallback_headers", fallbackResponse.StatusCode)
		return fallbackResponse, fallbackEgress, nil
	}
	attemptedWebSocket := codexUseWebSocket
	s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "attempted", 0)
	resp, finalEgress, err := s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
	releaseCacheFlight()
	if err != nil && attemptedWebSocket {
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "websocket_transport_error", 0)
		resp, finalEgress, err = fallbackToHTTPS("transport_error")
		if errors.Is(err, errWebSocketFallbackContextRecovery) {
			return webSocketFallbackContextResult()
		}
	}
	if err != nil {
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, lease.Egress, "transport_error", 0)
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		if allowRetry && movable {
			return retry() // transport error — a fresh account/egress may succeed
		}
		writeError(w, http.StatusBadGateway, err)
		return codexAttemptResult{Outcome: outcomeDone}
	}
	if attemptedWebSocket && resp.StatusCode >= http.StatusBadRequest {
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "websocket_handshake_response", resp.StatusCode)
		originalStatus := resp.StatusCode
		originalHeader := resp.Header.Clone()
		originalBody := readUpstreamErrorBody(resp.Body)
		originalEgress := finalEgress
		_ = resp.Body.Close()
		fallbackResponse, fallbackEgress, fallbackErr := fallbackToHTTPS("handshake_http_status")
		if errors.Is(fallbackErr, errWebSocketFallbackContextRecovery) {
			return webSocketFallbackContextResult()
		}
		if fallbackErr != nil {
			log.Printf("codex websocket HTTPS fallback %s: %v", lease.Account.ID, fallbackErr)
			resp = &upstream.Response{
				StatusCode: originalStatus,
				Header:     originalHeader,
				Body:       io.NopCloser(bytes.NewReader(originalBody)),
			}
			finalEgress = originalEgress
		} else {
			resp, finalEgress = fallbackResponse, fallbackEgress
		}
	}
	s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "response_headers", resp.StatusCode)
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	codexResetRetried := false
	statefulRuleRecoveryAttempted := false
	handleResponsesContextAttemptError := func(status int, header http.Header, body []byte, reason string) (codexAttemptResult, bool) {
		contextError := responsesContextError(status, body)
		if contextError == leakfilter.ResponsesContextErrorNone {
			return codexAttemptResult{}, false
		}
		entrypoint := "responses"
		if isChat {
			entrypoint = "chat_completions"
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider: "codex", Entrypoint: entrypoint, Model: model, Status: status,
			Header: header, Body: body, Streaming: streamReq,
		})
		explicitRuleAction := ruleMatched && decision.Match.DownstreamAction != upstreamrules.DownstreamActionBuiltin
		// This is an upstream fact, not a downstream presentation choice: once
		// the original upstream no longer knows a previous response, no later
		// request may reuse that mapping. Strict CPA now promotes a durable
		// checkpoint into a fresh epoch in the same client request, before any
		// bytes are committed. That preserves long-running work across an
		// upstream context loss (notably the ultra path) rather than surfacing a
		// permanent previous_response_not_found.
		if contextError == leakfilter.ResponsesContextErrorPreviousResponseNotFound {
			if explicitRuleAction {
				if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "upstream_context_invalid"); err != nil &&
					!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
					log.Printf("[CODEX-SESSION-MAPPING] upstream context retirement request_id=%s: %v", requestIDFromContext(r.Context()), err)
				}
			} else if mapping := codexSessionMappingFromContext(r.Context()); strictNativeCPA && mapping != nil && mapping.enabled && mapping.binding != nil {
				_ = s.settleBillingHold(r.Context(), holdID, "responses_context_error_recovering")
				return codexAttemptResult{
					Outcome:              outcomeContextRecovery,
					RecoveryContextError: contextError,
					RecoveryReason:       "previous_response_not_found",
					RecoveryStatus:       status,
					RecoveryHeader:       header.Clone(),
					RecoveryBody:         append([]byte(nil), body...),
				}, true
			}
			if !explicitRuleAction {
				if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "upstream_context_invalid"); err != nil &&
					!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
					log.Printf("[CODEX-SESSION-MAPPING] upstream context retirement request_id=%s: %v", requestIDFromContext(r.Context()), err)
				}
			}
		}
		// An administrator may deliberately choose a different outcome even for
		// a native context error (for example a custom error or an explicit
		// failover/epoch rotation). Evaluate that policy before the built-in CPA
		// context-loss behavior. No request body is reconstructed in either case.
		if ruleMatched {
			s.applyRuleAccountAction(r.Context(), lease.Account, status, header, body, decision)
			_ = s.settleBillingHold(r.Context(), holdID, "responses_context_error_rule")
			if result, handled := applyCodexRule(decision, status, header, body, streamReq); handled {
				return result, true
			}
		}
		_ = reason
		_ = s.settleBillingHold(r.Context(), holdID, "responses_context_error_terminal")
		// A failed native recovery remains terminal, but account-local response and
		// tool identifiers must never be exposed to the downstream client.
		s.writeFilteredError(r.Context(), w, "codex", status, header, body, codexScrubber)
		return codexAttemptResult{Outcome: outcomeDone}, true
	}

codexResponse:
	if resp.StatusCode >= 400 {
		errorBody := readUpstreamErrorBody(resp.Body)
		errorBody = redactAgentIdentityError(token, errorBody)
		if handled, ok := handleResponsesContextAttemptError(resp.StatusCode, resp.Header, errorBody, "http_context_error"); ok {
			return handled
		}
		if lease.Account.IgnoreRateLimitControls && codexIgnoredRateLimitResponse(resp.StatusCode, errorBody) {
			_ = resp.Body.Close()
			resp, finalEgress, err = s.retryCodexSameAccountAfterRateLimit(r.Context(), lease, func() upstream.Request {
				return requestForToken(token)
			}, resp.StatusCode, resp.Header, errorBody)
			if err != nil {
				_ = s.settleBillingHold(r.Context(), holdID, "ignored_rate_limit_retry_interrupted")
				if r.Context().Err() != nil {
					return codexAttemptResult{Outcome: outcomeDone}
				}
				writeError(w, http.StatusBadGateway, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if resp.StatusCode < http.StatusBadRequest {
				goto codexSuccess
			}
			goto codexResponse
		}
		detection := cf.Detect(resp.StatusCode, resp.Header, errorBody)
		v := ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
		if isInvalidAgentIdentityTask(resp.StatusCode, errorBody, token) {
			oldTaskID := token.AgentTaskID
			if recovered, recoverErr := s.ensureAgentIdentityTask(r.Context(), lease.Account, token, finalEgress, lease.Binding.CookieJarKey, oldTaskID); recoverErr == nil {
				token = recovered
				_ = resp.Body.Close()
				resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "failed_after_agent_task_recovery")
					if allowRetry && movable {
						return retry()
					}
					writeError(w, http.StatusBadGateway, err)
					return codexAttemptResult{Outcome: outcomeDone}
				}
				if resp.StatusCode < 400 {
					goto codexSuccess
				}
				errorBody = redactAgentIdentityError(token, readUpstreamErrorBody(resp.Body))
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			} else {
				log.Printf("agent identity task recovery %s: %v", lease.Account.ID, recoverErr)
			}
		}
		if v.State == ban.AuthExpired && !cf.EdgeOnly(detection) && !upstream.AccountUsesAPIKey(token) && !isAgentIdentityToken(token) {
			if refreshed, rerr := s.refreshCodexToken(r.Context(), token); rerr == nil && refreshed.Refreshed {
				token = refreshed.Token
				_ = resp.Body.Close()
				resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "failed_after_refresh")
					if allowRetry && movable {
						return retry()
					}
					writeError(w, http.StatusBadGateway, err)
					return codexAttemptResult{Outcome: outcomeDone}
				}
				if resp.StatusCode < 400 {
					goto codexSuccess
				}
				errorBody = redactAgentIdentityError(token, readUpstreamErrorBody(resp.Body))
				if handled, ok := handleResponsesContextAttemptError(resp.StatusCode, resp.Header, errorBody, "http_context_error_after_auth_refresh"); ok {
					return handled
				}
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			} else if rerr != nil {
				log.Printf("codex auth refresh %s: %v", lease.Account.ID, rerr)
				s.handleCodexRefreshFailure(r.Context(), lease.Account, refreshed, rerr, "gateway")
			}
		}
		if codexClientVersion == "" && codexRequiresNewerVersion(errorBody) {
			codexClientVersion = s.cfg.ClientVersion
			if !isChat && !isCompact && streamRequestWithMeta(body, forwardMeta) &&
				(webSocketSession == nil || !webSocketSession.UseHTTPSFallback()) {
				codexUseWebSocket = true
			}
			withCodexUsageContext()
			_ = resp.Body.Close()
			resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
			if err != nil {
				_ = s.settleBillingHold(r.Context(), holdID, "failed_after_version_retry")
				if allowRetry && movable {
					return retry()
				}
				writeError(w, http.StatusBadGateway, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if resp.StatusCode < 400 {
				goto codexSuccess
			}
			errorBody = redactAgentIdentityError(token, readUpstreamErrorBody(resp.Body))
			if handled, ok := handleResponsesContextAttemptError(resp.StatusCode, resp.Header, errorBody, "http_context_error_after_version_retry"); ok {
				return handled
			}
			detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
			v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
		}
		if !codexResetRetried && codexResetTriggerAllowed(resp.StatusCode, errorBody) &&
			!upstream.AccountUsesAPIKey(token) &&
			!isAgentIdentityToken(token) &&
			s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, resp.StatusCode, resp.Header, errorBody, "http_error") {
			codexResetRetried = true
			if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
				token = latest
			}
			_ = resp.Body.Close()
			resp, finalEgress, err = s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
			if err != nil {
				log.Printf("codex reset-credit retry %s: %v", lease.Account.ID, err)
			} else if resp.StatusCode < 400 {
				goto codexSuccess
			} else {
				errorBody = readUpstreamErrorBody(resp.Body)
				if handled, ok := handleResponsesContextAttemptError(resp.StatusCode, resp.Header, errorBody, "http_context_error_after_reset_retry"); ok {
					return handled
				}
				detection = cf.Detect(resp.StatusCode, resp.Header, errorBody)
				v = ban.Classify(false, resp.StatusCode, resp.Header, errorBody)
			}
		}
		if isModelNotFoundError(resp.StatusCode, errorBody) {
			s.rejectAccountModel(r.Context(), lease.Account, model, resp.StatusCode)
			_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
			if allowRetry && movable {
				retry()
				return codexAttemptResult{Outcome: outcomeModelRetry}
			}
			_ = s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, false, routeGroup, "codex", model, "")
			return codexAttemptResult{Outcome: outcomeDone}
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
			Streaming:  streamReq,
		})
		if cf.EdgeOnly(detection) {
			if !lease.Account.IgnoreRateLimitControls {
				_ = s.store.BenchBindingForRecheck(r.Context(), lease.Account.ID, storage.Now()+60)
			}
			v = ban.Verdict{State: ban.RegionBlocked, Reason: "cloudflare_edge"}
		} else if ruleMatched {
			v = s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody, decision)
		} else {
			v = s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
		}
		// Downstream session/thread identifiers are lookup aliases only. If the
		// locally generated upstream root receives a risk-class error, rotate the
		// mapped epoch before committing any bytes downstream. Recovery removes the
		// old previous_response_id and rebuilds from durable context when available,
		// so stale server-side state is never attached to the new UUID.
		mapping := codexSessionMappingFromContext(r.Context())
		rotationAllowed := !ruleMatched ||
			decision.Match.DownstreamAction == upstreamrules.DownstreamActionBuiltin ||
			decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover
		hasFailoverCandidate := false
		if rotationAllowed && strictNativeCPA && mapping != nil && mapping.mainCLI() && codexMappedSessionRiskError(resp.StatusCode, errorBody) {
			hasFailoverCandidate = s.hasCodexFailoverCandidate(r.Context(), routeGroup, model, lease.Account.ID)
		}
		if rotationAllowed && strictNativeCPA && mapping != nil && mapping.mainCLI() &&
			codexMappedSessionRotationRequired(resp.StatusCode, resp.Header, errorBody, movable, hasFailoverCandidate) {
			// Once the request is rebuilt as a fresh root it is safe to move accounts.
			// Prefer that boundary when one exists so a session-risk response cannot
			// immediately bind the replacement UUID back to the same failing account.
			if hasFailoverCandidate && (retryableForFailover(v, resp.StatusCode) || codexExplicitSessionRisk(errorBody)) {
				exclude[lease.Account.ID] = true
			}
			_ = s.settleBillingHold(r.Context(), holdID, "mapped_session_risk_rotating")
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
				AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
				Action: "codex_mapped_session_rotation", State: "requested",
				Reason: fmt.Sprintf("http_%d", resp.StatusCode), Detail: "main_cli_new_upstream_uuid",
			})
			return codexAttemptResult{
				Outcome:              outcomeContextRecovery,
				RecoveryReason:       "mapped_session_risk",
				RecoveryStatus:       resp.StatusCode,
				RecoveryHeader:       resp.Header.Clone(),
				RecoveryBody:         append([]byte(nil), errorBody...),
				RecoveryContextError: leakfilter.ResponsesContextErrorNone,
			}
		}
		// An administrator may ask to fail over after a stateful failure, but CPA
		// cannot send that previous_response_id to another account or real exit.
		// Reissue the exact same logical turn once through its leased binding before
		// deciding on the rule's visible terminal. The stable Codex identity/turn id
		// makes this a same-chain recovery, not local context reconstruction.
		if ruleMatched && decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover &&
			strictNativeCPA && !movable && !statefulRuleRecoveryAttempted {
			statefulRuleRecoveryAttempted = true
			_ = resp.Body.Close()
			if retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, true); retryErr == nil && retryResp != nil {
				resp, finalEgress = retryResp, retryEgress
				goto codexResponse
			} else if retryResp != nil && retryResp.Body != nil {
				_ = retryResp.Body.Close()
			}
		}
		_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
		if ruleMatched {
			if decision.Match.DownstreamAction == upstreamrules.DownstreamActionIdleStream && streamReq {
				if resp.Body != nil {
					_ = resp.Body.Close()
					resp.Body = nil
				}
			}
			if result, handled := applyCodexRule(decision, resp.StatusCode, resp.Header, errorBody, streamReq); handled {
				return result
			}
		}
		if movable && retryableForFailover(v, resp.StatusCode) {
			if v.State == ban.RateLimited || resp.StatusCode == http.StatusTooManyRequests {
				return retryAfterCapacity()
			}
			if !allowRetry {
				s.writeFilteredError(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if v.State == ban.PermissionDenied && !s.hasCodexFailoverCandidate(r.Context(), routeGroup, model, lease.Account.ID) {
				s.writeFilteredError(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			// Recoverable error → move to a fresh account. The downstream never sees this.
			return retry()
		}
		s.writeFilteredError(r.Context(), w, "codex", resp.StatusCode, resp.Header, errorBody, codexScrubber)
		return codexAttemptResult{Outcome: outcomeDone}
	}
codexSuccess:
	s.verifyAccountModel(r.Context(), lease.Account, model, "")
	normalizeCodexStreamContentType(resp.Header, streamRequestWithMeta(body, forwardMeta))
	resetHeaderExhaustion := false
	if !codexResetRetried && !upstream.AccountUsesAPIKey(token) && !isAgentIdentityToken(token) && exhaustedCooldown(resp.Header, storage.Now()) > 0 {
		resetHeaderExhaustion = s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, resp.StatusCode, resp.Header, nil, "success_header_exhaustion")
	}
	if !resetHeaderExhaustion {
		s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header)
	}
	s.captureQuota(r.Context(), lease.Account.ID, "codex", model, resp.Header)

	if isChat && !streamReq {
		responseBody, err := s.readUpstreamResponseBody(resp.Body)
		if err != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "failed_response_too_large")
			writeError(w, http.StatusBadGateway, err)
			return codexAttemptResult{Outcome: outcomeDone}
		}
		if s.leakScrubEnabled(r.Context()) {
			responseBody, _ = responsefilter.StripSafetyBufferingJSON(responseBody)
		}
		// Session 33: Detect rate-limit/quota-exhausted signals in a 200 response
		// body (ChatGPT backend-api may return usage_limit_exceeded with HTTP 200
		// instead of 429). When detected, cool the account and fail over to a fresh
		// one so the downstream never sees the error.
		if cd := usageLimitCooldown(200, responseBody); cd > 0 {
			if lease.Account.IgnoreRateLimitControls {
				_ = resp.Body.Close()
				resp, finalEgress, err = s.retryCodexSameAccountAfterRateLimit(r.Context(), lease, func() upstream.Request {
					return requestForToken(token)
				}, resp.StatusCode, resp.Header, responseBody)
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "ignored_rate_limit_retry_interrupted")
					if r.Context().Err() != nil {
						return codexAttemptResult{Outcome: outcomeDone}
					}
					writeError(w, http.StatusBadGateway, err)
					return codexAttemptResult{Outcome: outcomeDone}
				}
				if resp.StatusCode < http.StatusBadRequest {
					goto codexSuccess
				}
				goto codexResponse
			}
			if !codexResetRetried && codexResetTriggerAllowed(200, responseBody) &&
				!isAgentIdentityToken(token) &&
				s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, 200, resp.Header, responseBody, "soft_200_body") {
				codexResetRetried = true
				if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
					token = latest
				}
				_ = resp.Body.Close()
				retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
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
			s.benchOnLimitForAccount(r.Context(), lease.Account, 200, resp.Header, responseBody)
			_ = s.settleBillingHold(r.Context(), holdID, "rate_limited_in_200_body")
			// Rate limit in a 200 body is admission pressure, just like a 429. A
			// movable request may switch immediately when another account is
			// healthy; otherwise it waits for quota recovery instead of leaking a
			// synthetic 503 merely because the distinct-account retry budget ended.
			if movable {
				return retryAfterCapacity()
			}
			if !strictNativeCPA && s.leakScrubEnabled(r.Context()) {
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
				repairReq.SetBodyBytes(appendDeveloperTurn(body, s.reliabilityEnvelopeRole(r.Context()), reliability.RepairInstruction(findings)))
				if r2, _, e2 := s.doWithCFRetry(r.Context(), repairReq, lease, mappingTransportStrict); e2 == nil && r2 != nil {
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
		s.persistCodexStateBindings(r.Context(), r, body, affinity, responseBody, resp.Header, lease, finalEgress, model, isCompact)
		s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
		_ = s.settleBillingHold(r.Context(), holdID, "settled")
		// A soft 200 "failed" response carrying limit/quota/switch-model state must
		// not reach the downstream (envelope-only check; never inspects content).
		if !strictNativeCPA && s.leakScrubEnabled(r.Context()) {
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
		var prefix []byte
		relayBody := io.Reader(resp.Body)
		entrypoint := "responses"
		if isChat {
			entrypoint = "chat_completions"
		}
		// A configured terminal rule must be able to handle an HTTP-200 SSE
		// response.failed before its raw frame commits downstream. Compatibility
		// traffic always uses the existing bounded probe. Strict CPA uses it only
		// when an administrator has a scope-compatible terminal rule; otherwise it
		// retains the immediate native relay for long-running sessions.
		if !strictNativeCPA || (strictNativeCPA && !movable) || lease.Account.IgnoreRateLimitControls || s.hasPotentialTerminalResponseRule(r.Context(), "codex", entrypoint, model) {
			// Hold back only a bounded early prefix so a retryable failure frame can still
			// fail over before any downstream bytes are committed. As soon as real content
			// appears (or the 64KiB/8-frame probe budget is reached), stream live. The old
			// full-response capture destroyed TTFT and manufactured 503s once a successful
			// stream exceeded its memory/disk capture cap. This probe runs before both the
			// native Responses and Chat Completions adapters. Downstream-visible safety
			// progress also releases the probe so a long safety check can be kept alive;
			// once either adapter writes HTTP 200, transparent failover is no longer possible.
			var streamFailure leakfilter.CodexFailureFrame
			var terminalStream bool
			var probeErr error
			if keepalive := s.streamKeepAliveInterval(r.Context()); keepalive > 0 {
				idleRelease := earlySSECreatedIdleRelease
				if keepalive < idleRelease {
					idleRelease = keepalive
				}
				prefix, relayBody, streamFailure, terminalStream, probeErr = probeEarlyCodexSSEFailureWithIdleRelease(resp.Body, idleRelease)
			} else {
				prefix, streamFailure, terminalStream, probeErr = probeEarlyCodexSSEFailure(resp.Body)
			}
			if probeErr != nil {
				if codexUseWebSocket {
					s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "websocket_stream_probe_error", 0)
					_ = resp.Body.Close()
					fallbackResponse, fallbackEgress, fallbackErr := fallbackToHTTPS("stream_probe_error")
					if errors.Is(fallbackErr, errWebSocketFallbackContextRecovery) {
						return webSocketFallbackContextResult()
					}
					if fallbackErr == nil {
						resp, finalEgress = fallbackResponse, fallbackEgress
						if resp.StatusCode < http.StatusBadRequest {
							goto codexSuccess
						}
						goto codexResponse
					}
					log.Printf("codex websocket stream-probe HTTPS fallback %s: %v", lease.Account.ID, fallbackErr)
				}
				_ = s.settleBillingHold(r.Context(), holdID, "stream_probe_failed")
				if allowRetry && movable {
					return retry()
				}
				writePublicUnavailable(w, http.StatusServiceUnavailable)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if terminalStream {
				failureHeader := resp.Header.Clone()
				for name, values := range streamFailure.Header {
					failureHeader.Del(name)
					for _, value := range values {
						failureHeader.Add(name, value)
					}
				}
				failureStatus := streamFailure.StatusCode
				failureBody := streamFailure.Body
				decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
					Provider:   "codex",
					Entrypoint: entrypoint,
					Model:      model,
					Status:     failureStatus,
					Header:     failureHeader,
					Body:       failureBody,
					Streaming:  true,
				})
				explicitRuleAction := ruleMatched && decision.Match.DownstreamAction != upstreamrules.DownstreamActionBuiltin
				// A rule may override the current response presentation, but it must
				// not leave a proven-dead upstream response alias active. The automatic
				// recovery branch below retires as part of its new-epoch transition.
				if streamFailure.ContextError == leakfilter.ResponsesContextErrorPreviousResponseNotFound && explicitRuleAction {
					if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "upstream_context_invalid"); err != nil &&
						!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
						log.Printf("[CODEX-SESSION-MAPPING] streamed context retirement request_id=%s: %v", requestIDFromContext(r.Context()), err)
					}
				}
				if streamFailure.ContextError != leakfilter.ResponsesContextErrorNone && !explicitRuleAction {
					if streamFailure.ContextError == leakfilter.ResponsesContextErrorPreviousResponseNotFound {
						if mapping := codexSessionMappingFromContext(r.Context()); strictNativeCPA && mapping != nil && mapping.enabled && mapping.binding != nil {
							_ = resp.Body.Close()
							_ = s.settleBillingHold(r.Context(), holdID, "stream_context_error_recovering")
							return codexAttemptResult{
								Outcome:              outcomeContextRecovery,
								RecoveryContextError: streamFailure.ContextError,
								RecoveryReason:       "previous_response_not_found",
								RecoveryStatus:       failureStatus,
								RecoveryHeader:       failureHeader.Clone(),
								RecoveryBody:         append([]byte(nil), failureBody...),
							}
						}
						if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "upstream_context_invalid"); err != nil &&
							!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
							log.Printf("[CODEX-SESSION-MAPPING] streamed context retirement request_id=%s: %v", requestIDFromContext(r.Context()), err)
						}
					}
					// Preserve native context-loss handling by default, but do not let it
					// silently bypass an administrator's explicitly selected account
					// action. A builtin action remains the historical no-op here.
					if ruleMatched && decision.Match.AccountAction != upstreamrules.AccountActionBuiltin {
						s.applyRuleAccountAction(r.Context(), lease.Account, failureStatus, failureHeader, failureBody, decision)
					}
					_ = resp.Body.Close()
					_ = s.settleBillingHold(r.Context(), holdID, "responses_context_error_terminal")
					writeRaw(w, failureStatus, failureHeader, failureBody)
					return codexAttemptResult{Outcome: outcomeDone}
				}
				if lease.Account.IgnoreRateLimitControls && codexIgnoredRateLimitResponse(failureStatus, failureBody) {
					_ = resp.Body.Close()
					resp, finalEgress, err = s.retryCodexSameAccountAfterRateLimit(r.Context(), lease, func() upstream.Request {
						return requestForToken(token)
					}, failureStatus, failureHeader, failureBody)
					if err != nil {
						_ = s.settleBillingHold(r.Context(), holdID, "ignored_rate_limit_retry_interrupted")
						if r.Context().Err() != nil {
							return codexAttemptResult{Outcome: outcomeDone}
						}
						writeError(w, http.StatusBadGateway, err)
						return codexAttemptResult{Outcome: outcomeDone}
					}
					if resp.StatusCode < http.StatusBadRequest {
						goto codexSuccess
					}
					goto codexResponse
				}
				handleStreamFailure := streamFailure.BuiltinRetryable || explicitRuleAction
				if ruleMatched && !handleStreamFailure {
					// An account-only rule can still observe an ordinary client error, but
					// downstream_action=builtin preserves the default pass-through policy.
					s.applyRuleAccountAction(r.Context(), lease.Account, failureStatus, failureHeader, failureBody, decision)
				}
				if handleStreamFailure {
					shouldRetry := !ruleMatched ||
						decision.Match.DownstreamAction == upstreamrules.DownstreamActionBuiltin ||
						decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover

					// A reset credit is an in-place recovery on the same account and therefore
					// precedes cooldown/failover. Explicit pass/custom/neutralize/idle rules do
					// not consume credits behind the operator's back.
					if shouldRetry {
						// The WebSocket-to-SSE pipe is unbuffered. Closing before another request
						// prevents its terminal [DONE] write from retaining the WS request mutex.
						_ = resp.Body.Close()
					}
					if shouldRetry && !codexResetRetried && !isAgentIdentityToken(token) && codexResetTriggerAllowed(failureStatus, failureBody) &&
						s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, failureStatus, failureHeader, failureBody, "stream_retryable_limit") {
						codexResetRetried = true
						if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
							token = latest
						}
						retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
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
					mapping := codexSessionMappingFromContext(r.Context())
					rotationAllowed := !ruleMatched ||
						decision.Match.DownstreamAction == upstreamrules.DownstreamActionBuiltin ||
						decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover
					hasFailoverCandidate := false
					if rotationAllowed && strictNativeCPA && mapping != nil && mapping.mainCLI() && codexMappedSessionRiskError(failureStatus, failureBody) {
						hasFailoverCandidate = s.hasCodexFailoverCandidate(r.Context(), routeGroup, model, lease.Account.ID)
					}
					if rotationAllowed && strictNativeCPA && mapping != nil && mapping.mainCLI() &&
						codexMappedSessionRotationRequired(failureStatus, failureHeader, failureBody, movable, hasFailoverCandidate) {
						failureVerdict := ban.Classify(false, failureStatus, failureHeader, failureBody)
						if hasFailoverCandidate && (retryableForFailover(failureVerdict, failureStatus) || codexExplicitSessionRisk(failureBody)) {
							exclude[lease.Account.ID] = true
						}
						if ruleMatched {
							s.applyRuleAccountAction(r.Context(), lease.Account, failureStatus, failureHeader, failureBody, decision)
						} else {
							s.onUpstreamError(r.Context(), lease.Account, failureStatus, failureHeader, failureBody)
						}
						_ = resp.Body.Close()
						_ = s.settleBillingHold(r.Context(), holdID, "stream_mapped_session_risk_rotating")
						_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
							AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
							Action: "codex_mapped_session_rotation", State: "requested",
							Reason: fmt.Sprintf("stream_%d", failureStatus), Detail: "main_cli_new_upstream_uuid",
						})
						return codexAttemptResult{
							Outcome:              outcomeContextRecovery,
							RecoveryReason:       "mapped_session_risk",
							RecoveryStatus:       failureStatus,
							RecoveryHeader:       failureHeader.Clone(),
							RecoveryBody:         append([]byte(nil), failureBody...),
							RecoveryContextError: leakfilter.ResponsesContextErrorNone,
						}
					}
					// The early-frame probe has not committed any downstream bytes, so a
					// stateful failover rule may make one safe retry through the exact
					// same account+egress. Do not convert an ordinary repeated upstream
					// failure into an epoch retirement: only a confirmed context-loss
					// response is allowed to retire the CPA tree.
					if ruleMatched && decision.Match.DownstreamAction == upstreamrules.DownstreamActionFailover &&
						strictNativeCPA && !movable && !statefulRuleRecoveryAttempted {
						statefulRuleRecoveryAttempted = true
						retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, true)
						if retryErr == nil && retryResp != nil && retryResp.StatusCode < http.StatusBadRequest {
							resp, finalEgress = retryResp, retryEgress
							goto codexSuccess
						}
						if retryResp != nil && retryResp.Body != nil {
							_ = retryResp.Body.Close()
						}
					}
					failureVerdict := ban.Classify(false, failureStatus, failureHeader, failureBody)
					if ruleMatched {
						failureVerdict = s.applyRuleAccountAction(r.Context(), lease.Account, failureStatus, failureHeader, failureBody, decision)
					} else {
						failureVerdict = s.onUpstreamError(r.Context(), lease.Account, failureStatus, failureHeader, failureBody)
					}
					_ = s.settleBillingHold(r.Context(), holdID, "stream_retryable_error_retry")
					if ruleMatched {
						_ = resp.Body.Close()
						if result, handled := applyCodexRule(decision, failureStatus, failureHeader, failureBody, true); handled {
							return result
						}
					}
					_ = resp.Body.Close()
					if movable && failureVerdict.State == ban.RateLimited {
						return retryAfterCapacity()
					}
					if allowRetry && movable {
						return retry()
					}
					writePublicUnavailable(w, http.StatusServiceUnavailable)
					return codexAttemptResult{Outcome: outcomeDone}
				}
			}
		}
		streamBody := io.MultiReader(bytes.NewReader(prefix), relayBody)
		if isChat {
			// Third-party OpenAI client streaming against a GPT model: convert the
			// upstream Responses SSE into chat.completion.chunk frames it can parse
			// (incl. streamed tool calls), teeing through a usage scanner so streamed
			// chat traffic records tokens like the native path.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			uscan := usage.NewStreamScanner("codex")
			nativeRecorder := s.newCodexStreamLedgerRecorder(r.Context())
			defer nativeRecorder.Close()
			responsesStreamToChatSSE(w, io.TeeReader(streamBody, io.MultiWriter(uscan, nativeRecorder)), model, includeChatStreamUsage, codexScrubber)
			if nativeRecorder.completedSuccessfully() {
				responseID, responseModel, _ := nativeRecorder.metadata()
				if mapping := codexSessionMappingFromContext(r.Context()); mapping != nil && mapping.enabled {
					turnState := nativeRecorder.responseTurnState()
					var responseJSON []byte
					if turnState == "" {
						responseJSON = nativeRecorder.ResponseJSON()
						turnState = responseTurnState(resp.Header, responseJSON)
					}
					if err := s.commitCodexSessionMapping(r.Context(), mapping, lease, finalEgress, responseID, turnState, isCompact); err != nil {
						log.Printf("[CODEX-SESSION-MAPPING] chat stream terminal commit request_id=%s: %v", requestIDFromContext(r.Context()), err)
					}
				} else {
					s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "terminal_success", http.StatusOK)
					s.persistCodexBindingAliases(r.Context(), affinity, responseID, responseModel, lease, finalEgress, model)
				}
				if s.goalContinuityEnabled(r.Context()) {
					s.persistCodexGoalContinuity(r.Context(), r, body, nativeRecorder.ResponseJSON())
				}
			}
			if parsed, ok := uscan.Parsed(); ok {
				s.recordParsedUsage(r.Context(), lease.Account.ID, affinity.Hash, parsed)
			}
			_ = s.settleBillingHold(r.Context(), holdID, "settled_streaming")
			return codexAttemptResult{Outcome: outcomeDone}
		}
		streamRecorder := s.newCodexStreamLedgerRecorder(r.Context())
		defer streamRecorder.Close()
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
		commitStreamMapping := func(recorder *codexStreamLedgerRecorder, upstreamHeader http.Header, committedEgress storage.EgressProfile) error {
			if recorder == nil || !recorder.completedSuccessfully() {
				return nil
			}
			responseID, responseModel, _ := recorder.metadata()
			var mappingErr error
			if mapping := codexSessionMappingFromContext(r.Context()); mapping != nil && mapping.enabled {
				turnState := recorder.responseTurnState()
				if turnState == "" {
					turnState = responseTurnState(upstreamHeader, recorder.ResponseJSON())
				}
				if err := s.commitCodexSessionMapping(r.Context(), mapping, lease, committedEgress, responseID, turnState, isCompact); err != nil {
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "codex_session_mapping_commit_failed", State: "retryable", Reason: codexMappingErrorCode(err), Detail: "stream_terminal_metadata_only"})
					mappingErr = err
				}
			} else {
				// Compatibility mode retains only the old affinity metadata; no Codex
				// goal/checkpoint/journal write is permitted.
				s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, committedEgress, "terminal_success", http.StatusOK)
				s.persistCodexBindingAliases(r.Context(), affinity, responseID, responseModel, lease, committedEgress, model)
			}
			// A mapping-alias conflict must not discard the encrypted recovery
			// checkpoint. In particular, downstream Responses WebSockets reach this
			// native stream path, and losing a completed tool call here makes a later
			// quota failover impossible to rebuild safely on another account.
			if s.goalContinuityEnabled(r.Context()) {
				s.persistCodexGoalContinuity(r.Context(), r, body, recorder.ResponseJSON())
			}
			return mappingErr
		}
		commitWriter := newTerminalCommitWriter(w, func() error {
			return commitStreamMapping(streamRecorder, resp.Header, finalEgress)
		})
		streamErr := s.streamSSE(streamCtx, commitWriter, recordingStream, codexScrubber, "codex", lease.Account.ID, affinity.Hash)
		streamRecorder.finish()
		if closeErr := commitWriter.Close(); streamErr == nil {
			streamErr = closeErr
		}
		if persistErr := commitWriter.PersistenceError(); persistErr != nil {
			log.Printf("[CODEX-SESSION-MAPPING] stream terminal commit request_id=%s: %v", requestIDFromContext(r.Context()), persistErr)
		}
		// A retryable/risk terminal can arrive only after text or a tool call was
		// already relayed. Replaying that turn would duplicate output and billing,
		// but keeping the same upstream UUID would trap the next retry in the same
		// risk loop. Retire only a durable main-CLI epoch; children and prospective
		// first turns never invalidate the root tree.
		if failure, ok := streamRecorder.terminalFailure(); ok {
			mapping := codexSessionMappingFromContext(r.Context())
			if strictNativeCPA && mapping != nil && mapping.durableMainCLI() && codexMappedSessionRiskError(failure.StatusCode, failure.Body) {
				if err := s.retireCodexSessionMapping(r.Context(), mapping, "late_stream_mapped_session_risk"); err != nil &&
					!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
					log.Printf("[CODEX-SESSION-MAPPING] late stream rotation request_id=%s: %v", requestIDFromContext(r.Context()), err)
				}
			}
		}
		streamStalled := errors.Is(streamErr, errUpstreamStreamStalled)
		if streamStalled {
			// Stop the old producer before issuing a continuation. Two live producers
			// on one logical turn could duplicate text, tool calls, and billing.
			_ = resp.Body.Close()
			streamErr = nil
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
				AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
				Action: "goal_stream_stall_detected", State: "recovering",
				Reason: "upstream_idle_without_terminal", Detail: "old stream cancelled before continuation",
			})
		}
		// Sidecar v2 reports an error discovered after its response headers through
		// HTTP trailers. A body read error on a sidecar transport is treated the
		// same way: the sidecar may have lost its own client connection before it
		// could deliver a trailer. Neither condition is a real upstream EOF, so
		// never native-continue it, never rotate the CPA epoch, and never append a
		// bare HTTP error after SSE bytes have reached the client.
		sidecarStreamFailure := resp.SidecarStreamFailure()
		sidecarStreamInterrupted := sidecarStreamFailure != nil || (storage.IsSidecarEgress(finalEgress) && streamErr != nil)
		sidecarFailureCode := "sidecar_stream_interrupted"
		sidecarFailurePhase := "stream"
		sidecarFailureRetryable := false
		if sidecarStreamFailure != nil {
			sidecarFailureCode = sidecarStreamFailure.Code
			sidecarFailurePhase = firstNonEmpty(sidecarStreamFailure.Phase, sidecarFailurePhase)
			sidecarFailureRetryable = sidecarStreamFailure.Retryable
		}
		if sidecarStreamInterrupted {
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
				AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
				Action: "sidecar_stream_failure", State: "terminal", Reason: sidecarFailureCode,
				Detail: "phase=" + sidecarFailurePhase + " retryable=" + strconv.FormatBool(sidecarFailureRetryable),
			})
		}
		// Strict CPA relays terminal SSE failures immediately rather than holding
		// them in the legacy early-retry probe. Preserve the same epoch rule here:
		// only an upstream-confirmed missing previous response retires the whole
		// tree; a missing tool output remains an ordinary visible client error.
		if streamRecorder.terminalContextError() == leakfilter.ResponsesContextErrorPreviousResponseNotFound {
			if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "upstream_context_invalid"); err != nil &&
				!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
				log.Printf("[CODEX-SESSION-MAPPING] streamed context retirement request_id=%s: %v", requestIDFromContext(r.Context()), err)
			}
		}
		_, _, streamRateLimits := streamRecorder.metadata()
		continuationFailed := false
		nativeContinueAttempted := false
		continuationSidecarInterrupted := false
		compatContinuationFailed := false
		mapping := codexSessionMappingFromContext(r.Context())
		if !sidecarStreamInterrupted && !streamRecorder.reachedTerminal() && r.Context().Err() == nil {
			responseID, responseModel, _ := streamRecorder.metadata()
			if mapping != nil && mapping.enabled && streamErr != nil {
				_ = s.emitCodexNativeContinuationFailure(r.Context(), w, responseID, responseModel, "stream_interrupted")
			} else if mapping != nil && mapping.enabled {
				if responseID == "" || hasPendingClientToolCall(streamRecorder.partialItems()) {
					continuationFailed = true
					_ = s.emitCodexNativeContinuationFailure(r.Context(), w, responseID, responseModel, "truncated_eof_uncontinuable")
				} else if continuationBody, buildErr := nativeCodexContinueBody(body, responseID, s.autoContinueText(r.Context())); buildErr != nil {
					continuationFailed = true
					_ = s.emitCodexNativeContinuationFailure(r.Context(), w, responseID, responseModel, "native_continue_build_failed")
				} else {
					turnState := streamRecorder.responseTurnState()
					if turnState == "" {
						turnState = responseTurnState(resp.Header, streamRecorder.ResponseJSON())
					}
					continuationHeaders := hdr.Clone()
					if turnState != "" {
						continuationHeaders.Set("X-Codex-Turn-State", turnState)
					}
					continuationSnapshot := mapping.nextContinueSnapshot(turnState)
					continuationReq := requestForTokenWithIdentity(token, continuationBody, continuationHeaders, continuationSnapshot)
					nativeContinueAttempted = true
					continueReason := "truncated_eof"
					if streamStalled {
						continueReason = "upstream_idle_stall"
					}
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "codex_native_continue", State: "attempted", Reason: continueReason, Detail: "same_account_egress_epoch"})
					s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "native_continue_attempted", 0)
					continuationResp, continuationEgress, continuationErr := s.doWithCFRetry(r.Context(), continuationReq, lease, true)
					atomic.AddUint64(&s.codexNativeContinues, 1)
					if continuationErr != nil || continuationResp == nil || continuationResp.StatusCode >= http.StatusBadRequest || !isEventStream(continuationResp.Header) {
						continuationSidecarInterrupted = continuationErr != nil && storage.IsSidecarEgress(continuationEgress)
						if !continuationSidecarInterrupted {
							continuationFailed = true
						}
						if continuationResp != nil && continuationResp.Body != nil {
							_ = continuationResp.Body.Close()
						}
						if continuationSidecarInterrupted {
							_ = writeCodexSidecarStreamFailure(w, responseID, responseModel, "request")
						} else {
							_ = s.emitCodexNativeContinuationFailure(r.Context(), w, responseID, responseModel, "native_continue_request_failed")
						}
					} else {
						s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, continuationEgress, "native_continue_headers", continuationResp.StatusCode)
						continuationRecorder := s.newCodexStreamLedgerRecorder(r.Context())
						defer continuationRecorder.Close()
						continuationWriter := newTerminalCommitWriter(w, func() error {
							return commitStreamMapping(continuationRecorder, continuationResp.Header, continuationEgress)
						})
						continuationStreamErr := s.streamSSE(streamCtx, continuationWriter, io.TeeReader(continuationResp.Body, continuationRecorder), codexScrubber, "codex", lease.Account.ID, affinity.Hash)
						continuationRecorder.finish()
						if closeErr := continuationWriter.Close(); continuationStreamErr == nil {
							continuationStreamErr = closeErr
						}
						_ = continuationResp.Body.Close()
						if persistErr := continuationWriter.PersistenceError(); persistErr != nil {
							log.Printf("[CODEX-SESSION-MAPPING] native continue terminal commit request_id=%s: %v", requestIDFromContext(r.Context()), persistErr)
						}
						_, _, continuationRateLimits := continuationRecorder.metadata()
						if continuationRateLimits.any() {
							streamRateLimits = continuationRateLimits
						}
						continuationSidecarFailure := continuationResp.SidecarStreamFailure()
						continuationSidecarInterrupted = continuationSidecarFailure != nil || (storage.IsSidecarEgress(continuationEgress) && continuationStreamErr != nil)
						if continuationSidecarInterrupted {
							if !continuationRecorder.reachedTerminal() {
								phase := "stream"
								if continuationSidecarFailure != nil {
									phase = firstNonEmpty(continuationSidecarFailure.Phase, phase)
								}
								_ = writeCodexSidecarStreamFailure(w, responseID, responseModel, phase)
							}
						} else if continuationStreamErr != nil || !continuationRecorder.completedSuccessfully() {
							continuationFailed = true
							if !continuationRecorder.reachedTerminal() {
								_ = s.emitCodexNativeContinuationFailure(r.Context(), w, responseID, responseModel, "native_continue_stream_failed")
							}
						}
					}
				}
			} else if mapping == nil || !mapping.enabled {
				// Stateless compatibility mode has the full request context locally, so
				// it can recover a truncated stream without relying on an upstream
				// previous_response_id. Keep the exact account and egress: once any SSE
				// bytes have reached the client, replaying on another account could
				// duplicate model output or tool effects.
				rfForContinue, _ := streamCtx.Value(responseRuleFilterKey{}).(*responseRuleFilter)
				continueEnabled := s.goalContinuityEnabled(r.Context()) || s.autoContinueEnabled(r.Context(), autoContinueDecisionFromFilter(rfForContinue))
				if continueEnabled {
					continueReason := "truncated_eof"
					if streamStalled {
						continueReason = "upstream_idle_stall"
					} else if streamErr != nil {
						continueReason = "upstream_stream_error"
					}
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
						AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
						Action: "codex_stateless_continue", State: "attempted", Reason: continueReason,
						Detail: "same_account_same_egress",
					})
					continuationToken := token
					continuationHeader := http.Header(nil)
					reissue := func(cctx context.Context, continuationBody []byte) (io.ReadCloser, error) {
						if latest, tokenErr := s.store.GetToken(cctx, lease.Account.ID); tokenErr == nil {
							continuationToken = latest
						}
						if continuationToken.ExpiresAt > 0 && continuationToken.ExpiresAt <= time.Now().Add(time.Minute).Unix() &&
							!upstream.AccountUsesAPIKey(continuationToken) && !isAgentIdentityToken(continuationToken) {
							if refreshed, refreshErr := s.refreshCodexToken(cctx, continuationToken); refreshErr == nil && refreshed.Refreshed {
								continuationToken = refreshed.Token
							}
						}
						for authAttempt := 0; authAttempt < 2; authAttempt++ {
							continuationReq := requestForTokenWithIdentity(continuationToken, continuationBody, hdr.Clone(), codexIdentity)
							continuationReq.Egress = finalEgress
							continuationResp, continuationErr := s.doCodexUpstream(cctx, continuationReq)
							if continuationErr != nil {
								return nil, continuationErr
							}
							if continuationResp.StatusCode < http.StatusBadRequest && isEventStream(continuationResp.Header) {
								continuationHeader = continuationResp.Header.Clone()
								heartbeatEvery := s.streamKeepAliveInterval(cctx)
								if ruleInterval := responseRuleHeartbeatInterval(rfForContinue); ruleInterval > 0 {
									heartbeatEvery = ruleInterval
								}
								return newSemanticSSERelayReadCloser(cctx, continuationResp.Body, rfForContinue, "codex", s.streamStallRecoveryInterval(cctx), heartbeatEvery), nil
							}
							errorHeader := continuationResp.Header.Clone()
							errorBody := readUpstreamErrorBody(continuationResp.Body)
							_ = continuationResp.Body.Close()
							if authAttempt == 0 && isInvalidAgentIdentityTask(continuationResp.StatusCode, errorBody, continuationToken) {
								if recovered, recoverErr := s.ensureAgentIdentityTask(cctx, lease.Account, continuationToken, finalEgress, lease.Binding.CookieJarKey, continuationToken.AgentTaskID); recoverErr == nil {
									continuationToken = recovered
									continue
								}
							}
							verdict := ban.Classify(false, continuationResp.StatusCode, errorHeader, errorBody)
							if authAttempt == 0 && verdict.State == ban.AuthExpired && !upstream.AccountUsesAPIKey(continuationToken) && !isAgentIdentityToken(continuationToken) {
								if refreshed, refreshErr := s.refreshCodexToken(cctx, continuationToken); refreshErr == nil && refreshed.Refreshed {
									continuationToken = refreshed.Token
									continue
								}
							}
							s.onUpstreamError(cctx, lease.Account, continuationResp.StatusCode, errorHeader, errorBody)
							return nil, fmt.Errorf("codex continuation unavailable (status %d)", continuationResp.StatusCode)
						}
						return nil, errors.New("codex continuation authentication retry exhausted")
					}
					scrubbedWriter := newScrubbingFrameWriter(w, s.leakScrubEnabled(r.Context()), codexScrubber, "codex")
					continuationRecorder, continueErr := s.maybeAutoContinueCodex(r.Context(), scrubbedWriter, body, streamRecorder, reissue)
					if continuationRecorder != nil && continuationRecorder != streamRecorder {
						defer continuationRecorder.Close()
					}
					if continueErr != nil {
						compatContinuationFailed = true
						log.Printf("[AUTO-CONTINUE] codex stateless request_id=%s: %v", requestIDFromContext(r.Context()), continueErr)
						_ = closeCodexStreamGracefully(scrubbedWriter, streamRecorder.partialItems(), streamRecorder.partialText(), responseID, responseModel)
					} else if continuationRecorder != nil && continuationRecorder.completedSuccessfully() {
						streamErr = nil
						if err := commitStreamMapping(continuationRecorder, continuationHeader, finalEgress); err != nil {
							log.Printf("[GOAL-CONTINUITY] codex stateless continuation persistence degraded request_id=%s: %v", requestIDFromContext(r.Context()), err)
						}
						s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, continuationRecorder.ResponseJSON())
					} else {
						compatContinuationFailed = true
					}
					scrubbedWriter.Flush()
				} else {
					compatContinuationFailed = true
					_ = closeCodexStreamGracefully(w, streamRecorder.partialItems(), streamRecorder.partialText(), responseID, responseModel)
				}
				if compatContinuationFailed {
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
						AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
						Action: "goal_stream_terminal_synthesized", State: "retryable",
						Reason: "codex_stateless_continuation_unavailable", Detail: "generic response.failed emitted",
					})
					s.markGoalStreamRetryable(r.Context(), r, "codex", body, "continuation_unavailable")
				}
			}
		} else if sidecarStreamInterrupted {
			if mapping != nil && mapping.enabled {
				id, streamModel, _ := streamRecorder.metadata()
				_ = writeCodexSidecarStreamFailure(w, id, streamModel, sidecarFailurePhase)
			}
		} else if streamErr != nil {
			if mapping != nil && mapping.enabled {
				id, streamModel, _ := streamRecorder.metadata()
				_ = s.emitCodexNativeContinuationFailure(r.Context(), w, id, streamModel, "stream_interrupted")
			}
		}
		if nativeContinueAttempted && continuationFailed && !continuationSidecarInterrupted {
			if err := s.retireCodexSessionMapping(r.Context(), codexSessionMappingFromContext(r.Context()), "native_eof_continue_failed"); err != nil && !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
				log.Printf("[CODEX-SESSION-MAPPING] native continue rotation request_id=%s: %v", requestIDFromContext(r.Context()), err)
			}
		}
		// Real-time Codex quota: the codex.rate_limits frame the stream just carried (and
		// leakfilter dropped downstream) refreshes the 5h/7d quota rows so the quota view
		// is current between /wham/usage polls.
		if streamRateLimits.any() {
			s.captureCodexStreamRateLimits(lease.Account.ID, streamRateLimits)
		}
		if streamErr != nil || sidecarStreamInterrupted || compatContinuationFailed {
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
	if s.leakScrubEnabled(r.Context()) {
		responseBody, _ = responsefilter.StripSafetyBufferingJSON(responseBody)
	}
	// backend-api can encode a semantic Responses 400 inside a HTTP 200 JSON error
	// envelope. Treat the typed body error as the actual upstream result before the
	// generic soft-200 rule path; otherwise a previous_response_not_found escapes
	// CPA retirement and its unusable alias remains active.
	if responsesContextError(http.StatusBadRequest, responseBody) != leakfilter.ResponsesContextErrorNone {
		s.recordCodexUpstreamAttempt(r.Context(), mapping, lease, finalEgress, "soft_context_error", http.StatusBadRequest)
		if handled, ok := handleResponsesContextAttemptError(http.StatusBadRequest, resp.Header, responseBody, "http_200_context_error"); ok {
			return handled
		}
	}
	// Some backend-api failures arrive as a JSON error envelope with HTTP 200 but
	// do not carry one of the built-in quota signatures.  An administrator's
	// status/body rule is still authoritative on strict CPA traffic, so evaluate
	// explicit terminal actions before treating this as an ordinary completed body.
	if decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
		Provider: "codex", Entrypoint: "responses", Model: model, Status: resp.StatusCode,
		Header: resp.Header, Body: responseBody, Streaming: false,
	}); ruleMatched {
		if decision.Match.AccountAction != upstreamrules.AccountActionBuiltin {
			s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, responseBody, decision)
		}
		if decision.Match.DownstreamAction != upstreamrules.DownstreamActionBuiltin {
			// Preserve the historic default classifier for builtin rules; explicit
			// administrator actions take precedence over it.
			if decision.Match.AccountAction == upstreamrules.AccountActionBuiltin {
				s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, responseBody, decision)
			}
			_ = s.settleBillingHold(r.Context(), holdID, "success_body_rule")
			if result, handled := applyCodexRule(decision, resp.StatusCode, resp.Header, responseBody, false); handled {
				return result
			}
		}
	}
	// Session 33: Success-path body scan for rate-limit signals in 200 response.
	// Same logic as the isChat non-streaming path above: when the upstream returns
	// a soft limit error in a 200 body, cool the account and fail over.
	if cd := usageLimitCooldown(200, responseBody); cd > 0 {
		if lease.Account.IgnoreRateLimitControls {
			_ = resp.Body.Close()
			resp, finalEgress, err = s.retryCodexSameAccountAfterRateLimit(r.Context(), lease, func() upstream.Request {
				return requestForToken(token)
			}, resp.StatusCode, resp.Header, responseBody)
			if err != nil {
				_ = s.settleBillingHold(r.Context(), holdID, "ignored_rate_limit_retry_interrupted")
				if r.Context().Err() != nil {
					return codexAttemptResult{Outcome: outcomeDone}
				}
				writeError(w, http.StatusBadGateway, err)
				return codexAttemptResult{Outcome: outcomeDone}
			}
			if resp.StatusCode < http.StatusBadRequest {
				goto codexSuccess
			}
			goto codexResponse
		}
		if !codexResetRetried && codexResetTriggerAllowed(200, responseBody) &&
			s.tryAutoConsumeCodexResetCredit(r.Context(), lease.Account, token, lease.Egress, true, 200, resp.Header, responseBody, "soft_200_body") {
			codexResetRetried = true
			if latest, terr := s.store.GetToken(r.Context(), lease.Account.ID); terr == nil {
				token = latest
			}
			_ = resp.Body.Close()
			retryResp, retryEgress, retryErr := s.doWithCFRetry(r.Context(), requestForToken(token), lease, mappingTransportStrict)
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
		s.benchOnLimitForAccount(r.Context(), lease.Account, 200, resp.Header, responseBody)
		_ = s.settleBillingHold(r.Context(), holdID, "rate_limited_in_200_body")
		// Honor failover here too: a soft rate-limit in a 200 body is the same condition
		// as a 429, so a movable request should fail over rather than surface it.
		if movable {
			return retryAfterCapacity()
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
	s.persistCodexStateBindings(r.Context(), r, body, affinity, responseBody, resp.Header, lease, finalEgress, model, isCompact)
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
	_ = s.settleBillingHold(r.Context(), holdID, "settled")
	s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Output guard: deterministically downgrade a non-streaming Responses answer that
	// claims unverified test/command/file results (no-op when reliability/guard is off).
	if !strictNativeCPA {
		responseBody = s.reliabilityGuardResponsesBody(r.Context(), responseBody, relTurn)
	}
	// Administrator-configured response filters are deliberately independent of
	// the strict CPA request boundary: they shape only the completed downstream
	// response and never alter the upstream session, previous_response_id, or a
	// client tool result.
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

func (s *Server) persistCodexStateBindings(ctx context.Context, r *http.Request, requestBody []byte, affinity routing.AffinityKey, responseBody []byte, responseHeader http.Header, lease scheduler.Lease, egress storage.EgressProfile, requestedModel string, compact bool) {
	var response struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(responseBody, &response)
	// RemoteCompactionV2 is committed by the official client only after a real
	// completed terminal. The legacy /responses/compact unary endpoint also sets
	// compact=true, but its successful full JSON response may omit status entirely.
	// Identify V2 by its normal Responses route plus the terminal input trigger so
	// legacy compaction retains its existing compatibility behavior.
	remoteCompactionV2 := codexRemoteCompactionV2Request(r, requestBody)
	if (remoteCompactionV2 && !strings.EqualFold(strings.TrimSpace(response.Status), "completed")) ||
		(!remoteCompactionV2 && response.Status != "" && !strings.EqualFold(response.Status, "completed")) {
		return
	}
	if mapping := codexSessionMappingFromContext(ctx); mapping != nil && mapping.enabled {
		if err := s.commitCodexSessionMapping(ctx, mapping, lease, egress, response.ID, responseTurnState(responseHeader, responseBody), compact); err != nil {
			log.Printf("[CODEX-SESSION-MAPPING] terminal commit request_id=%s: %v", requestIDFromContext(ctx), err)
			_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "codex_session_mapping_commit_failed", State: "retryable", Reason: codexMappingErrorCode(err), Detail: "terminal_metadata_only"})
		}
	} else {
		// Compatibility mode retains ordinary affinity metadata only. Codex never
		// writes goal/checkpoint/journal bodies unless CPA-v2 is active.
		s.recordCodexUpstreamAttempt(ctx, mapping, lease, egress, "terminal_success", http.StatusOK)
		s.persistCodexBindingAliases(ctx, affinity, response.ID, response.Model, lease, egress, requestedModel)
	}
	s.persistCodexGoalContinuity(ctx, r, requestBody, responseBody)
}

func codexRemoteCompactionV2Request(r *http.Request, requestBody []byte) bool {
	if r != nil && r.URL != nil && strings.Contains(strings.ToLower(r.URL.Path), "/responses/compact") {
		return false
	}
	request, err := decodeContextJSONMap(requestBody)
	if err != nil {
		return false
	}
	input, ok := request["input"].([]interface{})
	if !ok || len(input) == 0 {
		return false
	}
	last, ok := input[len(input)-1].(map[string]interface{})
	return ok && strings.EqualFold(strings.TrimSpace(streamString(last["type"])), "compaction_trigger")
}

// persistCodexGoalContinuity writes an encrypted, incremental replay checkpoint
// only for CPA-v2 sessions. The native upstream session remains the normal path;
// this state is used exclusively to migrate a tree whose bound account or upstream
// previous_response_id became unavailable.
func (s *Server) persistCodexGoalContinuity(ctx context.Context, r *http.Request, requestBody, responseBody []byte) {
	// A downstream WebSocket that has switched permanently to HTTPS rebuilds each
	// response.append as a self-contained turn and intentionally disables native
	// session mapping. It still needs the encrypted Goal chain advanced so the next
	// append can resolve the just-completed HTTP response id without losing history.
	if (!s.codexSessionMappingEnabled(ctx) && !codexResponsesWebSocketUsesHTTPSFallback(ctx)) || !s.goalContinuityEnabled(ctx) {
		return
	}
	if _, err := s.persistGoalContinuity(ctx, r, "codex", requestBody, responseBody); err != nil {
		log.Printf("[GOAL-CONTINUITY] codex persistence degraded request_id=%s: %v", requestIDFromContext(ctx), err)
		s.auditGoalPersistenceDegraded(ctx, "codex_terminal", err)
	}
}

func (s *Server) persistCodexBindingAliases(ctx context.Context, affinity routing.AffinityKey, responseID, actualModel string, lease scheduler.Lease, egress storage.EgressProfile, requestedModel string) {
	if s.codexSessionMappingEnabled(ctx) {
		// CPA-v2 stores downstream response/thread aliases only as HMACs in its
		// dedicated mapping tables. Never let a compatibility call write a raw
		// Responses id back into affinity_bindings while it is enabled.
		return
	}
	model := firstNonEmpty(actualModel, lease.ResolvedModel, requestedModel)
	egressID := firstNonEmpty(egress.ID, lease.Egress.ID)
	upsert := func(key routing.AffinityKey) {
		if key.Hash == "" {
			return
		}
		_ = s.scheduler.UpsertAffinityBinding(ctx, storage.AffinityBinding{
			RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source,
			AccountID: lease.Account.ID, Provider: "codex", Model: model, EgressID: egressID,
		})
	}
	upsert(affinity)
	upsert(routing.ResponseAffinityKey(responseID))
}

var errInvalidUpstreamResponse = errors.New("upstream transport returned an invalid response")

func (s *Server) doCodexUpstream(ctx context.Context, req upstream.Request) (*upstream.Response, error) {
	if s == nil || s.upstreamDo == nil {
		return nil, fmt.Errorf("%w: client is unavailable", errInvalidUpstreamResponse)
	}
	resp, err := s.upstreamDo(ctx, req)
	if err != nil {
		return resp, err
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: nil response", errInvalidUpstreamResponse)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("%w: nil body (status %d)", errInvalidUpstreamResponse, resp.StatusCode)
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	return resp, nil
}

func (s *Server) doWithCFRetry(ctx context.Context, req upstream.Request, lease scheduler.Lease, strict bool) (*upstream.Response, storage.EgressProfile, error) {
	mapping := codexSessionMappingFromContext(ctx)
	// This canonical start is emitted at the actual transport boundary, so every
	// auth/version/native/HTTPS retry and every selected real exit is observable.
	// It is diagnostic-only: routing quality below uses explicit terminal success
	// or egress_failure rows and never treats an unfinished start as a failure.
	s.recordCodexUpstreamAttempt(ctx, mapping, lease, req.Egress, "transport_attempted", 0)
	resp, err := s.doCodexUpstream(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			s.recordCodexUpstreamAttempt(ctx, mapping, lease, req.Egress, "egress_failure", 0)
		}
		if !strict {
			return s.doCodexStandbyEgressRetries(ctx, req, lease, nil, nil, err)
		}
		return nil, req.Egress, err
	}
	if resp.StatusCode < 400 {
		return resp, req.Egress, nil
	}
	body, err := upstream.DrainAndClose(resp.Body)
	if err != nil {
		if ctx.Err() == nil {
			s.recordCodexUpstreamAttempt(ctx, mapping, lease, req.Egress, "egress_failure", resp.StatusCode)
		}
		return nil, req.Egress, err
	}
	detection := cf.Detect(resp.StatusCode, resp.Header, body)
	if !detection.Matched && resp.StatusCode < http.StatusInternalServerError {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	if detection.Matched && cf.EdgeOnly(detection) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	egressFailureRecorded := false
	if detection.Matched && cf.Recordable(detection) {
		s.recordCodexUpstreamAttempt(ctx, mapping, lease, req.Egress, "egress_failure", resp.StatusCode)
		egressFailureRecorded = true
		s.handleCFEvent(ctx, req.Account, req.Egress, resp.StatusCode, detection)
	}
	if strict {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, req.Egress, nil
	}
	// Reaching standby retry is itself the classification boundary for an
	// ordinary 5xx. Record the real exit exactly once; account/quota 4xx responses
	// returned above remain unclassified and never depress outlet quality.
	if !egressFailureRecorded {
		s.recordCodexUpstreamAttempt(ctx, mapping, lease, req.Egress, "egress_failure", resp.StatusCode)
	}
	return s.doCodexStandbyEgressRetries(ctx, req, lease, resp, body, nil)
}

// doCodexStandbyEgressRetries exhausts the selected account's ordered standby
// outlets before the caller is allowed to move to another account or user-group
// target. It is called only before downstream response commitment.
func (s *Server) doCodexStandbyEgressRetries(ctx context.Context, req upstream.Request, lease scheduler.Lease, fallback *upstream.Response, fallbackBody []byte, fallbackErr error) (*upstream.Response, storage.EgressProfile, error) {
	retryBinding := lease.Binding
	retryBinding.AccountID = req.Account.ID
	standbys := append([]string(nil), retryBinding.StandbyIDs()...)

	// A CF handler may have appended a WARP standby to the account binding. Merge
	// only newly-added standbys into the dynamic group/provider order; replacing the
	// whole binding here would resurrect a stale account-level primary.
	if updated, err := s.store.GetEgressBinding(ctx, req.Account.ID); err == nil {
		if retryBinding.SidecarEgressID == "" {
			retryBinding.SidecarEgressID = updated.SidecarEgressID
		}
		seen := map[string]bool{req.Egress.ID: true}
		for _, id := range standbys {
			seen[id] = true
		}
		for _, id := range updated.StandbyIDs() {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				standbys = append(standbys, id)
			}
		}
	}

	lastResp, lastBody, lastEgress, lastErr := fallback, fallbackBody, req.Egress, fallbackErr
	for _, standbyID := range standbys {
		if standbyID == "" || standbyID == req.Egress.ID {
			continue
		}
		standby, err := s.store.GetEgressProfile(ctx, standbyID)
		if err != nil || !scheduler.EgressHealthy(standby, storage.Now()) {
			continue
		}
		standby, err = s.store.ApplySidecarEgressBinding(ctx, retryBinding, standby)
		if err != nil {
			// Explicit sidecar bindings are fail-closed on the retry path too.
			continue
		}
		retryReq := req
		retryReq.Egress = standby
		retryReq.CookieJarKey = req.Account.ID + ":" + standby.ID
		mapping := codexSessionMappingFromContext(ctx)
		s.recordCodexUpstreamAttempt(ctx, mapping, lease, standby, "transport_attempted", 0)
		retryResp, err := s.doCodexUpstream(ctx, retryReq)
		if err != nil {
			if ctx.Err() == nil {
				s.recordCodexUpstreamAttempt(ctx, mapping, lease, standby, "egress_failure", 0)
			}
			lastErr = err
			continue
		}
		if retryResp.StatusCode < http.StatusBadRequest {
			return retryResp, standby, nil
		}
		retryBody, drainErr := upstream.DrainAndClose(retryResp.Body)
		if drainErr != nil {
			if ctx.Err() == nil {
				s.recordCodexUpstreamAttempt(ctx, mapping, lease, standby, "egress_failure", retryResp.StatusCode)
			}
			lastErr = drainErr
			continue
		}
		lastResp, lastBody, lastEgress, lastErr = retryResp, retryBody, standby, nil
		detection := cf.Detect(retryResp.StatusCode, retryResp.Header, retryBody)
		egressFailureRecorded := false
		if detection.Matched && !cf.EdgeOnly(detection) && cf.Recordable(detection) {
			s.recordCodexUpstreamAttempt(ctx, mapping, lease, standby, "egress_failure", retryResp.StatusCode)
			egressFailureRecorded = true
			s.handleCFEvent(ctx, req.Account, standby, retryResp.StatusCode, detection)
		}
		if (!detection.Matched || cf.EdgeOnly(detection)) && retryResp.StatusCode < http.StatusInternalServerError {
			retryResp.Body = io.NopCloser(bytes.NewReader(retryBody))
			return retryResp, standby, nil
		}
		if !egressFailureRecorded && !(detection.Matched && cf.EdgeOnly(detection)) {
			s.recordCodexUpstreamAttempt(ctx, mapping, lease, standby, "egress_failure", retryResp.StatusCode)
		}
	}
	if lastResp != nil {
		lastResp.Body = io.NopCloser(bytes.NewReader(lastBody))
		return lastResp, lastEgress, nil
	}
	return nil, lastEgress, lastErr
}

// ignoredRateLimitRetryFloor is deliberately a variable so focused gateway tests
// can exercise same-account retry without sleeping for the production floor.
// It is not a user-facing global setting: the behavior is opted into per account.
var ignoredRateLimitRetryFloor = time.Second

func codexIgnoredRateLimitResponse(status int, body []byte) bool {
	return status == http.StatusTooManyRequests || usageLimitSignal(body)
}

func ignoredRateLimitRetryDelay(header http.Header) time.Duration {
	now := storage.Now()
	seconds := retryAfterSeconds(header, now)
	if seconds <= 0 {
		seconds = resetSeconds(header, now)
	}
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if ignoredRateLimitRetryFloor > 0 {
		return ignoredRateLimitRetryFloor
	}
	return time.Second
}

func waitForIgnoredRateLimitRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryCodexSameAccountAfterRateLimit suppresses only a rate/quota response from a
// checked account. Every retry retains the already-leased account and egress, so a
// strict CPA turn remains in the same native upstream chain. It intentionally keeps
// retrying until the caller cancels; no 429 is written to the downstream client.
// A non-rate-limit response is returned untouched for the ordinary response path.
func (s *Server) retryCodexSameAccountAfterRateLimit(ctx context.Context, lease scheduler.Lease, request func() upstream.Request, status int, header http.Header, body []byte) (*upstream.Response, storage.EgressProfile, error) {
	for attempts := 1; codexIgnoredRateLimitResponse(status, body); attempts++ {
		delay := ignoredRateLimitRetryDelay(header)
		log.Printf("[CODEX-RATE-LIMIT-OVERRIDE] account=%s attempt=%d retry_in=%s same_account_egress=true", lease.Account.ID, attempts, delay.Round(time.Millisecond))
		if err := waitForIgnoredRateLimitRetry(ctx, delay); err != nil {
			return nil, lease.Egress, err
		}
		// Strict=true intentionally disables the ordinary standby-egress CF retry.
		// This opt-in loop must remain on the exact account/egress lease, including
		// for non-CPA Codex requests.
		resp, egress, err := s.doWithCFRetry(ctx, request(), lease, true)
		if err != nil {
			return nil, egress, err
		}
		if resp.StatusCode < http.StatusBadRequest {
			return resp, egress, nil
		}
		nextBody := readUpstreamErrorBody(resp.Body)
		if !codexIgnoredRateLimitResponse(resp.StatusCode, nextBody) {
			resp.Body = io.NopCloser(bytes.NewReader(nextBody))
			return resp, egress, nil
		}
		_ = resp.Body.Close()
		status, header, body = resp.StatusCode, resp.Header, nextBody
	}
	return nil, lease.Egress, context.Canceled
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
	if account.IgnoreRateLimitControls {
		// Do not feed this account into StormBreaker's cooldown/quarantine ladder.
		// In particular, no account or shared-egress state may be mutated merely
		// because this explicitly opted-in account met a CF interstitial.
		log.Printf("[CF] account=%s ignored automatic CF controls category=%s status=%d", account.ID, detection.Category, status)
		return
	}
	_ = (cf.StormBreaker{Store: s.store}).Record(ctx, account.ID, egress.ID, status, detection)
	// StormBreaker may synchronously trip the shared egress profile. Publish that
	// health/cooldown mutation before another fresh request reads the 30s cache.
	s.scheduler.InvalidateEgressCache()
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
		} else {
			s.scheduler.InvalidateEgressCache()
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
	sidecarEndpoint := strings.TrimSpace(s.cfg.DefaultSidecarEndpoint)
	if storage.IsSidecarEgress(egress) {
		sidecarEndpoint = strings.TrimSpace(egress.Endpoint)
	}
	if sc := sidecarEndpoint; sc != "" {
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
	if isInternalCall(ctx) {
		return // relay-internal (moderation) calls must not be metered against pool accounts
	}
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
				// The estimate is an INPUT-side approximation (RuneCount(body)/4). Record
				// it as prompt tokens with total = prompt (completion unknown → 0) so the
				// row is internally consistent (total == prompt+completion) instead of the
				// old prompt=estimate/2, total=estimate, completion=0 shape that never
				// reconciled. Preserve the model ParseResponse extracted when available.
				estModel := parsed.Model
				if estModel == "" {
					estModel = "unknown"
				}
				parsed = usage.Parsed{
					Model:        estModel,
					PromptTokens: estimate,
					TotalTokens:  estimate,
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
	modelDiag := modelDiagnosticsFromCtx(ctx)
	diag.BillingHoldID = holdIDFromCtx(ctx)
	diag.RequestedModel, diag.ResolvedModel, diag.ModelOverrideSource = modelDiag.Requested, modelDiag.Resolved, modelDiag.Source
	if diag.UsageEventID == "" {
		diag.UsageEventID = usageEventIDFromContext(ctx)
	}
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

func (s *Server) hasCodexFailoverCandidate(ctx context.Context, groupName, model string, excludeAccountIDs ...string) bool {
	excluded := map[string]bool{}
	for _, accountID := range excludeAccountIDs {
		if accountID != "" {
			excluded[accountID] = true
		}
	}
	// This check runs only after an upstream failure. Imports, rechecks and quota
	// updates may have occurred since the request's first lease, so force the same
	// fresh snapshot Select would use on its stale-cache retry path.
	s.scheduler.InvalidateAccountCache()
	count, err := s.scheduler.EligibleCandidateCount(ctx, scheduler.Route{Group: groupName, Provider: "codex", Model: model, Exclude: excluded})
	return err == nil && count > 0
}

// recordParsedUsage stores an already-parsed usage figure (the streaming path, where
// usage is extracted incrementally from the SSE frames rather than a buffered body).
func (s *Server) recordParsedUsage(ctx context.Context, accountID, routeHash string, parsed usage.Parsed) {
	if isInternalCall(ctx) {
		return // relay-internal (moderation) calls must not be metered against pool accounts
	}
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
					PromptTokens: estimate,
					TotalTokens:  estimate,
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
	modelDiag := modelDiagnosticsFromCtx(ctx)
	diag.BillingHoldID = holdIDFromCtx(ctx)
	diag.RequestedModel, diag.ResolvedModel, diag.ModelOverrideSource = modelDiag.Requested, modelDiag.Resolved, modelDiag.Source
	if diag.UsageEventID == "" {
		diag.UsageEventID = usageEventIDFromContext(ctx)
	}
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
