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

func TestRegisterRequestNormalizationDefaults(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

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
	if req.EgressID != storage.DefaultDirectEgressID {
		t.Fatalf("egress_id = %q, want %q", req.EgressID, storage.DefaultDirectEgressID)
	}
	if req.IdentityMode != "phone" {
		t.Fatalf("identity_mode = %q, want phone", req.IdentityMode)
	}
}

func TestRegisterRequestManualCountryFallbackAndValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
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
