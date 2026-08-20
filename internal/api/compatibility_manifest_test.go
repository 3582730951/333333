package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/compatmanifest"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCompatibilityManifestCannotDowngradeBundledOrPinnedClient(t *testing.T) {
	cfg := config.Default()
	downgrade := compatmanifest.Payload{Codex: compatmanifest.ClientProfile{Version: "0.146.0"}}
	got := applyCompatibilityManifestConfig(cfg, downgrade)
	if got.ClientVersion != config.DefaultClientVersion || got.CodexCLIVersionOverride != "" {
		t.Fatalf("downgrade changed client config: %+v", got)
	}

	cfg.CodexCLIVersionOverride = "9.9.9"
	got = applyCompatibilityManifestConfig(cfg, compatmanifest.Payload{Codex: compatmanifest.ClientProfile{Version: "10.0.0"}})
	if got.CodexCLIVersionOverride != "9.9.9" {
		t.Fatalf("manifest replaced operator pin: %+v", got)
	}

	// An official release can move ahead of the wire fingerprints compiled into
	// this build. Discovery may report it, but automatic request shaping must stay
	// on an exact verified profile until that version joins the fingerprint library.
	cfg = config.Default()
	got = applyCompatibilityManifestConfig(cfg, compatmanifest.Payload{Codex: compatmanifest.ClientProfile{Version: "0.149.0"}})
	if got.ClientVersion != config.DefaultClientVersion || got.CodexCLIVersionOverride != "" {
		t.Fatalf("unknown future release bypassed the verified fingerprint library: %+v", got)
	}

	server := &Server{compatibilityManifest: compatmanifest.New(t.TempDir(), nil)}
	if err := server.canaryCompatibilityManifest(context.Background(), downgrade); err == nil {
		t.Fatal("canary accepted a version older than the bundled client")
	}
	status, ok := server.compatibilityManifestStatus().(compatmanifest.Status)
	if !ok || status.Canary != "rejected_version_downgrade" {
		t.Fatalf("downgrade status=%+v", server.compatibilityManifestStatus())
	}
}

func TestCompatibilityManifestPersistedDisablePreventsStartupLKGVisibility(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload := compatmanifest.Payload{
		SchemaVersion: compatmanifest.SchemaVersion,
		Generation:    now.Unix(),
		IssuedAt:      now.Add(-time.Minute).Unix(),
		ExpiresAt:     now.Add(time.Hour).Unix(),
		Source:        "signed_custom",
		Models: []compatmanifest.Model{{
			Slug: "startup-lkg-only", ContextWindow: 128000, MaxContextWindow: 128000,
		}},
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]interface{}{
		"payload": json.RawMessage(payloadRaw),
		"signature": base64.StdEncoding.EncodeToString(
			ed25519.Sign(privateKey, payloadRaw),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(envelope)
	}))
	defer source.Close()

	dataDir := t.TempDir()
	manifestCfg := compatmanifest.Config{
		Enabled: true, Source: "signed_custom", URL: source.URL,
		PublicKey: base64.StdEncoding.EncodeToString(publicKey), MaxStale: 24 * time.Hour,
	}
	manager := compatmanifest.New(dataDir, source.Client())
	if _, changed, refreshErr := manager.Refresh(context.Background(), manifestCfg, nil); refreshErr != nil || !changed {
		t.Fatalf("seed LKG changed=%v err=%v", changed, refreshErr)
	}

	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.SetSetting(context.Background(), "compatibility_manifest_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	serverCfg := config.Default()
	serverCfg.DataDir = dataDir
	serverCfg.CompatibilityManifestSource = "signed_custom"
	serverCfg.CompatibilityManifestPublicKey = manifestCfg.PublicKey
	server := NewServer(Dependencies{Config: serverCfg, Store: store, DeferRuntimeStart: true})
	if _, active := server.compatibilityManifest.Active(); active {
		t.Fatal("persisted disable exposed an LKG manifest before runtime startup")
	}
	status, ok := server.compatibilityManifestStatus().(compatmanifest.Status)
	if !ok || status.Enabled || status.State != "disabled" {
		t.Fatalf("disabled startup status=%+v", server.compatibilityManifestStatus())
	}
	for _, model := range capability.StaticCodexModels("account") {
		if model.ModelSlug == "startup-lkg-only" {
			t.Fatal("disabled startup published an LKG fallback model")
		}
	}
}
