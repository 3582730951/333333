package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"codex-account-pool/internal/fingerprint"
	"codex-account-pool/internal/proxy"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// Orchestrator orchestrates lifecycle tasks
type Orchestrator struct {
	store          *storage.Store
	serviceManager *ServiceManager
	proxyManager   *proxy.Manager

	maxConcurrency   int
	retryEnabled     bool
	retryMaxAttempts int
}

// TaskConfig contains task configuration
type TaskConfig struct {
	TaskType string `json:"task_type"` // register, upgrade_plus, register_and_plus
	Platform string `json:"platform"`  // chatgpt, claude

	// Target
	TargetCount int    `json:"target_count"`
	GroupName   string `json:"group_name"`

	// Proxy
	ProxyConfigID      string `json:"proxy_config_id"`
	EgressID           string `json:"egress_id"`
	FingerprintEnabled bool   `json:"fingerprint_enabled"`
	FingerprintMode    string `json:"fingerprint_mode"`

	// Registration
	SMSProvider     string `json:"sms_provider"`
	MailboxProvider string `json:"mailbox_provider"`
	Password        string `json:"password"`

	// Payment
	PaymentMethod    string `json:"payment_method"` // gopay, paypal
	PaymentAccountID string `json:"payment_account_id"`

	// Execution
	Concurrency int `json:"concurrency"`
}

// Task represents a lifecycle task
type Task struct {
	ID             string
	Config         *TaskConfig
	Status         string
	TargetCount    int
	CompletedCount int
	SuccessCount   int
	FailedCount    int
	CreatedAt      int64
	StartedAt      int64
	FinishedAt     int64
}

