package api

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

func TestDiagnosticsWSRegressionDataset(t *testing.T) {
	raw, err := os.ReadFile("testdata/diagnostics_ws_20260727.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Incidents []struct {
			OuterStatus int             `json:"outer_status"`
			InnerStatus int             `json:"inner_status"`
			Frame       json.RawMessage `json:"frame"`
		} `json:"incidents"`
		MappingStorm struct {
			CommitFailed   int `json:"commit_failed"`
			VisibleErrors  int `json:"visible_errors"`
			UpstreamNon200 int `json:"upstream_non_200"`
		} `json:"mapping_storm"`
		SnapshotDrift struct {
			AuditManifestRows   int `json:"audit_manifest_rows"`
			AuditCSVRows        int `json:"audit_csv_rows"`
			BillingManifestRows int `json:"billing_manifest_rows"`
			BillingCSVRows      int `json:"billing_csv_rows"`
			UsageManifestRows   int `json:"usage_manifest_rows"`
			UsageCSVRows        int `json:"usage_csv_rows"`
		} `json:"snapshot_drift"`
		BillingIntegrity struct {
			Holds                     int `json:"holds"`
			UsageRecords              int `json:"usage_records"`
			SettledStreaming          int `json:"settled_streaming"`
			SettledStreamingWithUsage int `json:"settled_streaming_with_usage"`
			UsageMissing              int `json:"usage_missing"`
			FreshHeldCSV              int `json:"fresh_held_csv"`
			FreshHeldSummary          int `json:"fresh_held_summary"`
			UsageRouteEpochZero       int `json:"usage_route_epoch_zero"`
		} `json:"billing_integrity"`
		UnboundedCardinality struct {
			AffinityBindings           int `json:"affinity_bindings"`
			PreviousResponseAffinities int `json:"previous_response_affinities"`
			UpstreamAttempts           int `json:"upstream_attempts"`
			UpstreamAttemptTrees       int `json:"upstream_attempt_trees"`
			MaxAttemptsPerTree         int `json:"max_attempts_per_tree"`
		} `json:"unbounded_cardinality"`
		GoalWriteAmplification struct {
			CheckpointCommitted int `json:"checkpoint_committed"`
			AliasBound          int `json:"alias_bound"`
			CompactionStarted   int `json:"compaction_started"`
			CompactionCompleted int `json:"compaction_completed"`
		} `json:"goal_write_amplification"`
		CacheTelemetryLag struct {
			PendingUsageRequests int  `json:"pending_usage_requests"`
			UsageLagSeconds      int  `json:"usage_lag_seconds"`
			PartialData          bool `json:"partial_data"`
		} `json:"cache_telemetry_lag"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Incidents) != 4 {
		t.Fatalf("WS quota incidents=%d", len(fixture.Incidents))
	}
	for index, incident := range fixture.Incidents {
		if incident.OuterStatus != http.StatusOK || incident.InnerStatus != http.StatusTooManyRequests {
			t.Fatalf("incident %d statuses=%d/%d", index, incident.OuterStatus, incident.InnerStatus)
		}
		if codexMappedSessionRiskError(incident.OuterStatus, nil) {
			t.Fatalf("incident %d was misclassified from synthetic HTTP 200 alone", index)
		}
		if !codexMappedSessionRiskError(incident.InnerStatus, incident.Frame) || !codexMappedSessionRotationRequired(incident.InnerStatus, nil, incident.Frame, false, true) {
			t.Fatalf("incident %d WS frame no longer activates mapped failover: %s", index, incident.Frame)
		}
	}
	if fixture.MappingStorm.CommitFailed != 61 || fixture.MappingStorm.VisibleErrors != 60 || fixture.MappingStorm.UpstreamNon200 != 0 {
		t.Fatalf("mapping storm fixture changed: %+v", fixture.MappingStorm)
	}
	drift := fixture.SnapshotDrift
	if drift.AuditCSVRows <= drift.AuditManifestRows || drift.BillingCSVRows <= drift.BillingManifestRows || drift.UsageCSVRows <= drift.UsageManifestRows {
		t.Fatalf("fixture no longer demonstrates a non-atomic diagnostic snapshot: %+v", drift)
	}
	billing := fixture.BillingIntegrity
	if billing.Holds != 4214 || billing.UsageRecords != 354 || billing.SettledStreaming != 3942 || billing.SettledStreamingWithUsage != 349 || billing.UsageMissing != 115 || billing.FreshHeldCSV <= billing.FreshHeldSummary || billing.UsageRouteEpochZero != billing.UsageRecords {
		t.Fatalf("billing integrity fixture changed: %+v", billing)
	}
	cardinality := fixture.UnboundedCardinality
	if cardinality.AffinityBindings != 72255 || cardinality.PreviousResponseAffinities != 70483 || cardinality.UpstreamAttempts != 77869 || cardinality.UpstreamAttemptTrees != 653 || cardinality.MaxAttemptsPerTree != 6131 {
		t.Fatalf("cardinality fixture changed: %+v", cardinality)
	}
	goal := fixture.GoalWriteAmplification
	if goal.CheckpointCommitted != 3884 || goal.AliasBound != 3782 || goal.CompactionStarted != 235 || goal.CompactionCompleted != 235 {
		t.Fatalf("goal write amplification fixture changed: %+v", goal)
	}
	lag := fixture.CacheTelemetryLag
	if lag.PendingUsageRequests != 60 || lag.UsageLagSeconds != 5182 || !lag.PartialData {
		t.Fatalf("cache telemetry lag fixture changed: %+v", lag)
	}
}
