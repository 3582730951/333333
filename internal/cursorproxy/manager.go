// Package cursorproxy manages the independently versioned cursor-api-proxy
// bridge used by Cursor accounts. Each selected pool account gets one lazy local
// bridge process, so credentials and Cursor CLI state never bleed across accounts.
package cursorproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

const (
	ProviderID        = "cursor"
	CredentialBrowser = "cursor_browser"
	ReferenceProject  = "https://github.com/anyrobert/cursor-api-proxy"
	ReferenceVersion  = "1.4.0"
	// ReferenceCommit is the gitHead embedded in the pinned npm release.
	ReferenceCommit = "b04c55599cb63e3310b798b66349d161bd323e62"
	// ReviewedHeadCommit records the separately reviewed, recently maintained
	// upstream head. It is provenance only; production executes the lockfile's
	// immutable ReferenceVersion artifact.
	ReviewedHeadCommit = "6364d9e5eb6811980fccadc413af2c0d11b995f2"
	defaultBinary      = "/usr/local/lib/codex-pool/releases/docker/cursor-proxy/node_modules/.bin/cursor-api-proxy"
	defaultUsageURL    = "https://api2.cursor.sh/auth/usage"
	failedStartTTL     = 30 * time.Second
	startTimeout       = 20 * time.Second
	maxInstances       = 64
)

type Credential struct {
	BridgeKey string
	APIKey    string
	ConfigDir string
	ProxyURL  string
}

type ModelUsage struct {
	NumRequests      int64  `json:"numRequests"`
	NumRequestsTotal int64  `json:"numRequestsTotal"`
	NumTokens        int64  `json:"numTokens"`
	MaxTokenUsage    *int64 `json:"maxTokenUsage"`
	MaxRequestUsage  *int64 `json:"maxRequestUsage"`
}

type Usage struct {
	StartOfMonth string
	Models       map[string]ModelUsage
}

type instance struct {
	fingerprint string
	baseURL     string
	cmd         *exec.Cmd
	ready       chan struct{}
	finishOnce  sync.Once
	leases      int
	err         error
	retryAt     time.Time
	lastUsed    time.Time
	stopped     bool
}

type Manager struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	binary      string
	agentBinary string
	instances   map[string]*instance
	http        *http.Client
	healthHTTP  *http.Client
	usageURL    string
	available   bool
}

func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	binary := strings.TrimSpace(os.Getenv("CODEX_CURSOR_PROXY_BIN"))
	if binary == "" {
		binary = defaultBinary
	}
	agentBinary := strings.TrimSpace(os.Getenv("CURSOR_AGENT_BIN"))
	if agentBinary == "" {
		agentBinary = "agent"
	}
	return &Manager{
		ctx: ctx, cancel: cancel, binary: binary, agentBinary: agentBinary, instances: make(map[string]*instance),
		http: &http.Client{Timeout: 8 * time.Second},
		healthHTTP: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				Proxy: nil,
			},
		},
		usageURL: defaultUsageURL,
	}
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.cancel()
	m.mu.Lock()
	for _, item := range m.instances {
		item.stopped = true
		if item.cmd != nil && item.cmd.Process != nil {
			_ = item.cmd.Process.Kill()
		}
	}
	m.instances = make(map[string]*instance)
	m.mu.Unlock()
}

func (m *Manager) Stop(accountID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	item := m.instances[accountID]
	delete(m.instances, accountID)
	if item != nil {
		item.stopped = true
		if item.cmd != nil && item.cmd.Process != nil {
			_ = item.cmd.Process.Kill()
		}
	}
	m.mu.Unlock()
}

func (m *Manager) Available() error {
	if m == nil {
		return errors.New("Cursor proxy manager is unavailable")
	}
	m.mu.Lock()
	if m.available {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if filepath.IsAbs(m.binary) {
		info, err := os.Stat(m.binary)
		if err != nil {
			return fmt.Errorf("Cursor proxy module is not installed at %s", m.binary)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("Cursor proxy module is not executable at %s", m.binary)
		}
	} else if _, err := exec.LookPath(m.binary); err != nil {
		return fmt.Errorf("Cursor proxy module %q is not on PATH", m.binary)
	}
	if filepath.IsAbs(m.agentBinary) {
		info, err := os.Stat(m.agentBinary)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("Cursor Agent is not executable at %s", m.agentBinary)
		}
	} else if _, err := exec.LookPath(m.agentBinary); err != nil {
		return fmt.Errorf("Cursor Agent %q is not on PATH", m.agentBinary)
	}
	m.mu.Lock()
	m.available = true
	m.mu.Unlock()
	return nil
}

