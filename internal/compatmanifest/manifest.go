// Package compatmanifest maintains a validated last-known-good compatibility
// profile for upstream client fingerprints and optional Codex fallback models.
// It is deliberately independent from database startup and request serving: a
// broken source, signature, snapshot, or network path can never gate the relay.
package compatmanifest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion     = 1
	SnapshotVersion   = 1
	maxManifestBytes  = 2 << 20
	maxManifestModels = 256
	maxContextWindow  = int64(4_000_000)
	// The official Codex CLI is distributed as @openai/codex. Prefer the small npm
	// dist-tag document: unauthenticated GitHub API calls are commonly rate-limited
	// with HTTP 403. GitHub remains a one-shot fallback when the registry is down.
	officialCodexNPMURL    = "https://registry.npmjs.org/@openai/codex/latest"
	officialCodexGitHubURL = "https://api.github.com/repos/openai/codex/releases/latest"
	officialClaudeURL      = "https://registry.npmjs.org/@anthropic-ai/claude-code/latest"
	officialUserAgent      = "codex-account-pool-compatibility-manifest/1"
	defaultHTTPTimeout     = 20 * time.Second
	maximumManifestLife    = 180 * 24 * time.Hour
)

var dottedVersionRE = regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+\.[0-9]+\.[0-9]+)(?:[-+][0-9a-z.-]+)?(?:$|[^0-9])`)

// Config is the hot-reloadable source and persistence policy used for one load
// or refresh operation.
type Config struct {
	Enabled      bool
	Source       string
	URL          string
	PublicKey    string
	MaxStale     time.Duration
	RefreshEvery time.Duration
}

type ClientProfile struct {
	Version          string `json:"version,omitempty"`
	CLIVersion       string `json:"cli_version,omitempty"`
	NodeVersion      string `json:"node_version,omitempty"`
	StainlessVersion string `json:"stainless_version,omitempty"`
	// AttributionSuffix is an empirical assertion ONLY a signed_custom manifest can
	// make — verified by running the official client binary against a capture server
	// — of whether the billing block's cc_version currently carries the
	// message-derived `.xxx` attribution suffix ("live") or not ("plain"). The
	// gateway mirrors it by enabling (or disabling) ClaudeAttributionFingerprint. The
	// "official" npm-derived payload can never assert it, because npm does not run
	// the client.
	AttributionSuffix string `json:"attribution_suffix,omitempty"`
}

// Model describes only conservative, client-visible fallback metadata. A live
// account-scoped /models response remains authoritative for routability.
type Model struct {
	Slug                  string   `json:"slug"`
	ContextWindow         int64    `json:"context_window"`
	MaxContextWindow      int64    `json:"max_context_window"`
	AutoCompactTokenLimit int64    `json:"auto_compact_token_limit,omitempty"`
	MinimumClientVersion  string   `json:"minimum_client_version,omitempty"`
	RequiresCurrentClient bool     `json:"requires_current_client,omitempty"`
	PreferWebSocket       bool     `json:"prefer_websocket,omitempty"`
	ResponsesLite         bool     `json:"responses_lite,omitempty"`
	ReasoningLevels       []string `json:"reasoning_levels,omitempty"`
}

type Payload struct {
	SchemaVersion int           `json:"schema_version"`
	Generation    int64         `json:"generation"`
	IssuedAt      int64         `json:"issued_at"`
	ExpiresAt     int64         `json:"expires_at"`
	Source        string        `json:"source"`
	Codex         ClientProfile `json:"codex"`
	Claude        ClientProfile `json:"claude"`
	Models        []Model       `json:"models,omitempty"`
}

type Status struct {
	Enabled          bool   `json:"enabled"`
	Source           string `json:"source"`
	State            string `json:"state"`
	Digest           string `json:"digest,omitempty"`
	Generation       int64  `json:"generation,omitempty"`
	FetchedAt        int64  `json:"fetched_at,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	LastAttemptAt    int64  `json:"last_attempt_at,omitempty"`
	LastSuccessAt    int64  `json:"last_success_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	SnapshotSlot     string `json:"snapshot_slot,omitempty"`
	SignatureChecked bool   `json:"signature_checked"`
	Canary           string `json:"canary,omitempty"`
	ModelCount       int    `json:"model_count"`
}

type persistedSnapshot struct {
	Version   int             `json:"version"`
	Sequence  uint64          `json:"sequence"`
	FetchedAt int64           `json:"fetched_at"`
	Source    string          `json:"source"`
	Digest    string          `json:"digest"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature,omitempty"`
}