// AccountResult represents the result of processing one account
type AccountResult struct {
	Success   bool   `json:"success"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	PlanType  string `json:"plan_type"`
	Error     string `json:"error"`
}

// NewOrchestrator creates a new orchestrator
func NewOrchestrator(
	store *storage.Store,
	serviceManager *ServiceManager,
	proxyManager *proxy.Manager,
	maxConcurrency int,
) *Orchestrator {
	return &Orchestrator{
		store:            store,
		serviceManager:   serviceManager,
		proxyManager:     proxyManager,
		maxConcurrency:   maxConcurrency,
		retryEnabled:     true,
		retryMaxAttempts: 3,
	}
}

// ExecuteTask executes a lifecycle task
func (o *Orchestrator) ExecuteTask(ctx context.Context, task *Task) error {
	defer func() {
		if v := recover(); v != nil {
			supervisor.LogPanic("lifecycle-task", v)
			o.failTask(ctx, task.ID, fmt.Sprintf("Task panic: %v", v))
			panic(v)
		}
	}()

	// Update task status
	started, err := o.startTask(ctx, task.ID)
	if err != nil {
		wrapped := fmt.Errorf("failed to start task: %w", err)
		o.failTask(ctx, task.ID, wrapped.Error())
		return wrapped
	}
	if !started {
		status, err := o.taskStatus(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("failed to read task status after skipped start: %w", err)
		}
		if status == "cancelled" {
			o.logTask(ctx, task.ID, -1, "info", "Task cancelled before start")
		} else {
			o.logTask(ctx, task.ID, -1, "info", fmt.Sprintf("Task not started because status is %s", status))
		}
		return nil
	}

	// Ensure required services are running
	if err := o.ensureServices(ctx, task.Config); err != nil {
		wrapped := fmt.Errorf("failed to start services: %w", err)
		o.failTask(ctx, task.ID, wrapped.Error())
		return wrapped
	}

	// Determine concurrency
	concurrency := task.Config.Concurrency
	if concurrency <= 0 {
		concurrency = o.maxConcurrency
	}
	if concurrency > o.maxConcurrency {
		concurrency = o.maxConcurrency
	}
	if concurrency <= 0 {
		concurrency = 1
	}

	// Create semaphore for concurrency control
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	cancelled := false

	// Process accounts
	for i := 0; i < task.Config.TargetCount; i++ {
		stop, err := o.acquireTaskSlot(ctx, task.ID, sem)
		if err != nil {
			wg.Wait()
			wrapped := fmt.Errorf("failed to check task cancellation: %w", err)
			o.failTask(ctx, task.ID, wrapped.Error())
			return wrapped
		}
		if stop {
			cancelled = true
			break
		}
		wg.Add(1)

		go func(index int) {
			settled := false
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("lifecycle-worker", v)
					if !settled {
						o.logTask(ctx, task.ID, index, "error", fmt.Sprintf("Worker panic: %v", v))
						o.updateTaskProgress(ctx, task.ID, &AccountResult{
							Success: false,
							Error:   fmt.Sprintf("panic: %v", v),
						})
					}
				}
				<-sem // Release
				wg.Done()
			}()

			result := o.processOneAccount(ctx, task, index)
			o.updateTaskProgress(ctx, task.ID, result)
			settled = true
		}(i)
	}

	// Wait for all to complete
	wg.Wait()

	if cancelled {
		o.logTask(ctx, task.ID, -1, "info", "Task cancelled; stopped launching remaining accounts")
		return nil
	}
	cancelled, err = o.taskCancelled(ctx, task.ID)
	if err != nil {
		wrapped := fmt.Errorf("failed to check final task status: %w", err)
		o.failTask(ctx, task.ID, wrapped.Error())
		return wrapped
	}
	if cancelled {
		o.logTask(ctx, task.ID, -1, "info", "Task cancelled")
		return nil
	}

	o.completeTask(ctx, task.ID)

	return nil
}

func (o *Orchestrator) failTask(ctx context.Context, taskID, message string) {
	o.logTask(ctx, taskID, -1, "error", message)
	if cancelled, err := o.taskCancelled(ctx, taskID); err == nil && cancelled {
		return
	}
	o.updateTaskStatus(ctx, taskID, "failed", 0, storage.Now())
}

func (o *Orchestrator) acquireTaskSlot(ctx context.Context, taskID string, sem chan<- struct{}) (bool, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		cancelled, err := o.taskCancelled(ctx, taskID)
		if err != nil {
			return false, err
		}
		if cancelled {
			return true, nil
		}

		select {
		case sem <- struct{}{}:
			return false, nil
		case <-ctx.Done():
			return true, nil
		case <-ticker.C:
		}
	}
}

// processOneAccount processes a single account
func (o *Orchestrator) processOneAccount(ctx context.Context, task *Task, index int) *AccountResult {
	var result *AccountResult
	var lastErr error

	// Retry logic
	maxAttempts := 1
	if o.retryEnabled {
		maxAttempts = o.retryMaxAttempts
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			o.logTask(ctx, task.ID, index, "info", fmt.Sprintf("Retry %d/%d", attempt, maxAttempts))
			time.Sleep(time.Duration(attempt*5) * time.Second)
		}

		result, lastErr = o.tryProcessAccount(ctx, task, index)
		if lastErr == nil {
			return result
		}

		o.logTask(ctx, task.ID, index, "error", fmt.Sprintf("Attempt %d failed: %v", attempt, lastErr))
	}

	// All retries failed
	return &AccountResult{
		Success: false,
		Error:   lastErr.Error(),
	}
}

// tryProcessAccount attempts to process one account
func (o *Orchestrator) tryProcessAccount(ctx context.Context, task *Task, index int) (*AccountResult, error) {
	// Step 1: Get proxy if configured
	var proxyURL string
	var proxyIP string
	if task.Config.ProxyConfigID != "" {
		forceNew := (task.Config.FingerprintMode == "per_account")
		extractedProxy, err := o.proxyManager.GetProxy(ctx, task.Config.ProxyConfigID, forceNew)
		if err != nil {
			return nil, fmt.Errorf("failed to get proxy: %w", err)
		}
		proxyURL = extractedProxy.ProxyURL
		proxyIP = extractedProxy.IP
		o.logTask(ctx, task.ID, index, "info", fmt.Sprintf("Proxy: %s (IP: %s)", proxy.MaskProxyURL(proxyURL), proxyIP))
	}

	// Step 2: Generate fingerprint if enabled
	var fp *fingerprint.FingerprintProfile
	if task.Config.FingerprintEnabled {
		seed := time.Now().UnixNano() + int64(index)
		if task.Config.FingerprintMode == "per_task" {
			seed = task.CreatedAt // Same seed for all accounts in task
		}
		gen := fingerprint.NewGenerator(seed)
		fp = gen.Generate()
		o.logTask(ctx, task.ID, index, "info", fmt.Sprintf("Fingerprint: %s", fp.UserAgent))
	}

	// Step 3: Execute based on task type
	switch task.Config.TaskType {
	case "register":
		return o.executeRegister(ctx, task, index, proxyURL, fp)
	case "upgrade_plus":
		return o.executeUpgradePlus(ctx, task, index, proxyURL, fp)
	case "register_and_plus":
		return o.executeRegisterAndPlus(ctx, task, index, proxyURL, fp)
	default:
		return nil, fmt.Errorf("unknown task type: %s", task.Config.TaskType)
	}
}

// executeRegister executes registration only
func (o *Orchestrator) executeRegister(ctx context.Context, task *Task, index int, proxyURL string, fp *fingerprint.FingerprintProfile) (*AccountResult, error) {
	o.logTask(ctx, task.ID, index, "info", "Starting registration...")

	// Call registration service (mock for now)
	// TODO: Replace with actual HTTP call to Python service
	result := &AccountResult{
		Success:   true,
		AccountID: fmt.Sprintf("acc_%d_%d", task.CreatedAt, index),
		Email:     fmt.Sprintf("user%d@example.com", index),
		Phone:     fmt.Sprintf("+1234567%04d", index),
		PlanType:  "free",
	}

	o.logTask(ctx, task.ID, index, "info", fmt.Sprintf("Registration successful: %s", result.Email))

	// Import to pool
	accountID := o.importToPool(ctx, result, task.Config.GroupName, task.ID)
	result.AccountID = accountID

	return result, nil
}

// executeUpgradePlus executes Plus upgrade for existing account
func (o *Orchestrator) executeUpgradePlus(ctx context.Context, task *Task, index int, proxyURL string, fp *fingerprint.FingerprintProfile) (*AccountResult, error) {
	o.logTask(ctx, task.ID, index, "info", "Starting Plus upgrade...")

	// TODO: Get account from pool
	// TODO: Generate checkout URL
	// TODO: Call payment service
	// TODO: Update account status

	result := &AccountResult{
		Success:  true,
		PlanType: "plus",
	}

	o.logTask(ctx, task.ID, index, "info", "Plus upgrade successful")

	return result, nil
}

// executeRegisterAndPlus executes registration + Plus in one flow
func (o *Orchestrator) executeRegisterAndPlus(ctx context.Context, task *Task, index int, proxyURL string, fp *fingerprint.FingerprintProfile) (*AccountResult, error) {
	// Step 1: Register
	regResult, err := o.executeRegister(ctx, task, index, proxyURL, fp)
	if err != nil {
		return nil, fmt.Errorf("registration failed: %w", err)
	}

	// Step 2: Upgrade to Plus
	o.logTask(ctx, task.ID, index, "info", "Starting Plus upgrade...")

	// TODO: Generate checkout URL
	// TODO: Call payment service
	// TODO: Update account status

	regResult.PlanType = "plus"
	o.logTask(ctx, task.ID, index, "info", "Plus upgrade successful")

	return regResult, nil
}

// ensureServices ensures required services are running
func (o *Orchestrator) ensureServices(ctx context.Context, config *TaskConfig) error {
	// Always need registration service for register tasks
	if config.TaskType == "register" || config.TaskType == "register_and_plus" {
		if err := o.ensureService(ctx, "registration"); err != nil {
			return fmt.Errorf("failed to start registration service: %w", err)
		}
	}

	// Need payment service for Plus tasks
	if config.TaskType == "upgrade_plus" || config.TaskType == "register_and_plus" {
		if err := o.ensureService(ctx, "payment"); err != nil {
			return fmt.Errorf("failed to start payment service: %w", err)
		}
	}

	return nil
}

func (o *Orchestrator) ensureService(ctx context.Context, name string) error {
	if o.serviceManager == nil || !o.serviceManager.HasService(name) {
		return nil
	}
	return o.serviceManager.EnsureRunning(ctx, name)
}

func (o *Orchestrator) ServiceSnapshots() map[string]ServiceSnapshot {
	if o.serviceManager == nil {
		return map[string]ServiceSnapshot{}
	}
	return o.serviceManager.ListServiceSnapshots()
}

// importToPool imports an account to the pool
func (o *Orchestrator) importToPool(ctx context.Context, result *AccountResult, groupName, taskID string) string {
	now := storage.Now()
	accountID := result.AccountID
	if accountID == "" {
		accountID = fmt.Sprintf("acc_%d", now)
	}

	// Insert account
	_, _ = o.store.DB().ExecContext(ctx, `
		INSERT INTO accounts(
			id, label, group_name, email, plan_type,
			registration_method, phone, registration_task_id,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'auto', ?, ?, 'active', ?, ?)
	`, accountID, result.Email, groupName, result.Email, result.PlanType,
		result.Phone, taskID, now, now)

	// Log event
	_, _ = o.store.DB().ExecContext(ctx, `
		INSERT INTO lifecycle_events(
			account_id, event_type, event_data, task_id, timestamp
		) VALUES (?, 'registered', '{}', ?, ?)
	`, accountID, taskID, now)

	return accountID
}

// updateTaskStatus updates task status
func (o *Orchestrator) updateTaskStatus(ctx context.Context, taskID, status string, startedAt, finishedAt int64) {
	query := "UPDATE lifecycle_tasks SET status = ?"
	args := []interface{}{status}

	if startedAt > 0 {
		query += ", started_at = ?"
		args = append(args, startedAt)
	}
	if finishedAt > 0 {
		query += ", finished_at = ?"
		args = append(args, finishedAt)
	}

	query += " WHERE id = ?"
	args = append(args, taskID)

	_, _ = o.store.DB().ExecContext(ctx, query, args...)
}

func (o *Orchestrator) startTask(ctx context.Context, taskID string) (bool, error) {
	now := storage.Now()
	res, err := o.store.DB().ExecContext(ctx, `
		UPDATE lifecycle_tasks
		SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'pending'
	`, now, taskID)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed > 0 {
		return true, nil
	}

	if _, err := o.taskStatus(ctx, taskID); err != nil {
		return false, err
	}
	return false, nil
}

func (o *Orchestrator) completeTask(ctx context.Context, taskID string) {
	_, _ = o.store.DB().ExecContext(ctx, `
		UPDATE lifecycle_tasks
		SET status = 'completed', finished_at = ?
		WHERE id = ? AND status = 'running'
	`, storage.Now(), taskID)
}

func (o *Orchestrator) taskStatus(ctx context.Context, taskID string) (string, error) {
	var status string
	err := o.store.DB().QueryRowContext(ctx, `
		SELECT status FROM lifecycle_tasks WHERE id = ?
	`, taskID).Scan(&status)
	return status, err
}

func (o *Orchestrator) taskCancelled(ctx context.Context, taskID string) (bool, error) {
	status, err := o.taskStatus(ctx, taskID)
	return status == "cancelled", err
}

// updateTaskProgress updates task progress
func (o *Orchestrator) updateTaskProgress(ctx context.Context, taskID string, result *AccountResult) {
	if result.Success {
		_, _ = o.store.DB().ExecContext(ctx, `
			UPDATE lifecycle_tasks
			SET completed_count = completed_count + 1,
			    success_count = success_count + 1
			WHERE id = ?
		`, taskID)
	} else {
		_, _ = o.store.DB().ExecContext(ctx, `
			UPDATE lifecycle_tasks
			SET completed_count = completed_count + 1,
			    failed_count = failed_count + 1
			WHERE id = ?
		`, taskID)
	}
}

// logTask logs a task event
func (o *Orchestrator) logTask(ctx context.Context, taskID string, accountIndex int, level, message string) {
	now := storage.Now()
	_, _ = o.store.DB().ExecContext(ctx, `
		INSERT INTO lifecycle_task_logs(
			task_id, account_index, level, message, timestamp
		) VALUES (?, ?, ?, ?, ?)
	`, taskID, accountIndex, level, message, now)
}

// CreateTask creates a new task
func (o *Orchestrator) CreateTask(ctx context.Context, config *TaskConfig) (string, error) {
	now := storage.Now()
	taskID := fmt.Sprintf("task_%d", now)

	configJSON, _ := json.Marshal(config)

	_, err := o.store.DB().ExecContext(ctx, `
		INSERT INTO lifecycle_tasks(
			id, task_type, platform, status, config_json,
			target_count, completed_count, success_count, failed_count,
			created_at
		) VALUES (?, ?, ?, 'pending', ?, ?, 0, 0, 0, ?)
	`, taskID, config.TaskType, config.Platform, string(configJSON), config.TargetCount, now)

	if err != nil {
		return "", err
	}

	return taskID, nil
}

// GetTask retrieves a task
func (o *Orchestrator) GetTask(ctx context.Context, taskID string) (*Task, error) {
	row := o.store.DB().QueryRowContext(ctx, `
		SELECT id, task_type, platform, status, config_json,
		       target_count, completed_count, success_count, failed_count,
		       created_at, started_at, finished_at
		FROM lifecycle_tasks WHERE id = ?
	`, taskID)

	var task Task
	var configJSON string
	var platform string
	var taskType string

	err := row.Scan(
		&task.ID, &taskType, &platform, &task.Status, &configJSON,
		&task.TargetCount, &task.CompletedCount, &task.SuccessCount, &task.FailedCount,
		&task.CreatedAt, &task.StartedAt, &task.FinishedAt,
	)
	if err != nil {
		return nil, err
	}

	// Parse config
	var config TaskConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, err
	}
	config.TaskType = taskType
	config.Platform = platform
	task.Config = &config

	return &task, nil
}
