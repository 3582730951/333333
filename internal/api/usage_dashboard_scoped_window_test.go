package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestAdminUsageDashboardScopedWindowsPreserveUnscopedCompatibility(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.SetSetting(context.Background(), "usage_accuracy_cutover_at", "0"); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix() - 600
	insertUsageAt(t, h, "account-old", "model-shared", "hash-old", "route-old", 10, 0, 10, 0, 0, 0, base+10)
	insertUsageAt(t, h, "account-recent", "model-shared", "hash-recent", "route-recent", 20, 0, 20, 0, 0, 0, base+130)

	globalSince := base + 60
	timeseriesSince := base + 120
	modelsSince := base
	until := base + 180
	path := fmt.Sprintf(
		"/admin/usage/dashboard?since=%d&until=%d&timeseries_since=%d&models_since=%d&bucket=60&fields=summary&allow_partial=true",
		globalSince, until, timeseriesSince, modelsSince,
	)
	code, raw := grpReq(t, h, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("scoped dashboard = %d: %s", code, raw)
	}
	var got struct {
		Accounts                   []storage.UsageSummaryRow `json:"accounts"`
		Timeseries                 []storage.UsageBucket     `json:"timeseries"`
		Models                     []storage.UserUsageRow    `json:"models"`
		EffectiveStartAt           int64                     `json:"effective_start_at"`
		TimeseriesEffectiveStartAt int64                     `json:"timeseries_effective_start_at"`
		TimeseriesEffectiveUntilAt int64                     `json:"timeseries_effective_until_at"`
		ModelsEffectiveStartAt     int64                     `json:"models_effective_start_at"`
		ModelsEffectiveUntilAt     int64                     `json:"models_effective_until_at"`
		UnavailableSections        []string                  `json:"unavailable_sections"`
		Cache                      struct {
			EffectiveStartAt int64 `json:"effective_start_at"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode scoped dashboard: %v (%s)", err, raw)
	}
	if got.EffectiveStartAt != globalSince || got.Cache.EffectiveStartAt != globalSince {
		t.Fatalf("unscoped account/cache windows changed: %+v", got)
	}
	if got.TimeseriesEffectiveStartAt != timeseriesSince || got.TimeseriesEffectiveUntilAt != until {
		t.Fatalf("timeseries window = [%d,%d), want [%d,%d)", got.TimeseriesEffectiveStartAt, got.TimeseriesEffectiveUntilAt, timeseriesSince, until)
	}
	if got.ModelsEffectiveStartAt != modelsSince || got.ModelsEffectiveUntilAt != until {
		t.Fatalf("models window = [%d,%d), want [%d,%d)", got.ModelsEffectiveStartAt, got.ModelsEffectiveUntilAt, modelsSince, until)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].AccountID != "account-recent" {
		t.Fatalf("accounts must retain the unscoped window: %#v", got.Accounts)
	}
	var timeseriesTokens int64
	for _, row := range got.Timeseries {
		timeseriesTokens += row.TotalTokens
	}
	if timeseriesTokens != 20 {
		t.Fatalf("timeseries tokens = %d, want recent-window 20: %#v", timeseriesTokens, got.Timeseries)
	}
	if len(got.Models) != 1 || got.Models[0].TotalTokens != 30 {
		t.Fatalf("models must use the independent wider window: %#v", got.Models)
	}
	if len(got.UnavailableSections) != 0 {
		t.Fatalf("successful partial-capable response marked unavailable sections: %#v", got.UnavailableSections)
	}

	for _, invalidPath := range []string{
		"/admin/usage/dashboard?timeseries_since=-1",
		"/admin/usage/dashboard?models_since=bad",
		"/admin/usage/dashboard?allow_partial=maybe",
	} {
		if code, raw := grpReq(t, h, http.MethodGet, invalidPath, ""); code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", invalidPath, code, raw)
		}
	}
}

func TestAdminUsageDashboardPartialModeRetainsIndependentSections(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "usage_accuracy_cutover_at", "0"); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix() - 300
	insertUsageAt(t, h, "account-partial", "model-partial", "hash-partial", "route-partial", 20, 0, 20, 0, 0, 0, base+10)
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE usage_records SET cache_breakpoint_count = 'not-an-integer'`); err != nil {
		t.Fatalf("seed cache-only scan failure: %v", err)
	}
	query := fmt.Sprintf("since=%d&until=%d&fields=summary,by_account", base, base+60)

	if code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/dashboard?"+query, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("default aggregate failure = %d, want 503: %s", code, raw)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/usage/dashboard?"+query+"&allow_partial=true", "")
	if code != http.StatusOK {
		t.Fatalf("partial aggregate = %d: %s", code, raw)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode partial aggregate: %v (%s)", err, raw)
	}
	var unavailable []string
	if err := json.Unmarshal(payload["unavailable_sections"], &unavailable); err != nil {
		t.Fatalf("decode unavailable sections: %v (%s)", err, raw)
	}
	if len(unavailable) != 1 || unavailable[0] != "cache" {
		t.Fatalf("unavailable sections = %#v, want cache only", unavailable)
	}
	for _, retained := range []string{"accounts", "timeseries", "models"} {
		if _, ok := payload[retained]; !ok {
			t.Fatalf("partial response dropped successful %s section: %s", retained, raw)
		}
	}
	if _, ok := payload["cache"]; ok {
		t.Fatalf("partial response included failed cache section: %s", raw)
	}
	var partialData bool
	if err := json.Unmarshal(payload["partial_data"], &partialData); err != nil {
		t.Fatalf("decode partial_data: %v (%s)", err, raw)
	}
	if !partialData {
		t.Fatalf("partial response reported partial_data=false: %s", raw)
	}
}
