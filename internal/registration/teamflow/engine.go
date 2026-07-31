// Package teamflow provides a durable, connector-neutral member lifecycle.
// External systems are represented by narrow idempotent ports; workflow rows
// contain opaque references and never access tokens, phone numbers, or passwords.
package teamflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

type Repository interface {
	ClaimTeamLifecycleWorkflow(context.Context, string, int64, int64) (storage.TeamLifecycleWorkflow, bool, error)
	RenewTeamLifecycleLease(context.Context, string, int64, string, int64) error
	TransitionTeamLifecycleWorkflow(context.Context, string, int64, string, storage.TeamLifecycleUpdate) (storage.TeamLifecycleWorkflow, error)
}

type Operation struct {
	Workflow     storage.TeamLifecycleWorkflow
	OperationKey string
}

type CredentialResolution struct {
	Available     bool
	CredentialRef string
}

type OAuthResult struct {
	CredentialRef     string
	PhoneRequired     bool
	PhoneChallengeRef string
}

// Adapter methods must honor OperationKey as an idempotency key. Retrying the
// same operation returns the original result instead of repeating a remote action.
type Adapter interface {
	Invite(context.Context, Operation) (membershipRef string, err error)
	ResolveCredential(context.Context, Operation) (CredentialResolution, error)
	LoginWithCredential(context.Context, Operation) (credentialRef string, err error)
	OAuthLogin(context.Context, Operation) (OAuthResult, error)
	VerifyPhone(context.Context, Operation) (credentialRef string, err error)
	ImportAccount(context.Context, Operation) (accountID string, err error)
	ObserveQuota(context.Context, Operation) (remainingBasisPoints int, err error)
	RemoveMember(context.Context, Operation) error
	EnqueueReplacement(context.Context, Operation) (jobRef string, err error)
}

type ClassifiedError struct {
	Class     string
	Retryable bool
	Err       error
}

// OAuthFallbackError tells the durable engine that a stored access credential
// was present but could not establish the requested workspace. This is a
// control-flow outcome rather than a terminal failure: the next checkpoint is
// the OAuth path, which can perform interactive login and conditional phone
// verification.
type OAuthFallbackError struct {
	Err error
}

func (e *OAuthFallbackError) Error() string {
	if e == nil || e.Err == nil {
		return "credential login requires OAuth"
	}
	return e.Err.Error()
}

func (e *OAuthFallbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func FallbackToOAuth(err error) error {
	if err == nil {
		err = errors.New("credential login requires OAuth")
	}
	return &OAuthFallbackError{Err: err}
}

func (e *ClassifiedError) Error() string {
	if e == nil || e.Err == nil {
		return "team lifecycle connector error"
	}
	return e.Err.Error()
}

func (e *ClassifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func Retryable(class string, err error) error {
	return &ClassifiedError{Class: normalizeErrorClass(class), Retryable: true, Err: err}
}

func Permanent(class string, err error) error {
	return &ClassifiedError{Class: normalizeErrorClass(class), Retryable: false, Err: err}
}

func normalizeErrorClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "connector_failure"
	}
	var out strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '_':
			out.WriteRune(char)
		case char == '-' || char == ' ':
			out.WriteByte('_')
		}
		if out.Len() >= 64 {
			break
		}
	}
	if out.Len() == 0 {
		return "connector_failure"
	}
	return out.String()
}

func classifyError(err error) (string, bool) {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return normalizeErrorClass(classified.Class), classified.Retryable
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", true
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	default:
		return "connector_failure", true
	}
}

type Options struct {
	RetryBase         time.Duration
	RetryMax          time.Duration
	QuotaPollInterval time.Duration
	StepTimeout       time.Duration
}

