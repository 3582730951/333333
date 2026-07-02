package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/storage"
)

func registrationJobCount(t *testing.T, h *testHarness) int {
	t.Helper()
	var count int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM registration_jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func insertRegistrationJob(t *testing.T, h *testHarness, id, status string) {
	t.Helper()
	now := storage.Now()
	if _, err := h.store.DB().ExecContext(context.Background(),
		`INSERT INTO registration_jobs (id, platform, method, total, status, config_json, created_at, updated_at)
		 VALUES (?, 'chatgpt', 'node', 1, ?, '{}', ?, ?)`,
		id, status, now, now); err != nil {
		t.Fatal(err)
	}
}

func configureRegistrationEgressPools(t *testing.T, h *testHarness) (registrationPoolID, runtimePoolID string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID:             "reg_proxy",
		Name:           "registration proxy",
		Type:           "http_proxy",
		Endpoint:       "http://user:pass@reg.example:8000",
		IPMode:         "dynamic_residential",
		ProviderKey:    "cliproxy",
		StreamCapable:  true,
		Health:         "healthy",
		MaxConcurrency: 16,
	}); err != nil {
		t.Fatalf("upsert registration profile: %v", err)
	}
	if err := h.store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID:             "runtime_sidecar",
		Name:           "runtime sidecar",
		Type:           "curl_cffi_sidecar",
		Endpoint:       "http://127.0.0.1:8790",
		StreamCapable:  true,
		Health:         "healthy",
		MaxConcurrency: 16,
	}); err != nil {
		t.Fatalf("upsert runtime profile: %v", err)
	}
	if err := h.store.UpsertEgressPool(ctx, storage.EgressPool{ID: "pool_registration", Purpose: "registration", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatalf("upsert registration pool: %v", err)
	}
	if err := h.store.UpsertEgressPoolMember(ctx, storage.EgressPoolMember{PoolID: "pool_registration", EgressID: "reg_proxy", Enabled: true}); err != nil {
		t.Fatalf("upsert registration member: %v", err)
	}
	if err := h.store.UpsertEgressPool(ctx, storage.EgressPool{ID: "pool_runtime", Purpose: "runtime", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatalf("upsert runtime pool: %v", err)
	}
	if err := h.store.UpsertEgressPoolMember(ctx, storage.EgressPoolMember{PoolID: "pool_runtime", EgressID: "runtime_sidecar", Enabled: true}); err != nil {
		t.Fatalf("upsert runtime member: %v", err)
	}
	if err := h.store.UpsertGroupEgressPolicy(ctx, storage.GroupEgressPolicy{
		GroupName:          config.DefaultGroupName,
		RegistrationPoolID: "pool_registration",
		RuntimePoolID:      "pool_runtime",
		AssignmentStrategy: "sticky_least_used",
	}); err != nil {
		t.Fatalf("upsert group egress policy: %v", err)
	}
	return "pool_registration", "pool_runtime"
}

func registrationJobStatus(t *testing.T, h *testHarness, id string) string {
	t.Helper()
	var status string
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT status FROM registration_jobs WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestRegisterBatchRejectsInvalidRequestsBeforeQueueing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	configureRegistrationEgressPools(t, h)

	cases := []string{
		`{"count":0}`,
		`{"count":101}`,
		`{"count":1,"method":"shell"}`,
		`{"count":1,"platform":"claude"}`,
		`{"count":1,"group_name":"missing"}`,
		`{"count":1,"egress_id":"missing"}`,
		`{"count":1,"identity_mode":"oauth"}`,
	}
	for _, body := range cases {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", body)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid register batch %s = %d, want 400: %s", body, code, raw)
		}
	}
	if count := registrationJobCount(t, h); count != 0 {
		t.Fatalf("invalid register batch requests wrote %d jobs, want 0", count)
	}
}

func TestRegisterBatchRequiresRegistrationAndRuntimePools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", `{"count":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("register batch without pools = %d, want 400: %s", code, raw)
	}
	if count := registrationJobCount(t, h); count != 0 {
		t.Fatalf("register batch without pools wrote %d jobs, want 0", count)
	}
}

