package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"codex-account-pool/internal/api"
	"codex-account-pool/internal/cfsolve"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/incident"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/secretbox"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
	"codex-account-pool/internal/warp"
)

func main() {
	os.Exit(run())
}

func loadRuntimeKey(path string, explicit bool) ([]byte, error) {
	if explicit {
		return datadir.LoadCredentialKey(path)
	}
	return datadir.LoadOrCreateKey(path)
}

func run() int {
	var configPath string
	var unixSocket string
	var releaseID string
	var deploymentRole string
	var selfTest bool
	flag.StringVar(&configPath, "config", "", "path to JSON configuration file")
	flag.StringVar(&unixSocket, "unix-socket", "", "serve on a private Unix socket instead of the configured TCP address")
	flag.StringVar(&releaseID, "release-id", strings.TrimSpace(os.Getenv("CODEX_POOL_RELEASE_ID")), "deployed release identifier exposed by /readyz")
	flag.StringVar(&deploymentRole, "deployment-role", strings.TrimSpace(os.Getenv("CODEX_POOL_DEPLOYMENT_ROLE")), "worker role: auto, active, or standby")
	flag.BoolVar(&selfTest, "self-test", false, "verify that the worker binary can start")
	flag.Parse()
	if selfTest {
		fmt.Println("codex-pool-server self-test ok")
		return 0
	}
	if releaseID == "" {
		releaseID = "development"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("load config: %v", err)
		return 1
	}
	explicitJournal := strings.TrimSpace(cfg.UsageJournalDir)
	explicitMasterKey := strings.TrimSpace(cfg.MasterKeyFile) != ""
	explicitIdentityKey := strings.TrimSpace(cfg.IdentityKeyFile) != ""
	explicitDiagnosticAliasKey := strings.TrimSpace(cfg.DiagnosticAliasKeyFile) != ""
	layout, err := datadir.Prepare(cfg.DataDir, cfg.BodySpoolDir, explicitJournal)
	if err != nil {
		log.Printf("persistent data preflight: %v", err)
		return 1
	}
	// Install the exception callback as soon as the private data layout exists.
	// Storage is deliberately nil until Init completes: startup failures are
	// fsynced to the fallback journal and replayed by the next healthy process.
	incidentReporter, err := incident.Open(filepath.Join(layout.Journal, "exception-events"), nil)
	if err != nil {
		log.Printf("initialize exception diagnostics: %v", err)
		return 1
	}
	incidentCallback, err := supervisor.RegisterEventCallback(incidentReporter.CallbackOptions("durable-diagnostic-events"))
	if err != nil {
		log.Printf("register exception diagnostics callback: %v", err)
		return 1
	}
	defer incidentCallback.Unregister()
	reportStartupFailure := func(operation string, startupErr error) {
		supervisor.ReportError("process-startup", operation, startupErr)
	}
	cfg.DataDir = layout.Root
	cfg.BodySpoolDir = layout.Spool
	cfg.DiagnosticsDir = layout.Diagnostics
	if explicitJournal == "" {
		cfg.UsageJournalDir = filepath.Join(layout.Journal, journalOwnerDirectory(cfg.NodeID, os.Getenv("CODEX_POOL_INSTANCE_ID")))
		if err := datadir.EnsureDirectory(cfg.UsageJournalDir); err != nil {
			log.Printf("usage journal preflight: %v", err)
			reportStartupFailure("usage_journal_preflight", err)
			return 1
		}
	} else {
		cfg.UsageJournalDir = layout.Journal
	}
	if strings.TrimSpace(cfg.MasterKeyFile) == "" {
		cfg.MasterKeyFile = filepath.Join(layout.Keys, "master.key")
	}
	if strings.TrimSpace(cfg.IdentityKeyFile) == "" {
		cfg.IdentityKeyFile = filepath.Join(layout.Keys, "identity.key")
	}
	if strings.TrimSpace(cfg.DiagnosticAliasKeyFile) == "" {
		cfg.DiagnosticAliasKeyFile = filepath.Join(layout.Keys, "diagnostic-alias.key")
	}
	masterKey, err := loadRuntimeKey(cfg.MasterKeyFile, explicitMasterKey)
	if err != nil {
		log.Printf("load storage master key: %v", err)
		reportStartupFailure("load_storage_master_key", err)
		return 1
	}
	cfg.RuntimeIdentityKey, err = loadRuntimeKey(cfg.IdentityKeyFile, explicitIdentityKey)
	if err != nil {
		log.Printf("load stable identity key: %v", err)
		reportStartupFailure("load_identity_key", err)
		return 1
	}
	cfg.RuntimeDiagnosticAliasKey, err = loadRuntimeKey(cfg.DiagnosticAliasKeyFile, explicitDiagnosticAliasKey)
	if err != nil {
		log.Printf("load diagnostic alias key: %v", err)
		reportStartupFailure("load_diagnostic_alias_key", err)
		return 1
	}
	// Advisory only: surface fingerprint version-override combinations that risk looking
	// unlike any real client (a relay signal). Never blocks startup.
	for _, warning := range cfg.FingerprintWarnings() {
		log.Printf("[FINGERPRINT-WARN] %s", warning)
	}
	// Refuse the one deployment that hands the whole account pool (live OAuth tokens,
	// exportable in plaintext via /admin/accounts/export) to the public internet with
	// no gate: a non-loopback bind AND an empty admin token. Weak-token / open-relay
	// cases only warn. Override the hard stop with CODEX_POOL_ALLOW_INSECURE_BIND=1.
	enforceBindSecurity(cfg)

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	startHeapProfileSignal(ctx)
	stopCPUProfile := startCPUProfile()
	defer stopCPUProfile()
	storageInitStarted := time.Now()
	log.Printf("startup: initializing storage")
	store, err := storage.OpenWithConfig(cfg)
	if err != nil {
		log.Printf("open storage: %v", err)
		reportStartupFailure("open_storage", err)
		return 1
	}
	defer store.Close()

	if err := initStorageWithLockRetry(ctx, store.InitWithProgress, func(phase string) {
		log.Printf("startup: storage phase=%s elapsed=%s", phase, time.Since(storageInitStarted).Round(time.Millisecond))
	}, log.Printf); err != nil {
		log.Printf("init storage: %v", err)
		reportStartupFailure("init_storage", err)
		return 1
	}
	log.Printf("startup: storage initialized in %s", time.Since(storageInitStarted).Round(time.Millisecond))
	incidentReporter.SetStore(store)
	if replayed, replayErr := incidentReporter.Replay(ctx); replayErr != nil && !errors.Is(replayErr, incident.ErrPrimaryUnavailable) {
		log.Printf("replay exception diagnostics: %v", replayErr)
		supervisor.ReportError("exception-reporter", "startup_replay", replayErr)
	} else if replayed > 0 {
		log.Printf("startup: replayed %d durable exception event(s)", replayed)
	}
	// Keep the former host/config-derived key readable for exactly this migration
	// window. Every value is then rewritten with the independent persistent master.
	legacyStorageKey := secretbox.DeriveKey(identity.ResolveSecret([]byte(cfg.IdentitySecret)))
	legacyKeys := [][]byte(nil)
	if string(legacyStorageKey) != string(masterKey) {
		legacyKeys = append(legacyKeys, legacyStorageKey)
	}
	if err := store.SetTokenMasterKey(masterKey, legacyKeys...); err != nil {
		log.Printf("[SECURITY] configure storage encryption: %v", err)
		reportStartupFailure("configure_storage_encryption", err)
		return 1
	}
	if err := store.ValidateEncryptionSentinel(ctx); err != nil {
		log.Printf("[SECURITY] encryption sentinel: %v", err)
		reportStartupFailure("validate_encryption_sentinel", err)
		return 1
	}
	if n, err := store.EncryptExistingTokens(ctx); err != nil {
		log.Printf("[SECURITY] encrypt existing tokens at rest: %v", err)
		reportStartupFailure("encrypt_existing_tokens", err)
		return 1
	} else if n > 0 {
		log.Printf("[SECURITY] encrypted %d plaintext account token row(s) at rest", n)
	}
	store.EnableStrictEncryption()
	if err := store.CryptoError(); err != nil {
		log.Printf("[SECURITY] credential validation: %v", err)
		reportStartupFailure("validate_credentials", err)
		return 1
	}
	coreStateWriter, coreStateErr := openCoreStateWriter(layout)
	if coreStateErr != nil {
		// The relay can keep using either previously committed A/B snapshot. A
		// state-writer outage reduces future failover freshness but must not make
		// the worker or its existing contexts unavailable.
		log.Printf("core state writer unavailable: %v", coreStateErr)
		supervisor.ReportError("core-state-writer", "open", coreStateErr)
	}
	up := upstream.NewClient(cfg)
	// WARP CF-fallback manager. The prober wraps ProbeEgress so the manager can refresh
	// an exit's IP after a re-registration without importing the upstream package.
	warpProber := func(pctx context.Context, eg storage.EgressProfile) (string, string, error) {
		r, perr := up.ProbeEgress(pctx, eg)
		return r.IP, r.Country, perr
	}
	warpMgr := warp.NewManager(cfg, store, warpProber, log.Printf)
	solver := cfsolve.NewClient(cfg)

	leaseCoordinator, err := scheduler.NewLeaseCoordinator(cfg)
	if err != nil {
		log.Printf("init lease coordinator: %v", err)
		reportStartupFailure("init_lease_coordinator", err)
		return 1
	}
	app := api.NewServer(api.Dependencies{
		Config:            cfg,
		Store:             store,
		IncidentReporter:  incidentReporter,
		Scheduler:         scheduler.NewWithLeaseCoordinator(store, cfg, leaseCoordinator),
		Upstream:          up,
		Planner:           virtual.NewPlanner(store, cfg),
		Warp:              warpMgr,
		Solver:            solver,
		DeferRuntimeStart: true,
	})

	deployment := newDeploymentHandler(app, releaseID, unixSocket)
	deployment.check = func(checkCtx context.Context) error {
		if err := datadir.RecoverDirectory(cfg.BodySpoolDir); err != nil {
			return err
		}
		if err := datadir.RecoverDirectory(cfg.UsageJournalDir); err != nil {
			return err
		}
		if err := store.CryptoError(); err != nil {
			return err
		}
		if err := store.ValidateEncryptionSentinel(checkCtx); err != nil {
			return err
		}
		if err := store.CheckWritable(checkCtx); err != nil {
			return err
		}
		if !app.StorageAdmissionReady() {
			return errors.New("storage admission unavailable")
		}
		return nil
	}
	deferredMigrations := newDeferredMigrationTask(store.RunDeferredMigrations, log.Printf)
	startActive := func(activeCtx context.Context, fencingToken int64) error {
		if err := commitActiveWorker(coreStateWriter, releaseID, unixSocket, fencingToken); err != nil {
			log.Printf("core state commit unavailable release=%s fencing_token=%d: %v", releaseID, fencingToken, err)
			supervisor.ReportError("core-state-writer", "commit_active_worker", err)
		}
		if err := app.StartRuntime(); err != nil {
			return fmt.Errorf("start active runtime: %w", err)
		}
		paymentRemoval, err := store.RemovePaymentFeatureData(activeCtx)
		if err != nil {
			return fmt.Errorf("remove legacy payment state: %w", err)
		}
		if paymentRemoval.CancelledTasks+paymentRemoval.ClearedPaymentRows+paymentRemoval.QuarantinedMocks+paymentRemoval.AccountsForReview > 0 {
			log.Printf("[SECURITY] removed legacy payment state: cancelled_tasks=%d cleared_rows=%d quarantined_mocks=%d manual_review=%d",
				paymentRemoval.CancelledTasks, paymentRemoval.ClearedPaymentRows, paymentRemoval.QuarantinedMocks, paymentRemoval.AccountsForReview)
		}
		if cfg.DefaultSidecarEndpoint != "" {
			if err := store.UpsertEgressProfile(activeCtx, storage.EgressProfile{
				ID:             "egress_sidecar",
				Name:           "local curl_cffi sidecar",
				Type:           "curl_cffi_sidecar",
				Endpoint:       cfg.DefaultSidecarEndpoint,
				ChainProxy:     cfg.DefaultSidecarChainProxy,
				StreamCapable:  true,
				Health:         "healthy",
				MaxConcurrency: 0,
			}); err != nil {
				return fmt.Errorf("init sidecar egress: %w", err)
			}
		}
		if err := warpMgr.EnsurePool(activeCtx); err != nil {
			log.Printf("warp: ensure pool: %v", err)
		}
		app.StartBackground(activeCtx)
		app.StartAutomation(activeCtx)
		app.StartQuotaPoller(activeCtx)
		app.StartModelQualityMonitor(activeCtx)
		startActiveLedgerPurge(activeCtx, store, cfg)
		deferredMigrations.Start(activeCtx)
		log.Printf("worker role active release=%s fencing_token=%d", releaseID, fencingToken)
		return nil
	}
	roleOwner := fmt.Sprintf("%s:%d:%d", releaseID, os.Getpid(), time.Now().UnixNano())
	roleController, err := newWorkerRoleController(store, deployment, deploymentRole, unixSocket, roleOwner, startActive)
	if err != nil {
		log.Printf("deployment role: %v", err)
		reportStartupFailure("init_worker_role", err)
		return 1
	}
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           deployment,
		ReadHeaderTimeout: 15 * time.Second,
		// Bound slow request-body uploads without imposing a WriteTimeout on SSE or
		// WebSocket responses. Individual upstream calls still use their own context.
		ReadTimeout: cfg.RequestTimeout(),
	}

	serveErr := make(chan error, 1)
	deployment.standbyReady.Store(true)
	serveHTTPServerAsync(serveErr, func() error { return serveHTTPServerOn(httpServer, unixSocket) })
	supervisor.Go(ctx, "exception-journal-replay", incidentReporter.Run)
	supervisor.Go(ctx, "worker-role", roleController.Run)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)

	var serveFailure error
	select {
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
	case err := <-serveErr:
		if err != nil {
			serveFailure = err
			log.Printf("http server: %v", err)
		}
	}
	deployment.beginDraining()

	// Graceful drain: stop accepting new connections and let in-flight requests (the
	// long-lived SSE streams this relay serves) finish, up to the configured window.
	// On timeout we log and return (not Fatalf) so deferred storage cleanup still
	// runs for a clean exit; the unit's TimeoutStopSec is set well
	// above this so systemd never SIGKILLs mid-drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout())
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful drain exceeded %s, exiting with streams still active: %v", cfg.ShutdownDrainTimeout(), err)
	}
	cancelBackground()
	registrationCtx, cancelRegistration := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.StopRegistrationJobs(registrationCtx); err != nil {
		log.Printf("bounded registration shutdown incomplete: %v", err)
	}
	cancelRegistration()
	// In-flight requests have now drained, so no new async writes will be enqueued.
	// Flush the deferred fire-and-forget DB writes (usage / virtual-ledger rows) before
	// the deferred store.Close runs, so a clean shutdown loses no recorded usage.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.FlushWritesContext(flushCtx); err != nil {
		log.Printf("bounded write flush incomplete; durable journal retained: %v", err)
	}
	cancelFlush()
	if serveFailure != nil {
		return 1
	}
	return 0
}

