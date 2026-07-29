package api

import (
	"net/http"
)

func (s *Server) handleRemovedPayment(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	writeError(w, http.StatusGone, &PublicError{
		Code:    "feature_removed",
		Message: "Payment and Plus upgrade features have been removed.",
	})
}

func (s *Server) handleRemovedLifecycle(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	writeError(w, http.StatusGone, &PublicError{
		Code:    "feature_removed",
		Message: "The legacy lifecycle endpoint has been removed; use /admin/register/batch.",
	})
}

func (s *Server) handleRemovedRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	writeError(w, http.StatusGone, &PublicError{
		Code:    "feature_removed",
		Message: "The legacy registration endpoint has been removed; use /admin/register/batch.",
	})
}
