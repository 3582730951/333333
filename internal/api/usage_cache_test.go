package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"
)

func TestAdminUsageCacheMetricsEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called")
	})
	ctx := context.Background()
	if err := h.store.InsertUsageRecord(ctx, "acc-a", "route-a", "abcdef1234567890", "", "gpt-5.5", 100, 10, 110, 40, json.RawMessage(`{"real":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecord(ctx, "acc-a", "route-a", "abcdef1234567890", "", "gpt-5.5", 200, 20, 220, 0, json.RawMessage(`{"estimated":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecord(ctx, "acc-b", "route-b", "fedcba9876543210", "", "gpt-5.4", 300, 30, 330, 120, json.RawMessage(`{"real":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecordWithCacheDetails(ctx, "acc-c", "route-c", "facefeed12345678", "", "claude-sonnet", 400, 40, 440, 0, 0, 210, json.RawMessage(`{"real":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecordWithCacheDetails(ctx, "acc-d", "route-d", "ca11ab1e12345678", "", "claude-sonnet", 50, 5, 55, 700, 700, 0, json.RawMessage(`{"usage":{"input_tokens":50,"cache_read_input_tokens":700}}`)); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(h.pool.URL + "/admin/usage/cache?since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/usage/cache status %d: %s", resp.StatusCode, raw)
	}
	var got struct {
		Summary struct {
			Requests            int64   `json:"requests"`
			HitRequests         int64   `json:"hit_requests"`
			RequestHitRate      float64 `json:"request_hit_rate"`
			PromptTokens        int64   `json:"prompt_tokens"`
			CachedTokens        int64   `json:"cached_tokens"`
			CacheInputTokens    int64   `json:"cache_input_tokens"`
			CacheReadTokens     int64   `json:"cache_read_tokens"`
			CacheCreationTokens int64   `json:"cache_creation_tokens"`
			TokenHitRate        float64 `json:"token_hit_rate"`
			EstimatedRequests   int64   `json:"estimated_requests"`
			EstimatedRate       float64 `json:"estimated_rate"`
		} `json:"summary"`
		ByAPIKey []struct {
			APIKeyHashPrefix string `json:"api_key_hash_prefix"`
			Requests         int64  `json:"requests"`
		} `json:"by_api_key"`
		ByAccountModel []struct {
			AccountID   string `json:"account_id"`
			Model       string `json:"model"`
			HitRequests int64  `json:"hit_requests"`
		} `json:"by_account_model"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode /admin/usage/cache: %v (%s)", err, raw)
	}
	if got.Summary.Requests != 5 || got.Summary.HitRequests != 3 {
		t.Fatalf("summary requests wrong: %+v", got.Summary)
	}
	if got.Summary.PromptTokens != 1050 || got.Summary.CachedTokens != 860 || got.Summary.CacheInputTokens != 1960 || got.Summary.CacheReadTokens != 860 || got.Summary.CacheCreationTokens != 210 || got.Summary.EstimatedRequests != 1 {
		t.Fatalf("summary tokens wrong: %+v", got.Summary)
	}
	if math.Abs(got.Summary.RequestHitRate-0.6) > 0.0001 {
		t.Fatalf("request_hit_rate = %v", got.Summary.RequestHitRate)
	}
	if math.Abs(got.Summary.TokenHitRate-(860.0/1960.0)) > 0.0001 {
		t.Fatalf("token_hit_rate = %v", got.Summary.TokenHitRate)
	}
	if got.Summary.TokenHitRate > 1 {
		t.Fatalf("token_hit_rate must never exceed 1, got %v", got.Summary.TokenHitRate)
	}
	if math.Abs(got.Summary.EstimatedRate-0.2) > 0.0001 {
		t.Fatalf("estimated_rate = %v", got.Summary.EstimatedRate)
	}
	if len(got.ByAPIKey) == 0 || got.ByAPIKey[0].APIKeyHashPrefix == "abcdef1234567890" {
		t.Fatalf("api key hash should be shortened, got %+v", got.ByAPIKey)
	}
	if len(got.ByAccountModel) == 0 {
		t.Fatalf("by_account_model missing")
	}
}
