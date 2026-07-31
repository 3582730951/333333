package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSMSMarketEndpointExplainsHourlyPriceAndHistoricalSelection(t *testing.T) {
	harness := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx := t.Context()
	now := time.Now().Unix()
	if err := harness.store.SetSettings(ctx, map[string]string{
		"sms_min_price": "0.02", "sms_max_price": "0.08", "sms_preferred_countries": "BR,CO,PL",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.store.DB().ExecContext(ctx, `
INSERT INTO sms_country_price_snapshots(
 provider,service,country_id,country_iso,country_name,price,inventory,provider_rank,balance,fetched_at
) VALUES(?,?,?,?,?,?,?,?,?,?)`, "herosms", "dr", "73", "BR", "Brazil", 0.045, 42, 0, 5.0, now); err != nil {
		t.Fatal(err)
	}
	for index, status := range []string{"success", "success", "failed"} {
		if _, err := harness.store.DB().ExecContext(ctx, `
INSERT INTO registration_records(id,job_id,status,created_at,sms_provider,sms_country,sms_cost)
VALUES(?,?,?,?,?,?,?)`, "sms-market-history-"+string(rune('a'+index)), "sms-market-job", status, now, "herosms", "BR", 0.045); err != nil {
			t.Fatal(err)
		}
	}
	response, body := teamLifecycleRequest(t, harness.pool.Client(), http.MethodGet,
		harness.pool.URL+"/admin/register/sms-market", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var payload struct {
		Items []struct {
			Provider       string  `json:"provider"`
			CountryISO     string  `json:"country_iso"`
			Attempts       int     `json:"attempts"`
			Succeeded      int     `json:"succeeded"`
			SuccessRate    float64 `json:"success_rate"`
			SelectionBasis string  `json:"selection_basis"`
		} `json:"items"`
		MinPrice       float64  `json:"min_price"`
		MaxPrice       float64  `json:"max_price"`
		RefreshSeconds int      `json:"refresh_interval_seconds"`
		Preferred      []string `json:"preferred_countries"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0].CountryISO != "BR" || payload.Items[0].Attempts != 3 ||
		payload.Items[0].Succeeded != 2 || payload.Items[0].SelectionBasis != "historical_success_rate" {
		t.Fatalf("payload=%s", body)
	}
	if payload.MinPrice != 0.02 || payload.MaxPrice != 0.08 || payload.RefreshSeconds != 3600 || len(payload.Preferred) != 3 {
		t.Fatalf("policy=%s", body)
	}
}
