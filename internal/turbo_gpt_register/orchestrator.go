package turbo_gpt_register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/google/uuid"
)

type ImportFunc func(context.Context, Job, TokenData) (string, error)

type Options struct {
	MaxConcurrent int
	PhaseTimeout  time.Duration
	Import        ImportFunc
}

// Orchestrator advances durable jobs through browser phases while preventing
// duplicate concurrent execution of the same job.
type Orchestrator struct {
	repo         Repository
	executor     Executor
	phaseTimeout time.Duration
	importToken  ImportFunc
	sem          chan struct{}
	mu           sync.Mutex
	running      map[string]struct{}
}

func New(repo Repository, executor Executor, opts Options) *Orchestrator {
	if opts.MaxConcurrent < 1 {
		opts.MaxConcurrent = 1
	}
	if opts.PhaseTimeout <= 0 {
		opts.PhaseTimeout = 20 * time.Minute
	}
	return &Orchestrator{
		repo: repo, executor: executor, phaseTimeout: opts.PhaseTimeout,
		importToken: opts.Import, sem: make(chan struct{}, opts.MaxConcurrent),
		running: map[string]struct{}{},
	}
}

func (o *Orchestrator) CreateJob(ctx context.Context, req CreateJobRequest) (Job, error) {
	if o == nil || o.repo == nil {
		return Job{}, errors.New("turbo register orchestrator unavailable")
	}
	configJSON := "{}"
	if req.Config != nil {
		if raw, err := json.Marshal(req.Config); err == nil {
			configJSON = string(raw)
		} else {
			return Job{}, fmt.Errorf("encode job config: %w", err)
		}
	}
	job := Job{
		ID:     "tgr_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Status: StatusPending, Phase: Phase1, FullName: strings.TrimSpace(req.FullName),
		BirthDate: strings.TrimSpace(req.BirthDate), PhoneCountryCode: strings.TrimSpace(req.PhoneCountryCode),
		PhoneCountryDialCode: strings.TrimSpace(req.PhoneCountryDialCode), MailDomain: strings.TrimSpace(req.MailDomain),
		ConfigJSON: configJSON, ResultJSON: "{}", AutoImport: req.AutoImport,
	}
	if err := o.repo.CreateTurboGPTRegisterJob(ctx, job); err != nil {
		return Job{}, err
	}
	return o.repo.GetTurboGPTRegisterJob(ctx, job.ID)
}

func (o *Orchestrator) GetJob(ctx context.Context, id string) (Job, error) {
	return o.repo.GetTurboGPTRegisterJob(ctx, id)
}

func (o *Orchestrator) ListJobs(ctx context.Context, status string, limit int) ([]Job, error) {
	return o.repo.ListTurboGPTRegisterJobs(ctx, status, limit)
}

func (o *Orchestrator) GetToken(ctx context.Context, jobID string) (TokenData, error) {
	return o.repo.GetTurboGPTRegisterToken(ctx, jobID)
}

func (o *Orchestrator) DeleteJob(ctx context.Context, id string) error {
	o.mu.Lock()
	_, running := o.running[id]
	o.mu.Unlock()
	if running {
		return errors.New("job is running")
	}
	return o.repo.DeleteTurboGPTRegisterJob(ctx, id)
}

func (o *Orchestrator) Retry(ctx context.Context, id string) (Job, error) {
	job, err := o.repo.GetTurboGPTRegisterJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if job.Status == StatusCompleted {
		return Job{}, errors.New("completed job cannot be retried")
	}
	job.Status = StatusPending
	job.Error = ""
	if err := o.repo.UpdateTurboGPTRegisterJob(ctx, job); err != nil {
		return Job{}, err
	}
	return job, o.Start(id)
}

// Start schedules exactly one phase and returns immediately.
func (o *Orchestrator) Start(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("job id required")
	}
	o.mu.Lock()
	if _, exists := o.running[id]; exists {
		o.mu.Unlock()
		return errors.New("job is already running")
	}
	o.running[id] = struct{}{}
	o.mu.Unlock()
	go func() {
		defer supervisor.Recover("turbo-gpt-register-job")
		o.sem <- struct{}{}
		defer func() {
			<-o.sem
			o.mu.Lock()
			delete(o.running, id)
			o.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), o.phaseTimeout)
		defer cancel()
		_ = o.RunNext(ctx, id)
	}()
	return nil
}

