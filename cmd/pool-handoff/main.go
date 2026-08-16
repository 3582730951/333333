// Command pool-handoff is the stable public listener for A/B worker deployments.
// Each request resolves backend-link, so an atomic symlink rename moves new traffic
// to the new worker while established HTTP, SSE, and WebSocket connections continue
// using the worker selected when they began.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-account-pool/internal/corestate"
	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/supervisor"
)

const (
	pausePath                    = "/_codex_pool/handoff/pause"
	resumePath                   = "/_codex_pool/handoff/resume"
	publicRetryAfter             = "3"
	publicServiceUnavailableText = "The relay service is temporarily unavailable. Please retry."
)

var handoffRequestSequence atomic.Uint64

type pauseState struct {
	PausedAt time.Time `json:"paused_at"`
	Reason   string    `json:"reason,omitempty"`
	Release  string    `json:"release,omitempty"`
}

// admissionGate is a replaceable-channel barrier. Requests which have already
// entered the proxy never consult it again, so pausing admission cannot freeze or
// sever an established HTTP, SSE, or WebSocket stream. A pause state file makes the
// barrier fail closed if the handoff process happens to restart during a deployment.
type admissionGate struct {
	mu        sync.Mutex
	paused    bool
	wait      chan struct{}
	queued    int64
	pausedAt  time.Time
	reason    string
	release   string
	stateFile string
}

func newAdmissionGate(stateFile string) *admissionGate {
	g := &admissionGate{stateFile: strings.TrimSpace(stateFile)}
	g.wait = make(chan struct{})
	close(g.wait)
	if g.stateFile == "" {
		return g
	}
	data, err := os.ReadFile(g.stateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			g.paused = true
			g.wait = make(chan struct{})
			g.pausedAt = time.Now().UTC()
			g.reason = "pause_state_unreadable"
		}
		return g
	}
	state := pauseState{PausedAt: time.Now().UTC(), Reason: strings.TrimSpace(string(data))}
	if err := json.Unmarshal(data, &state); err != nil {
		state.Reason = strings.TrimSpace(string(data))
	}
	if state.PausedAt.IsZero() {
		state.PausedAt = time.Now().UTC()
	}
	g.paused = true
	g.wait = make(chan struct{})
	g.pausedAt, g.reason, g.release = state.PausedAt, state.Reason, state.Release
	return g
}

func (g *admissionGate) pause(reason, release string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return nil
	}
	state := pauseState{
		PausedAt: time.Now().UTC(),
		Reason:   strings.TrimSpace(reason),
		Release:  strings.TrimSpace(release),
	}
	if err := g.persistLocked(state); err != nil {
		return err
	}
	g.paused = true
	g.wait = make(chan struct{})
	g.pausedAt, g.reason, g.release = state.PausedAt, state.Reason, state.Release
	return nil
}

func (g *admissionGate) resume() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stateFile != "" {
		if err := os.Remove(g.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove admission pause state: %w", err)
		}
	}
	if !g.paused {
		return nil
	}
	g.paused = false
	close(g.wait)
	g.pausedAt = time.Time{}
	g.reason, g.release = "", ""
	return nil
}

func (g *admissionGate) persistLocked(state pauseState) error {
	if g.stateFile == "" {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := g.stateFile + ".next." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write admission pause state: %w", err)
	}
	if err := os.Rename(tmp, g.stateFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit admission pause state: %w", err)
	}
	return nil
}

func (g *admissionGate) await(ctx context.Context) error {
	g.mu.Lock()
	if !g.paused {
		g.mu.Unlock()
		return nil
	}
	wait := g.wait
	g.queued++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.queued--
		g.mu.Unlock()
	}()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseForShutdown prevents http.Server.Shutdown from waiting forever on handlers
// parked at the barrier. It deliberately retains the state file: if this was an
// unexpected restart during installation, the replacement handoff starts paused too.
func (g *admissionGate) releaseForShutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.wait)
	}
}

func (g *admissionGate) snapshot() (bool, int64, time.Time, string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused, g.queued, g.pausedAt, g.reason, g.release
}

