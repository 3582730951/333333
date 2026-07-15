package api

import (
	"log"
	"net/http"

	"codex-account-pool/internal/storage"
)

// adminContextJournal removes only encrypted Responses replay state and then
// compacts SQLite. Accounts, credentials, usage, audit data and active requests are
// not modified; an in-flight response may create a new journal row after this call.
func (s *Server) adminContextJournal(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	s.WaitForAsyncWrites()
	deleted, err := s.store.ClearContextJournal(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reclaimed := true
	warning := ""
	if err := s.store.ReclaimLogStorage(r.Context()); err != nil {
		reclaimed = false
		warning = err.Error()
	}
	log.Printf("[CONTEXT-JOURNAL] manual clear deleted=%d reclaimed=%t", deleted, reclaimed)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":               true,
		"deleted_contexts": deleted,
		"space_reclaimed":  reclaimed,
		"reclaim_warning":  warning,
		"ttl_seconds":      s.settingInt(r.Context(), "context_journal_ttl_seconds", s.cfg.ContextJournalTTLSeconds),
		"completed_at":     storage.Now(),
	})
}