// RunNext executes one phase synchronously and persists every transition.
func (o *Orchestrator) RunNext(ctx context.Context, id string) error {
	job, err := o.repo.GetTurboGPTRegisterJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == StatusCompleted || job.Phase == PhaseDone {
		return nil
	}
	if job.Phase == "" {
		job.Phase = Phase1
	}
	config, err := o.repo.GetTurboGPTRegisterConfig(ctx)
	if err != nil {
		return err
	}
	job.Status = StatusRunning
	job.Error = ""
	job.Attempts++
	if job.StartedAt == 0 {
		job.StartedAt = storage.Now()
	}
	if err := o.repo.UpdateTurboGPTRegisterJob(ctx, job); err != nil {
		return err
	}
	result, runErr := o.executor.Execute(ctx, job.Phase, ExecutorInput{Job: job, Config: config})
	if runErr != nil {
		job.Status = StatusFailed
		job.Error = runErr.Error()
		if updateErr := o.repo.UpdateTurboGPTRegisterJob(context.Background(), job); updateErr != nil {
			return fmt.Errorf("phase failed: %v; persist failure: %w", runErr, updateErr)
		}
		return runErr
	}
	if err := applyExecutorResult(&job, result); err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		_ = o.repo.UpdateTurboGPTRegisterJob(context.Background(), job)
		return err
	}
	now := storage.Now()
	completedThrough := stringValue(result.Data["completed_through"])
	switch job.Phase {
	case Phase1:
		job.Phase1CompletedAt = now
		if completedThrough == Phase2 {
			job.Phase2CompletedAt = now
			job.Phase = Phase3
		} else {
			job.Phase = Phase15
		}
		job.Status = StatusPending
	case Phase15:
		job.Phase = Phase2
		job.Status = StatusPending
	case Phase2:
		job.Phase2CompletedAt = now
		job.Phase = Phase3
		job.Status = StatusPending
	case Phase3:
		job.Phase3CompletedAt = now
		job.CompletedAt = now
		job.Phase = PhaseDone
		job.Status = StatusCompleted
		token, tokenErr := tokenFromResult(job, result)
		if tokenErr != nil {
			job.Status = StatusFailed
			job.Phase = Phase3
			job.Error = tokenErr.Error()
			_ = o.repo.UpdateTurboGPTRegisterJob(context.Background(), job)
			return tokenErr
		}
		if err := o.repo.UpsertTurboGPTRegisterToken(ctx, token); err != nil {
			return err
		}
		if job.AutoImport && o.importToken != nil {
			accountID, importErr := o.importToken(ctx, job, token)
			if importErr != nil {
				job.Error = "auto import: " + importErr.Error()
			} else {
				job.ImportedAccountID = accountID
			}
		}
	default:
		return fmt.Errorf("unknown registration phase %q", job.Phase)
	}
	return o.repo.UpdateTurboGPTRegisterJob(ctx, job)
}

func (o *Orchestrator) GetConfig(ctx context.Context) (map[string]string, error) {
	return o.repo.GetTurboGPTRegisterConfig(ctx)
}

func (o *Orchestrator) SetConfig(ctx context.Context, values map[string]string) error {
	for key, value := range values {
		if err := o.repo.SetTurboGPTRegisterConfig(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

func applyExecutorResult(job *Job, result ExecutorResult) error {
	raw, err := json.Marshal(result.Data)
	if err != nil {
		return err
	}
	job.ResultJSON = string(raw)
	set := func(key string, dst *string) {
		if value := stringValue(result.Data[key]); value != "" {
			*dst = value
		}
	}
	set("phone", &job.Phone)
	set("email", &job.Email)
	set("password", &job.Password)
	set("full_name", &job.FullName)
	set("birth_date", &job.BirthDate)
	set("phone_country_code", &job.PhoneCountryCode)
	set("phone_country_dial_code", &job.PhoneCountryDialCode)
	set("sms_platform", &job.SMSPlatform)
	set("sms_operator", &job.SMSOperator)
	set("sms_activation_id", &job.SMSActivationID)
	set("mail_domain", &job.MailDomain)
	return nil
}

func tokenFromResult(job Job, result ExecutorResult) (TokenData, error) {
	data := result.Data
	for _, key := range []string{"token", "tokens", "oauth"} {
		if nested, ok := data[key].(map[string]interface{}); ok {
			data = nested
			break
		}
	}
	refreshToken := stringValue(data["refresh_token"])
	if refreshToken == "" {
		return TokenData{}, errors.New("phase3 result missing refresh_token")
	}
	raw, _ := json.Marshal(data)
	return TokenData{
		JobID: job.ID, Email: firstNonEmptyString(stringValue(data["email"]), job.Email),
		AccessToken: stringValue(data["access_token"]), RefreshToken: refreshToken,
		IDToken: stringValue(data["id_token"]), AccountID: stringValue(data["account_id"]),
		ExpiresAt: int64Value(data["expires_at"]), RawJSON: string(raw),
	}, nil
}

func stringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func int64Value(value interface{}) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var _ Repository = (*storage.Store)(nil)