type handoff struct {
	backendLink string
	instanceID  string
	startedAt   time.Time
	inflight    atomic.Int64
	gate        *admissionGate
	coreState   *corestate.Reader
	routeSource atomic.Value
	snapshotGen atomic.Uint64
	proxy       *httputil.ReverseProxy
}

type backendCandidate struct {
	path       string
	source     string
	generation uint64
}

func newHandoff(backendLink string) *handoff {
	return newHandoffWithState(backendLink, "", "")
}

func newHandoffWithState(backendLink, pauseStateFile, instanceID string) *handoff {
	h := &handoff{
		backendLink: backendLink,
		instanceID:  strings.TrimSpace(instanceID),
		startedAt:   time.Now().UTC(),
		gate:        newAdmissionGate(pauseStateFile),
	}
	target := &url.URL{Scheme: "http", Host: "worker"}
	transport := &http.Transport{
		// A backend connection is pinned for the lifetime of an HTTP request/upgrade.
		// Disabling idle reuse makes the symlink the linearization point for the next
		// request after a deployment switch.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, _, err := h.dialBackend(ctx)
			return conn, err
		},
	}
	h.proxy = httputil.NewSingleHostReverseProxy(target)
	h.proxy.Transport = transport
	h.proxy.FlushInterval = -1
	h.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		requestID := newHandoffRequestID()
		log.Printf("[HANDOFF] request_id=%s backend unavailable: %v", requestID, err)
		writeHandoffUnavailable(w, requestID)
	}
	return h
}

func (h *handoff) setCoreStateReader(reader *corestate.Reader) {
	h.coreState = reader
}

// dialBackend prefers the atomic active-worker symlink. If the link is missing or
// points at a failed generation during a worker crash, it tries only authenticated
// A/B snapshot routes. Optional-module, database, migration and SPA failures never
// participate in this decision.
func (h *handoff) dialBackend(ctx context.Context) (net.Conn, backendCandidate, error) {
	candidates, resolveErr := h.backendCandidates()
	var failures []error
	if resolveErr != nil {
		failures = append(failures, resolveErr)
	}
	for _, candidate := range candidates {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", candidate.path)
		if err != nil {
			failures = append(failures, fmt.Errorf("dial %s route %s: %w", candidate.source, candidate.path, err))
			continue
		}
		h.routeSource.Store(candidate.source)
		h.snapshotGen.Store(candidate.generation)
		return conn, candidate, nil
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no relay backend route is available"))
	}
	return nil, backendCandidate{}, errors.Join(failures...)
}

func (h *handoff) backendCandidates() ([]backendCandidate, error) {
	seen := make(map[string]struct{}, 3)
	candidates := make([]backendCandidate, 0, 3)
	add := func(candidate backendCandidate) {
		candidate.path = filepath.Clean(strings.TrimSpace(candidate.path))
		if candidate.path == "." || candidate.path == "" || !filepath.IsAbs(candidate.path) {
			return
		}
		if _, exists := seen[candidate.path]; exists {
			return
		}
		seen[candidate.path] = struct{}{}
		candidates = append(candidates, candidate)
	}
	var failures []error
	resolved, err := filepath.EvalSymlinks(h.backendLink)
	if err == nil {
		add(backendCandidate{path: resolved, source: "active-link"})
	} else {
		failures = append(failures, fmt.Errorf("resolve active worker: %w", err))
	}
	if h.coreState != nil {
		snapshot, snapshotErr := h.coreState.Load()
		if snapshotErr != nil {
			failures = append(failures, fmt.Errorf("load core state: %w", snapshotErr))
		} else {
			add(backendCandidate{path: snapshot.ActiveWorker, source: "core-snapshot", generation: snapshot.Generation})
			add(backendCandidate{path: snapshot.PreviousWorker, source: "core-snapshot-previous", generation: snapshot.Generation})
		}
	}
	return candidates, errors.Join(failures...)
}

func newHandoffRequestID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err == nil {
		return fmt.Sprintf("REQ-%X", random[:])
	}
	return fmt.Sprintf("REQ-%016X", handoffRequestSequence.Add(1))
}