func startActiveLedgerPurge(ctx context.Context, store *storage.Store, cfg config.Config) {
	supervisor.Go(ctx, "virtual-ledger-purge", func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deleted, err := store.PurgeVirtualLedger(ctx, cfg.VirtualContextLedgerTTLSeconds)
				if err != nil {
					log.Printf("[ledger] purge: %v", err)
				} else if deleted > 0 {
					log.Printf("[ledger] purged %d old rows", deleted)
				}
				journalDeleted, err := store.CleanupContextJournal(ctx)
				if err != nil {
					log.Printf("[context-journal] purge: %v", err)
				} else if journalDeleted > 0 {
					log.Printf("[context-journal] purged %d expired rows", journalDeleted)
				}
			}
		}
	})
}

// deploymentHandler provides the worker-local deployment contract used by the
// stable handoff process. It deliberately lives outside the API router so readiness
// remains available while normal admission middleware is saturated.
type deploymentHandler struct {
	next         http.Handler
	releaseID    string
	workerAddr   string
	startedAt    time.Time
	ready        atomic.Bool
	standbyReady atomic.Bool
	draining     atomic.Bool
	inflight     atomic.Int64
	fencingToken atomic.Int64
	check        func(context.Context) error
}

func newDeploymentHandler(next http.Handler, releaseID, workerAddr string) *deploymentHandler {
	return &deploymentHandler{next: next, releaseID: releaseID, workerAddr: workerAddr, startedAt: time.Now().UTC()}
}