func ValidateCredential(credential Credential) error {
	if strings.TrimSpace(credential.BridgeKey) == "" {
		return errors.New("Cursor bridge key is empty")
	}
	hasAPIKey := strings.TrimSpace(credential.APIKey) != ""
	hasConfig := strings.TrimSpace(credential.ConfigDir) != ""
	if hasAPIKey == hasConfig {
		return errors.New("provide exactly one of Cursor API key or browser-login config directory")
	}
	if hasConfig {
		info, err := os.Stat(filepath.Join(credential.ConfigDir, "cli-config.json"))
		if err != nil || info.IsDir() {
			return errors.New("Cursor browser login is incomplete: cli-config.json not found")
		}
	}
	return nil
}

func credentialFingerprint(credential Credential) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{credential.BridgeKey, credential.APIKey, credential.ConfigDir, credential.ProxyURL}, "\x00")))
	return hex.EncodeToString(digest[:])
}

// Acquire returns the account-specific local OpenAI-compatible base URL and a
// release function. Calls for the same account are singleflighted, a failed
// spawn is negatively cached, and an instance with an active lease is never
// selected for LRU eviction. Callers must release after the response body has
// finished, not merely after opening the local HTTP request.
func (m *Manager) Acquire(ctx context.Context, accountID string, credential Credential) (string, func(), error) {
	noRelease := func() {}
	if err := ValidateCredential(credential); err != nil {
		return "", noRelease, err
	}
	if err := m.Available(); err != nil {
		return "", noRelease, err
	}
	fingerprint := credentialFingerprint(credential)
	m.mu.Lock()
	if current := m.instances[accountID]; current != nil && current.fingerprint == fingerprint {
		current.lastUsed = time.Now()
		ready := current.ready
		m.mu.Unlock()
		select {
		case <-ready:
			if current.err != nil && time.Now().Before(current.retryAt) {
				return "", noRelease, current.err
			}
			if current.err == nil {
				if baseURL, release, ok := m.acquireReady(accountID, current); ok {
					return baseURL, release, nil
				}
				return m.Acquire(ctx, accountID, credential)
			}
			m.mu.Lock()
			if m.instances[accountID] == current {
				delete(m.instances, accountID)
			}
			m.mu.Unlock()
			return m.Acquire(ctx, accountID, credential)
		case <-ctx.Done():
			return "", noRelease, ctx.Err()
		}
	}
	if old := m.instances[accountID]; old != nil {
		if old.leases > 0 {
			m.mu.Unlock()
			return "", noRelease, errors.New("Cursor account credentials changed while requests are still active; retry after they finish")
		}
		old.stopped = true
		if old.cmd != nil && old.cmd.Process != nil {
			_ = old.cmd.Process.Kill()
		}
		delete(m.instances, accountID)
	}
	if len(m.instances) >= maxInstances {
		if !m.evictOldestLocked() {
			m.mu.Unlock()
			return "", noRelease, fmt.Errorf("Cursor proxy instance limit %d reached; all instances are active or starting", maxInstances)
		}
	}
	current := &instance{fingerprint: fingerprint, ready: make(chan struct{}), lastUsed: time.Now()}
	m.instances[accountID] = current
	m.mu.Unlock()
	go m.start(accountID, current, credential)
	select {
	case <-current.ready:
		if current.err != nil {
			return "", noRelease, current.err
		}
		if baseURL, release, ok := m.acquireReady(accountID, current); ok {
			return baseURL, release, nil
		}
		return m.Acquire(ctx, accountID, credential)
	case <-ctx.Done():
		return "", noRelease, ctx.Err()
	}
}

