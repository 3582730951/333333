package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/lifecycle"
	"codex-account-pool/internal/proxy"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"

	"github.com/gorilla/websocket"
)

// LifecycleHandlers handles lifecycle management API endpoints
type LifecycleHandlers struct {
	store        *storage.Store
	orchestrator *lifecycle.Orchestrator
	proxyManager *proxy.Manager
	taskMu       sync.Mutex
	taskCancels  map[string]*lifecycleTaskCancel
}

type lifecycleTaskCancel struct {
	cancel context.CancelFunc
}

type lifecycleTaskLog struct {
	ID           int64  `json:"id"`
	AccountIndex int    `json:"account_index"`
	Level        string `json:"level"`
	Message      string `json:"message"`
	Timestamp    int64  `json:"timestamp"`
}

// NewLifecycleHandlers creates new lifecycle handlers
func NewLifecycleHandlers(
	store *storage.Store,
	orchestrator *lifecycle.Orchestrator,
	proxyManager *proxy.Manager,
) *LifecycleHandlers {
	h := &LifecycleHandlers{
		store:        store,
		orchestrator: orchestrator,
		proxyManager: proxyManager,
		taskCancels:  make(map[string]*lifecycleTaskCancel),
	}
	if err := h.recoverInterruptedTasks(context.Background()); err != nil {
		log.Printf("[LIFECYCLE] recover interrupted tasks failed: %v", err)
	}
	return h
}

// CreateTask handles POST /admin/lifecycle/tasks
func (h *LifecycleHandlers) CreateTask(w http.ResponseWriter, r *http.Request) {
	var req lifecycle.TaskConfig
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Validate
	if req.TaskType == "" {
		writeError(w, http.StatusBadRequest, errors.New("task_type is required"))
		return
	}
	if req.TargetCount <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("target_count must be > 0"))
		return
	}

	// Create task
	ctx := r.Context()
	taskID, err := h.orchestrator.CreateTask(ctx, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	h.startTask(taskID)

	writeJSON(w, http.StatusOK, map[string]string{
		"task_id": taskID,
		"status":  "created",
	})
}

func (h *LifecycleHandlers) startTask(taskID string) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := &lifecycleTaskCancel{cancel: cancel}
	h.taskMu.Lock()
	if h.taskCancels == nil {
		h.taskCancels = make(map[string]*lifecycleTaskCancel)
	}
	h.taskCancels[taskID] = entry
	h.taskMu.Unlock()

	go func() {
		defer supervisor.Recover("lifecycle-task")
		defer h.unregisterTaskCancel(taskID, entry)

		task, err := h.orchestrator.GetTask(ctx, taskID)
		if err != nil {
			log.Printf("[LIFECYCLE] task=%s load failed: %v", taskID, err)
			return
		}
		if err := h.orchestrator.ExecuteTask(ctx, task); err != nil {
			log.Printf("[LIFECYCLE] task=%s execute failed: %v", taskID, err)
		}
	}()
}

func (h *LifecycleHandlers) unregisterTaskCancel(taskID string, entry *lifecycleTaskCancel) {
	h.taskMu.Lock()
	defer h.taskMu.Unlock()
	if h.taskCancels[taskID] != entry {
		return
	}
	delete(h.taskCancels, taskID)
}

func (h *LifecycleHandlers) cancelTask(taskID string) bool {
	h.taskMu.Lock()
	entry := h.taskCancels[taskID]
	h.taskMu.Unlock()
	if entry == nil || entry.cancel == nil {
		return false
	}
	entry.cancel()
	return true
}