func (h *deploymentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "release_id": h.releaseID})
		return
	}
	if r.URL.Path == "/readyz" {
		h.serveReady(w, r)
		return
	}
	if r.URL.Path == "/standbyz" {
		h.serveStandbyReady(w, r)
		return
	}
	if !h.ready.Load() || h.draining.Load() || h.next == nil {
		writeWorkerUnavailable(w)
		return
	}
	h.inflight.Add(1)
	defer h.inflight.Add(-1)
	w.Header().Set("X-Codex-Pool-Release", h.releaseID)
	h.next.ServeHTTP(w, r)
}

func (h *deploymentHandler) serveReady(w http.ResponseWriter, r *http.Request) {
	// Read the deployment flags once so a concurrent role transition cannot emit
	// a contradictory payload such as state=draining with ready=true.
	draining := h.draining.Load()
	fencingToken := h.fencingToken.Load()
	active := h.ready.Load() && fencingToken > 0
	standby := h.standbyReady.Load()
	ready := active && !draining
	var checkErr error
	if ready && h.check != nil {
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		checkErr = h.check(checkCtx)
		cancel()
		ready = checkErr == nil
	}
	state := "active"
	status := http.StatusOK
	if draining {
		state = "draining"
		status = http.StatusServiceUnavailable
	} else if standby && !ready {
		state = "standby_ready"
		status = http.StatusServiceUnavailable
	} else if !ready {
		state = "starting"
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Codex-Pool-Release", h.releaseID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":               ready,
		"ready":            ready,
		"release_id":       h.releaseID,
		"deployment_state": state,
		"inflight":         h.inflight.Load(),
		"started_at":       h.startedAt.Format(time.RFC3339Nano),
		"worker_socket":    h.workerAddr,
		"fencing_token":    fencingToken,
		"checks":           map[string]bool{"storage": checkErr == nil},
	})
}

