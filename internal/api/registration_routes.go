package api

import (
	"errors"
	"net/http"
	"strings"
)

// Registration API route wrappers. Auth is unified on adminAllowed (a logged-in admin
// SESSION + CSRF, or the legacy admin_token Bearer) — the previous token-only check
// rejected operators who signed into the portal by session, which is why every button
// on the registration/provider/automation pages returned 401 / nothing.

func (s *Server) regReady(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminAllowed(w, r) {
		return false
	}
	if s.regHandler == nil {
		writeError(w, http.StatusNotImplemented, errors.New("registration not initialized"))
		return false
	}
	return true
}

func (s *Server) handleRegisterBatch(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	// GET lists jobs (the task list); POST starts a batch.
	if r.Method == http.MethodGet {
		s.regHandler.HandleJobList(w, r)
		return
	}
	s.regHandler.HandleRegisterBatch(w, r)
}

func (s *Server) handleRegisterJobs(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	s.regHandler.HandleJobList(w, r)
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	s.regHandler.HandleJobStatus(w, r)
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	s.regHandler.HandleJobEvents(w, r)
}

// handleJobAction handles the /admin/register/job/{id}/cancel subtree (the exact
// /status and /events routes registered alongside it take precedence for those paths).
func (s *Server) handleJobAction(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/register/job/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "cancel" {
		s.regHandler.HandleJobCancel(w, r, parts[0])
		return
	}
	writeError(w, http.StatusNotFound, errors.New("registration job action not found"))
}

func (s *Server) handleProviderSettings(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	s.regHandler.HandleProviderSettings(w, r)
}

func (s *Server) handleRegisterStats(w http.ResponseWriter, r *http.Request) {
	if !s.regReady(w, r) {
		return
	}
	s.regHandler.HandleStats(w, r)
}