func writeHandoffUnavailable(w http.ResponseWriter, requestID string) {
	headers := w.Header()
	for name := range headers {
		headers.Del(name)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", publicRetryAfter)
	headers.Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":       "server_error",
			"code":       "service_unavailable",
			"message":    publicServiceUnavailableText,
			"request_id": requestID,
		},
	})
}

func (h *handoff) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	emergencyDiagnostic := isHandoffEmergencyDiagnosticRequest(r)
	switch r.URL.Path {
	case "/handoffz":
		h.serveStatus(w, false)
		return
	case pausePath, resumePath:
		h.serveControl(w, r, false)
		return
	case "/readyz", "/healthz":
		// Deployment and load-balancer probes must remain observable while normal
		// admission is paused. /readyz is also the installer's post-switch proof.
	default:
		if emergencyDiagnostic {
			paused, _, _, _, _ := h.gate.snapshot()
			if paused {
				// A synchronous full snapshot could prolong the SQLite lock recovery
				// that paused admission. Ask the worker for its database-independent,
				// in-memory archive while retaining normal downstream admin auth.
				query := r.URL.Query()
				query.Set("mode", "emergency")
				r.URL.RawQuery = query.Encode()
			}
		} else {
			if err := h.gate.await(r.Context()); err != nil {
				return
			}
		}
	}
	h.inflight.Add(1)
	defer h.inflight.Add(-1)
	h.proxy.ServeHTTP(w, r)
}

func isHandoffEmergencyDiagnosticRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet || r.URL == nil || r.URL.Path != "/admin/export/logs" {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	return mode == "rescue" || mode == "emergency"
}

func (h *handoff) serveControl(w http.ResponseWriter, r *http.Request, trustedUnixSocket bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !trustedUnixSocket && !loopbackRemote(r.RemoteAddr) {
		http.Error(w, "local control endpoint", http.StatusForbidden)
		return
	}
	var err error
	switch r.URL.Path {
	case pausePath:
		err = h.gate.pause(r.URL.Query().Get("reason"), r.URL.Query().Get("release"))
	case resumePath:
		err = h.gate.resume()
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	paused, queued, pausedAt, reason, release := h.gate.snapshot()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":               true,
		"admission_paused": paused,
		"queued":           queued,
		"paused_at":        formatOptionalTime(pausedAt),
		"pause_reason":     reason,
		"pause_release":    release,
	})
}

func loopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func formatOptionalTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