func (m *Manager) acquireReady(accountID string, item *instance) (string, func(), bool) {
	m.mu.Lock()
	if m.instances[accountID] != item || item.err != nil || item.stopped || item.baseURL == "" {
		m.mu.Unlock()
		return "", func() {}, false
	}
	item.leases++
	item.lastUsed = time.Now()
	baseURL := item.baseURL
	m.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			if item.leases > 0 {
				item.leases--
			}
			item.lastUsed = time.Now()
			m.mu.Unlock()
		})
	}
	return baseURL, release, true
}

func (m *Manager) evictOldestLocked() bool {
	var oldestID string
	var oldest *instance
	for id, item := range m.instances {
		if item.leases != 0 || !instanceReady(item) {
			continue
		}
		if oldest == nil || item.lastUsed.Before(oldest.lastUsed) {
			oldestID, oldest = id, item
		}
	}
	if oldest == nil {
		return false
	}
	oldest.stopped = true
	if oldest.cmd != nil && oldest.cmd.Process != nil {
		_ = oldest.cmd.Process.Kill()
	}
	delete(m.instances, oldestID)
	return true
}

func instanceReady(item *instance) bool {
	if item == nil || item.ready == nil {
		return false
	}
	select {
	case <-item.ready:
		return true
	default:
		return false
	}
}

func reserveLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func (m *Manager) start(accountID string, item *instance, credential Credential) {
	var runtimeHome string
	var command *exec.Cmd
	finish := func(baseURL string, err error) {
		item.finishOnce.Do(func() {
			m.mu.Lock()
			item.baseURL = baseURL
			item.err = err
			if err != nil {
				item.retryAt = time.Now().Add(failedStartTTL)
			}
			close(item.ready)
			m.mu.Unlock()
		})
	}
	defer func() {
		if panicValue := recover(); panicValue != nil {
			supervisor.LogPanic("cursor-proxy-start", panicValue)
			if command != nil && command.Process != nil {
				_ = command.Process.Kill()
			}
			if runtimeHome != "" {
				_ = os.RemoveAll(runtimeHome)
			}
			finish("", errors.New("Cursor proxy start panicked"))
		}
	}()
	port, err := reserveLocalPort()
	if err != nil {
		finish("", err)
		return
	}
	m.mu.Lock()
	stopped := item.stopped
	m.mu.Unlock()
	if stopped {
		finish("", errors.New("Cursor proxy account was stopped"))
		return
	}
	runtimeHome, err = os.MkdirTemp("", "codex-pool-cursor-")
	if err != nil {
		finish("", fmt.Errorf("create isolated Cursor runtime: %w", err))
		return
	}
	if err := os.Chmod(runtimeHome, 0o700); err != nil {
		_ = os.RemoveAll(runtimeHome)
		finish("", fmt.Errorf("secure isolated Cursor runtime: %w", err))
		return
	}
	command = exec.CommandContext(m.ctx, m.binary)
	command.Env = cursorChildEnvironment(credential, port, m.agentBinary, runtimeHome)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(runtimeHome)
		finish("", fmt.Errorf("start Cursor proxy module: %w", err))
		return
	}
	m.mu.Lock()
	item.cmd = command
	stopped = item.stopped
	m.mu.Unlock()
	if stopped {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = os.RemoveAll(runtimeHome)
		finish("", errors.New("Cursor proxy account was stopped"))
		return
	}
	go func() {
		defer supervisor.Recover("cursor-proxy-process-wait")
		waitErr := command.Wait()
		_ = os.RemoveAll(runtimeHome)
		select {
		case <-item.ready:
			// Startup already completed. A successfully started process is removed
			// below so the next request can replace the exited instance.
		default:
			processErr := errors.New("Cursor proxy exited before it became ready")
			if waitErr != nil {
				processErr = fmt.Errorf("Cursor proxy exited before it became ready: %w", waitErr)
			}
			// Preserve this instance as a negative-cache entry. Without completing
			// the flight here, every request arriving during the 20-second health
			// loop could spawn another immediately-exiting process.
			finish("", processErr)
		}
		m.mu.Lock()
		if current := m.instances[accountID]; current == item && item.err == nil {
			delete(m.instances, accountID)
		}
		m.mu.Unlock()
	}()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-item.ready:
			return
		default:
		}
		m.mu.Lock()
		stopped = item.stopped
		m.mu.Unlock()
		if stopped {
			finish("", errors.New("Cursor proxy account was stopped"))
			return
		}
		req, _ := http.NewRequestWithContext(m.ctx, http.MethodGet, base+"/healthz", nil)
		resp, requestErr := m.healthHTTP.Do(req)
		if requestErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				finish(base+"/v1", nil)
				return
			}
		}
		select {
		case <-m.ctx.Done():
			finish("", m.ctx.Err())
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = command.Process.Kill()
	finish("", errors.New("Cursor proxy module did not become ready within 20 seconds"))
}