func (o Options) normalized() Options {
	if o.RetryBase <= 0 {
		o.RetryBase = 5 * time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 15 * time.Minute
	}
	if o.RetryMax < o.RetryBase {
		o.RetryMax = o.RetryBase
	}
	if o.QuotaPollInterval <= 0 {
		o.QuotaPollInterval = 15 * time.Minute
	}
	if o.StepTimeout <= 0 {
		o.StepTimeout = 8 * time.Minute
	}
	return o
}

type Engine struct {
	repository Repository
	adapter    Adapter
	options    Options
	now        func() time.Time
}

func NewEngine(repository Repository, adapter Adapter, options Options) *Engine {
	return &Engine{
		repository: repository,
		adapter:    adapter,
		options:    options.normalized(),
		now:        time.Now,
	}
}

func (e *Engine) operation(workflow storage.TeamLifecycleWorkflow) Operation {
	key := workflow.ID + ":" + workflow.State
	if workflow.State == storage.TeamLifecycleActive {
		key = fmt.Sprintf("%s:%s:%d", workflow.ID, workflow.State, workflow.NextAttemptAt)
	}
	return Operation{Workflow: workflow, OperationKey: key}
}

func cleanOpaqueRef(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s reference is empty", name)
	}
	if len(value) > 512 {
		return "", fmt.Errorf("%s reference exceeds 512 bytes", name)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", fmt.Errorf("%s reference contains a control character", name)
		}
	}
	return value, nil
}

func detail(value map[string]interface{}) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func (e *Engine) transition(
	ctx context.Context,
	workflow storage.TeamLifecycleWorkflow,
	owner string,
	update storage.TeamLifecycleUpdate,
) (storage.TeamLifecycleWorkflow, error) {
	return e.repository.TransitionTeamLifecycleWorkflow(
		ctx, workflow.ID, workflow.Version, owner, update,
	)
}

func (e *Engine) success(
	ctx context.Context,
	workflow storage.TeamLifecycleWorkflow,
	owner, toState, eventType string,
	update storage.TeamLifecycleUpdate,
) (storage.TeamLifecycleWorkflow, error) {
	update.ToState = toState
	update.Attempt = 0
	update.ClearError = true
	update.ClearResume = true
	update.EventType = eventType
	return e.transition(ctx, workflow, owner, update)
}

func (e *Engine) retryDelay(attempt int) time.Duration {
	delay := e.options.RetryBase
	for index := 1; index < attempt && delay < e.options.RetryMax; index++ {
		delay *= 2
		if delay >= e.options.RetryMax {
			return e.options.RetryMax
		}
	}
	return delay
}

func (e *Engine) fail(
	ctx context.Context,
	workflow storage.TeamLifecycleWorkflow,
	owner string,
	stepErr error,
) (storage.TeamLifecycleWorkflow, error) {
	class, retryable := classifyError(stepErr)
	attempt := workflow.Attempt + 1
	update := storage.TeamLifecycleUpdate{
		ResumeState:     workflow.State,
		Attempt:         attempt,
		ErrorClass:      class,
		EventDetailJSON: detail(map[string]interface{}{"attempt": attempt, "error_class": class}),
	}
	if !retryable || attempt >= workflow.MaxAttempts {
		update.ToState = storage.TeamLifecycleReviewRequired
		update.EventType = "review_required"
		return e.transition(ctx, workflow, owner, update)
	}
	update.ToState = storage.TeamLifecycleRetryWait
	update.NextAttemptAt = e.now().Add(e.retryDelay(attempt)).Unix()
	update.EventType = "retry_scheduled"
	return e.transition(ctx, workflow, owner, update)
}

