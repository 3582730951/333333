// Package passivehealth aggregates payload-free observations from real upstream
// traffic. It never launches probes and keeps strict cardinality and retention
// bounds so diagnostics remain available during provider or request floods.
package passivehealth

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"codex-account-pool/internal/upstream"
)

const (
	DefaultMaxSeries = 512
	DefaultRetention = 24 * time.Hour
	ewmaAlpha        = 0.2
	maxLabelBytes    = 96
)

type Series struct {
	Provider        string  `json:"provider"`
	Model           string  `json:"model"`
	EgressID        string  `json:"egress_id"`
	Health          string  `json:"health"`
	Observations    uint64  `json:"observations"`
	HealthSamples   uint64  `json:"health_samples"`
	Successes       uint64  `json:"successes"`
	Failures        uint64  `json:"failures"`
	Canceled        uint64  `json:"canceled"`
	RateLimited     uint64  `json:"rate_limited"`
	SuccessEWMA     float64 `json:"success_ewma"`
	LatencyEWMAms   float64 `json:"latency_ewma_ms"`
	LastStatusCode  int     `json:"last_status_code"`
	LastErrorClass  string  `json:"last_error_class,omitempty"`
	FirstObservedAt int64   `json:"first_observed_at"`
	LastObservedAt  int64   `json:"last_observed_at"`
}

type Snapshot struct {
	GeneratedAt      int64    `json:"generated_at"`
	RetentionSeconds int64    `json:"retention_seconds"`
	MaxSeries        int      `json:"max_series"`
	SeriesCount      int      `json:"series_count"`
	Evictions        uint64   `json:"evictions"`
	Series           []Series `json:"series"`
}

type Monitor struct {
	mu        sync.Mutex
	maxSeries int
	retention time.Duration
	now       func() time.Time
	series    map[string]*Series
	evictions uint64
	nextPrune time.Time
}

func New(maxSeries int, retention time.Duration) *Monitor {
	if maxSeries <= 0 {
		maxSeries = DefaultMaxSeries
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Monitor{maxSeries: maxSeries, retention: retention, now: time.Now, series: make(map[string]*Series)}
}

func (m *Monitor) Observe(observation upstream.AttemptObservation) {
	if m == nil {
		return
	}
	now := observation.ObservedAt
	if now.IsZero() {
		now = m.now()
	}
	provider := safeLabel(observation.Provider, "unknown")
	model := safeLabel(observation.Model, "unknown")
	egressID := safeLabel(observation.EgressID, "direct")
	errorClass := safeLabel(observation.ErrorClass, "")
	key := provider + "\x00" + model + "\x00" + egressID

	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneIfDueLocked(now)
	row := m.series[key]
	if row == nil {
		if len(m.series) >= m.maxSeries {
			// Preserve established operational series and reject unbounded label
			// churn in O(1). Scanning/evicting the oldest of 512 entries for every
			// attacker-controlled model label would turn this diagnostic observer
			// into a request-path CPU amplifier.
			m.evictions++
			return
		}
		row = &Series{Provider: provider, Model: model, EgressID: egressID, FirstObservedAt: now.Unix()}
		m.series[key] = row
	}
	row.Observations++
	row.LastObservedAt = now.Unix()
	row.LastStatusCode = observation.StatusCode
	row.LastErrorClass = errorClass
	if errorClass == "client_canceled" || errorClass == "client_deadline" {
		row.Canceled++
		row.Health = healthState(row)
		return
	}
	row.HealthSamples++
	value := 0.0
	if observation.Success {
		row.Successes++
		value = 1
	} else {
		row.Failures++
	}
	if errorClass == "rate_limited" {
		row.RateLimited++
	}
	if row.HealthSamples == 1 {
		row.SuccessEWMA = value
	} else {
		row.SuccessEWMA = ewmaAlpha*value + (1-ewmaAlpha)*row.SuccessEWMA
	}
	latencyMS := float64(observation.Latency) / float64(time.Millisecond)
	if latencyMS > 0 {
		if latencyMS > float64((10*time.Minute)/time.Millisecond) {
			latencyMS = float64((10 * time.Minute) / time.Millisecond)
		}
		if row.LatencyEWMAms == 0 {
			row.LatencyEWMAms = latencyMS
		} else {
			row.LatencyEWMAms = ewmaAlpha*latencyMS + (1-ewmaAlpha)*row.LatencyEWMAms
		}
	}
	row.Health = healthState(row)
}

func (m *Monitor) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	m.nextPrune = now.Add(m.pruneInterval())
	rows := make([]Series, 0, len(m.series))
	for _, row := range m.series {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].EgressID < rows[j].EgressID
	})
	return Snapshot{GeneratedAt: now.Unix(), RetentionSeconds: int64(m.retention / time.Second), MaxSeries: m.maxSeries,
		SeriesCount: len(rows), Evictions: m.evictions, Series: rows}
}

func (m *Monitor) pruneIfDueLocked(now time.Time) {
	if !m.nextPrune.IsZero() && now.Before(m.nextPrune) {
		return
	}
	m.pruneLocked(now)
	m.nextPrune = now.Add(m.pruneInterval())
}

func (m *Monitor) pruneInterval() time.Duration {
	interval := time.Minute
	if m.retention < interval {
		interval = m.retention
	}
	if interval <= 0 {
		return time.Minute
	}
	return interval
}

func (m *Monitor) pruneLocked(now time.Time) {
	cutoff := now.Add(-m.retention).Unix()
	for key, row := range m.series {
		if row.LastObservedAt > 0 && row.LastObservedAt < cutoff {
			delete(m.series, key)
			m.evictions++
		}
	}
}

func healthState(row *Series) string {
	if row == nil || row.HealthSamples == 0 {
		return "unknown"
	}
	if row.SuccessEWMA >= 0.95 {
		return "healthy"
	}
	if row.SuccessEWMA >= 0.70 {
		return "degraded"
	}
	return "unhealthy"
}

func safeLabel(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	if len(value) > maxLabelBytes {
		value = value[:maxLabelBytes]
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._:/-()", r) {
			continue
		}
		return fallback
	}
	return value
}
