package compatmanifest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type signedManifestFixture struct {
	mu      sync.Mutex
	payload Payload
	private ed25519.PrivateKey
	badSig  bool
}

func (f *signedManifestFixture) serve(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(f.payload)
	signature := ed25519.Sign(f.private, raw)
	if f.badSig {
		signature[0] ^= 0xff
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"payload": json.RawMessage(raw), "signature": base64.StdEncoding.EncodeToString(signature),
	})
}

func testPayload(now time.Time, generation int64, version string) Payload {
	return Payload{
		SchemaVersion: SchemaVersion, Generation: generation, IssuedAt: now.Add(-time.Minute).Unix(),
		ExpiresAt: now.Add(24 * time.Hour).Unix(), Source: "signed_custom",
		Codex:  ClientProfile{Version: version},
		Models: []Model{{Slug: "gpt-test", ContextWindow: 128000, MaxContextWindow: 256000, ReasoningLevels: []string{"low", "high"}}},
	}
}

func TestSignedManifestABLastKnownGoodAndRollbackProtection(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	fixture := &signedManifestFixture{payload: testPayload(now, 10, "1.2.3"), private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	cfg := Config{Enabled: true, Source: "signed_custom", URL: server.URL,
		PublicKey: base64.StdEncoding.EncodeToString(public), MaxStale: 30 * 24 * time.Hour}
	dir := t.TempDir()
	manager := New(dir, server.Client())
	manager.now = func() time.Time { return now }
	first, changed, err := manager.Refresh(context.Background(), cfg, nil)
	if err != nil || !changed || first.Codex.Version != "1.2.3" {
		t.Fatalf("first refresh payload=%+v changed=%v err=%v", first, changed, err)
	}

	fixture.mu.Lock()
	fixture.payload = testPayload(now, 11, "1.2.4")
	fixture.mu.Unlock()
	second, changed, err := manager.Refresh(context.Background(), cfg, nil)
	if err != nil || !changed || second.Codex.Version != "1.2.4" {
		t.Fatalf("second refresh payload=%+v changed=%v err=%v", second, changed, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compatibility-manifest-b.json"), []byte("half-written"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := New(dir, server.Client())
	reloaded.now = func() time.Time { return now }
	lkg, loaded, err := reloaded.Load(cfg)
	if err != nil || !loaded || lkg.Generation != 10 {
		t.Fatalf("A/B fallback payload=%+v loaded=%v err=%v", lkg, loaded, err)
	}

	fixture.mu.Lock()
	fixture.payload = testPayload(now, 9, "1.2.2")
	fixture.mu.Unlock()
	if _, _, err = manager.Refresh(context.Background(), cfg, nil); err == nil {
		t.Fatal("rollback generation was accepted")
	}
	active, ok := manager.Active()
	if !ok || active.Generation != 11 {
		t.Fatalf("rollback replaced active LKG: %+v ok=%v", active, ok)
	}
}

func TestSignedManifestRejectsInvalidSignatureAndComparesVersions(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	fixture := &signedManifestFixture{payload: testPayload(now, 1, "1.2.3"), private: private, badSig: true}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	manager := New(t.TempDir(), server.Client())
	_, _, err := manager.Refresh(context.Background(), Config{Enabled: true, Source: "signed_custom", URL: server.URL,
		PublicKey: base64.StdEncoding.EncodeToString(public)}, nil)
	if err == nil {
		t.Fatal("invalid Ed25519 signature was accepted")
	}
	if CompareDottedVersions("0.147.0", "0.146.9") <= 0 || CompareDottedVersions("v1.2.3", "1.2.3") != 0 || CompareDottedVersions("bad", "1.0.0") >= 0 {
		t.Fatal("dotted version comparison is incorrect")
	}
}

func TestSignedManifestRequiresStrictPayloadAndMatchingTrustSource(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if _, err := decodePayloadStrict([]byte(`{"schema_version":1,"generation":1,"issued_at":1799999900,"expires_at":1800003600,"source":"signed_custom","codex":{},"claude":{},"unexpected":true}`)); err == nil {
		t.Fatal("unknown signed payload field was accepted")
	}
	payload := testPayload(now, 1, "1.2.3")
	payload.Models[0].MinimumClientVersion = "future 9.9.9"
	if err := Validate(payload, now, false); err == nil {
		t.Fatal("non-canonical minimum client version was accepted")
	}
	payload = testPayload(now, 1, "1.2.3")
	payload.Models[0].MinimumClientVersion = "1.2.4"
	if err := Validate(payload, now, false); err == nil {
		t.Fatal("model requiring a client newer than the manifest was accepted")
	}

	public, private, _ := ed25519.GenerateKey(rand.Reader)
	payload = testPayload(now, 1, "1.2.3")
	payload.Source = "official"
	fixture := &signedManifestFixture{payload: payload, private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	manager := New(t.TempDir(), server.Client())
	manager.now = func() time.Time { return now }
	if _, _, err := manager.Refresh(context.Background(), Config{Enabled: true, Source: "signed_custom", URL: server.URL,
		PublicKey: base64.StdEncoding.EncodeToString(public)}, nil); err == nil {
		t.Fatal("payload source mismatch was accepted")
	}
}