type fetchedManifest struct {
	payload   Payload
	raw       []byte
	signature string
}

type sourceHTTPError struct {
	status     int
	retryAfter time.Duration
}

func (e *sourceHTTPError) Error() string {
	return fmt.Sprintf("source returned HTTP %d", e.status)
}

// SuggestedRetryDelay returns the longest bounded Retry-After/rate-limit reset
// advertised anywhere in a wrapped multi-source refresh error. Callers can combine
// it with their own exponential backoff so outages and 403/429 rate limits never
// turn into an aggressive poll loop.
func SuggestedRetryDelay(err error) time.Duration {
	var longest time.Duration
	var visit func(error)
	visit = func(candidate error) {
		if candidate == nil {
			return
		}
		if sourceErr, ok := candidate.(*sourceHTTPError); ok && sourceErr.retryAfter > longest {
			longest = sourceErr.retryAfter
		}
		switch wrapped := candidate.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	const maximumRetryDelay = 24 * time.Hour
	if longest > maximumRetryDelay {
		return maximumRetryDelay
	}
	return longest
}

type refreshFlight struct {
	key     string
	done    chan struct{}
	payload Payload
	changed bool
	err     error
}

// Manager owns the active immutable profile and A/B snapshots. Callers may use
// Active and Status concurrently with refreshes.
type Manager struct {
	mu       sync.RWMutex
	flightMu sync.Mutex
	flight   *refreshFlight
	dataDir  string
	client   *http.Client
	now      func() time.Time
	active   Payload
	raw      []byte
	digest   string
	sequence uint64
	status   Status
}

func New(dataDir string, client *http.Client) *Manager {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Manager{dataDir: strings.TrimSpace(dataDir), client: client, now: time.Now,
		status: Status{State: "not_loaded"}}
}

func (m *Manager) Active() (Payload, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.raw) == 0 {
		return Payload{}, false
	}
	return clonePayload(m.active), true
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) SetEnabled(enabled bool, source string) {
	m.mu.Lock()
	m.status.Enabled = enabled
	m.status.Source = normalizeSource(source)
	if !enabled {
		m.status.State = "disabled"
	} else if len(m.raw) == 0 {
		m.status.State = "waiting"
	}
	m.mu.Unlock()
}

func (m *Manager) SetCanary(state string) {
	m.mu.Lock()
	m.status.Canary = strings.TrimSpace(state)
	m.mu.Unlock()
}

// ClearActive stops applying a profile after the operator disables the updater or
// changes trust roots. Persisted slots remain available for a later explicit load.
func (m *Manager) ClearActive(state string) {
	m.mu.Lock()
	m.active = Payload{}
	m.raw = nil
	m.digest = ""
	m.status.State = strings.TrimSpace(state)
	if m.status.State == "" {
		m.status.State = "waiting"
	}
	m.status.Digest = ""
	m.status.Generation = 0
	m.status.ExpiresAt = 0
	m.status.ModelCount = 0
	m.mu.Unlock()
}

