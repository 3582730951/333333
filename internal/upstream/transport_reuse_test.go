package upstream

import (
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// TestTransportReusedAcrossRequests guards the performance fix: the Transport
// (which owns Go's keep-alive connection pool) must be cached and reused across
// requests for the same egress, not rebuilt per request. A fresh Transport per
// request gives every call an empty pool and a full DNS+TLS handshake — the
// dominant per-request latency the relay used to pay on the direct/proxy path.
func TestTransportReusedAcrossRequests(t *testing.T) {
	c := NewClient(config.Default())

	direct := storage.EgressProfile{Type: "direct"}
	t1, err := c.transportForEgress(direct)
	if err != nil {
		t.Fatalf("transportForEgress direct: %v", err)
	}
	t2, err := c.transportForEgress(direct)
	if err != nil {
		t.Fatalf("transportForEgress direct (2): %v", err)
	}
	if t1 != t2 {
		t.Fatal("expected the same Transport instance to be reused for the same egress")
	}

	// "" (unset) is normalized to the same "direct" bucket.
	if t3, _ := c.transportForEgress(storage.EgressProfile{}); t3 != t1 {
		t.Fatal("empty egress type must share the direct Transport")
	}

	// A distinct proxy endpoint gets its own Transport (its own pool/dialer).
	proxyA, err := c.transportForEgress(storage.EgressProfile{Type: "http_proxy", Endpoint: "http://127.0.0.1:1080"})
	if err != nil {
		t.Fatalf("transportForEgress proxyA: %v", err)
	}
	if proxyA == t1 {
		t.Fatal("a proxy egress must not share the direct Transport")
	}
	proxyA2, _ := c.transportForEgress(storage.EgressProfile{Type: "http_proxy", Endpoint: "http://127.0.0.1:1080"})
	if proxyA2 != proxyA {
		t.Fatal("same proxy endpoint must reuse its Transport")
	}
}