func (e *Engine) Advance(
	ctx context.Context,
	workflow storage.TeamLifecycleWorkflow,
	owner string,
) (storage.TeamLifecycleWorkflow, error) {
	if e == nil || e.repository == nil {
		return storage.TeamLifecycleWorkflow{}, errors.New("team lifecycle engine repository is nil")
	}
	if e.adapter == nil {
		return e.fail(ctx, workflow, owner, Permanent("connector_not_configured", errors.New("team lifecycle adapter is nil")))
	}
	if storage.IsTerminalTeamLifecycleState(workflow.State) {
		return workflow, nil
	}
	now := e.now()
	if workflow.State == storage.TeamLifecycleRetryWait {
		resume := workflow.ResumeState
		if !storage.ValidTeamLifecycleState(resume) || storage.IsTerminalTeamLifecycleState(resume) || resume == storage.TeamLifecycleRetryWait {
			resume = storage.TeamLifecycleReviewRequired
		}
		return e.transition(ctx, workflow, owner, storage.TeamLifecycleUpdate{
			ToState:         resume,
			Attempt:         workflow.Attempt,
			ClearError:      true,
			ClearResume:     true,
			EventType:       "retry_resumed",
			EventDetailJSON: detail(map[string]interface{}{"attempt": workflow.Attempt}),
		})
	}
	if workflow.State == storage.TeamLifecycleQueued {
		return e.success(ctx, workflow, owner, storage.TeamLifecycleInviting, "operation_prepared", storage.TeamLifecycleUpdate{
			EventDetailJSON: detail(map[string]interface{}{"operation_key": workflow.ID + ":inviting"}),
		})
	}
	if workflow.ShadowMode {
		return e.transition(ctx, workflow, owner, storage.TeamLifecycleUpdate{
			ToState:     storage.TeamLifecycleReviewRequired,
			ResumeState: workflow.State,
			Attempt:     workflow.Attempt,
			ErrorClass:  "shadow_plan_ready",
			EventType:   "shadow_plan_ready",
			EventDetailJSON: detail(map[string]interface{}{
				"steps": []string{
					"invite", "resolve_credential", "credential_or_oauth_login",
					"conditional_phone_verification", "import", "quota_observe",
					"remove_member", "enqueue_replacement",
				},
				"rotate_threshold_bps": workflow.RotateThresholdBPS,
			}),
		})
	}

	stepCtx, cancel := context.WithTimeout(ctx, e.options.StepTimeout)
	defer cancel()
	op := e.operation(workflow)

	switch workflow.State {
	case storage.TeamLifecycleInviting:
		ref, err := e.adapter.Invite(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		ref, err = cleanOpaqueRef("membership", ref)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleResolvingCredential, "member_invited", storage.TeamLifecycleUpdate{
			MembershipRef:   ref,
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleResolvingCredential:
		result, err := e.adapter.ResolveCredential(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		if !result.Available {
			return e.success(ctx, workflow, owner, storage.TeamLifecycleOAuthLogin, "credential_fallback_selected", storage.TeamLifecycleUpdate{
				CredentialPath: "oauth",
			})
		}
		ref, err := cleanOpaqueRef("credential", result.CredentialRef)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleCredentialLogin, "credential_reference_resolved", storage.TeamLifecycleUpdate{
			CredentialPath:  "access_reference",
			CredentialRef:   ref,
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleCredentialLogin:
		ref, err := e.adapter.LoginWithCredential(stepCtx, op)
		if err != nil {
			var fallback *OAuthFallbackError
			if errors.As(err, &fallback) {
				return e.success(ctx, workflow, owner, storage.TeamLifecycleOAuthLogin, "credential_login_fallback_selected", storage.TeamLifecycleUpdate{
					CredentialPath: "oauth",
					EventDetailJSON: detail(map[string]interface{}{
						"operation_key": op.OperationKey,
						"reason":        "credential_rejected",
					}),
				})
			}
			return e.fail(ctx, workflow, owner, err)
		}
		ref, err = cleanOpaqueRef("credential", ref)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleImporting, "credential_login_completed", storage.TeamLifecycleUpdate{
			CredentialRef:   ref,
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleOAuthLogin:
		result, err := e.adapter.OAuthLogin(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		if result.PhoneRequired {
			ref, cleanErr := cleanOpaqueRef("phone challenge", result.PhoneChallengeRef)
			if cleanErr != nil {
				return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", cleanErr))
			}
			return e.success(ctx, workflow, owner, storage.TeamLifecyclePhoneVerification, "phone_verification_requested", storage.TeamLifecycleUpdate{
				PhoneChallengeRef: ref,
				EventDetailJSON:   detail(map[string]interface{}{"operation_key": op.OperationKey}),
			})
		}
		ref, cleanErr := cleanOpaqueRef("credential", result.CredentialRef)
		if cleanErr != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", cleanErr))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleImporting, "oauth_login_completed", storage.TeamLifecycleUpdate{
			CredentialRef:   ref,
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecyclePhoneVerification:
		ref, err := e.adapter.VerifyPhone(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		ref, err = cleanOpaqueRef("credential", ref)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleImporting, "phone_verification_completed", storage.TeamLifecycleUpdate{
			CredentialRef:   ref,
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleImporting:
		accountID, err := e.adapter.ImportAccount(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		accountID, err = cleanOpaqueRef("account", accountID)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleActive, "account_imported", storage.TeamLifecycleUpdate{
			ImportedAccountID: accountID,
			NextAttemptAt:     now.Add(e.options.QuotaPollInterval).Unix(),
			EventDetailJSON:   detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleActive:
		remaining, err := e.adapter.ObserveQuota(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		if remaining < 0 || remaining > 10000 {
			return e.fail(ctx, workflow, owner, Permanent("invalid_quota_observation", errors.New("quota basis points outside 0..10000")))
		}
		update := storage.TeamLifecycleUpdate{
			SetQuota:          true,
			QuotaRemainingBPS: remaining,
			EventDetailJSON: detail(map[string]interface{}{
				"remaining_bps": remaining,
				"threshold_bps": workflow.RotateThresholdBPS,
			}),
		}
		if remaining <= workflow.RotateThresholdBPS {
			return e.success(ctx, workflow, owner, storage.TeamLifecycleRemoving, "rotation_threshold_reached", update)
		}
		update.NextAttemptAt = now.Add(e.options.QuotaPollInterval).Unix()
		return e.success(ctx, workflow, owner, storage.TeamLifecycleActive, "quota_observed", update)

	case storage.TeamLifecycleRemoving:
		if err := e.adapter.RemoveMember(stepCtx, op); err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleEnqueueReplacement, "member_removed", storage.TeamLifecycleUpdate{
			EventDetailJSON: detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	case storage.TeamLifecycleEnqueueReplacement:
		jobRef, err := e.adapter.EnqueueReplacement(stepCtx, op)
		if err != nil {
			return e.fail(ctx, workflow, owner, err)
		}
		jobRef, err = cleanOpaqueRef("replacement job", jobRef)
		if err != nil {
			return e.fail(ctx, workflow, owner, Permanent("invalid_connector_result", err))
		}
		return e.success(ctx, workflow, owner, storage.TeamLifecycleCompleted, "replacement_enqueued", storage.TeamLifecycleUpdate{
			ReplacementJobRef: jobRef,
			CompletedAt:       now.Unix(),
			EventDetailJSON:   detail(map[string]interface{}{"operation_key": op.OperationKey}),
		})

	default:
		return e.fail(ctx, workflow, owner, Permanent(
			"invalid_workflow_state",
			fmt.Errorf("unsupported team lifecycle state %q", workflow.State),
		))
	}
}

type CoordinatorOptions struct {
	Workers      int
	PollInterval time.Duration
	Lease        time.Duration
}

type Coordinator struct {
	repository Repository
	engine     *Engine
	options    CoordinatorOptions
	owner      string
	wake       chan struct{}
}

func NewCoordinator(
	repository Repository,
	adapter Adapter,
	engineOptions Options,
	coordinatorOptions CoordinatorOptions,
	owner string,
) *Coordinator {
	if coordinatorOptions.Workers <= 0 {
		coordinatorOptions.Workers = 1
	}
	if coordinatorOptions.Workers > 8 {
		coordinatorOptions.Workers = 8
	}
	if coordinatorOptions.PollInterval <= 0 {
		coordinatorOptions.PollInterval = 2 * time.Second
	}
	if coordinatorOptions.Lease <= 0 {
		coordinatorOptions.Lease = 10 * time.Minute
	}
	if coordinatorOptions.Lease < 30*time.Second {
		coordinatorOptions.Lease = 30 * time.Second
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = fmt.Sprintf("teamflow-%d", time.Now().UnixNano())
	}
	return &Coordinator{
		repository: repository,
		engine:     NewEngine(repository, adapter, engineOptions),
		options:    coordinatorOptions,
		owner:      owner,
		wake:       make(chan struct{}, 1),
	}
}

func (c *Coordinator) Wake() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) Run(ctx context.Context) {
	if c == nil || c.repository == nil || c.engine == nil {
		return
	}
	var workers sync.WaitGroup
	for index := 0; index < c.options.Workers; index++ {
		workers.Add(1)
		go func(worker int) {
			defer supervisor.Recover("team-lifecycle-worker")
			defer workers.Done()
			c.runWorker(ctx, fmt.Sprintf("%s-%d", c.owner, worker+1))
		}(index)
	}
	workers.Wait()
}

func (c *Coordinator) runWorker(ctx context.Context, owner string) {
	for {
		workflow, claimed, err := c.repository.ClaimTeamLifecycleWorkflow(
			ctx, owner, time.Now().Unix(), int64(c.options.Lease/time.Second),
		)
		if err == nil && claimed {
			c.advanceWithHeartbeat(ctx, workflow, owner)
			continue
		}
		timer := time.NewTimer(c.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (c *Coordinator) advanceWithHeartbeat(
	ctx context.Context,
	workflow storage.TeamLifecycleWorkflow,
	owner string,
) {
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer supervisor.Recover("team-lifecycle-heartbeat")
		defer close(done)
		interval := c.options.Lease / 3
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stepCtx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.repository.RenewTeamLifecycleLease(
					heartbeatCtx,
					workflow.ID,
					workflow.Version,
					owner,
					time.Now().Add(c.options.Lease).Unix(),
				)
				heartbeatCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	_, _ = c.engine.Advance(stepCtx, workflow, owner)
	cancel()
	<-done
}

// UnconfiguredAdapter makes execute-mode readiness failures explicit while
// leaving shadow workflows fully inspectable.
type UnconfiguredAdapter struct{}

func (UnconfiguredAdapter) unavailable() error {
	return Permanent("connector_not_configured", errors.New("team lifecycle connector not configured"))
}

func (a UnconfiguredAdapter) Invite(context.Context, Operation) (string, error) {
	return "", a.unavailable()
}
func (a UnconfiguredAdapter) ResolveCredential(context.Context, Operation) (CredentialResolution, error) {
	return CredentialResolution{}, a.unavailable()
}
func (a UnconfiguredAdapter) LoginWithCredential(context.Context, Operation) (string, error) {
	return "", a.unavailable()
}
func (a UnconfiguredAdapter) OAuthLogin(context.Context, Operation) (OAuthResult, error) {
	return OAuthResult{}, a.unavailable()
}
func (a UnconfiguredAdapter) VerifyPhone(context.Context, Operation) (string, error) {
	return "", a.unavailable()
}
func (a UnconfiguredAdapter) ImportAccount(context.Context, Operation) (string, error) {
	return "", a.unavailable()
}
func (a UnconfiguredAdapter) ObserveQuota(context.Context, Operation) (int, error) {
	return 0, a.unavailable()
}
func (a UnconfiguredAdapter) RemoveMember(context.Context, Operation) error {
	return a.unavailable()
}
func (a UnconfiguredAdapter) EnqueueReplacement(context.Context, Operation) (string, error) {
	return "", a.unavailable()
}
