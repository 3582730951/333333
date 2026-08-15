package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/upstream"
)

func TestDiagnosticsAlwaysIncludesPassiveHealthAndCompatibilityStatus(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.passiveHealth.Observe(upstream.AttemptObservation{
		Provider: "kiro", Model: "claude-sonnet-5", EgressID: "direct", StatusCode: 503,
		ErrorClass: "upstream_5xx", Latency: 25 * time.Millisecond, ObservedAt: time.Now(),
	})
	raw := awaitLegacyDiagnosticExport(t, h)
	files := readZipFiles(t, raw)
	for _, name := range []string{"manifest.json", "diagnostic_summary.json", "passive_provider_health.json", "usage_records.csv", "goal_continuity.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("diagnostic bundle missing mandatory %q", name)
		}
	}
	var health struct {
		SeriesCount int `json:"series_count"`
		Series      []struct {
			Provider string `json:"provider"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(files["passive_provider_health.json"]), &health); err != nil {
		t.Fatal(err)
	}
	if health.SeriesCount != 1 || len(health.Series) != 1 || health.Series[0].Provider != "kiro" {
		t.Fatalf("passive health export=%s", files["passive_provider_health.json"])
	}
	if strings.Contains(files["passive_provider_health.json"], "prompt") || strings.Contains(files["passive_provider_health.json"], "credential") {
		t.Fatal("passive health export contains forbidden request data")
	}
	if !strings.Contains(files["diagnostic_summary.json"], "compatibility_manifest") {
		t.Fatal("diagnostic summary omitted compatibility manifest status")
	}
}
