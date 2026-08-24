package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestAmbientEnergyBlendsPoolPressureAndLoad(t *testing.T) {
	cases := []struct {
		name    string
		summary storage.AccountPoolSummary
		cpu     float64
		want    float64
	}{
		{"empty pool contributes no pressure", storage.AccountPoolSummary{}, 0, 0},
		{"a fully healthy pool on an idle host is calm", storage.AccountPoolSummary{Total: 10, Active: 10}, 0, 0},
		// Pool state is weighted 0.65 and host load 0.35, so neither can mask the
		// other: a healthy pool on a pinned box still reads as busy.
		{"host load alone still registers", storage.AccountPoolSummary{Total: 10, Active: 10}, 100, 0.35},
		{"pool degradation alone still registers", storage.AccountPoolSummary{Total: 10, Cooling: 10}, 0, 0.65},
		{"both saturated clamps to one", storage.AccountPoolSummary{Total: 4, Cooling: 2, Quarantined: 1, Recheck: 1}, 100, 1},
		{"half degraded, half loaded", storage.AccountPoolSummary{Total: 10, Cooling: 5}, 50, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ambientEnergy(tc.summary, tc.cpu)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("ambientEnergy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAmbientEnergyStaysInRangeOnNonsenseInput(t *testing.T) {
	// Total is the divisor and the counts are independent columns, so a snapshot
	// taken mid-transition can report more degraded accounts than the pool holds.
	// The shader multiplies by this value; a number outside 0-1 there is a visibly
	// blown-out background rather than a caught error.
	for _, cpu := range []float64{-50, 0, 250} {
		got := ambientEnergy(storage.AccountPoolSummary{Total: 2, Cooling: 9, Quarantined: 9}, cpu)
		if got < 0 || got > 1 {
			t.Fatalf("ambientEnergy(cpu=%v) = %v, outside 0-1", cpu, got)
		}
	}
}

func TestAmbientDeltaSendsOnlyWhatChanged(t *testing.T) {
	seen := map[string]string{}

	first := ambientDelta(seen, ambientSample{"total": 3, "energy": 0.25})
	if len(first) != 2 {
		t.Fatalf("first sample must be complete, got %v", first)
	}

	// Same values again: an idle pool must cost nothing but the heartbeat.
	if again := ambientDelta(seen, ambientSample{"total": 3, "energy": 0.25}); len(again) != 0 {
		t.Fatalf("unchanged sample produced a delta: %v", again)
	}

	changed := ambientDelta(seen, ambientSample{"total": 3, "energy": 0.4})
	if len(changed) != 1 {
		t.Fatalf("delta = %v, want only the changed key", changed)
	}
	if _, ok := changed["energy"]; !ok {
		t.Fatalf("delta = %v, want energy", changed)
	}

	// A key that appears later is new, not unchanged-and-absent.
	added := ambientDelta(seen, ambientSample{"cpu_pct": 12.5})
	if len(added) != 1 || added["cpu_pct"] != 12.5 {
		t.Fatalf("delta = %v, want the newly present key", added)
	}
}

func TestAmbientDeltaTreatsZeroAsAValue(t *testing.T) {
	// Comparing on the marshalled form rather than on emptiness: a count that drops
	// to zero is exactly the transition an operator most needs pushed, and a
	// truthiness check would swallow it.
	seen := map[string]string{}
	ambientDelta(seen, ambientSample{"cooling": 4})
	got := ambientDelta(seen, ambientSample{"cooling": 0})
	if len(got) != 1 {
		t.Fatalf("drop to zero produced no delta: %v", got)
	}
}

func TestAmbientStreamIntervalClamps(t *testing.T) {
	cases := map[string]time.Duration{
		"":       ambientStreamDefaultInterval,
		"junk":   ambientStreamDefaultInterval,
		"0":      ambientStreamMinInterval,
		"-30":    ambientStreamMinInterval,
		"1":      ambientStreamMinInterval,
		"5":      5 * time.Second,
		"600":    ambientStreamMaxInterval,
		"999999": ambientStreamMaxInterval,
	}
	for raw, want := range cases {
		if got := ambientStreamInterval(raw); got != want {
			t.Fatalf("ambientStreamInterval(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestAdminAmbientStreamWritesSSESnapshotAndStops(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "a1", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Driven over the harness server rather than by calling the handler directly,
	// because adminAllowed is part of what is being tested and a bare
	// httptest.NewRequest carries none of the harness's admin context.
	reqCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, h.pool.URL+"/admin/stream/ambient?interval=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Without this an nginx or CDN edge buffers the whole response and delivers the
	// pulse in one lump when the connection finally closes.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}

	// The response never ends on its own, so read line by line and stop at the
	// first snapshot payload instead of draining to EOF.
	reader := bufio.NewReader(resp.Body)
	var sawRetry bool
	var sawSnapshotEvent bool
	var payload string
	for i := 0; i < 40 && payload == ""; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "retry: "):
			sawRetry = true
		case line == "event: snapshot":
			sawSnapshotEvent = true
		case sawSnapshotEvent && strings.HasPrefix(line, "data: "):
			payload = strings.TrimPrefix(line, "data: ")
		}
	}
	if !sawRetry {
		t.Fatal("stream did not advertise a reconnect interval")
	}
	if payload == "" {
		t.Fatal("stream did not open with a snapshot payload")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("snapshot is not JSON: %v\n%s", err, payload)
	}
	if _, ok := decoded["energy"]; !ok {
		t.Fatalf("snapshot has no energy field: %v", decoded)
	}
	if total, ok := decoded["total"].(float64); !ok || total != 1 {
		t.Fatalf("snapshot total = %v, want 1", decoded["total"])
	}

	// Cancelling the request context is how a closed tab reaches the handler; the
	// loop must notice and return rather than tick against a dead connection.
	cancel()
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("stream stayed readable after the request context was cancelled")
	}
}

func TestAdminAmbientStreamRejectsNonGet(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _ := grpReq(t, h, http.MethodPost, "/admin/stream/ambient", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /admin/stream/ambient = %d, want 405", code)
	}
}
