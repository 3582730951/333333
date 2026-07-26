package turbo_gpt_register

import "codex-account-pool/internal/storage"

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"

	Phase1    = "phase1"
	Phase15   = "phase1_5"
	Phase2    = "phase2"
	Phase3    = "phase3"
	PhaseDone = "completed"
)

type Job = storage.TurboGPTRegisterJob
type TokenData = storage.TurboGPTRegisterToken

type CreateJobRequest struct {
	FullName             string                 `json:"full_name,omitempty"`
	BirthDate            string                 `json:"birth_date,omitempty"`
	PhoneCountryCode     string                 `json:"phone_country_code,omitempty"`
	PhoneCountryDialCode string                 `json:"phone_country_dial_code,omitempty"`
	MailDomain           string                 `json:"mail_domain,omitempty"`
	AutoImport           bool                   `json:"auto_import"`
	Config               map[string]interface{} `json:"config,omitempty"`
}

type ExecutorInput struct {
	Job    Job               `json:"job"`
	Config map[string]string `json:"config,omitempty"`
}

type ExecutorResult struct {
	Success bool                   `json:"success"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
}
