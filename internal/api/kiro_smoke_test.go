package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestKiroRealCredentialSmoke is opt-in and never consumes repository secrets. Set
// KIRO_SMOKE_JSON to a Social/IdC/API-key credential JSON to exercise refresh,
// getUsageLimits, encrypted persistence and capability seeding against the real service.
func TestKiroRealCredentialSmoke(t *testing.T) {
	raw := os.Getenv("KIRO_SMOKE_JSON")
	if raw == "" {
		t.Skip("set KIRO_SMOKE_JSON to run the real Kiro smoke test")
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	payload, _ := json.Marshal(map[string]interface{}{"kiro_json_text": raw, "label": "kiro-smoke", "group_name": "cyber", "egress_id": "egress_direct"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.pool.URL+"/admin/accounts/import-kiro-json", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var result struct {
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Imported < 1 {
		t.Fatalf("Kiro smoke import failed: %s", body)
	}
}