func (h *LifecycleHandlers) recoverInterruptedTasks(ctx context.Context) error {
	rows, err := h.store.DB().QueryContext(ctx, `
		SELECT id FROM lifecycle_tasks
		WHERE status IN ('pending', 'running')
		ORDER BY created_at ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	taskIDs := make([]string, 0)
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(taskIDs) == 0 {
		return nil
	}

	now := storage.Now()
	recovered := 0
	for _, taskID := range taskIDs {
		res, err := h.store.DB().ExecContext(ctx, `
			UPDATE lifecycle_tasks
			SET status = 'failed', finished_at = ?
			WHERE id = ? AND status IN ('pending', 'running')
		`, now, taskID)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			continue
		}
		recovered++
		_, _ = h.store.DB().ExecContext(ctx, `
			INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
			VALUES (?, -1, 'error', ?, ?)
		`, taskID, "Task interrupted by server restart", now)
	}
	if recovered > 0 {
		log.Printf("[LIFECYCLE] recovered %d interrupted task(s)", recovered)
	}
	return nil
}

// ListTasks handles GET /admin/lifecycle/tasks
func (h *LifecycleHandlers) ListTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.store.DB().QueryContext(ctx, `
		SELECT id, task_type, platform, status, config_json,
		       target_count, completed_count, success_count, failed_count,
		       created_at, started_at, finished_at
		FROM lifecycle_tasks
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	tasks := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, taskType, platform, status, configJSON string
		var targetCount, completedCount, successCount, failedCount int
		var createdAt, startedAt, finishedAt int64

		if err := rows.Scan(&id, &taskType, &platform, &status,
			&configJSON, &targetCount, &completedCount, &successCount, &failedCount,
			&createdAt, &startedAt, &finishedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		var cfg lifecycle.TaskConfig
		if strings.TrimSpace(configJSON) != "" {
			if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}

		tasks = append(tasks, map[string]interface{}{
			"id":              id,
			"task_type":       taskType,
			"platform":        platform,
			"status":          status,
			"target_count":    targetCount,
			"completed_count": completedCount,
			"success_count":   successCount,
			"failed_count":    failedCount,
			"group_name":      cfg.GroupName,
			"egress_id":       cfg.EgressID,
			"concurrency":     cfg.Concurrency,
			"created_at":      createdAt,
			"started_at":      startedAt,
			"finished_at":     finishedAt,
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, tasks)
}

// GetTask handles GET /admin/lifecycle/tasks/:id
func (h *LifecycleHandlers) GetTask(w http.ResponseWriter, r *http.Request, taskID string) {
	ctx := r.Context()
	task, err := h.orchestrator.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, lifecycleTaskNotFoundError(taskID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// GetTaskLogs handles GET /admin/lifecycle/tasks/:id/logs
func (h *LifecycleHandlers) GetTaskLogs(w http.ResponseWriter, r *http.Request, taskID string) {
	ctx := r.Context()

	if _, err := h.lifecycleTaskStatus(ctx, taskID); err != nil {
		h.writeTaskStatusError(w, taskID, err)
		return
	}

	sinceID := lifecycleLogSinceID(r)
	var rows *sql.Rows
	var err error
	if sinceID > 0 {
		rows, err = h.store.DB().QueryContext(ctx, `
			SELECT id, account_index, level, message, timestamp
			FROM lifecycle_task_logs
			WHERE task_id = ? AND id > ?
			ORDER BY id ASC
			LIMIT 1000
		`, taskID, sinceID)
	} else {
		rows, err = h.store.DB().QueryContext(ctx, `
			SELECT id, account_index, level, message, timestamp
			FROM lifecycle_task_logs
			WHERE task_id = ?
			ORDER BY id DESC
			LIMIT 1000
		`, taskID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	logs, _, err := readLifecycleLogRows(rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, logs)
}

// StreamTaskLogs handles WebSocket connection for real-time logs
var upgrader = websocket.Upgrader{
	CheckOrigin: sameOriginWebSocket,
}

func (h *LifecycleHandlers) StreamTaskLogs(w http.ResponseWriter, r *http.Request, taskID string) {
	if _, err := h.lifecycleTaskStatus(r.Context(), taskID); err != nil {
		h.writeTaskStatusError(w, taskID, err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx := r.Context()
	lastID := lifecycleLogSinceID(r)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Poll for new logs
			rows, err := h.store.DB().QueryContext(ctx, `
				SELECT id, account_index, level, message, timestamp
				FROM lifecycle_task_logs
				WHERE task_id = ? AND id > ?
				ORDER BY id ASC
				LIMIT 1000
			`, taskID, lastID)
			if err != nil {
				writeLifecycleStreamError(conn, err)
				return
			}

			newLogs, newestID, err := readLifecycleLogRows(rows)
			if err != nil {
				writeLifecycleStreamError(conn, err)
				return
			}
			if newestID > lastID {
				lastID = newestID
			}

			// Send new logs
			if len(newLogs) > 0 {
				if err := conn.WriteJSON(newLogs); err != nil {
					return
				}
			}
		}
	}
}

// CancelTask handles POST /admin/lifecycle/tasks/:id/cancel
func (h *LifecycleHandlers) CancelTask(w http.ResponseWriter, r *http.Request, taskID string) {
	ctx := r.Context()

	// Update task status to cancelled
	res, err := h.store.DB().ExecContext(ctx, `
		UPDATE lifecycle_tasks
		SET status = 'cancelled', finished_at = ?
		WHERE id = ? AND status IN ('pending', 'running')
	`, storage.Now(), taskID)

	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := res.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed == 0 {
		status, err := h.lifecycleTaskStatus(ctx, taskID)
		if err != nil {
			h.writeTaskStatusError(w, taskID, err)
			return
		}
		writeError(w, http.StatusConflict, fmt.Errorf("lifecycle task %q is %s and cannot be cancelled", taskID, status))
		return
	}

	if h.cancelTask(taskID) {
		log.Printf("[LIFECYCLE] task=%s cancellation signalled", taskID)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"task_id": taskID,
		"status":  "cancelled",
	})
}

func (h *LifecycleHandlers) lifecycleTaskStatus(ctx context.Context, taskID string) (string, error) {
	var status string
	err := h.store.DB().QueryRowContext(ctx, `
		SELECT status FROM lifecycle_tasks WHERE id = ?
	`, taskID).Scan(&status)
	return status, err
}

func (h *LifecycleHandlers) writeTaskStatusError(w http.ResponseWriter, taskID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, lifecycleTaskNotFoundError(taskID))
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func lifecycleTaskNotFoundError(taskID string) error {
	return fmt.Errorf("lifecycle task %q not found", taskID)
}

func readLifecycleLogRows(rows *sql.Rows) ([]lifecycleTaskLog, int64, error) {
	defer rows.Close()

	logs := make([]lifecycleTaskLog, 0)
	var newestID int64
	for rows.Next() {
		var entry lifecycleTaskLog
		if err := rows.Scan(&entry.ID, &entry.AccountIndex, &entry.Level, &entry.Message, &entry.Timestamp); err != nil {
			return nil, newestID, err
		}
		logs = append(logs, entry)
		if entry.ID > newestID {
			newestID = entry.ID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, newestID, err
	}
	return logs, newestID, nil
}

func lifecycleLogSinceID(r *http.Request) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get("since_id"))
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

func writeLifecycleStreamError(conn *websocket.Conn, err error) {
	_ = conn.WriteJSON(map[string]interface{}{
		"error": map[string]interface{}{
			"message": err.Error(),
			"type":    "codex_pool_error",
		},
	})
}

func sameOriginWebSocket(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return equalHost(u.Host, r.Host)
}

func equalHost(a, b string) bool {
	aHost, aPort, aErr := net.SplitHostPort(a)
	bHost, bPort, bErr := net.SplitHostPort(b)
	if aErr == nil && bErr == nil {
		return strings.EqualFold(aHost, bHost) && aPort == bPort
	}
	return strings.EqualFold(a, b)
}