func (h *deploymentHandler) serveStandbyReady(w http.ResponseWriter, r *http.Request) {
	draining := h.draining.Load()
	fencingToken := h.fencingToken.Load()
	active := h.ready.Load() && fencingToken > 0
	standby := h.standbyReady.Load()
	ready := !draining && (standby || active)
	var checkErr error
	if ready && h.check != nil {
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		checkErr = h.check(checkCtx)
		cancel()
		ready = checkErr == nil
	}
	state := "standby_ready"
	if active && !draining {
		state = "active"
	} else if draining {
		state = "draining"
	} else if !ready {
		state = "starting"
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Codex-Pool-Release", h.releaseID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":               ready,
		"standby_ready":    ready,
		"release_id":       h.releaseID,
		"deployment_state": state,
		"inflight":         h.inflight.Load(),
		"started_at":       h.startedAt.Format(time.RFC3339Nano),
		"worker_socket":    h.workerAddr,
		"fencing_token":    fencingToken,
		"checks":           map[string]bool{"storage": checkErr == nil},
	})
}

func (h *deploymentHandler) markActive(fencingToken int64) {
	h.fencingToken.Store(fencingToken)
	h.standbyReady.Store(true)
	h.draining.Store(false)
	h.ready.Store(true)
}

func (h *deploymentHandler) beginDraining() {
	h.ready.Store(false)
	h.draining.Store(true)
}

