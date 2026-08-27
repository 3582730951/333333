package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

// End-to-end wiring of the calculate_money estimation scheme: a wham poll
// snapshots used_percent together with the relay's priced spend inside the same
// window cycle, and the admin quota summary picks up the empirical dollar
// estimate from those samples.
func TestQuotaWindowPollRecordsSamplesAndAttachesEstimate(t *testing.T) {
	var poll int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&poll, 1)
		body := `{"plan_type":"pro","rate_limit":{"limit_reached":false,"primary_window":{"used_percent":8,"limit_window_seconds":18000,"reset_after_seconds":3600,"status":"allowed"},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":7200,"status":"allowed"}}}`
		if n > 1 {
			body = `{"plan_type":"pro","rate_limit":{"limit_reached":false,"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600,"status":"allowed"},"secondary_window":{"used_percent":33,"limit_window_seconds":604800,"reset_after_seconds":7200,"status":"allowed"}}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	oldURL := whamUsageURL
	whamUsageURL = upstream.URL
	t.Cleanup(func() { whamUsageURL = oldURL })

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "quota-window-integration", Provider: "codex", GroupName: "cyber", Status: "active"}
	token := storage.AccountToken{AccountID: account.ID, AccessToken: "access-token"}
	if err := h.store.UpsertAccount(ctx, account, token); err != nil {
		t.Fatal(err)
	}
	egress := storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}

	// Priced gpt-5 usage inside the 5h cycle window gives the samples their USD
	// cost basis (record 1: 0.0325, both records together: 0.045).
	now := storage.Now()
	insertUsage := func(prompt, completion, createdAt int64) {
		if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO usage_records(account_id, route_key_hash, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, raw_usage_json, created_at) VALUES(?, '', 'gpt-5', ?, ?, ?, 0, '{}', ?)`,
			account.ID, prompt, completion, prompt+completion, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	insertUsage(10000, 2000, now-7200)
	if err := h.app.pollOneCodexQuota(ctx, account, token, egress); err != nil {
		t.Fatal(err)
	}
	// Samples are keyed by unix-second sample_at; production polls run minutes
	// apart, so wait out the current second so the two polls yield distinct rows.
	for storage.Now() == now {
		time.Sleep(5 * time.Millisecond)
	}
	insertUsage(10000, 0, now-100)
	if err := h.app.pollOneCodexQuota(ctx, account, token, egress); err != nil {
		t.Fatal(err)
	}

	cycleStarts := quotaWindowCyclesFor(t, h, account.ID, quotaWindowKind5h)
	if len(cycleStarts) != 1 {
		t.Fatalf("5h cycles = %v, want exactly one (poll-to-poll reset drift is bucketed)", cycleStarts)
	}
	samples, err := h.store.QuotaWindowSamples(ctx, account.ID, quotaWindowKind5h, cycleStarts[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("5h samples = %d, want one per poll: %+v", len(samples), samples)
	}
	if samples[0].UsedPercent != 8 || samples[1].UsedPercent != 10 {
		t.Fatalf("5h used_percents = %v, %v; want 8, 10", samples[0].UsedPercent, samples[1].UsedPercent)
	}
	if samples[0].CostUSD <= 0 || samples[1].CostUSD <= samples[0].CostUSD {
		t.Fatalf("5h costs must be positive and rising: %v, %v", samples[0].CostUSD, samples[1].CostUSD)
	}

	cycle7d := quotaWindowCyclesFor(t, h, account.ID, quotaWindowKind7d)
	if len(cycle7d) != 1 {
		t.Fatalf("7d cycles = %v, want exactly one", cycle7d)
	}
	if samples7d, err := h.store.QuotaWindowSamples(ctx, account.ID, quotaWindowKind7d, cycle7d[0]); err != nil {
		t.Fatal(err)
	} else if len(samples7d) != 2 {
		t.Fatalf("7d samples = %d, want one per poll: %+v", len(samples7d), samples7d)
	}

	// The admin quota summary now carries the empirical dollar estimate.
	snaps, err := h.store.ListAccountRateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var filtered []storage.AccountRateLimit
	for _, snap := range snaps {
		if snap.AccountID == account.ID {
			filtered = append(filtered, snap)
		}
	}
	summary := BuildQuotaSummary(account, &token, filtered, storage.Now())
	h.app.attachQuotaWindowEstimate(ctx, account, &summary, storage.Now())
	if summary.Estimate == nil {
		t.Fatal("summary has no estimate")
	}
	if !summary.Estimate.Estimated || summary.Estimate.Method != "window_cost_estimate" {
		t.Fatalf("estimate = %+v, want method window_cost_estimate", summary.Estimate)
	}
	if summary.Estimate.LimitUSD <= 0 || summary.Estimate.UsedUSD <= 0 || summary.Estimate.RemainingUSD <= 0 {
		t.Fatalf("estimate dollars = limit %v used %v remaining %v, want all positive", summary.Estimate.LimitUSD, summary.Estimate.UsedUSD, summary.Estimate.RemainingUSD)
	}
	if summary.Estimate.Confidence == "" || summary.Estimate.LimitUSDMin <= 0 || summary.Estimate.LimitUSDMax <= summary.Estimate.LimitUSDMin {
		t.Fatalf("estimate band = %+v, want confidence and a sane USD interval", summary.Estimate)
	}
}

func quotaWindowCyclesFor(t *testing.T, h *testHarness, accountID, windowKind string) []int64 {
	t.Helper()
	rows, err := h.store.DB().QueryContext(context.Background(),
		`SELECT cycle_start FROM quota_window_cycles WHERE account_id = ? AND window_kind = ?`, accountID, windowKind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var c int64
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}
