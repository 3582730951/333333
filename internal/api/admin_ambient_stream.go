package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/sysmetrics"
)

// Ambient stream: the console's live pulse, pushed rather than polled.
//
// The dashboard used to reach for /admin/accounts/summary and /admin/system on a
// 15s timer per open tab. That is the wrong shape for one-way server-to-client
// data: every tab pays a full request/response and TLS round trip to learn that
// nothing changed, and the numbers are stale for up to 15s regardless. Server-Sent
// Events costs one long-lived HTTP/1.1 response per tab, needs no new protocol on
// either side (EventSource is native), and lets the server decide when there is
// something worth saying.
//
// Two properties matter for server load and are both implemented below:
//
//   - Diff only. The first event carries the whole snapshot; after that only the
//     fields whose values actually changed are written. An idle pool emits nothing
//     but a heartbeat comment, so a parked tab costs a few bytes a minute.
//   - Bounded work. The sampling interval is clamped, and each tick does exactly
//     one summary query plus one metrics collection -- the same work the old poll
//     did, now shared by every listener on that tab instead of duplicated per view.
const (
	ambientStreamMinInterval     = 2 * time.Second
	ambientStreamMaxInterval     = 60 * time.Second
	ambientStreamDefaultInterval = 5 * time.Second
	// A comment line keeps intermediaries from reaping an idle connection without
	// waking any handler on the client: EventSource ignores comments entirely.
	ambientStreamHeartbeat = 20 * time.Second
)

type ambientSample map[string]any

// ambientEnergy folds pool pressure and machine load into the single 0-1 scalar the
// atmosphere shader consumes. It is deliberately a blend rather than a single
// metric: a pool that is entirely healthy on a machine that is pinned should still
// read as "under load", and vice versa. Weighted toward pool state because that is
// the thing an operator is actually watching this console for.
func ambientEnergy(summary storage.AccountPoolSummary, cpuPct float64) float64 {
	pressure := 0.0
	if summary.Total > 0 {
		degraded := summary.Cooling + summary.Quarantined + summary.Recheck
		pressure = float64(degraded) / float64(summary.Total)
	}
	load := cpuPct / 100
	energy := pressure*0.65 + load*0.35
	if math.IsNaN(energy) || math.IsInf(energy, 0) {
		return 0
	}
	return math.Min(1, math.Max(0, energy))
}

func ambientStreamInterval(raw string) time.Duration {
	if raw == "" {
		return ambientStreamDefaultInterval
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return ambientStreamDefaultInterval
	}
	interval := time.Duration(seconds) * time.Second
	if interval < ambientStreamMinInterval {
		return ambientStreamMinInterval
	}
	if interval > ambientStreamMaxInterval {
		return ambientStreamMaxInterval
	}
	return interval
}

func (s *Server) ambientSample(ctx context.Context) ambientSample {
	sample := ambientSample{}
	summary, err := s.store.AccountPoolSummary(ctx, storage.Now())
	if err == nil {
		sample["total"] = summary.Total
		sample["active"] = summary.Active
		sample["cooling"] = summary.Cooling
		sample["quarantined"] = summary.Quarantined
		sample["recheck"] = summary.Recheck
		sample["codex"] = summary.Codex
		sample["claude"] = summary.Claude
	}
	metrics := sysmetrics.Collect(filepath.Dir(s.cfg.DatabasePath))
	cpu := 0.0
	if metrics.Supported {
		cpu = metrics.CPU.UsagePct
		sample["cpu_pct"] = math.Round(cpu*10) / 10
		sample["mem_pct"] = math.Round(metrics.Mem.UsedPct*10) / 10
	}
	// Rounded before it is diffed: the raw float jitters in the third decimal on
	// every sample, which would defeat the diff and turn an idle pool into a
	// continuous stream of meaningless updates.
	sample["energy"] = math.Round(ambientEnergy(summary, cpu)*1000) / 1000
	return sample
}

// ambientDelta returns only the entries of next whose value differs from prev, and
// updates prev in place. Comparison is on the marshalled form so that ints, floats
// and absent-vs-zero all compare the way the client will actually see them.
func ambientDelta(prev map[string]string, next ambientSample) ambientSample {
	delta := ambientSample{}
	for key, value := range next {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		text := string(encoded)
		if prev[key] == text {
			continue
		}
		prev[key] = text
		delta[key] = value
	}
	return delta
}

func (s *Server) adminAmbientStream(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// nginx and several CDN edges buffer an unknown-length response by default,
	// which turns a 5s pulse into one delivery when the connection finally closes.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	interval := ambientStreamInterval(r.URL.Query().Get("interval"))
	// Tells EventSource how long to wait before reconnecting after a drop. The
	// client applies its own exponential backoff on top for repeated failures.
	fmt.Fprintf(w, "retry: %d\n\n", interval.Milliseconds())

	ctx := r.Context()
	seen := map[string]string{}
	write := func(event string, payload ambientSample) bool {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !write("snapshot", ambientDelta(seen, s.ambientSample(ctx))) {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(ambientStreamHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			delta := ambientDelta(seen, s.ambientSample(ctx))
			if len(delta) == 0 {
				// Nothing moved. Saying so would cost a frame on every client for
				// no information, so the tick is simply skipped.
				continue
			}
			if !write("delta", delta) {
				return
			}
		}
	}
}
