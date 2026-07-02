package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"

	"github.com/gorilla/websocket"
)

func lifecycleTaskCount(t *testing.T, h *testHarness) int {
	t.Helper()
	var count int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM lifecycle_tasks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func insertLifecycleTask(t *testing.T, h *testHarness, id, status, configJSON string) {
	t.Helper()
	if configJSON == "" {
		configJSON = `{"task_type":"register","platform":"chatgpt","target_count":1,"group_name":"cyber","egress_id":"egress_direct","concurrency":1}`
	}
	_, err := h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_tasks(
			id, task_type, platform, status, config_json, target_count, created_at
		) VALUES (?, 'register', 'chatgpt', ?, ?, 1, ?)
	`, id, status, configJSON, storage.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func lifecycleTaskStatus(t *testing.T, h *testHarness, id string) string {
	t.Helper()
	var status string
	if err := h.store.DB().QueryRowContext(context.Background(), `
		SELECT status FROM lifecycle_tasks WHERE id = ?
	`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestLifecycleHandlerRecoversInterruptedTasksOnBoot(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	now := storage.Now()
	for _, row := range []struct {
		id     string
		status string
	}{
		{"task_boot_pending", "pending"},
		{"task_boot_running", "running"},
		{"task_boot_completed", "completed"},
	} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO lifecycle_tasks(
				id, task_type, platform, status, config_json, target_count, created_at
			) VALUES (?, 'register', 'chatgpt', ?, '{}', 1, ?)
		`, row.id, row.status, now); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	NewLifecycleHandlers(store, nil, nil)

	statuses := map[string]string{}
	rows, err := store.DB().QueryContext(ctx, `
		SELECT id, status FROM lifecycle_tasks WHERE id LIKE 'task_boot_%'
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		statuses[id] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if statuses["task_boot_pending"] != "failed" || statuses["task_boot_running"] != "failed" {
		t.Fatalf("interrupted statuses = %#v, want pending/running failed", statuses)
	}
	if statuses["task_boot_completed"] != "completed" {
		t.Fatalf("completed status = %q, want completed", statuses["task_boot_completed"])
	}

	var logCount int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM lifecycle_task_logs
		WHERE task_id IN ('task_boot_pending', 'task_boot_running')
		  AND level = 'error'
		  AND message = 'Task interrupted by server restart'
	`).Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if logCount != 2 {
		t.Fatalf("interrupted task log count = %d, want 2", logCount)
	}
}

