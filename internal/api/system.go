package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/sysmetrics"
	"codex-account-pool/internal/upstream"
)

var sidecarMetricsClient = &http.Client{Timeout: 500 * time.Millisecond}

// adminSystem reports host + process resource metrics (CPU/memory/disk/uptime, the Go
// runtime, and the registration tasks' node/Chrome/Xvfb memory) so an admin can see the
// VPS's state and how much RAM auto-registration is using — entirely from the web UI.
// Admin-gated; GET only. On a non-Linux host it returns {supported:false}.
func (s *Server) adminSystem(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	dataDir := filepath.Dir(s.cfg.DatabasePath)
	payload := struct {
		sysmetrics.Metrics
		Admission          interface{}                 `json:"admission"`
		Scheduler          interface{}                 `json:"scheduler"`
		RouteAvailability  routeAvailabilitySnapshot   `json:"route_availability"`
		BodyStorage        bodysource.BudgetSnapshot   `json:"body_storage"`
		UsageJournal       map[string]interface{}      `json:"usage_journal"`
		UsageRollup        interface{}                 `json:"usage_rollup"`
		HTTP               httpRequestMetricsSnapshot  `json:"http"`
		UpstreamCaches     upstream.UpstreamCacheStats `json:"upstream_caches"`
		RoutingAudit       map[string]interface{}      `json:"routing_audit"`
		ContextRebuilt     uint64                      `json:"context_rebuilt"`
		ContextDegraded    uint64                      `json:"context_degraded"`
		CodexSessionMap    map[string]interface{}      `json:"codex_session_mapping"`
		Sidecar            interface{}                 `json:"sidecar,omitempty"`
		DiskGuard          DiskGuardSnapshot           `json:"disk_guard"`
		SupervisorEvents   []supervisor.Event          `json:"supervisor_events"`
		SupervisorModules  []supervisor.ModuleState    `json:"supervisor_modules"`
		ExceptionCallbacks []supervisor.CallbackState  `json:"exception_callbacks"`
		ExceptionJournal   interface{}                 `json:"exception_journal"`
	}{
		Metrics:   sysmetrics.Collect(dataDir),
		Admission: s.scheduler.AdmissionSnapshot(),
		Scheduler: s.scheduler.Metrics(),
		RouteAvailability: func() routeAvailabilitySnapshot {
			if s.routeAvailability == nil {
				return routeAvailabilitySnapshot{}
			}
			return s.routeAvailability.Snapshot()
		}(),
		BodyStorage:  s.bodyBudgetSnapshot(),
		UsageJournal: s.usageJournalMetrics(),
		UsageRollup: func() interface{} {
			if s.store == nil {
				return map[string]interface{}{"rows": 0, "ready": false}
			}
			stats, err := s.store.UsageHourlyRollupStats(r.Context())
			if err != nil {
				return map[string]interface{}{"rows": 0, "ready": false}
			}
			return stats
		}(),
		HTTP: s.httpMetrics.snapshot(),
		UpstreamCaches: func() upstream.UpstreamCacheStats {
			if s.upstream == nil {
				return upstream.UpstreamCacheStats{}
			}
			return s.upstream.CacheStats()
		}(),
		RoutingAudit:   s.routingAuditDiagnostics(),
		ContextRebuilt: atomic.LoadUint64(&s.contextRebuilt), ContextDegraded: atomic.LoadUint64(&s.contextDegraded),
		CodexSessionMap:    s.codexSessionMappingStats(r.Context()),
		Sidecar:            s.sidecarMetrics(r.Context()),
		DiskGuard:          s.diskGuardSnapshot(),
		SupervisorEvents:   supervisor.RecentEvents(),
		SupervisorModules:  supervisor.ModuleStates(),
		ExceptionCallbacks: supervisor.EventCallbackStates(),
		ExceptionJournal: func() interface{} {
			if s.incidentReporter == nil {
				return map[string]interface{}{"configured": false}
			}
			return s.incidentReporter.Snapshot()
		}(),
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) bodyBudgetSnapshot() bodysource.BudgetSnapshot {
	if s == nil || s.bodyBudget == nil {
		return bodysource.BudgetSnapshot{}
	}
	return s.bodyBudget.Snapshot()
}

func (s *Server) sidecarMetrics(parent context.Context) interface{} {
	if s.upstream == nil || strings.TrimSpace(s.upstream.SidecarEndpoint()) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.upstream.SidecarEndpoint(), "/")+"/metrics", nil)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	resp, err := sidecarMetricsClient.Do(req)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	return out
}