// Load chooses the newest valid source-matching A/B snapshot. Expired data may
// be used only within MaxStale; corruption in either slot is ignored independently.
func (m *Manager) Load(cfg Config) (Payload, bool, error) {
	cfg = normalizeConfig(cfg)
	if m.dataDir == "" {
		return Payload{}, false, errors.New("compatibility manifest data directory is empty")
	}
	var candidates []persistedSnapshot
	var failures []string
	for _, slot := range []string{"a", "b"} {
		snapshot, err := m.readSnapshot(slot, cfg)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, slot+":"+err.Error())
			}
			continue
		}
		candidates = append(candidates, snapshot)
	}
	if len(candidates) == 0 {
		if len(failures) > 0 {
			return Payload{}, false, fmt.Errorf("no valid A/B snapshot (%s)", strings.Join(failures, "; "))
		}
		return Payload{}, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Sequence > candidates[j].Sequence })
	selected := candidates[0]
	var payload Payload
	if err := json.Unmarshal(selected.Payload, &payload); err != nil {
		return Payload{}, false, err
	}
	slot := slotForSequence(selected.Sequence)
	m.mu.Lock()
	m.active = clonePayload(payload)
	m.raw = append([]byte(nil), selected.Payload...)
	m.digest = selected.Digest
	m.sequence = selected.Sequence
	m.status = Status{
		Enabled: cfg.Enabled, Source: cfg.Source, State: "last_known_good",
		Digest: selected.Digest, Generation: payload.Generation, FetchedAt: selected.FetchedAt,
		ExpiresAt: payload.ExpiresAt, LastSuccessAt: selected.FetchedAt, SnapshotSlot: slot,
		SignatureChecked: cfg.Source == "signed_custom", ModelCount: len(payload.Models),
	}
	m.mu.Unlock()
	return clonePayload(payload), true, nil
}

// Refresh coalesces concurrent refreshes for the same trust configuration. This is
// both a local singleflight and an upstream-load guard: a burst of settings writes
// or callers produces one registry/release lookup, and all followers receive the
// leader's result. A trust-source change waits for the current flight before starting
// its own refresh, so differently trusted candidates are never mixed.
func (m *Manager) Refresh(ctx context.Context, cfg Config, accept func(context.Context, Payload) error) (Payload, bool, error) {
	cfg = normalizeConfig(cfg)
	key := refreshConfigKey(cfg)
	for {
		m.flightMu.Lock()
		if m.flight == nil {
			flight := &refreshFlight{key: key, done: make(chan struct{})}
			m.flight = flight
			m.flightMu.Unlock()

			flight.payload, flight.changed, flight.err = m.refreshOnce(ctx, cfg, accept)
			m.flightMu.Lock()
			m.flight = nil
			close(flight.done)
			m.flightMu.Unlock()
			return clonePayload(flight.payload), flight.changed, flight.err
		}
		flight := m.flight
		m.flightMu.Unlock()
		select {
		case <-ctx.Done():
			return Payload{}, false, ctx.Err()
		case <-flight.done:
		}
		if flight.key == key {
			return clonePayload(flight.payload), flight.changed, flight.err
		}
		// A different trust configuration led the completed flight. Re-check the
		// slot and start this configuration's refresh only after it has finished.
	}
}

func refreshConfigKey(cfg Config) string {
	return strings.Join([]string{
		strconv.FormatBool(cfg.Enabled), cfg.Source, cfg.URL, cfg.PublicKey,
		strconv.FormatInt(int64(cfg.MaxStale), 10), strconv.FormatInt(int64(cfg.RefreshEvery), 10),
	}, "\x00")
}

