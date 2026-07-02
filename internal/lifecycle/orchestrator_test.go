package lifecycle

import (
	"context"
	"strings"
	"testing"

	"codex-account-pool/internal/proxy"
	"codex-account-pool/internal/storage"
)

func TestOrchestrator(t *testing.T) {
	// Setup
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create managers
	sm := NewServiceManager(ServiceManagerConfig{AutoStart: false})
	pm := proxy.NewManager(store)
	orch := NewOrchestrator(store, sm, pm, 3)

	// Test 1: Create task
	config := &TaskConfig{
		TaskType:    "register",
		Platform:    "chatgpt",
		TargetCount: 5,
		GroupName:   "test-group",
		Concurrency: 2,
	}

	taskID, err := orch.CreateTask(ctx, config)
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	if taskID == "" {
		t.Error("TaskID is empty")
	}

	t.Logf("✓ Task created: %s", taskID)

	// Test 2: Get task
	task, err := orch.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}

	if task.Config.TargetCount != 5 {
		t.Errorf("Expected TargetCount 5, got %d", task.Config.TargetCount)
	}

	t.Logf("✓ Task retrieved: %+v", task)

	// Test 3: Process one account (mock)
	result := orch.processOneAccount(ctx, task, 0)
	if !result.Success {
		t.Errorf("processOneAccount failed: %s", result.Error)
	}

	t.Logf("✓ Account processed: %s", result.Email)
}

func TestTaskConfig(t *testing.T) {
	config := &TaskConfig{
		TaskType:           "register_and_plus",
		Platform:           "chatgpt",
		TargetCount:        10,
		GroupName:          "premium",
		ProxyConfigID:      "proxy1",
		FingerprintEnabled: true,
		FingerprintMode:    "per_account",
		SMSProvider:        "smsbower",
		MailboxProvider:    "moemail",
		PaymentMethod:      "gopay",
		Concurrency:        3,
	}

	if config.TaskType != "register_and_plus" {
		t.Error("TaskType mismatch")
	}

	if config.TargetCount != 10 {
		t.Error("TargetCount mismatch")
	}

	t.Log("✓ TaskConfig structure validated")
}

func TestAccountResult(t *testing.T) {
	result := &AccountResult{
		Success:   true,
		AccountID: "acc_123",
		Email:     "test@example.com",
		Phone:     "+1234567890",
		PlanType:  "plus",
	}

	if !result.Success {
		t.Error("Result should be successful")
	}

	if result.AccountID == "" {
		t.Error("AccountID is empty")
	}

	t.Log("✓ AccountResult structure validated")
}

func TestOrchestratorConcurrency(t *testing.T) {
	store, _ := storage.OpenInMemory()
	defer store.Close()
	store.Init(context.Background())

	sm := NewServiceManager(ServiceManagerConfig{})
	pm := proxy.NewManager(store)

	// Test with different concurrency limits
	orch1 := NewOrchestrator(store, sm, pm, 1)
	if orch1.maxConcurrency != 1 {
		t.Error("Expected maxConcurrency 1")
	}

	orch5 := NewOrchestrator(store, sm, pm, 5)
	if orch5.maxConcurrency != 5 {
		t.Error("Expected maxConcurrency 5")
	}

	t.Log("✓ Concurrency limits validated")
}

func TestTaskStatusUpdates(t *testing.T) {
	store, _ := storage.OpenInMemory()
	defer store.Close()
	ctx := context.Background()
	store.Init(ctx)

	sm := NewServiceManager(ServiceManagerConfig{})
	pm := proxy.NewManager(store)
	orch := NewOrchestrator(store, sm, pm, 3)

	// Create task
	config := &TaskConfig{
		TaskType:    "register",
		Platform:    "chatgpt",
		TargetCount: 1,
		GroupName:   "test",
	}

	taskID, _ := orch.CreateTask(ctx, config)

	// Update status
	orch.updateTaskStatus(ctx, taskID, "running", storage.Now(), 0)

	// Get task and verify
	task, _ := orch.GetTask(ctx, taskID)
	if task.Status != "running" {
		t.Errorf("Expected status 'running', got '%s'", task.Status)
	}

	t.Log("✓ Task status updates work")
}

func TestExecuteTaskLogsServiceStartupFailure(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("registration", "true", "/path/that/does/not/exist", 0, "")
	pm := proxy.NewManager(store)
	orch := NewOrchestrator(store, sm, pm, 1)

	taskID, err := orch.CreateTask(ctx, &TaskConfig{
		TaskType:    "register",
		Platform:    "chatgpt",
		TargetCount: 1,
		GroupName:   "test",
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task, err := orch.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	err = orch.ExecuteTask(ctx, task)
	if err == nil {
		t.Fatal("ExecuteTask succeeded with broken registration service")
	}
	if !strings.Contains(err.Error(), "failed to start services") {
		t.Fatalf("ExecuteTask error = %v, want service startup failure", err)
	}

	task, err = orch.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after failure: %v", err)
	}
	if task.Status != "failed" {
		t.Fatalf("task status = %q, want failed", task.Status)
	}

	var level, message string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT level, message FROM lifecycle_task_logs
		WHERE task_id = ? AND account_index = -1
		ORDER BY timestamp DESC LIMIT 1
	`, taskID).Scan(&level, &message); err != nil {
		t.Fatalf("read lifecycle failure log: %v", err)
	}
	if level != "error" || !strings.Contains(message, "failed to start services") {
		t.Fatalf("failure log level=%q message=%q", level, message)
	}
}

func TestExecuteTaskDoesNotOverwriteCancelledTask(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sm := NewServiceManager(ServiceManagerConfig{})
	pm := proxy.NewManager(store)
	orch := NewOrchestrator(store, sm, pm, 0)

	taskID, err := orch.CreateTask(ctx, &TaskConfig{
		TaskType:    "register",
		Platform:    "chatgpt",
		TargetCount: 2,
		GroupName:   "test",
		Concurrency: 0,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task, err := orch.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `
		UPDATE lifecycle_tasks SET status = 'cancelled', finished_at = ? WHERE id = ?
	`, storage.Now(), taskID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	if err := orch.ExecuteTask(ctx, task); err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}

	task, err = orch.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after ExecuteTask: %v", err)
	}
	if task.Status != "cancelled" {
		t.Fatalf("task status = %q, want cancelled", task.Status)
	}
	if task.CompletedCount != 0 || task.SuccessCount != 0 || task.FailedCount != 0 {
		t.Fatalf("cancelled task counts changed: completed=%d success=%d failed=%d",
			task.CompletedCount, task.SuccessCount, task.FailedCount)
	}

	var imported int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM accounts WHERE registration_task_id = ?
	`, taskID).Scan(&imported); err != nil {
		t.Fatalf("count imported accounts: %v", err)
	}
	if imported != 0 {
		t.Fatalf("imported accounts = %d, want 0", imported)
	}
}

func BenchmarkProcessOneAccount(b *testing.B) {
	store, _ := storage.OpenInMemory()
	defer store.Close()
	ctx := context.Background()
	store.Init(ctx)

	sm := NewServiceManager(ServiceManagerConfig{})
	pm := proxy.NewManager(store)
	orch := NewOrchestrator(store, sm, pm, 10)

	config := &TaskConfig{
		TaskType:    "register",
		Platform:    "chatgpt",
		TargetCount: 1,
		GroupName:   "bench",
	}

	taskID, _ := orch.CreateTask(ctx, config)
	task, _ := orch.GetTask(ctx, taskID)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orch.processOneAccount(ctx, task, i)
	}
}
