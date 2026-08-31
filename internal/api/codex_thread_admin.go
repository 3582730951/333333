package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

const codexInterruptConfirmationTimeout = 10 * time.Second

// codexThreadView is the public, metadata-only projection. It intentionally
// omits raw thread/turn identifiers, transcript items, previews, prompts, tool
// inputs/outputs, and full cwd paths.
type codexThreadView struct {
	RuntimeID        string    `json:"runtime_id"`
	RuntimeLabel     string    `json:"runtime_label,omitempty"`
	ThreadKey        string    `json:"thread_key"`
	ThreadHandle     string    `json:"thread_handle"`
	Model            string    `json:"model,omitempty"`
	ModelProvider    string    `json:"model_provider,omitempty"`
	Source           string    `json:"source,omitempty"`
	Status           string    `json:"status,omitempty"`
	WaitingReason    string    `json:"waiting_reason,omitempty"`
	ActiveTurnHandle string    `json:"active_turn_handle,omitempty"`
	RuntimeAvailable bool      `json:"runtime_available"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	DirectInput      bool      `json:"direct_input,omitempty"`
	CWDBase          string    `json:"cwd_basename,omitempty"`
	Revision         uint64    `json:"revision,omitempty"`
}

func (s *Server) codexRuntimePrincipal(r *http.Request) codexRuntimePrincipal {
	if user, ok := s.currentUser(r); ok {
		return codexRuntimePrincipal{ID: user.ID, Group: user.TenantID}
	}
	// Bearer-token admin users do not have a portal principal. They can see only
	// unowned runtimes (or an explicitly admin-token-owned runtime).
	return codexRuntimePrincipal{ID: "admin-token"}
}

// RegisterCodexThreadRuntime is deliberately server-side only. A browser never
// chooses an adapter endpoint or supplies a runtime locator.
func (s *Server) RegisterCodexThreadRuntime(id string, runtime CodexThreadRuntime, descriptor CodexRuntimeDescriptor) error {
	if s == nil || s.codexRuntimeRegistry == nil {
		return errors.New("codex runtime registry unavailable")
	}
	return s.codexRuntimeRegistry.Register(id, runtime, descriptor)
}

func (s *Server) adminCodexRuntimes(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": s.codexRuntimeRegistry.ListFor(s.codexRuntimePrincipal(r)),
	})
}

func (s *Server) runtimeEntryForRequest(r *http.Request, runtimeID string) (*codexRuntimeEntry, bool) {
	entry, ok := s.codexRuntimeRegistry.get(runtimeID)
	if !ok || !entry.allows(s.codexRuntimePrincipal(r)) {
		return nil, false
	}
	return entry, true
}

func (s *Server) runtimeView(principal codexRuntimePrincipal, entry *codexRuntimeEntry, thread Thread) (codexThreadView, error) {
	entry.mu.RLock()
	runtimeID, label, available := entry.ID, entry.Label, entry.Available
	status := entry.statuses[thread.ID]
	entry.mu.RUnlock()
	if status.Revision > 0 {
		thread.Status = status.Status
		thread.WaitingReason = status.WaitingReason
		if status.TurnID != "" {
			thread.ActiveTurnID = status.TurnID
		}
	}
	threadHandle, err := s.codexRuntimeRegistry.ThreadHandle(principal, entry, thread)
	if err != nil {
		return codexThreadView{}, err
	}
	threadKey, err := s.codexRuntimeRegistry.ThreadKey(principal, entry, thread)
	if err != nil {
		return codexThreadView{}, err
	}
	turnHandle, err := s.codexRuntimeRegistry.TurnHandle(principal, entry, thread)
	if err != nil {
		return codexThreadView{}, err
	}
	cwdBase := ""
	if cwd := strings.TrimSpace(thread.CWD); cwd != "" {
		cwdBase = filepath.Base(cwd)
	}
	return codexThreadView{
		RuntimeID:        runtimeID,
		RuntimeLabel:     label,
		ThreadKey:        threadKey,
		ThreadHandle:     threadHandle,
		Model:            sanitizeInferenceFamily(thread.Model),
		ModelProvider:    sanitizeProviderFamily(thread.ModelProvider),
		Source:           sanitizeThreadSource(thread.Source),
		Status:           sanitizeThreadStatus(thread.Status),
		WaitingReason:    sanitizeThreadWaitingReason(thread.WaitingReason),
		ActiveTurnHandle: turnHandle,
		RuntimeAvailable: available,
		UpdatedAt:        thread.UpdatedAt,
		DirectInput:      thread.DirectInput,
		CWDBase:          cwdBase,
		Revision:         status.Revision,
	}, nil
}

func sanitizeInferenceFamily(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(v, "codex"):
		return "codex"
	case strings.HasPrefix(v, "gpt"), strings.HasPrefix(v, "o1"), strings.HasPrefix(v, "o3"), strings.HasPrefix(v, "o4"):
		return "gpt"
	case strings.Contains(v, "claude"):
		return "claude"
	case strings.Contains(v, "gemini"):
		return "gemini"
	default:
		return "unknown"
	}
}

func sanitizeProviderFamily(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "codex", "chatgpt":
		return "codex"
	case "anthropic", "claude":
		return "claude"
	case "google", "gemini", "antigravity":
		return "gemini"
	default:
		return "unknown"
	}
}

func sanitizeThreadSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "user", "cli", "rollout", "imported", "unknown":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func sanitizeThreadStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "notloaded", "not_loaded":
		return "notLoaded"
	case "idle":
		return "idle"
	case "systemerror", "system_error":
		return "systemError"
	case "active":
		return "active"
	case "turnaborted", "turn_aborted", "aborted":
		return "turnAborted"
	default:
		return "unknown"
	}
}

func sanitizeThreadWaitingReason(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "waitingonapproval", "waiting_on_approval":
		return "waitingOnApproval"
	case "waitingonuserinput", "waiting_on_user_input":
		return "waitingOnUserInput"
	default:
		return ""
	}
}

func codexListFilterHash(params ThreadListParams) string {
	payload, _ := json.Marshal(struct {
		SortKey       string
		SortDirection string
		Providers     []string
		Sources       []string
		Archived      *bool
		Pinned        *bool
		SearchTerm    string
	}{
		SortKey: params.SortKey, SortDirection: params.SortDirection, Providers: params.ModelProviders,
		Sources: params.SourceKinds, Archived: params.Archived, Pinned: params.IsPinned, SearchTerm: params.SearchTerm,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func parseCodexBool(raw string) (*bool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func parseCodexListFilter(raw string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" || len(item) > 64 {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}

func codexListParams(r *http.Request) (ThreadListParams, error) {
	query := r.URL.Query()
	archived, err := parseCodexBool(query.Get("archived"))
	if err != nil {
		return ThreadListParams{}, errors.New("invalid archived filter")
	}
	pinned, err := parseCodexBool(query.Get("is_pinned"))
	if err != nil {
		return ThreadListParams{}, errors.New("invalid is_pinned filter")
	}
	limitText := strings.TrimSpace(query.Get("limit"))
	limit := 0
	if limitText != "" {
		limit, err = strconv.Atoi(limitText)
		if err != nil {
			return ThreadListParams{}, errors.New("invalid limit")
		}
	}
	if limit < 0 || limit > 100 {
		return ThreadListParams{}, errors.New("limit must be between 0 and 100")
	}
	params := ThreadListParams{
		SortKey:        strings.TrimSpace(query.Get("sort_key")),
		SortDirection:  strings.TrimSpace(query.Get("sort_direction")),
		ModelProviders: parseCodexListFilter(query.Get("model_providers")),
		SourceKinds:    parseCodexListFilter(query.Get("source_kinds")),
		Archived:       archived,
		IsPinned:       pinned,
		SearchTerm:     strings.TrimSpace(query.Get("search_term")),
		Limit:          limit,
	}
	if len(params.SearchTerm) > 256 {
		return ThreadListParams{}, errors.New("search_term is too long")
	}
	return params, nil
}

func (s *Server) adminCodexThreads(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	runtimeID := strings.TrimSpace(r.URL.Query().Get("runtime_id"))
	if runtimeID == "" {
		writeCodexThreadError(w, http.StatusBadRequest, "codex_runtime_id_required", "runtime_id is required")
		return
	}
	entry, ok := s.runtimeEntryForRequest(r, runtimeID)
	if !ok {
		writeCodexThreadError(w, http.StatusNotFound, "codex_thread_access_denied", "thread not found")
		return
	}
	if !entry.available() {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime is unavailable")
		return
	}
	params, err := codexListParams(r)
	if err != nil {
		writeCodexThreadError(w, http.StatusBadRequest, "codex_invalid_filter", err.Error())
		return
	}
	principal := s.codexRuntimePrincipal(r)
	filterHash := codexListFilterHash(params)
	if cursor := strings.TrimSpace(r.URL.Query().Get("cursor")); cursor != "" {
		handle, err := s.codexRuntimeRegistry.open(cursor, "cursor", principal)
		if err != nil || handle.RuntimeID != runtimeID || handle.FilterHash != filterHash {
			writeCodexThreadError(w, http.StatusBadRequest, "codex_invalid_cursor", "cursor is invalid")
			return
		}
		entry.mu.RLock()
		generation := entry.Generation
		entry.mu.RUnlock()
		if handle.Generation != generation {
			writeCodexThreadError(w, http.StatusConflict, "codex_runtime_generation_stale", "runtime generation changed")
			return
		}
		params.Cursor = handle.Cursor
	}
	response, err := entry.Runtime.ListThreads(r.Context(), runtimeID, params)
	if err != nil {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime is unavailable")
		return
	}
	views := make([]codexThreadView, 0, len(response.Data))
	seen := make(map[string]struct{}, len(response.Data))
	for _, thread := range response.Data {
		if strings.TrimSpace(thread.ID) == "" {
			continue
		}
		if _, exists := seen[thread.ID]; exists {
			continue
		}
		seen[thread.ID] = struct{}{}
		view, err := s.runtimeView(principal, entry, thread)
		if err != nil {
			writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime handle unavailable")
			return
		}
		views = append(views, view)
	}
	entry.mu.RLock()
	generation := entry.Generation
	entry.mu.RUnlock()
	nextCursor, err := s.codexRuntimeRegistry.Cursor(principal, runtimeID, response.NextCursor, filterHash, generation)
	if err != nil {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime cursor unavailable")
		return
	}
	backwardCursor, err := s.codexRuntimeRegistry.Cursor(principal, runtimeID, response.BackwardsCursor, filterHash, generation)
	if err != nil {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime cursor unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": views, "next_cursor": nextCursor, "backwards_cursor": backwardCursor,
	})
}

func writeCodexThreadError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

var errCodexRuntimeGenerationStale = errors.New("codex runtime generation stale")

func (s *Server) decodeThreadHandle(r *http.Request, value string) (codexHandle, *codexRuntimeEntry, error) {
	principal := s.codexRuntimePrincipal(r)
	handle, err := s.codexRuntimeRegistry.open(value, "thread", principal)
	if err != nil {
		return codexHandle{}, nil, err
	}
	entry, ok := s.runtimeEntryForRequest(r, handle.RuntimeID)
	if !ok {
		return codexHandle{}, nil, errors.New("thread access denied")
	}
	entry.mu.RLock()
	generation := entry.Generation
	entry.mu.RUnlock()
	if generation != handle.Generation {
		return codexHandle{}, nil, errCodexRuntimeGenerationStale
	}
	return handle, entry, nil
}

func (s *Server) adminCodexThreadAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/codex-threads/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	threadHandle, entry, err := s.decodeThreadHandle(r, parts[0])
	if err != nil {
		if errors.Is(err, errCodexRuntimeGenerationStale) {
			writeCodexThreadError(w, http.StatusConflict, "codex_runtime_generation_stale", "runtime generation changed")
			return
		}
		writeCodexThreadError(w, http.StatusNotFound, "codex_thread_access_denied", "thread not found")
		return
	}
	if !entry.available() {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime is unavailable")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		// The HTTP DTO is metadata-only; never ask an adapter to materialize turns
		// or transcript content just to render this control-plane detail view.
		thread, err := entry.Runtime.ReadThread(r.Context(), threadHandle.RuntimeID, threadHandle.ThreadID, false)
		if err != nil {
			writeCodexThreadError(w, http.StatusConflict, "codex_thread_not_loaded", "thread is not loaded")
			return
		}
		view, err := s.runtimeView(s.codexRuntimePrincipal(r), entry, thread)
		if err != nil {
			writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime handle unavailable")
			return
		}
		writeJSON(w, http.StatusOK, view)
	case len(parts) == 2 && parts[1] == "resume" && r.Method == http.MethodPost:
		thread, err := entry.Runtime.ResumeThread(r.Context(), threadHandle.RuntimeID, ThreadResumeParams{ThreadID: threadHandle.ThreadID})
		if err != nil {
			writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime is unavailable")
			return
		}
		view, err := s.runtimeView(s.codexRuntimePrincipal(r), entry, thread)
		if err != nil {
			writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime handle unavailable")
			return
		}
		s.enqueueAudit(codexThreadAudit("codex_thread_resumed", "success"))
		writeJSON(w, http.StatusOK, view)
	case len(parts) == 4 && parts[1] == "turns" && parts[3] == "interrupt" && r.Method == http.MethodPost:
		s.adminCodexInterrupt(w, r, entry, threadHandle, parts[2])
	default:
		http.NotFound(w, r)
	}
}

func codexThreadAudit(action, state string) storage.AuditLogRow {
	return storage.AuditLogRow{Action: action, State: state, Reason: "app_server_thread", Detail: "metadata_only"}
}

func (s *Server) adminCodexInterrupt(w http.ResponseWriter, r *http.Request, entry *codexRuntimeEntry, threadHandle codexHandle, opaqueTurnHandle string) {
	principal := s.codexRuntimePrincipal(r)
	turnHandle, err := s.codexRuntimeRegistry.open(opaqueTurnHandle, "turn", principal)
	if err != nil || turnHandle.RuntimeID != threadHandle.RuntimeID || turnHandle.ThreadID != threadHandle.ThreadID || turnHandle.Generation != threadHandle.Generation {
		writeCodexThreadError(w, http.StatusConflict, "codex_stale_turn", "turn is stale")
		return
	}
	thread, err := entry.Runtime.ReadThread(r.Context(), threadHandle.RuntimeID, threadHandle.ThreadID, false)
	if err != nil {
		writeCodexThreadError(w, http.StatusConflict, "codex_thread_not_loaded", "thread is not loaded")
		return
	}
	if strings.TrimSpace(thread.ActiveTurnID) == "" {
		writeCodexThreadError(w, http.StatusConflict, "codex_no_active_turn", "thread has no active turn")
		return
	}
	if thread.ActiveTurnID != turnHandle.TurnID {
		writeCodexThreadError(w, http.StatusConflict, "codex_stale_turn", "turn is stale")
		return
	}
	// Subscribe first. A disconnect or absent TurnAborted notification is never a
	// successful stop, even if the RPC write itself returned 2xx.
	statusCtx, cancel := context.WithTimeout(r.Context(), codexInterruptConfirmationTimeout)
	defer cancel()
	statuses, err := s.codexRuntimeRegistry.Subscribe(statusCtx, threadHandle.RuntimeID)
	if err != nil {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime status is unavailable")
		return
	}
	if err := entry.Runtime.InterruptTurn(statusCtx, threadHandle.RuntimeID, threadHandle.ThreadID, turnHandle.TurnID); err != nil {
		writeCodexThreadError(w, http.StatusConflict, "codex_stale_turn", "turn is stale")
		return
	}
	for {
		select {
		case <-statusCtx.Done():
			writeCodexThreadError(w, http.StatusGatewayTimeout, "codex_interrupt_not_confirmed", "interrupt was not confirmed")
			return
		case status, ok := <-statuses:
			if !ok {
				writeCodexThreadError(w, http.StatusGatewayTimeout, "codex_interrupt_not_confirmed", "interrupt was not confirmed")
				return
			}
			s.codexRuntimeRegistry.updateStatus(threadHandle.RuntimeID, status)
			if status.ThreadID == threadHandle.ThreadID && status.TurnID == turnHandle.TurnID && codexStatusTurnAborted(status.Status) {
				s.enqueueAudit(codexThreadAudit("codex_thread_interrupted", "success"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
}

func codexStatusTurnAborted(status string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(status), "_", "")) {
	case "turnaborted", "aborted":
		return true
	default:
		return false
	}
}

func (s *Server) adminCodexThreadEvents(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	runtimeID := strings.TrimSpace(r.URL.Query().Get("runtime_id"))
	if runtimeID == "" {
		writeCodexThreadError(w, http.StatusBadRequest, "codex_runtime_id_required", "runtime_id is required")
		return
	}
	entry, ok := s.runtimeEntryForRequest(r, runtimeID)
	if !ok {
		writeCodexThreadError(w, http.StatusNotFound, "codex_thread_access_denied", "thread not found")
		return
	}
	statuses, err := s.codexRuntimeRegistry.Subscribe(r.Context(), runtimeID)
	if err != nil {
		writeCodexThreadError(w, http.StatusServiceUnavailable, "codex_runtime_unavailable", "Codex runtime is unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeCodexThreadError(w, http.StatusInternalServerError, "codex_sse_unavailable", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	principal := s.codexRuntimePrincipal(r)
	for {
		select {
		case <-r.Context().Done():
			return
		case status, ok := <-statuses:
			if !ok {
				return
			}
			if !s.codexRuntimeRegistry.updateStatus(runtimeID, status) {
				continue
			}
			thread := Thread{ID: status.ThreadID, Status: status.Status, WaitingReason: status.WaitingReason, ActiveTurnID: status.TurnID}
			view, err := s.runtimeView(principal, entry, thread)
			if err != nil {
				return
			}
			payload, _ := json.Marshal(view)
			_, _ = fmt.Fprintf(w, "event: thread/status/changed\ndata: %s\n\n", payload)
			flusher.Flush()
		}
	}
}