// refreshOnce fetches and validates a candidate, optionally runs a caller-supplied
// non-billable canary, commits an fsync'd A/B snapshot, then atomically publishes
// it. Failed candidates never replace the active profile.
func (m *Manager) refreshOnce(ctx context.Context, cfg Config, accept func(context.Context, Payload) error) (Payload, bool, error) {
	now := m.now()
	m.mu.Lock()
	m.status.Enabled = cfg.Enabled
	m.status.Source = cfg.Source
	m.status.LastAttemptAt = now.Unix()
	if !cfg.Enabled {
		m.status.State = "disabled"
		m.mu.Unlock()
		return Payload{}, false, nil
	}
	m.status.State = "refreshing"
	m.mu.Unlock()

	fetched, err := m.fetch(ctx, cfg, now)
	if err != nil {
		m.recordFailure(err)
		return Payload{}, false, err
	}
	if accept != nil {
		if err = accept(ctx, clonePayload(fetched.payload)); err != nil {
			err = fmt.Errorf("compatibility canary rejected candidate: %w", err)
			m.recordFailure(err)
			return Payload{}, false, err
		}
	}
	digest := digestBytes(fetched.raw)
	m.mu.RLock()
	unchanged := subtle.ConstantTimeCompare([]byte(digest), []byte(m.digest)) == 1
	activeGeneration := m.active.Generation
	nextSequence := m.sequence + 1
	m.mu.RUnlock()
	if unchanged {
		m.recordSuccess(fetched.payload, digest, now.Unix(), "unchanged", "")
		return clonePayload(fetched.payload), false, nil
	}
	if activeGeneration > 0 && fetched.payload.Generation <= activeGeneration {
		err = fmt.Errorf("manifest rollback/equivocation rejected: candidate generation %d is not newer than %d", fetched.payload.Generation, activeGeneration)
		m.recordFailure(err)
		return Payload{}, false, err
	}
	slot := ""
	if m.dataDir != "" {
		snapshot := persistedSnapshot{Version: SnapshotVersion, Sequence: nextSequence,
			FetchedAt: now.Unix(), Source: cfg.Source, Digest: digest,
			Payload: append(json.RawMessage(nil), fetched.raw...), Signature: fetched.signature}
		slot = slotForSequence(nextSequence)
		if err = m.writeSnapshot(slot, snapshot); err != nil {
			err = fmt.Errorf("persist compatibility snapshot: %w", err)
			m.recordFailure(err)
			return Payload{}, false, err
		}
	}
	m.mu.Lock()
	m.active = clonePayload(fetched.payload)
	m.raw = append([]byte(nil), fetched.raw...)
	m.digest = digest
	m.sequence = nextSequence
	m.status = Status{
		Enabled: true, Source: cfg.Source, State: "current", Digest: digest,
		Generation: fetched.payload.Generation, FetchedAt: now.Unix(), ExpiresAt: fetched.payload.ExpiresAt,
		LastAttemptAt: now.Unix(), LastSuccessAt: now.Unix(), SnapshotSlot: slot,
		SignatureChecked: cfg.Source == "signed_custom", Canary: m.status.Canary,
		ModelCount: len(fetched.payload.Models),
	}
	m.mu.Unlock()
	return clonePayload(fetched.payload), true, nil
}

func (m *Manager) recordFailure(err error) {
	m.mu.Lock()
	m.status.LastError = boundedError(err)
	if len(m.raw) > 0 {
		m.status.State = "degraded_last_known_good"
	} else {
		m.status.State = "unavailable"
	}
	m.mu.Unlock()
}

func (m *Manager) recordSuccess(payload Payload, digest string, fetchedAt int64, state, slot string) {
	m.mu.Lock()
	m.status.State = state
	m.status.Digest = digest
	m.status.Generation = payload.Generation
	m.status.FetchedAt = fetchedAt
	m.status.ExpiresAt = payload.ExpiresAt
	m.status.LastAttemptAt = fetchedAt
	m.status.LastSuccessAt = fetchedAt
	m.status.LastError = ""
	m.status.ModelCount = len(payload.Models)
	if slot != "" {
		m.status.SnapshotSlot = slot
	}
	m.mu.Unlock()
}

func (m *Manager) fetch(ctx context.Context, cfg Config, now time.Time) (fetchedManifest, error) {
	var result fetchedManifest
	var err error
	switch cfg.Source {
	case "official":
		result, err = m.fetchOfficial(ctx, now)
	case "signed_custom":
		result, err = m.fetchSignedCustom(ctx, cfg)
	default:
		err = fmt.Errorf("unsupported compatibility source %q", cfg.Source)
	}
	if err != nil {
		return fetchedManifest{}, err
	}
	if err = Validate(result.payload, now, false); err != nil {
		return fetchedManifest{}, err
	}
	if result.payload.Source != cfg.Source {
		return fetchedManifest{}, fmt.Errorf("manifest payload source %q does not match configured source %q", result.payload.Source, cfg.Source)
	}
	return result, nil
}