func TestRegisterRequestNormalizationDefaults(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	regPool, runtimePool := configureRegistrationEgressPools(t, h)

	req := pipeline.RegisterRequest{Count: 1}
	if err := h.app.regHandler.normalizeRegisterRequest(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	if req.Platform != "chatgpt" {
		t.Fatalf("platform = %q, want chatgpt", req.Platform)
	}
	if req.Method != "node" {
		t.Fatalf("method = %q, want node", req.Method)
	}
	if req.GroupName != config.DefaultGroupName {
		t.Fatalf("group_name = %q, want %q", req.GroupName, config.DefaultGroupName)
	}
	if req.RegistrationEgressPoolID != regPool {
		t.Fatalf("registration_egress_pool_id = %q, want %q", req.RegistrationEgressPoolID, regPool)
	}
	if req.RuntimeEgressPoolID != runtimePool {
		t.Fatalf("runtime_egress_pool_id = %q, want %q", req.RuntimeEgressPoolID, runtimePool)
	}
	if req.EgressID != "" {
		t.Fatalf("egress_id = %q, want empty until a worker selects a registration-pool member", req.EgressID)
	}
	if req.IdentityMode != "phone" {
		t.Fatalf("identity_mode = %q, want phone", req.IdentityMode)
	}
}

func TestRegisterRequestManualCountryFallbackAndValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	configureRegistrationEgressPools(t, h)
	ctx := context.Background()

	if err := h.store.SetSetting(ctx, "sms_platform_strategy", "manual"); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", `{"count":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("manual country missing status = %d, want 400: %s", code, raw)
	}
	if count := registrationJobCount(t, h); count != 0 {
		t.Fatalf("manual country missing wrote %d jobs, want 0", count)
	}

	if err := h.store.SetSetting(ctx, "sms_manual_country", "br"); err != nil {
		t.Fatal(err)
	}
	req := pipeline.RegisterRequest{Count: 1}
	if err := h.app.regHandler.normalizeRegisterRequest(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if req.Country != "BR" {
		t.Fatalf("manual country fallback = %q, want BR", req.Country)
	}

	if err := h.store.SetSetting(ctx, "sms_manual_country", "ZZZ"); err != nil {
		t.Fatal(err)
	}
	req = pipeline.RegisterRequest{Count: 1}
	if err := h.app.regHandler.normalizeRegisterRequest(ctx, &req); err == nil {
		t.Fatal("normalizeRegisterRequest with invalid sms_manual_country succeeded, want error")
	}
}

func TestRegisterRequestRejectsUnsupportedIdentityForMethod(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	configureRegistrationEgressPools(t, h)

	cases := []string{
		`{"count":1,"method":"node","identity_mode":"email"}`,
		`{"count":1,"method":"browser","identity_mode":"email"}`,
		`{"count":1,"method":"protocol_v2","identity_mode":"phone"}`,
		`{"count":1,"method":"browser_v3","identity_mode":"phone"}`,
	}
	for _, body := range cases {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", body)
		if code != http.StatusBadRequest {
			t.Fatalf("unsupported identity request %s = %d, want 400: %s", body, code, raw)
		}
	}
	if count := registrationJobCount(t, h); count != 0 {
		t.Fatalf("unsupported identity requests wrote %d jobs, want 0", count)
	}
}

func TestManualSMSCountryNotRequiredForProtocolV2Email(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	configureRegistrationEgressPools(t, h)
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "sms_platform_strategy", "manual"); err != nil {
		t.Fatal(err)
	}

	req := pipeline.RegisterRequest{Count: 1, Method: "protocol_v2", IdentityMode: "email"}
	if err := h.app.regHandler.normalizeRegisterRequest(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if req.IdentityMode != "email" {
		t.Fatalf("identity_mode = %q, want email", req.IdentityMode)
	}
	if req.Country != "" {
		t.Fatalf("country = %q, want empty for protocol_v2 email", req.Country)
	}
}

func TestRegisteredAccountIsBoundToRuntimePool(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	_, runtimePool := configureRegistrationEgressPools(t, h)
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "acc-registered", GroupName: config.DefaultGroupName, Status: "active"}, storage.AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	if err := h.app.regHandler.bindRegisteredAccountToRuntimePool(ctx, pipeline.RegisterRequest{RuntimeEgressPoolID: runtimePool}, "acc-registered"); err != nil {
		t.Fatalf("bindRegisteredAccountToRuntimePool: %v", err)
	}
	binding, err := h.store.GetEgressBinding(ctx, "acc-registered")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if binding.PrimaryEgressID != "runtime_sidecar" {
		t.Fatalf("primary egress = %q, want runtime_sidecar", binding.PrimaryEgressID)
	}
}

func TestRegisterJobCancelReportsState(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/job/missing/cancel", "")
	if code != http.StatusNotFound {
		t.Fatalf("cancel missing registration job = %d, want 404: %s", code, raw)
	}

	insertRegistrationJob(t, h, "job_pending", "pending")
	code, raw = grpReq(t, h, http.MethodPost, "/admin/register/job/job_pending/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("cancel pending registration job = %d, want 200: %s", code, raw)
	}
	if status := registrationJobStatus(t, h, "job_pending"); status != "cancelled" {
		t.Fatalf("cancelled job status = %q, want cancelled", status)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/register/job/job_pending/cancel", "")
	if code != http.StatusConflict {
		t.Fatalf("cancel already-cancelled registration job = %d, want 409: %s", code, raw)
	}

	insertRegistrationJob(t, h, "job_completed", "completed")
	code, raw = grpReq(t, h, http.MethodPost, "/admin/register/job/job_completed/cancel", "")
	if code != http.StatusConflict {
		t.Fatalf("cancel completed registration job = %d, want 409: %s", code, raw)
	}
}

func TestRegisterJobStatusAndEventsErrorsAreStructured(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/job/status", "")
	if code != http.StatusBadRequest {
		t.Fatalf("status without job id = %d, want 400: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/register/job/status?id=missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("status missing registration job = %d, want 404: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/register/job/events", "")
	if code != http.StatusBadRequest {
		t.Fatalf("events without job id = %d, want 400: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/register/job/events?id=missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("events missing registration job = %d, want 404: %s", code, raw)
	}
}
