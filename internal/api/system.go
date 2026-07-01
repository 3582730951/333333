package api

import (
	"net/http"
	"path/filepath"

	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/sysmetrics"
)

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
		SupervisorEvents  []supervisor.Event       `json:"supervisor_events"`
		SupervisorModules []supervisor.ModuleState `json:"supervisor_modules"`
	}{
		Metrics:           sysmetrics.Collect(dataDir),
		SupervisorEvents:  supervisor.RecentEvents(),
		SupervisorModules: supervisor.ModuleStates(),
	}
	writeJSON(w, http.StatusOK, payload)
}
