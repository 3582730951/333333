package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-account-pool/internal/api"
	"codex-account-pool/internal/cfsolve"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/gopay"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/payment"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
	"codex-account-pool/internal/warp"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to JSON configuration file")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("load config: %v", err)
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
		return 1
	}
	defer store.Close()

	if err := store.Init(ctx); err != nil {
		log.Printf("init storage: %v", err)
		return 1
	}
	log.Printf("startup: storage initialized in %s", time.Since(storageInitStarted).Round(time.Millisecond))
	// Encrypt account secrets (tokens, session cookies) at rest with a key derived from
	// the deployment identity secret, so a leaked DB file / backup does not hand over
	// every account. Reads transparently decrypt; legacy plaintext rows are upgraded
	// once here. Set a unique identity_secret in production (an unset one falls back to
	// a host-derived key — better than nothing, but rotating the host invalidates it).
	store.SetTokenEncryptionKey(identity.ResolveSecret([]byte(cfg.IdentitySecret)))
	if n, err := store.EncryptExistingTokens(ctx); err != nil {
		log.Printf("[SECURITY] encrypt existing tokens at rest: %v", err)
	} else if n > 0 {
		log.Printf("[SECURITY] encrypted %d plaintext account token row(s) at rest", n)
	}
	if cfg.DefaultSidecarEndpoint != "" {
		if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{
			ID:             "egress_sidecar",
			Name:           "local curl_cffi sidecar",
			Type:           "curl_cffi_sidecar",
			Endpoint:       cfg.DefaultSidecarEndpoint,
			ChainProxy:     cfg.DefaultSidecarChainProxy,
			StreamCapable:  true,
			Health:         "healthy",
			MaxConcurrency: 0,
		}); err != nil {
			log.Printf("init sidecar egress: %v", err)
			return 1
		}
	}

	gopayMgr := gopay.NewManager(cfg, store)
	// Payment manager abstracts Plus upgrade providers (GoPay/PayPal). Registers both;
	// the automation scheduler picks via policy config.
	paymentMgr := payment.NewManager()
	paymentMgr.Register(payment.NewGopayProvider(gopayMgr, store))
	paymentMgr.Register(payment.NewPaypalProvider(store, nil)) // settings loaded from automation policy config
	up := upstream.NewClient(cfg)
	// WARP CF-fallback manager. The prober wraps ProbeEgress so the manager can refresh
	// an exit's IP after a re-registration without importing the upstream package.
	warpProber := func(pctx context.Context, eg storage.EgressProfile) (string, string, error) {
		r, perr := up.ProbeEgress(pctx, eg)
		return r.IP, r.Country, perr
	}
	warpMgr := warp.NewManager(cfg, store, warpProber, log.Printf)
	if err := warpMgr.EnsurePool(ctx); err != nil {
		log.Printf("warp: ensure pool: %v", err)
	}
	solver := cfsolve.NewClient(cfg)

	leaseCoordinator, err := scheduler.NewLeaseCoordinator(cfg)
	if err != nil {
		log.Printf("init lease coordinator: %v", err)
		return 1
	}
	app := api.NewServer(api.Dependencies{
		Config:     cfg,
		Store:      store,
		Scheduler:  scheduler.NewWithLeaseCoordinator(store, cfg, leaseCoordinator),
		Upstream:   up,
		Planner:    virtual.NewPlanner(store, cfg),
		Gopay:      gopayMgr,
		PaymentMgr: paymentMgr,
		Warp:       warpMgr,
		Solver:     solver,
	})

	// Background model-capability probe sweep (periodic; honors
	// ModelProbeIntervalHours, 0 = disabled). Stops when ctx is cancelled below.
	app.StartBackground(ctx)

	// Background registration-automation scheduler (operator-opt-in policies: auto-refill
	// keeps the active pool topped up). No-op until a policy is enabled.
	app.StartAutomation(ctx)

	// Background quota poller: periodically fetches /backend-api/wham/usage for
	// every active Codex account so the admin dashboard quota gauges show 5h/7d
	// usage even for curl_cffi_sidecar-egressed accounts (their responses carry no
	// x-ratelimit-* headers).
	app.StartQuotaPoller(ctx)

	// Low-cost group×model intelligence/degradation monitor. It is opt-in and
	// never iterates accounts; one normal scheduler sample is used per combination.
	app.StartModelQualityMonitor(ctx)

	// GoPay auto-subscribe (default off). If an operator previously enabled it and
	// auto-start is on, bring the managed Python services up now; failure is logged
	// but never blocks the relay.
	if gopayMgr.Enabled(ctx) && cfg.GopayAutoStart {
		if err := gopayMgr.Start(ctx); err != nil {
			log.Printf("gopay: start skipped: %v", err)
		}
	}
	defer gopayMgr.Stop()

	// Background ledger purge: prevents unbounded virtual_context_ledger growth by
	// periodically evicting rows older than the configured TTL (default 1 hour).
	// Runs every 5 minutes; the purge itself uses batched DELETEs (500 rows/batch)
	// to avoid long write transactions on SQLite.
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

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app,
		ReadHeaderTimeout: 15 * time.Second,
		// Bound slow request-body uploads without imposing a WriteTimeout on SSE or
		// WebSocket responses. Individual upstream calls still use their own context.
		ReadTimeout: cfg.RequestTimeout(),
	}

	serveErr := make(chan error, 1)
	serveHTTPServerAsync(serveErr, func() error { return serveHTTPServer(httpServer) })
	// Historical usage diagnostics affect reports, not request correctness. Run their
	// potentially large JSON backfill only after the listener is accepting health
	// checks; synchronous execution here previously made an active systemd service look
	// dead long enough for install.sh to roll back both the new and previous binaries.
	supervisor.Go(ctx, "storage-deferred-migrations", func(ctx context.Context) {
		started := time.Now()
		if err := store.RunDeferredMigrations(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("deferred storage migrations: %v", err)
			}
			return
		}
		log.Printf("startup: deferred storage migrations completed in %s", time.Since(started).Round(time.Millisecond))
	})

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
	cancelBackground()

	// Graceful drain: stop accepting new connections and let in-flight requests (the
	// long-lived SSE streams this relay serves) finish, up to the configured window.
	// On timeout we log and return (not Fatalf) so the deferred store.Close /
	// gopay.Stop still run for a clean exit; the unit's TimeoutStopSec is set well
	// above this so systemd never SIGKILLs mid-drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout())
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful drain exceeded %s, exiting with streams still active: %v", cfg.ShutdownDrainTimeout(), err)
	}
	// In-flight requests have now drained, so no new async writes will be enqueued.
	// Flush the deferred fire-and-forget DB writes (usage / virtual-ledger rows) before
	// the deferred store.Close runs, so a clean shutdown loses no recorded usage.
	app.FlushWrites()
	if serveFailure != nil {
		return 1
	}
	return 0
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
