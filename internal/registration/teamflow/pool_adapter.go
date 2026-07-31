package teamflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

// AccountPool is the narrow local view required by the lifecycle adapter.
// Tokens are inspected only for presence and are never returned from this layer.
type AccountPool interface {
	GetToken(context.Context, string) (storage.AccountToken, error)
	GetAccountRateLimit(context.Context, string) (storage.AccountRateLimit, bool, error)
}

// RemoteConnector owns membership and interactive login operations. A deployment
// can bind its connector through api.Dependencies; the core engine remains
// provider-neutral and tests it with deterministic fixtures.
type RemoteConnector interface {
	Invite(context.Context, Operation) (string, error)
	LoginWithCredential(context.Context, Operation) (string, error)
	OAuthLogin(context.Context, Operation) (OAuthResult, error)
	VerifyPhone(context.Context, Operation) (string, error)
	ImportAccount(context.Context, Operation) (string, error)
	RemoveMember(context.Context, Operation) error
}

type ReplacementEnqueuer func(context.Context, storage.TeamLifecycleWorkflow) (string, error)

type PoolAdapter struct {
	pool               AccountPool
	remote             RemoteConnector
	enqueueReplacement ReplacementEnqueuer
}

func NewPoolAdapter(pool AccountPool, remote RemoteConnector, enqueue ReplacementEnqueuer) *PoolAdapter {
	return &PoolAdapter{pool: pool, remote: remote, enqueueReplacement: enqueue}
}

func (a *PoolAdapter) missingRemote() error {
	return Permanent("connector_not_configured", errors.New("team lifecycle remote connector not configured"))
}

func (a *PoolAdapter) Invite(ctx context.Context, operation Operation) (string, error) {
	if a == nil || a.remote == nil {
		return "", a.missingRemote()
	}
	return a.remote.Invite(ctx, operation)
}

func (a *PoolAdapter) ResolveCredential(ctx context.Context, operation Operation) (CredentialResolution, error) {
	if a == nil || a.pool == nil {
		return CredentialResolution{}, Permanent("account_pool_not_configured", errors.New("account pool not configured"))
	}
	accountID := strings.TrimSpace(operation.Workflow.ChildAccountID)
	token, err := a.pool.GetToken(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialResolution{Available: false}, nil
	}
	if err != nil {
		return CredentialResolution{}, Retryable("credential_lookup_failed", err)
	}
	hasPersonalAccessToken := strings.EqualFold(
		strings.TrimSpace(token.CredentialMode),
		accountprovider.CredentialModePersonalAccessToken,
	) && strings.TrimSpace(token.OpenAIAPIKey) != ""
	hasAPIKey := strings.TrimSpace(token.OpenAIAPIKey) != ""
	hasUsableAccessToken := strings.TrimSpace(token.AccessToken) != "" &&
		(token.ExpiresAt <= 0 || token.ExpiresAt > time.Now().Add(time.Minute).Unix())
	if !hasPersonalAccessToken && !hasAPIKey && !hasUsableAccessToken {
		return CredentialResolution{Available: false}, nil
	}
	return CredentialResolution{
		Available:     true,
		CredentialRef: "account_auth_tokens:" + accountID,
	}, nil
}

func (a *PoolAdapter) LoginWithCredential(ctx context.Context, operation Operation) (string, error) {
	if a == nil || a.remote == nil {
		return "", a.missingRemote()
	}
	return a.remote.LoginWithCredential(ctx, operation)
}

func (a *PoolAdapter) OAuthLogin(ctx context.Context, operation Operation) (OAuthResult, error) {
	if a == nil || a.remote == nil {
		return OAuthResult{}, a.missingRemote()
	}
	return a.remote.OAuthLogin(ctx, operation)
}

func (a *PoolAdapter) VerifyPhone(ctx context.Context, operation Operation) (string, error) {
	if a == nil || a.remote == nil {
		return "", a.missingRemote()
	}
	return a.remote.VerifyPhone(ctx, operation)
}

func (a *PoolAdapter) ImportAccount(ctx context.Context, operation Operation) (string, error) {
	if a == nil || a.remote == nil {
		return "", a.missingRemote()
	}
	return a.remote.ImportAccount(ctx, operation)
}

func (a *PoolAdapter) ObserveQuota(ctx context.Context, operation Operation) (int, error) {
	if a == nil || a.pool == nil {
		return 0, Permanent("account_pool_not_configured", errors.New("account pool not configured"))
	}
	accountID := strings.TrimSpace(operation.Workflow.ImportedAccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(operation.Workflow.ChildAccountID)
	}
	snapshot, ok, err := a.pool.GetAccountRateLimit(ctx, accountID)
	if err != nil {
		return 0, Retryable("quota_lookup_failed", err)
	}
	if !ok || snapshot.UsedPercent < 0 {
		return 0, Retryable("quota_not_observed", errors.New("quota snapshot not observed"))
	}
	if math.IsNaN(snapshot.UsedPercent) || math.IsInf(snapshot.UsedPercent, 0) || snapshot.UsedPercent > 100 {
		return 0, Permanent("invalid_quota_observation", fmt.Errorf("used percent %v is outside 0..100", snapshot.UsedPercent))
	}
	remaining := int(math.Round((100 - snapshot.UsedPercent) * 100))
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 10000 {
		remaining = 10000
	}
	return remaining, nil
}

func (a *PoolAdapter) RemoveMember(ctx context.Context, operation Operation) error {
	if a == nil || a.remote == nil {
		return a.missingRemote()
	}
	return a.remote.RemoveMember(ctx, operation)
}

func (a *PoolAdapter) EnqueueReplacement(ctx context.Context, operation Operation) (string, error) {
	if a == nil || a.enqueueReplacement == nil {
		return "", Permanent("registration_queue_not_configured", errors.New("registration queue not configured"))
	}
	jobRef, err := a.enqueueReplacement(ctx, operation.Workflow)
	if err != nil {
		return "", Retryable("registration_queue_unavailable", err)
	}
	return jobRef, nil
}