func (m *Manager) fetchOfficial(ctx context.Context, now time.Time) (fetchedManifest, error) {
	codexVersion, codexPublishedAt, err := m.fetchOfficialCodexVersion(ctx)
	if err != nil {
		return fetchedManifest{}, err
	}
	var npm struct {
		Version string `json:"version"`
	}
	if err := m.getJSON(ctx, officialClaudeURL, "official", &npm); err != nil {
		return fetchedManifest{}, fmt.Errorf("fetch official Claude release: %w", err)
	}
	claudeVersion := extractDottedVersion(npm.Version)
	if claudeVersion == "" {
		return fetchedManifest{}, fmt.Errorf("official Claude release returned invalid version %q", npm.Version)
	}
	issuedAt := now.Unix()
	if published, err := time.Parse(time.RFC3339, codexPublishedAt); err == nil && !published.After(now.Add(24*time.Hour)) {
		issuedAt = published.Unix()
	}
	payload := Payload{
		SchemaVersion: SchemaVersion, Generation: now.Unix(), IssuedAt: issuedAt,
		ExpiresAt: now.Add(14 * 24 * time.Hour).Unix(), Source: "official",
		Codex: ClientProfile{Version: codexVersion},
		// The npm registry proves the CLI version, but it does not prove the exact
		// embedded Node + Stainless tuple. Leave those axes empty rather than synthesize
		// a fingerprint combination that no real Claude Code release shipped.
		Claude: ClientProfile{CLIVersion: claudeVersion},
	}
	raw, err := json.Marshal(payload)
	return fetchedManifest{payload: payload, raw: raw}, err
}

// fetchOfficialCodexVersion uses the package actually installed by the official
// CLI distribution as its primary version source. The GitHub release API is queried
// only once, and only when npm fails or returns a malformed version; this removes the
// recurring unauthenticated-API 403 while preserving availability during a registry
// outage without retry loops inside a refresh.
func (m *Manager) fetchOfficialCodexVersion(ctx context.Context) (version, publishedAt string, err error) {
	var npm struct {
		Version string `json:"version"`
	}
	npmErr := m.getJSON(ctx, officialCodexNPMURL, "official", &npm)
	if npmErr == nil {
		if version = extractDottedVersion(npm.Version); version != "" {
			return version, "", nil
		}
		npmErr = fmt.Errorf("source returned invalid version %q", npm.Version)
	}

	var release struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
	}
	githubErr := m.getJSON(ctx, officialCodexGitHubURL, "official", &release)
	if githubErr == nil {
		if version = extractDottedVersion(release.TagName); version != "" {
			return version, release.PublishedAt, nil
		}
		githubErr = fmt.Errorf("source returned invalid version %q", release.TagName)
	}
	return "", "", fmt.Errorf("fetch official Codex release: %w", errors.Join(
		fmt.Errorf("npm: %w", npmErr),
		fmt.Errorf("GitHub fallback: %w", githubErr),
	))
}

func (m *Manager) fetchSignedCustom(ctx context.Context, cfg Config) (fetchedManifest, error) {
	if err := validateSourceURL(cfg.URL, "signed_custom"); err != nil {
		return fetchedManifest{}, err
	}
	var envelope struct {
		Payload   json.RawMessage `json:"payload"`
		Signature string          `json:"signature"`
	}
	if err := m.getJSON(ctx, cfg.URL, "signed_custom", &envelope); err != nil {
		return fetchedManifest{}, err
	}
	if len(envelope.Payload) == 0 || strings.TrimSpace(envelope.Signature) == "" {
		return fetchedManifest{}, errors.New("signed manifest requires payload and signature")
	}
	if err := verifySignature(envelope.Payload, envelope.Signature, cfg.PublicKey); err != nil {
		return fetchedManifest{}, err
	}
	payload, err := decodePayloadStrict(envelope.Payload)
	if err != nil {
		return fetchedManifest{}, fmt.Errorf("decode signed payload: %w", err)
	}
	return fetchedManifest{payload: payload, raw: append([]byte(nil), envelope.Payload...), signature: envelope.Signature}, nil
}

