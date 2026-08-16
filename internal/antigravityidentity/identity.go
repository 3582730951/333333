// Package antigravityidentity owns the process-wide Antigravity Hub runtime
// identity. Version discovery runs out of band so inference never waits for it.
package antigravityidentity

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

const (
	// FallbackVersion is pinned to the updater manifest value verified on
	// 2026-08-09. The background manifest refresh keeps it current afterward.
	FallbackVersion = "2.6.0"
	HubPlatform     = "darwin/arm64"

	versionCacheTTL = 6 * time.Hour
	fetchTimeout    = 10 * time.Second
	legacyManagedUA = "antigravity/hub/2.2.1 darwin/arm64"
)

var (
	hubLatestManifestURL = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"

	versionMu     sync.RWMutex
	cachedVersion = FallbackVersion
	versionExpiry time.Time
	updater       supervisor.Restartable
)

// StartVersionUpdater refreshes the Hub version in a background worker. The
// caller supplies the active-runtime context so standby workers and shutdown
// processes do not keep an updater alive.
func StartVersionUpdater(ctx context.Context) {
	updater.Start(ctx, supervisor.Options{Name: "antigravity-version-updater"}, runVersionUpdater)
}

func runVersionUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	refreshVersion(ctx)
	ticker := time.NewTicker(versionCacheTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshVersion(ctx)
		}
	}
}

func refreshVersion(ctx context.Context) {
	client := &http.Client{Timeout: fetchTimeout}
	version, err := fetchLatestVersion(ctx, client)

	versionMu.Lock()
	defer versionMu.Unlock()
	now := time.Now()
	if err == nil {
		cachedVersion = version
		versionExpiry = now.Add(versionCacheTTL)
		return
	}
	if cachedVersion == "" || now.After(versionExpiry) {
		cachedVersion = FallbackVersion
		versionExpiry = now.Add(versionCacheTTL)
		log.Printf("[ANTIGRAVITY] version manifest refresh failed; using fallback %s: %v", FallbackVersion, err)
	}
}

// LatestVersion returns a non-blocking cached version.
func LatestVersion() string {
	versionMu.RLock()
	defer versionMu.RUnlock()
	if cachedVersion != "" && time.Now().Before(versionExpiry) {
		return cachedVersion
	}
	return FallbackVersion
}

// UserAgent returns the short runtime UA used by model-list and inference calls.
func UserAgent() string {
	return fmt.Sprintf("antigravity/hub/%s %s", LatestVersion(), HubPlatform)
}

// RequestUserAgent resolves an account override. Empty values and the old
// process-managed fallback are upgraded from the live process cache; explicit
// custom values remain byte-for-byte stable.
func RequestUserAgent(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" || strings.EqualFold(configured, legacyManagedUA) ||
		strings.EqualFold(configured, fmt.Sprintf("antigravity/hub/%s %s", FallbackVersion, HubPlatform)) {
		return UserAgent()
	}
	return configured
}

func fetchLatestVersion(ctx context.Context, client *http.Client) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return "", errors.New("antigravity version HTTP client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubLatestManifestURL, nil)
	if err != nil {
		return "", fmt.Errorf("build Antigravity Hub manifest request: %w", err)
	}
	req.Header.Set("User-Agent", "electron-builder")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch Antigravity Hub manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Antigravity Hub manifest returned status %d", resp.StatusCode)
	}
	version, err := manifestVersion(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if !validSemVersion(version) {
		return "", fmt.Errorf("Antigravity Hub manifest returned invalid version %q", version)
	}
	return version, nil
}

func manifestVersion(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "version:") {
			version := strings.TrimSpace(strings.TrimPrefix(line, "version:"))
			version = strings.Trim(version, `"'`)
			if version == "" {
				return "", errors.New("Antigravity Hub manifest returned an empty version")
			}
			return version, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Antigravity Hub manifest: %w", err)
	}
	return "", errors.New("Antigravity Hub manifest is missing version")
}

func validSemVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}
