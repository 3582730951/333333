package passivehealth

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/upstream"
)

func TestMonitorSeparatesHealthFailuresFromClientCancellation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	monitor := New(8, time.Hour)
	monitor.now = func() time.Time { return now }
	monitor.Observe(upstream.AttemptObservation{Provider: "codex", Model: "gpt-5.6-sol", EgressID: "eg-1", StatusCode: 200, Success: true, Latency: 100 * time.Millisecond, ObservedAt: now})
	monitor.Observe(upstream.AttemptObservation{Provider: "codex", Model: "gpt-5.6-sol", EgressID: "eg-1", StatusCode: 503, ErrorClass: "upstream_5xx", Latency: 200 * time.Millisecond, ObservedAt: now})
	monitor.Observe(upstream.AttemptObservation{Provider: "codex", Model: "gpt-5.6-sol", EgressID: "eg-1", ErrorClass: "client_canceled", ObservedAt: now})

	snapshot := monitor.Snapshot()
	if snapshot.SeriesCount != 1 {
		t.Fatalf("series count=%d", snapshot.SeriesCount)
	}
	row := snapshot.Series[0]
	if row.Observations != 3 || row.HealthSamples != 2 || row.Successes != 1 || row.Failures != 1 || row.Canceled != 1 {
		t.Fatalf("unexpected passive health counters: %+v", row)
	}
	if row.LatencyEWMAms <= 100 || row.LatencyEWMAms >= 200 {
		t.Fatalf("latency EWMA=%f", row.LatencyEWMAms)
	}
}

func TestMonitorIsConcurrentCardinalityAndRetentionBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	monitor := New(16, time.Minute)
	monitor.now = func() time.Time { return now }
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for model := 0; model < 20; model++ {
				monitor.Observe(upstream.AttemptObservation{
					Provider: "provider", Model: fmt.Sprintf("model-%d-%d", worker, model), EgressID: "egress",
					StatusCode: 200, Success: true, Latency: time.Millisecond, ObservedAt: now,
				})
			}
		}()
	}
	wg.Wait()
	snapshot := monitor.Snapshot()
	if snapshot.SeriesCount != 16 || snapshot.Evictions == 0 {
		t.Fatalf("cardinality bound not enforced: %+v", snapshot)
	}
	now = now.Add(2 * time.Minute)
	snapshot = monitor.Snapshot()
	if snapshot.SeriesCount != 0 {
		t.Fatalf("retention did not evict stale series: %+v", snapshot)
	}
}

func TestMonitorCapacityChurnPreservesEstablishedSeries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	monitor := New(2, time.Hour)
	stable := upstream.AttemptObservation{
		Provider: "codex", Model: "stable", EgressID: "egress",
		StatusCode: 200, Success: true, ObservedAt: now,
	}
	monitor.Observe(stable)
	monitor.Observe(upstream.AttemptObservation{Provider: "claude", Model: "stable", EgressID: "egress", StatusCode: 200, Success: true, ObservedAt: now})
	for index := 0; index < 1000; index++ {
		monitor.Observe(upstream.AttemptObservation{
			Provider: "attacker", Model: fmt.Sprintf("random-%d", index), EgressID: "egress",
			StatusCode: 500, ObservedAt: now,
		})
	}
	monitor.Observe(stable)
	snapshot := monitor.Snapshot()
	if snapshot.SeriesCount != 2 || snapshot.Evictions != 1000 {
		t.Fatalf("capacity snapshot=%+v", snapshot)
	}
	for _, row := range snapshot.Series {
		if row.Provider == "codex" && row.Model == "stable" {
			if row.Observations != 2 {
				t.Fatalf("established series observations=%d", row.Observations)
			}
			return
		}
	}
	t.Fatal("established series was displaced by label churn")
}