func (h *deploymentHandler) markStandby() {
	h.ready.Store(false)
	h.standbyReady.Store(true)
	h.fencingToken.Store(0)
	if h.inflight.Load() == 0 {
		h.draining.Store(false)
	}
}

func writeWorkerUnavailable(w http.ResponseWriter) {
	var random [8]byte
	requestID := ""
	if _, err := rand.Read(random[:]); err == nil {
		requestID = fmt.Sprintf("REQ-%X", random[:])
	} else {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", os.Getpid(), time.Now().UnixNano())))
		requestID = fmt.Sprintf("REQ-%X", sum[:8])
	}
	for name := range w.Header() {
		w.Header().Del(name)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":       "server_error",
			"code":       "service_unavailable",
			"message":    "The relay service is temporarily unavailable. Please retry.",
			"request_id": requestID,
		},
	})
}

func journalOwnerDirectory(nodeID, instanceID string) string {
	if strings.TrimSpace(instanceID) == "" {
		instanceID = "default"
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(nodeID) + "\x00" + strings.TrimSpace(instanceID)))
	return fmt.Sprintf("owner-%x", sum[:12])
}

func startCPUProfile() func() {
	path := strings.TrimSpace(os.Getenv("CODEX_POOL_CPU_PROFILE"))
	if path == "" {
		return func() {}
	}
	file, err := os.Create(path)
	if err != nil {
		log.Printf("create CPU profile %s: %v", path, err)
		return func() {}
	}
	if err = pprof.StartCPUProfile(file); err != nil {
		_ = file.Close()
		log.Printf("start CPU profile %s: %v", path, err)
		return func() {}
	}
	return func() {
		pprof.StopCPUProfile()
		if err := file.Close(); err != nil {
			log.Printf("close CPU profile %s: %v", path, err)
		}
	}
}