func decodePayloadStrict(raw []byte) (Payload, error) {
	var payload Payload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Payload{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Payload{}, errors.New("manifest payload contains trailing JSON")
	}
	return payload, nil
}

func (m *Manager) getJSON(ctx context.Context, rawURL, source string, dst interface{}) error {
	if err := validateSourceURL(rawURL, source); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", officialUserAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil || validateSourceURL(resp.Request.URL.String(), source) != nil {
		return errors.New("compatibility source redirected outside its allowlist")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return &sourceHTTPError{status: resp.StatusCode, retryAfter: sourceRetryDelay(resp.Header, m.now())}
	}
	limited := io.LimitReader(resp.Body, maxManifestBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(raw) > maxManifestBytes {
		return fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if source == "signed_custom" {
		decoder.DisallowUnknownFields()
	}
	if err = decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("manifest contains trailing JSON")
	}
	return nil
}

func sourceRetryDelay(header http.Header, now time.Time) time.Duration {
	const maximumRetryDelay = 24 * time.Hour
	var retryAt time.Time
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			if seconds > int64(maximumRetryDelay/time.Second) {
				seconds = int64(maximumRetryDelay / time.Second)
			}
			retryAt = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(raw); err == nil {
			retryAt = parsed
		}
	}
	if raw := strings.TrimSpace(header.Get("X-RateLimit-Reset")); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
			reset := time.Unix(epoch, 0)
			if reset.After(retryAt) {
				retryAt = reset
			}
		}
	}
	if retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

