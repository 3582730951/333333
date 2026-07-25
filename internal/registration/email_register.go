package registration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// EmailRegOrchestrator manages email-based registration workflows.
type EmailRegOrchestrator struct {
	store    *storage.Store
	cfg      EmailRegConfig

	mu       sync.Mutex
	jobs     map[string]*EmailRegJob
}

// EmailRegConfig holds configuration for the email registration orchestrator.
type EmailRegConfig struct {
	Enabled       bool
	Concurrency   int
	TimeoutSecs   int
	DefaultGroup  string
	EgressPoolID  string
}

// EmailRegJob represents a running or completed email registration job.
type EmailRegJob struct {
	ID          string
	Total       int
	Succeeded   int
	Failed      int
	Status      string
	GroupName   string
	EgressPoolID string
	Error       string
	CreatedAt   int64
	cancel      context.CancelFunc
}

// NewEmailRegOrchestrator creates a new orchestrator instance.
func NewEmailRegOrchestrator(store *storage.Store, cfg EmailRegConfig) *EmailRegOrchestrator {
	return &EmailRegOrchestrator{
		store: store,
		cfg:   cfg,
		jobs:  make(map[string]*EmailRegJob),
	}
}

// StartJob begins a batch email registration job.
func (o *EmailRegOrchestrator) StartJob(ctx context.Context, count int, groupName, egressPoolID string) (*EmailRegJob, error) {
	if count < 1 {
		count = 1
	}
	if groupName == "" {
		groupName = o.cfg.DefaultGroup
	}

	now := storage.Now()
	jobID := fmt.Sprintf("emailreg_%d", now)

	// Insert job into database
	configJSON, _ := json.Marshal(map[string]interface{}{
		"group_name":       groupName,
		"egress_pool_id":   egressPoolID,
		"concurrency":      o.cfg.Concurrency,
	})
	_, err := o.store.DB().ExecContext(ctx,
		`INSERT INTO registration_jobs(id, platform, method, total, succeeded, failed, status, config_json, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		jobID, "codex", "email", count, 0, 0, "running", string(configJSON), now, now)
	if err != nil {
		return nil, fmt.Errorf("insert job: %w", err)
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	job := &EmailRegJob{
		ID:           jobID,
		Total:        count,
		Status:       "running",
		GroupName:    groupName,
		EgressPoolID: egressPoolID,
		CreatedAt:    now,
		cancel:       cancel,
	}

	o.mu.Lock()
	o.jobs[jobID] = job
	o.mu.Unlock()

	// Start worker goroutines
	go o.runJob(jobCtx, job)

	return job, nil
}

// CancelJob cancels a running job.
func (o *EmailRegOrchestrator) CancelJob(jobID string) error {
	o.mu.Lock()
	job, ok := o.jobs[jobID]
	o.mu.Unlock()
	if !ok || job.Status != "running" {
		return fmt.Errorf("job not found or not running")
	}
	job.cancel()
	job.Status = "cancelled"
	now := storage.Now()
	_, _ = o.store.DB().ExecContext(context.Background(),
		`UPDATE registration_jobs SET status = 'cancelled', updated_at = ? WHERE id = ?`, now, jobID)
	return nil
}

// runJob is the main worker loop for a registration job.
func (o *EmailRegOrchestrator) runJob(ctx context.Context, job *EmailRegJob) {
	defer supervisor.Recover("registration.email_run_job")
	defer func() {
		o.mu.Lock()
		delete(o.jobs, job.ID)
		o.mu.Unlock()
	}()

	sem := make(chan struct{}, o.cfg.Concurrency)
	var wg sync.WaitGroup

	for i := 0; i < job.Total; i++ {
		select {
		case <-ctx.Done():
			// Job cancelled
			wg.Wait()
			o.finalizeJob(ctx, job)
			return
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int) {
			defer supervisor.Recover("registration.email_register_one")
			defer wg.Done()
			defer func() { <-sem }()
			o.registerOne(ctx, job)
		}(i)
	}

	wg.Wait()
	o.finalizeJob(ctx, job)
}

// registerOne executes a single registration: reserve email → get OTP → register → import.
func (o *EmailRegOrchestrator) registerOne(ctx context.Context, job *EmailRegJob) {
	// 1. Reserve an email account from the pool
	emailAcct, err := o.store.ReserveEmailAccount(ctx, "")
	if err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("Failed to reserve email: %v", err))
		o.recordFailure(job)
		return
	}
	defer func() {
		status := "used"
		errMsg := ""
		if job.Status == "running" {
			// If still running, release as idle for retry
			status = "idle"
		}
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, status, errMsg)
	}()

	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Reserved email: %s", emailAcct.Email))

	// 2. Get IMAP access token
	timeout := time.Duration(o.cfg.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	accessToken, err := GetIMAPAccessToken(ctx, "", emailAcct.ClientID, emailAcct.RefreshToken)
	if err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("Microsoft token error: %v", err))
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "error", err.Error())
		o.recordFailure(job)
		return
	}

	// 3. Keygen flow: authorize → continue (prepares session, no OTP yet)
	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 1/6: Authorize + continue for %s", emailAcct.Email))
	proxyURL := "http://zdvw1182255-region-JP-sid-jR5m3s9C-t-5:d6kfytmo@us.arxlabs.io:3010"
	sidecar := NewSidecarHTTPClient("").SetProxy(proxyURL).SetCookieJarKey(emailAcct.Email)
	password := generatePassword()

	session, err := InitiateSignup(ctx, sidecar, emailAcct.Email, password, proxyURL)
	if err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("authorize+continue failed: %v", err))
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "error", err.Error())
		o.recordFailure(job)
		return
	}

	// 4. Register + Send OTP (this triggers the OTP email)
	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 2/6: Register + Send OTP for %s", emailAcct.Email))
	if err := RegisterAndSendOTP(ctx, session); err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("register+send OTP failed: %v", err))
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "error", err.Error())
		o.recordFailure(job)
		return
	}

	// 5. Poll IMAP for OTP
	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 3/6: Waiting for OTP for %s", emailAcct.Email))
	otpResult, err := WaitForOTP(ctx, accessToken, emailAcct.Email, nil, timeout)
	if err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("OTP wait timeout/error: %v", err))
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "error", err.Error())
		o.recordFailure(job)
		return
	}
	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 4/6: Got OTP for %s", emailAcct.Email))

	// 6. Complete registration: validate OTP + create account + get session
	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 5/6: Complete registration"))
	result, err := CompleteRegistration(ctx, session, otpResult.Code)
	if err != nil {
		o.logEvent(ctx, job.ID, "error", fmt.Sprintf("Complete registration failed: %v", err))
		_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "error", err.Error())
		o.recordFailure(job)
		return
	}

	o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Step 6/6: Registration success! account_id=%s plan=%s", result.AccountID, result.PlanType))

	// 6. Convert to Agent Identity
	httpClient := &http.Client{Timeout: 30 * time.Second}
	creds, err := ConvertToAgentIdentity(ctx, httpClient, result.AccessToken)
	if err != nil {
		o.logEvent(ctx, job.ID, "warn", fmt.Sprintf("Agent Identity registration failed (falling back to OAuth): %v", err))
		// Fall back to OAuth import
		account := &storage.Account{
			ID:                result.AccountID,
			Label:             result.Email,
			GroupName:         job.GroupName,
			UpstreamAccountID: result.AccountID,
			Email:             result.Email,
			PlanType:          result.PlanType,
			Provider:          "codex",
			Status:            "active",
			CreatedAt:         storage.Now(),
			UpdatedAt:         storage.Now(),
		}
		token := &storage.AccountToken{
			AccountID:    result.AccountID,
			AccessToken:  result.AccessToken,
			RefreshToken: result.SessionToken,
			LastRefresh:  storage.Now(),
			Scopes:       "openid email profile offline_access",
		}
		if err := o.store.UpsertAccount(ctx, *account, *token); err != nil {
			o.logEvent(ctx, job.ID, "error", fmt.Sprintf("Failed to save account: %v", err))
			o.recordFailure(job)
			return
		}
		o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Account saved (OAuth fallback): %s", account.ID))
	} else {
		// 7. Build account with Agent Identity and save
		account, token := BuildAccountFromResult(result, creds, job.GroupName)
		if err := o.store.UpsertAccount(ctx, *account, *token); err != nil {
			o.logEvent(ctx, job.ID, "error", fmt.Sprintf("Failed to save account: %v", err))
			o.recordFailure(job)
			return
		}
		o.logEvent(ctx, job.ID, "info", fmt.Sprintf("Account saved (Agent Identity): %s", account.ID))

		// Also export sub2api JSON for reference
		if sub2apiJSON, err := BuildSub2APIExport(result, creds); err == nil {
			o.logEvent(ctx, job.ID, "info", string(sub2apiJSON[:min(200, len(sub2apiJSON))]))
		}
	}

	_ = o.store.ReleaseEmailAccount(ctx, emailAcct.ID, "used", "")
	o.recordSuccess(job)
	log.Printf("[email-reg] Successfully registered %s (account_id=%s)", result.Email, result.AccountID)
}

func (o *EmailRegOrchestrator) recordSuccess(job *EmailRegJob) {
	o.mu.Lock()
	job.Succeeded++
	o.mu.Unlock()
	o.updateJobDB(job)
}

func (o *EmailRegOrchestrator) recordFailure(job *EmailRegJob) {
	o.mu.Lock()
	job.Failed++
	o.mu.Unlock()
	o.updateJobDB(job)
}

func (o *EmailRegOrchestrator) updateJobDB(job *EmailRegJob) {
	now := storage.Now()
	_, _ = o.store.DB().ExecContext(context.Background(),
		`UPDATE registration_jobs SET succeeded = ?, failed = ?, updated_at = ? WHERE id = ?`,
		job.Succeeded, job.Failed, now, job.ID)
}

func (o *EmailRegOrchestrator) finalizeJob(ctx context.Context, job *EmailRegJob) {
	o.mu.Lock()
	if job.Status == "running" {
		if job.Failed > 0 && job.Succeeded == 0 {
			job.Status = "failed"
		} else {
			job.Status = "completed"
		}
	}
	status := job.Status
	o.mu.Unlock()

	now := storage.Now()
	var errMsg string
	if status == "failed" {
		errMsg = "All registration attempts failed"
	}
	_, _ = o.store.DB().ExecContext(ctx,
		`UPDATE registration_jobs SET status = ?, completed_at = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, now, errMsg, now, job.ID)

	log.Printf("[email-reg] Job %s finished: status=%s succeeded=%d failed=%d", job.ID, status, job.Succeeded, job.Failed)
}

func (o *EmailRegOrchestrator) logEvent(ctx context.Context, jobID, level, message string) {
	now := storage.Now()
	detailJSON, _ := json.Marshal(map[string]string{"message": message})
	_, err := o.store.DB().ExecContext(ctx,
		`INSERT INTO registration_task_events(task_id, level, message, detail_json, created_at) VALUES(?,?,?,?,?)`,
		jobID, level, message, string(detailJSON), now)
	if err != nil {
		log.Printf("[email-reg] Failed to log event: %v", err)
	}
}

// GetJob returns a job by ID.
func (o *EmailRegOrchestrator) GetJob(jobID string) *EmailRegJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.jobs[jobID]
}

func generatePassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
	b := make([]byte, 16)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}