// startHeapProfileSignal writes an explicit, operator-requested heap snapshot on
// SIGUSR1. It is disabled unless CODEX_POOL_HEAP_PROFILE names a destination, so
// production does not expose a debug HTTP endpoint or write diagnostic data by default.
func startHeapProfileSignal(ctx context.Context) {
	path := strings.TrimSpace(os.Getenv("CODEX_POOL_HEAP_PROFILE"))
	if path == "" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	supervisor.Go(ctx, "heap-profile-signal", func(ctx context.Context) {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				file, err := os.Create(path)
				if err == nil {
					err = pprof.WriteHeapProfile(file)
					err = errors.Join(err, file.Close())
				}
				if err != nil {
					log.Printf("write heap profile %s: %v", path, err)
				} else {
					log.Printf("heap profile written to %s", path)
				}
			}
		}
	})
}

func serveHTTPServer(httpServer *http.Server) error {
	return serveHTTPServerOn(httpServer, "")
}

func serveHTTPServerOn(httpServer *http.Server, unixSocket string) error {
	if strings.TrimSpace(unixSocket) != "" {
		if err := os.Remove(unixSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale worker socket: %w", err)
		}
		ln, err := net.Listen("unix", unixSocket)
		if err != nil {
			return fmt.Errorf("listen on worker socket %s: %w", unixSocket, err)
		}
		defer func() {
			_ = ln.Close()
			_ = os.Remove(unixSocket)
		}()
		if err := os.Chmod(unixSocket, 0660); err != nil {
			return fmt.Errorf("chmod worker socket %s: %w", unixSocket, err)
		}
		log.Printf("codex pool worker listening on unix://%s", unixSocket)
		return cleanServeError(httpServer.Serve(ln))
	}
	// Prefer a socket passed by systemd (socket activation): the .socket unit owns
	// the listening fd, so `systemctl restart` keeps the socket open across the swap
	// and new connections queue in the kernel backlog instead of being refused during
	// the brief gap. Fall back to binding cfg.ListenAddr directly (manual runs /
	// non-systemd / no .socket unit).
	if ln, ok := listenerFromSystemd(); ok {
		log.Printf("codex pool server serving on systemd-activated socket")
		return cleanServeError(httpServer.Serve(ln))
	}
	log.Printf("codex pool server listening on %s", httpServer.Addr)
	return cleanServeError(httpServer.ListenAndServe())
}

func serveHTTPServerAsync(serveErr chan<- error, serve func() error) {
	supervisor.ModuleStarted("http-server")
	go func() {
		defer func() {
			if v := recover(); v != nil {
				supervisor.LogPanic("http-server", v)
				serveErr <- fmt.Errorf("http server panic: %v", v)
			}
		}()
		err := serve()
		if err != nil {
			supervisor.ModuleFailed("http-server", err)
		} else {
			supervisor.ModuleStopped("http-server")
		}
		serveErr <- err
	}()
}

func cleanServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// enforceBindSecurity is a startup guard against the catastrophic-but-easy misdeploy:
// copying the local-dev config (which binds 0.0.0.0 for convenience) to a public VPS
// without setting a real admin token. The admin API can export every account's
// access/refresh token in plaintext, so a public bind with no admin gate is instant,
// scriptable account theft. We hard-stop only that exact case; softer risks warn.
func enforceBindSecurity(cfg config.Config) {
	host := cfg.ListenAddr
	if h, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
		host = h
	}
	if isLoopbackBindHost(host) {
		return // localhost-only: not reachable off-box, nothing to enforce
	}
	allowInsecure, _ := strconv.ParseBool(os.Getenv("CODEX_POOL_ALLOW_INSECURE_BIND"))
	if strings.TrimSpace(cfg.AdminToken) == "" {
		msg := "refusing to start: listen_addr %q is not loopback and admin_token is empty — " +
			"the admin API would expose every account token to the internet with no auth. " +
			"Set a strong admin_token, bind 127.0.0.1 behind a reverse proxy, or set " +
			"CODEX_POOL_ALLOW_INSECURE_BIND=1 to override."
		if allowInsecure {
			log.Printf("[SECURITY-WARN] "+msg+" (overridden)", cfg.ListenAddr)
		} else {
			log.Fatalf("[SECURITY] "+msg, cfg.ListenAddr)
		}
	} else if looksLikeWeakToken(cfg.AdminToken) {
		log.Printf("[SECURITY-WARN] admin_token looks weak/placeholder while bound to a public "+
			"interface (%s); use a long random secret in production.", cfg.ListenAddr)
	}
	if !cfg.RequireDownstreamKey {
		log.Printf("[SECURITY-WARN] require_downstream_key is off while bound to a public " +
			"interface; anyone who can reach this port can spend pooled accounts. Enable it for public deploys.")
	}
}

// isLoopbackBindHost reports whether a bind host only accepts on-box connections. An
// empty host or 0.0.0.0/:: means "all interfaces" → NOT loopback (publicly reachable).
func isLoopbackBindHost(host string) bool {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "localhost", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// looksLikeWeakToken flags obviously non-production admin tokens (too short or carrying
// a giveaway substring like "test"/"changeme") so a public deploy gets a loud warning.
func looksLikeWeakToken(tok string) bool {
	if len(strings.TrimSpace(tok)) < 24 {
		return true
	}
	low := strings.ToLower(tok)
	for _, bad := range []string{"test", "local", "example", "changeme", "password", "secret", "admin", "dev"} {
		if strings.Contains(low, bad) {
			return true
		}
	}
	return false
}

// listenerFromSystemd returns the listening socket passed by systemd via the
// LISTEN_FDS socket-activation protocol, or ok=false to fall back to a normal
// net.Listen on cfg.ListenAddr. systemd passes the first socket as fd 3
// (SD_LISTEN_FDS_START) with LISTEN_PID set to this PID and LISTEN_FDS to the count.
// We consume only the first fd (the single HTTP socket). When active, the .socket
// unit retains ownership, so closing this fd on shutdown does NOT destroy the socket —
// systemd hands the same one to the next start (that is what avoids refused connections
// across a restart).
func listenerFromSystemd() (net.Listener, bool) {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, false
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil, false
	}
	const firstSocketFD = 3 // SD_LISTEN_FDS_START
	f := os.NewFile(uintptr(firstSocketFD), "systemd-socket")
	return listenerFromSystemdFile(f)
}

// listenerFromSystemdFile converts systemd's inherited descriptor into the
// independent descriptor owned by net.Listener. net.FileListener duplicates f, so
// the inherited descriptor must be closed in both success and failure paths; leaving
// it open leaks one socket descriptor on every service start.
func listenerFromSystemdFile(f *os.File) (net.Listener, bool) {
	if f == nil {
		return nil, false
	}
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, false
	}
	return ln, true
}
