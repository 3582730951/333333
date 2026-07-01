package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	tasklife "codex-account-pool/internal/lifecycle"
	"codex-account-pool/internal/proxy"
	"codex-account-pool/internal/storage"
)

func newServerLifecycleHandlers(store *storage.Store) *LifecycleHandlers {
	pm := proxy.NewManager(store)
	sm := tasklife.NewServiceManager(tasklife.ServiceManagerConfig{AutoStart: true})
	orch := tasklife.NewOrchestrator(store, sm, pm, 50)
	return NewLifecycleHandlers(store, orch, pm)
}

func (s *Server) lifecycleReady(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminAllowed(w, r) {
		return false
	}
	if s.lifecycleHandler == nil {
		writeError(w, http.StatusNotImplemented, errors.New("lifecycle subsystem is not initialized"))
		return false
	}
	return true
}

func (s *Server) handleLifecycleTasks(w http.ResponseWriter, r *http.Request) {
	if !s.lifecycleReady(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.lifecycleHandler.ListTasks(w, r)
	case http.MethodPost:
		s.lifecycleCreateTask(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleLifecycleServices(w http.ResponseWriter, r *http.Request) {
	if !s.lifecycleReady(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.lifecycleHandler.orchestrator.ServiceSnapshots())
}

func (s *Server) lifecycleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req tasklife.TaskConfig
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.normalizeLifecycleTaskConfig(r.Context(), &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	taskID, err := s.lifecycleHandler.orchestrator.CreateTask(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.lifecycleHandler.startTask(taskID)

	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID, "status": "created"})
}

func (s *Server) normalizeLifecycleTaskConfig(ctx context.Context, req *tasklife.TaskConfig) error {
	req.TaskType = strings.TrimSpace(req.TaskType)
	switch req.TaskType {
	case "register", "upgrade_plus", "register_and_plus":
	default:
		return fmt.Errorf("invalid task_type %q", req.TaskType)
	}

	req.Platform = strings.TrimSpace(req.Platform)
	if req.Platform == "" {
		req.Platform = "chatgpt"
	}
	switch req.Platform {
	case "chatgpt", "claude":
	default:
		return fmt.Errorf("invalid platform %q", req.Platform)
	}

	batchLimit := s.settingInt(ctx, "lifecycle_batch_size", 200)
	if req.TargetCount <= 0 {
		return errors.New("target_count must be > 0")
	}
	if req.TargetCount > batchLimit {
		return fmt.Errorf("target_count must be <= %d", batchLimit)
	}

	concurrencyLimit := s.settingInt(ctx, "lifecycle_concurrency", 10)
	if req.Concurrency <= 0 {
		req.Concurrency = concurrencyLimit
	}
	if req.Concurrency > concurrencyLimit {
		req.Concurrency = concurrencyLimit
	}

	req.GroupName = strings.TrimSpace(req.GroupName)
	if req.GroupName == "" {
		req.GroupName = s.cfg.DefaultGroup
	}
	if _, err := s.store.GetGroup(ctx, req.GroupName); err != nil {
		return fmt.Errorf("group %q not found", req.GroupName)
	}

	req.EgressID = strings.TrimSpace(req.EgressID)
	if req.EgressID == "" {
		req.EgressID = storage.DefaultDirectEgressID
	}
	if _, err := s.store.GetEgressProfile(ctx, req.EgressID); err != nil {
		return fmt.Errorf("egress %q not found", req.EgressID)
	}

	req.SMSProvider = strings.TrimSpace(req.SMSProvider)
	req.MailboxProvider = strings.TrimSpace(req.MailboxProvider)
	req.PaymentMethod = strings.TrimSpace(req.PaymentMethod)
	switch req.PaymentMethod {
	case "", "gopay", "paypal":
	default:
		return fmt.Errorf("invalid payment_method %q", req.PaymentMethod)
	}
	return nil
}

func (s *Server) handleLifecycleTaskAction(w http.ResponseWriter, r *http.Request) {
	if !s.lifecycleReady(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/lifecycle/tasks/"), "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	taskID := parts[0]
	if taskID == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.lifecycleHandler.GetTask(w, r, taskID)
		case http.MethodDelete:
			s.lifecycleHandler.CancelTask(w, r, taskID)
		default:
			methodNotAllowed(w)
		}
		return
	}

	if len(parts) == 2 {
		switch parts[1] {
		case "logs":
			if r.Method != http.MethodGet {
				methodNotAllowed(w)
				return
			}
			s.lifecycleHandler.GetTaskLogs(w, r, taskID)
			return
		case "stream":
			if r.Method != http.MethodGet {
				methodNotAllowed(w)
				return
			}
			s.lifecycleHandler.StreamTaskLogs(w, r, taskID)
			return
		case "cancel":
			if r.Method != http.MethodPost && r.Method != http.MethodDelete {
				methodNotAllowed(w)
				return
			}
			s.lifecycleHandler.CancelTask(w, r, taskID)
			return
		}
	}

	http.NotFound(w, r)
}
