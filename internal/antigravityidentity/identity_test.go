package antigravityidentity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchLatestVersionUsesHubManifestProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "electron-builder" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("manifest headers = %#v", r.Header)
		}
		_, _ = w.Write([]byte("version: '2.3.4'\nfiles: []\n"))
	}))
	defer server.Close()

	original := hubLatestManifestURL
	hubLatestManifestURL = server.URL
	defer func() { hubLatestManifestURL = original }()
	got, err := fetchLatestVersion(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got != "2.3.4" {
		t.Fatalf("version = %q, want 2.3.4", got)
	}
}

func TestRequestUserAgentUpgradesOnlyManagedFallback(t *testing.T) {
	versionMu.Lock()
	previousVersion, previousExpiry := cachedVersion, versionExpiry
	cachedVersion = "2.3.4"
	versionExpiry = time.Now().Add(time.Hour)
	versionMu.Unlock()
	defer func() {
		versionMu.Lock()
		cachedVersion, versionExpiry = previousVersion, previousExpiry
		versionMu.Unlock()
	}()

	want := "antigravity/hub/2.3.4 darwin/arm64"
	for _, configured := range []string{"", "antigravity/hub/2.2.1 darwin/arm64"} {
		if got := RequestUserAgent(configured); got != want {
			t.Fatalf("RequestUserAgent(%q) = %q, want %q", configured, got, want)
		}
	}
	const custom = "antigravity/hub/2.0.0 linux/amd64"
	if got := RequestUserAgent(custom); got != custom {
		t.Fatalf("custom UA changed to %q", got)
	}
}

func TestManifestVersionRejectsInvalidSemver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("version: 2.3.4-beta\n"))
	}))
	defer server.Close()
	original := hubLatestManifestURL
	hubLatestManifestURL = server.URL
	defer func() { hubLatestManifestURL = original }()
	if _, err := fetchLatestVersion(context.Background(), server.Client()); err == nil {
		t.Fatal("invalid semantic version was accepted")
	}
}