// Validate applies the same bounded schema rules to network and persisted data.
// allowExpired is used only for an explicitly bounded last-known-good load.
func Validate(payload Payload, now time.Time, allowExpired bool) error {
	if payload.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema %d", payload.SchemaVersion)
	}
	if payload.Generation <= 0 || payload.IssuedAt <= 0 || payload.ExpiresAt <= payload.IssuedAt {
		return errors.New("manifest generation/time window is invalid")
	}
	issued := time.Unix(payload.IssuedAt, 0)
	expires := time.Unix(payload.ExpiresAt, 0)
	if issued.After(now.Add(24 * time.Hour)) {
		return errors.New("manifest issued_at is too far in the future")
	}
	if expires.Sub(issued) > maximumManifestLife {
		return errors.New("manifest lifetime exceeds 180 days")
	}
	if !allowExpired && !expires.After(now) {
		return errors.New("manifest is expired")
	}
	if payload.Source != "official" && payload.Source != "signed_custom" {
		return fmt.Errorf("invalid manifest source %q", payload.Source)
	}
	for name, version := range map[string]string{
		"codex.version":            payload.Codex.Version,
		"claude.cli_version":       payload.Claude.CLIVersion,
		"claude.stainless_version": payload.Claude.StainlessVersion,
	} {
		if version != "" && extractDottedVersion(version) != strings.TrimPrefix(strings.TrimSpace(version), "v") {
			return fmt.Errorf("%s is not a dotted version", name)
		}
	}
	if node := strings.TrimSpace(payload.Claude.NodeVersion); node != "" {
		if extractDottedVersion(node) != strings.TrimPrefix(node, "v") {
			return errors.New("claude.node_version is not a dotted version")
		}
	}
	// AttributionSuffix is an empirical assertion only a signed capture pipeline can
	// make; the official npm-derived payload never runs the client and must not
	// claim it. "live" forces the pool's attribution suffix on (the verified current
	// state); "plain" forces it off (a future server-side turn-down). Restrict the
	// value so a bad manifest cannot inject arbitrary text.
	if suffix := strings.TrimSpace(payload.Claude.AttributionSuffix); suffix != "" {
		if payload.Source != "signed_custom" || (suffix != "live" && suffix != "plain") {
			return errors.New("claude.attribution_suffix may only be \"live\" or \"plain\" from a signed_custom manifest")
		}
	}
	if payload.Source == "official" && payload.Codex.Version == "" && payload.Claude.CLIVersion == "" {
		return errors.New("official manifest contains no client version")
	}
	if len(payload.Models) > maxManifestModels {
		return fmt.Errorf("manifest has more than %d models", maxManifestModels)
	}
	seen := make(map[string]struct{}, len(payload.Models))
	allowedEffort := map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}
	for i, model := range payload.Models {
		slug := strings.ToLower(strings.TrimSpace(model.Slug))
		if slug == "" || len(slug) > 128 || strings.ContainsAny(slug, " \t\r\n/\\") {
			return fmt.Errorf("models[%d].slug is invalid", i)
		}
		if _, ok := seen[slug]; ok {
			return fmt.Errorf("duplicate model slug %q", slug)
		}
		seen[slug] = struct{}{}
		if model.ContextWindow <= 0 || model.ContextWindow > maxContextWindow || model.MaxContextWindow < model.ContextWindow || model.MaxContextWindow > maxContextWindow {
			return fmt.Errorf("model %q context window is invalid", slug)
		}
		if model.AutoCompactTokenLimit < 0 || model.AutoCompactTokenLimit > model.MaxContextWindow {
			return fmt.Errorf("model %q auto compact limit is invalid", slug)
		}
		if minimum := strings.TrimSpace(model.MinimumClientVersion); minimum != "" {
			if extractDottedVersion(minimum) != strings.TrimPrefix(minimum, "v") {
				return fmt.Errorf("model %q minimum client version is invalid", slug)
			}
			if payload.Codex.Version != "" && CompareDottedVersions(minimum, payload.Codex.Version) > 0 {
				return fmt.Errorf("model %q requires client %s newer than manifest client %s", slug, minimum, payload.Codex.Version)
			}
		}
		if len(model.ReasoningLevels) > 12 {
			return fmt.Errorf("model %q has too many reasoning levels", slug)
		}
		for _, effort := range model.ReasoningLevels {
			if !allowedEffort[strings.ToLower(strings.TrimSpace(effort))] {
				return fmt.Errorf("model %q has invalid reasoning level %q", slug, effort)
			}
		}
	}
	return nil
}

func (m *Manager) readSnapshot(slot string, cfg Config) (persistedSnapshot, error) {
	path := m.snapshotPath(slot)
	file, err := os.Open(path)
	if err != nil {
		return persistedSnapshot{}, err
	}
	defer file.Close()
	const maxSnapshotBytes = maxManifestBytes + (64 << 10)
	raw, err := io.ReadAll(io.LimitReader(file, maxSnapshotBytes))
	if err != nil {
		return persistedSnapshot{}, err
	}
	if len(raw) >= maxSnapshotBytes {
		return persistedSnapshot{}, errors.New("snapshot is oversized")
	}
	var snapshot persistedSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&snapshot); err != nil {
		return persistedSnapshot{}, err
	}
	if snapshot.Version != SnapshotVersion || snapshot.Sequence == 0 || snapshot.Source != cfg.Source {
		return persistedSnapshot{}, errors.New("snapshot version/source/sequence mismatch")
	}
	if subtle.ConstantTimeCompare([]byte(snapshot.Digest), []byte(digestBytes(snapshot.Payload))) != 1 {
		return persistedSnapshot{}, errors.New("snapshot digest mismatch")
	}
	if cfg.Source == "signed_custom" {
		if err = verifySignature(snapshot.Payload, snapshot.Signature, cfg.PublicKey); err != nil {
			return persistedSnapshot{}, err
		}
	}
	payload, err := decodePayloadStrict(snapshot.Payload)
	if err != nil {
		return persistedSnapshot{}, err
	}
	now := m.now()
	if err = Validate(payload, now, true); err != nil {
		return persistedSnapshot{}, err
	}
	if payload.Source != cfg.Source {
		return persistedSnapshot{}, errors.New("snapshot payload source mismatch")
	}
	maxStale := cfg.MaxStale
	if maxStale <= 0 {
		maxStale = 30 * 24 * time.Hour
	}
	if now.After(time.Unix(payload.ExpiresAt, 0).Add(maxStale)) {
		return persistedSnapshot{}, errors.New("snapshot exceeds max stale age")
	}
	return snapshot, nil
}

