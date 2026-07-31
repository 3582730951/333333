package teamflow

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"codex-account-pool/internal/storage"
)

type fixturePool struct {
	token    storage.AccountToken
	tokenErr error
	quota    storage.AccountRateLimit
	hasQuota bool
	quotaErr error
}

func (p fixturePool) GetToken(context.Context, string) (storage.AccountToken, error) {
	return p.token, p.tokenErr
}

func (p fixturePool) GetAccountRateLimit(context.Context, string) (storage.AccountRateLimit, bool, error) {
	return p.quota, p.hasQuota, p.quotaErr
}

func TestPoolAdapterResolvesOpaqueCredentialReference(t *testing.T) {
	adapter := NewPoolAdapter(fixturePool{
		token: storage.AccountToken{AccountID: "child-ref", AccessToken: "fixture-secret-token"},
	}, nil, nil)
	result, err := adapter.ResolveCredential(context.Background(), Operation{
		Workflow: storage.TeamLifecycleWorkflow{ChildAccountID: "child-ref"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.CredentialRef != "account_auth_tokens:child-ref" {
		t.Fatalf("result=%+v", result)
	}
	if result.CredentialRef == "fixture-secret-token" {
		t.Fatal("adapter returned credential material instead of an opaque reference")
	}

	missing := NewPoolAdapter(fixturePool{tokenErr: sql.ErrNoRows}, nil, nil)
	result, err = missing.ResolveCredential(context.Background(), Operation{
		Workflow: storage.TeamLifecycleWorkflow{ChildAccountID: "child-ref"},
	})
	if err != nil || result.Available {
		t.Fatalf("missing credential result=%+v err=%v", result, err)
	}
}

func TestPoolAdapterConvertsQuotaToBasisPoints(t *testing.T) {
	adapter := NewPoolAdapter(fixturePool{
		quota:    storage.AccountRateLimit{UsedPercent: 99},
		hasQuota: true,
	}, nil, nil)
	remaining, err := adapter.ObserveQuota(context.Background(), Operation{
		Workflow: storage.TeamLifecycleWorkflow{ImportedAccountID: "imported-ref"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 100 {
		t.Fatalf("remaining=%d want=100", remaining)
	}

	withoutSnapshot := NewPoolAdapter(fixturePool{}, nil, nil)
	if _, err := withoutSnapshot.ObserveQuota(context.Background(), Operation{
		Workflow: storage.TeamLifecycleWorkflow{ChildAccountID: "child-ref"},
	}); err == nil {
		t.Fatal("missing quota snapshot should schedule a retry")
	} else {
		var classified *ClassifiedError
		if !errors.As(err, &classified) || !classified.Retryable || classified.Class != "quota_not_observed" {
			t.Fatalf("quota error=%v", err)
		}
	}
}

func TestPoolAdapterUsesUnifiedReplacementQueue(t *testing.T) {
	var received storage.TeamLifecycleWorkflow
	adapter := NewPoolAdapter(fixturePool{}, nil, func(_ context.Context, workflow storage.TeamLifecycleWorkflow) (string, error) {
		received = workflow
		return "registration-job-ref", nil
	})
	workflow := storage.TeamLifecycleWorkflow{ID: "workflow-ref", WorkspaceID: "workspace-ref"}
	jobRef, err := adapter.EnqueueReplacement(context.Background(), Operation{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	if jobRef != "registration-job-ref" || received.ID != workflow.ID {
		t.Fatalf("jobRef=%q received=%+v", jobRef, received)
	}
}