// cursorChildEnvironment starts from a minimal runtime allowlist, then adds the
// selected account's exact values. A third-party child must not inherit pool
// database/admin secrets, another Cursor credential, or a host-global egress.
func cursorChildEnvironment(credential Credential, port int, agentBinary, runtimeHome string) []string {
	environment := make([]string, 0, len(os.Environ())+16)
	allowed := map[string]struct{}{
		"PATH": {}, "USER": {}, "LOGNAME": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TZ": {},
		"TMPDIR": {}, "TMP": {}, "TEMP": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {}, "NODE_EXTRA_CA_CERTS": {},
	}
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(key))
		if _, ok := allowed[upper]; !ok {
			continue
		}
		environment = append(environment, item)
	}
	environment = append(environment,
		"HOME="+runtimeHome,
		"USERPROFILE="+runtimeHome,
		"CURSOR_BRIDGE_HOST=127.0.0.1",
		"CURSOR_BRIDGE_PORT="+strconv.Itoa(port),
		"CURSOR_BRIDGE_API_KEY="+credential.BridgeKey,
		"CURSOR_BRIDGE_USE_ACP=1",
		"CURSOR_BRIDGE_CHAT_ONLY_WORKSPACE=1",
		"CURSOR_BRIDGE_VERBOSE=0",
		"CURSOR_AGENT_BIN="+agentBinary,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	if credential.APIKey != "" {
		environment = append(environment, "CURSOR_API_KEY="+credential.APIKey, "CURSOR_AUTH_TOKEN="+credential.APIKey)
	} else {
		environment = append(environment, "CURSOR_CONFIG_DIRS="+credential.ConfigDir)
	}
	if proxyURL := strings.TrimSpace(credential.ProxyURL); proxyURL != "" {
		environment = append(environment,
			"HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL, "ALL_PROXY="+proxyURL,
		)
	}
	return environment
}

// FetchUsage makes exactly one bounded Cursor usage request. Browser accounts
// use the proxy module's private 0600 token cache when present; no login or
// inference request is triggered merely to obtain quota.
func (m *Manager) FetchUsage(ctx context.Context, credential Credential) (Usage, error) {
	return m.FetchUsageWithClient(ctx, credential, nil)
}

func (m *Manager) FetchUsageWithClient(ctx context.Context, credential Credential, client *http.Client) (Usage, error) {
	token := strings.TrimSpace(credential.APIKey)
	if token == "" && credential.ConfigDir != "" {
		raw, err := os.ReadFile(filepath.Join(credential.ConfigDir, ".cursor-token"))
		if err != nil {
			return Usage{}, errors.New("Cursor usage token is not cached yet; run one Cursor request first")
		}
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		return Usage{}, errors.New("Cursor usage credential is empty")
	}
	usageURL := m.usageURL
	if usageURL == "" {
		usageURL = defaultUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if client == nil {
		client = m.http
	}
	resp, err := client.Do(req)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Usage{}, fmt.Errorf("Cursor usage returned HTTP %d", resp.StatusCode)
	}
	var root map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&root); err != nil {
		return Usage{}, err
	}
	usage := Usage{Models: make(map[string]ModelUsage)}
	for key, raw := range root {
		if key == "startOfMonth" {
			_ = json.Unmarshal(raw, &usage.StartOfMonth)
			continue
		}
		var model ModelUsage
		if json.Unmarshal(raw, &model) == nil {
			usage.Models[key] = model
		}
	}
	return usage, nil
}