func (m *Manager) writeSnapshot(slot string, snapshot persistedSnapshot) error {
	if slot != "a" && slot != "b" {
		return errors.New("invalid snapshot slot")
	}
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(m.dataDir, ".compatibility-manifest-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tempPath, m.snapshotPath(slot)); err != nil {
		return err
	}
	keep = true
	dir, err := os.Open(m.dataDir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (m *Manager) snapshotPath(slot string) string {
	return filepath.Join(m.dataDir, "compatibility-manifest-"+slot+".json")
}

func normalizeConfig(cfg Config) Config {
	cfg.Source = normalizeSource(cfg.Source)
	if cfg.MaxStale <= 0 {
		cfg.MaxStale = 30 * 24 * time.Hour
	}
	if cfg.RefreshEvery <= 0 {
		cfg.RefreshEvery = 6 * time.Hour
	}
	return cfg
}

func normalizeSource(source string) string {
	if strings.EqualFold(strings.TrimSpace(source), "signed_custom") {
		return "signed_custom"
	}
	return "official"
}

func validateSourceURL(rawURL, source string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("compatibility source URL is invalid")
	}
	source = normalizeSource(source)
	if source == "official" {
		if u.String() != officialCodexNPMURL && u.String() != officialCodexGitHubURL && u.String() != officialClaudeURL {
			return errors.New("official compatibility URL is outside the allowlist")
		}
		return nil
	}
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return errors.New("signed custom compatibility URL must use HTTPS")
	}
	host := strings.TrimSpace(u.Hostname())
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("plain HTTP compatibility URL is allowed only on loopback")
	}
	return nil
}

func verifySignature(payload []byte, encodedSignature, encodedPublicKey string) error {
	decode := func(value string) ([]byte, error) {
		value = strings.TrimSpace(value)
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
			return raw, nil
		}
		return base64.RawStdEncoding.DecodeString(value)
	}
	publicKey, err := decode(encodedPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("compatibility manifest public key must be 32-byte base64 Ed25519")
	}
	signature, err := decode(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("compatibility manifest signature is invalid base64 Ed25519")
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return errors.New("compatibility manifest signature verification failed")
	}
	return nil
}

func extractDottedVersion(value string) string {
	match := dottedVersionRE.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// CompareDottedVersions compares numeric major.minor.patch versions. Invalid
// versions sort below valid versions; prerelease text is intentionally ignored
// because compatibility manifests accept only the dotted release core.
func CompareDottedVersions(left, right string) int {
	parse := func(value string) ([3]int64, bool) {
		var out [3]int64
		version := extractDottedVersion(value)
		if version == "" {
			return out, false
		}
		parts := strings.Split(version, ".")
		if len(parts) != 3 {
			return out, false
		}
		for i := range parts {
			n, err := strconv.ParseInt(parts[i], 10, 64)
			if err != nil {
				return [3]int64{}, false
			}
			out[i] = n
		}
		return out, true
	}
	a, aOK := parse(left)
	b, bOK := parse(right)
	if !aOK && !bOK {
		return 0
	}
	if !aOK {
		return -1
	}
	if !bOK {
		return 1
	}
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func slotForSequence(sequence uint64) string {
	if sequence%2 == 0 {
		return "b"
	}
	return "a"
}

func clonePayload(payload Payload) Payload {
	copyPayload := payload
	copyPayload.Models = append([]Model(nil), payload.Models...)
	for i := range copyPayload.Models {
		copyPayload.Models[i].ReasoningLevels = append([]string(nil), payload.Models[i].ReasoningLevels...)
	}
	return copyPayload
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
