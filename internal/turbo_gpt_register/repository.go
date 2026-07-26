package turbo_gpt_register

import (
	"context"

	"codex-account-pool/internal/storage"
)

// Repository isolates the orchestrator from SQLite details and keeps it easy to test.
type Repository interface {
	CreateTurboGPTRegisterJob(context.Context, storage.TurboGPTRegisterJob) error
	UpdateTurboGPTRegisterJob(context.Context, storage.TurboGPTRegisterJob) error
	GetTurboGPTRegisterJob(context.Context, string) (storage.TurboGPTRegisterJob, error)
	ListTurboGPTRegisterJobs(context.Context, string, int) ([]storage.TurboGPTRegisterJob, error)
	DeleteTurboGPTRegisterJob(context.Context, string) error
	UpsertTurboGPTRegisterToken(context.Context, storage.TurboGPTRegisterToken) error
	GetTurboGPTRegisterToken(context.Context, string) (storage.TurboGPTRegisterToken, error)
	SetTurboGPTRegisterConfig(context.Context, string, string) error
	GetTurboGPTRegisterConfig(context.Context) (map[string]string, error)
}