func (h *handoff) serveStatus(w http.ResponseWriter, trusted bool) {
	probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	conn, candidate, err := h.dialBackend(probeCtx)
	cancel()
	if err == nil {
		_ = conn.Close()
	}
	ready := err == nil
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	paused, queued, pausedAt, reason, release := h.gate.snapshot()
	body := map[string]interface{}{
		"ok":                  ready,
		"ready":               ready,
		"deployment_state":    "handoff",
		"instance_id":         h.instanceID,
		"inflight":            h.inflight.Load(),
		"admission_paused":    paused,
		"queued":              queued,
		"paused_at":           formatOptionalTime(pausedAt),
		"pause_reason":        reason,
		"pause_release":       release,
		"started_at":          h.startedAt.Format(time.RFC3339Nano),
		"route_source":        candidate.source,
		"snapshot_generation": candidate.generation,
	}
	if trusted {
		body["active_backend"] = candidate.path
		if err != nil {
			body["error"] = err.Error()
		}
	} else if err != nil {
		body["error"] = map[string]string{
			"code":    "worker_unavailable",
			"message": "The active worker is temporarily unavailable.",
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func coreStateReaderFromEnvironment() (*corestate.Reader, error) {
	dataDir := strings.TrimSpace(os.Getenv("CODEX_POOL_DATA_DIR"))
	stateDir := strings.TrimSpace(os.Getenv("CODEX_POOL_CORE_STATE_DIR"))
	keyPath := strings.TrimSpace(os.Getenv("CODEX_POOL_CORE_STATE_KEY_FILE"))
	if credentialDirectory := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY")); keyPath == "" && credentialDirectory != "" {
		keyPath = filepath.Join(credentialDirectory, "core-state.key")
	}
	if stateDir == "" && dataDir != "" {
		stateDir = filepath.Join(dataDir, "core-state")
	}
	if keyPath == "" && dataDir != "" {
		keyPath = filepath.Join(dataDir, "keys", "core-state.key")
	}
	if stateDir == "" || keyPath == "" {
		return nil, nil
	}
	key, err := datadir.LoadCredentialKey(keyPath)
	if err != nil {
		return nil, err
	}
	return corestate.NewReader(stateDir, key)
}

func main() {
	var listenAddr, backendLink, controlSocket, pauseStateFile, instanceID string
	var selfTest bool
	flag.StringVar(&listenAddr, "listen", "0.0.0.0:8787", "public TCP listen address")
	flag.StringVar(&backendLink, "backend-link", "/var/lib/codex-pool/run/active-worker.sock", "symlink to active worker Unix socket")
	flag.StringVar(&controlSocket, "control-socket", "/var/lib/codex-pool/run/handoff-control.sock", "local deployment control Unix socket")
	flag.StringVar(&pauseStateFile, "pause-state", "/var/lib/codex-pool/run/admission-paused.json", "persistent admission pause state")
	flag.StringVar(&instanceID, "instance-id", "", "handoff instance identifier exposed by /handoffz")
	flag.BoolVar(&selfTest, "self-test", false, "verify the handoff binary")
	flag.Parse()
	if selfTest {
		fmt.Println("codex-pool-handoff self-test ok")
		return
	}

	h := newHandoffWithState(backendLink, pauseStateFile, instanceID)
	if reader, readerErr := coreStateReaderFromEnvironment(); readerErr != nil {
		// A missing/corrupt snapshot must never gate the survival kernel. The
		// active symlink remains the primary route and status exposes any failure.
		log.Printf("core state fallback unavailable: %v", readerErr)
	} else {
		h.setCoreStateReader(reader)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	ln, err := activatedListener()
	if err != nil {
		log.Fatalf("activation listener: %v", err)
	}
	if ln == nil {
		ln, err = net.Listen("tcp", listenAddr)
		if err != nil {
			log.Fatalf("listen %s: %v", listenAddr, err)
		}
	}

	serveErr := make(chan error, 2)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				supervisor.LogPanic("pool-handoff-http", v)
				serveErr <- fmt.Errorf("HTTP server panic: %v", v)
			}
		}()
		serveErr <- srv.Serve(ln)
	}()

	var controlServer *http.Server
	if strings.TrimSpace(controlSocket) != "" {
		if err := os.Remove(controlSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("remove stale handoff control socket: %v", err)
		}
		controlListener, err := net.Listen("unix", controlSocket)
		if err != nil {
			log.Fatalf("listen handoff control socket: %v", err)
		}
		if err := os.Chmod(controlSocket, 0660); err != nil {
			_ = controlListener.Close()
			log.Fatalf("chmod handoff control socket: %v", err)
		}
		defer os.Remove(controlSocket)
		controlServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/handoffz" {
				h.serveStatus(w, true)
				return
			}
			h.serveControl(w, r, true)
		})}
		go func() {
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("pool-handoff-control", v)
					serveErr <- fmt.Errorf("control server panic: %v", v)
				}
			}()
			serveErr <- controlServer.Serve(controlListener)
		}()
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Printf("received %s, draining handoff", sig)
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
		return
	}
	h.gate.releaseForShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("handoff drain: %v", err)
	}
	if controlServer != nil {
		if err := controlServer.Shutdown(ctx); err != nil {
			log.Printf("handoff control shutdown: %v", err)
		}
	}
}

func activatedListener() (net.Listener, error) {
	if strings.TrimSpace(os.Getenv("LISTEN_PID")) != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil, nil
	}
	f := os.NewFile(3, "systemd-socket")
	if f == nil {
		return nil, errors.New("fd 3 unavailable")
	}
	ln, err := net.FileListener(f)
	_ = f.Close()
	return ln, err
}