func TestAdminLifecycleTasksUseRealStore(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/lifecycle/tasks", `{
		"task_type":"register",
		"platform":"chatgpt",
		"target_count":1,
		"group_name":"cyber",
		"egress_id":"egress_direct",
		"concurrency":1
	}`)
	if code != http.StatusOK {
		t.Fatalf("create lifecycle task = %d: %s", code, raw)
	}
	var created map[string]string
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create response: %v (%s)", err, raw)
	}
	if !strings.HasPrefix(created["task_id"], "task_") {
		t.Fatalf("task_id = %q, want task_*", created["task_id"])
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/lifecycle/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("list lifecycle tasks = %d: %s", code, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode task list: %v (%s)", err, raw)
	}
	if len(rows) != 1 {
		t.Fatalf("task list length = %d, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row["id"] != created["task_id"] || row["task_type"] != "register" || row["group_name"] != "cyber" || row["egress_id"] != "egress_direct" {
		t.Fatalf("unexpected task row: %#v", row)
	}
}

func TestAdminLifecycleServicesReturnsSnapshots(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/lifecycle/services", "")
	if code != http.StatusOK {
		t.Fatalf("lifecycle services = %d: %s", code, raw)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode lifecycle services: %v (%s)", err, raw)
	}
	if body == nil {
		t.Fatalf("lifecycle services decoded nil: %s", raw)
	}
}

func TestAdminLifecycleTasksRejectInvalidReferences(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	cases := []string{
		`{"task_type":"unknown","target_count":1}`,
		`{"task_type":"register","target_count":0}`,
		`{"task_type":"register","target_count":1,"group_name":"missing"}`,
		`{"task_type":"register","target_count":1,"group_name":"cyber","egress_id":"missing"}`,
		`{"task_type":"register","target_count":1,"payment_method":"stripe"}`,
	}
	for _, body := range cases {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/lifecycle/tasks", body)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid lifecycle task %s = %d, want 400: %s", body, code, raw)
		}
	}
	if count := lifecycleTaskCount(t, h); count != 0 {
		t.Fatalf("invalid lifecycle task requests wrote %d rows, want 0", count)
	}
}

func TestAdminLifecycleCancelReportsState(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodDelete, "/admin/lifecycle/tasks/missing-task", "")
	if code != http.StatusNotFound {
		t.Fatalf("cancel missing task = %d, want 404: %s", code, raw)
	}

	insertLifecycleTask(t, h, "task_cancel_pending", "pending", "")
	code, raw = grpReq(t, h, http.MethodDelete, "/admin/lifecycle/tasks/task_cancel_pending", "")
	if code != http.StatusOK {
		t.Fatalf("cancel pending task = %d, want 200: %s", code, raw)
	}
	if status := lifecycleTaskStatus(t, h, "task_cancel_pending"); status != "cancelled" {
		t.Fatalf("cancelled task status = %q, want cancelled", status)
	}

	code, raw = grpReq(t, h, http.MethodDelete, "/admin/lifecycle/tasks/task_cancel_pending", "")
	if code != http.StatusConflict {
		t.Fatalf("cancel already-cancelled task = %d, want 409: %s", code, raw)
	}
}

func TestAdminLifecycleCancelSignalsRunningTask(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	taskID := "task_running_signal"
	insertLifecycleTask(t, h, taskID, "running", "")

	ctx, cancel := context.WithCancel(context.Background())
	signalled := make(chan struct{})
	h.app.lifecycleHandler.taskMu.Lock()
	h.app.lifecycleHandler.taskCancels[taskID] = &lifecycleTaskCancel{
		cancel: func() {
			cancel()
			close(signalled)
		},
	}
	h.app.lifecycleHandler.taskMu.Unlock()

	code, raw := grpReq(t, h, http.MethodDelete, "/admin/lifecycle/tasks/"+taskID, "")
	if code != http.StatusOK {
		t.Fatalf("cancel running task = %d, want 200: %s", code, raw)
	}

	select {
	case <-signalled:
	case <-time.After(time.Second):
		t.Fatal("running task cancellation was not signalled")
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("running task context err = %v, want context.Canceled", err)
	}
	if status := lifecycleTaskStatus(t, h, taskID); status != "cancelled" {
		t.Fatalf("cancelled running task status = %q, want cancelled", status)
	}
}

func TestAdminLifecycleTaskLookupErrorsAreSpecific(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/lifecycle/tasks/missing-task", "")
	if code != http.StatusNotFound {
		t.Fatalf("get missing task = %d, want 404: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/lifecycle/tasks/missing-task/logs", "")
	if code != http.StatusNotFound {
		t.Fatalf("get missing task logs = %d, want 404: %s", code, raw)
	}

	insertLifecycleTask(t, h, "task_bad_config", "pending", "{")
	code, raw = grpReq(t, h, http.MethodGet, "/admin/lifecycle/tasks/task_bad_config", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("get task with invalid config = %d, want 500: %s", code, raw)
	}
}

func TestAdminLifecycleTaskLogsUsesIDCursor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	taskID := "task_logs_cursor"
	insertLifecycleTask(t, h, taskID, "pending", "")

	ts := storage.Now()
	oldResult, err := h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
		VALUES (?, 1, 'info', 'old same second', ?)
	`, taskID, ts)
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := oldResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
		VALUES (?, 1, 'info', 'new same second', ?)
	`, taskID, ts); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/lifecycle/tasks/"+taskID+"/logs?since_id="+strconv.FormatInt(oldID, 10), "")
	if code != http.StatusOK {
		t.Fatalf("task logs cursor = %d, want 200: %s", code, raw)
	}
	var logs []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &logs); err != nil {
		t.Fatalf("decode task logs: %v\n%s", err, raw)
	}
	if len(logs) != 1 || logs[0]["message"] != "new same second" {
		t.Fatalf("task logs cursor = %#v, want only new same-second log", logs)
	}
}

func TestAdminLifecycleTaskLogStreamGuards(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/admin/lifecycle/tasks/missing-task/stream"

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("stream missing task unexpectedly upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		if resp == nil {
			t.Fatalf("stream missing task response is nil, err=%v", err)
		}
		t.Fatalf("stream missing task = %d, want 404", resp.StatusCode)
	}

	insertLifecycleTask(t, h, "task_stream", "pending", "")
	wsURL = "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/admin/lifecycle/tasks/task_stream/stream"
	header := http.Header{"Origin": []string{"http://evil.example"}}
	_, resp, err = websocket.DefaultDialer.Dial(wsURL, header)
	if err == nil {
		t.Fatal("cross-origin lifecycle stream unexpectedly upgraded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		if resp == nil {
			t.Fatalf("cross-origin stream response is nil, err=%v", err)
		}
		t.Fatalf("cross-origin stream = %d, want 403", resp.StatusCode)
	}

	_, err = h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
		VALUES ('task_stream', 1, 'info', 'hello', ?)
	`, storage.Now())
	if err != nil {
		t.Fatal(err)
	}

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("same-origin stream = %d: %v", resp.StatusCode, err)
		}
		t.Fatalf("same-origin stream dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	var logs []map[string]interface{}
	if err := conn.ReadJSON(&logs); err != nil {
		t.Fatalf("read stream logs: %v", err)
	}
	if len(logs) != 1 || logs[0]["message"] != "hello" {
		t.Fatalf("stream logs = %#v, want one hello log", logs)
	}
}

func TestAdminLifecycleTaskLogStreamUsesIDCursor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	taskID := "task_stream_cursor"
	insertLifecycleTask(t, h, taskID, "pending", "")

	ts := storage.Now()
	oldResult, err := h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
		VALUES (?, 1, 'info', 'old same second', ?)
	`, taskID, ts)
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := oldResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `
		INSERT INTO lifecycle_task_logs(task_id, account_index, level, message, timestamp)
		VALUES (?, 1, 'info', 'new same second', ?)
	`, taskID, ts); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/admin/lifecycle/tasks/" + taskID + "/stream?since_id=" + strconv.FormatInt(oldID, 10)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("same-origin stream = %d: %v", resp.StatusCode, err)
		}
		t.Fatalf("same-origin stream dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	var logs []map[string]interface{}
	if err := conn.ReadJSON(&logs); err != nil {
		t.Fatalf("read stream logs: %v", err)
	}
	if len(logs) != 1 || logs[0]["message"] != "new same second" {
		t.Fatalf("stream logs = %#v, want only new same-second log", logs)
	}
}
