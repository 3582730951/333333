package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
)

// registration_readiness_test.go guards Phase ⑤'s observability deliverable: the
// /admin/register/readiness self-check that tells an operator whether "deploy →
// auto-fill the pool" is actually configured to run, and why not when it isn't.

func getReadiness(t *testing.T, h *testHarness) map[string]interface{} {
	t.Helper()
	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/readiness", "")
	if code != http.StatusOK {
		t.Fatalf("readiness = %d: %s", code, raw)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode readiness: %v (%s)", err, raw)
	}
	return m
}

func blockersContain(m map[string]interface{}, sub string) bool {
	switch arr := m["blockers"].(type) {
	case []interface{}:
		for _, v := range arr {
			if s, ok := v.(string); ok && strings.Contains(s, sub) {
				return true
			}
		}
	case []string:
		for _, s := range arr {
			if strings.Contains(s, sub) {
				return true
			}
		}
	}
	return false
}

func TestRegistrationReadiness(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	// Initially NOT ready: no refill policy and default node registration has no SMS provider.
	rd := getReadiness(t, h)
	if rd["ready"] != false {
		t.Fatalf("expected not-ready initially: %v", rd)
	}
	if !blockersContain(rd, "refill") || !blockersContain(rd, "SMS provider") {
		t.Fatalf("expected refill + SMS blockers, got %v", rd["blockers"])
	}

	// Enable the refill policy for the legacy protocol email path and configure mailbox.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{"type":"refill","enabled":true,"config":{"target":5,"threshold":2,"register_method":"protocol","identity_mode":"email"}}`); code != http.StatusOK {
		t.Fatalf("save refill policy = %d: %s", code, body)
	}
	if _, err := h.store.DB().ExecContext(context.Background(),
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('p1','mailbox','tempmail','TempMail',1,0,'{}','{}',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(),
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('email1','email','hotmail_otp','Hotmail OTP',1,0,'{"base_email":"ops@example.com","otp_url":"http://127.0.0.1/otp"}','{}',0,0)`); err != nil {
		t.Fatal(err)
	}

	// Now ready, with the mailbox provider counted for protocol+email.
	rd = getReadiness(t, h)
	if rd["ready"] != true {
		t.Fatalf("expected ready after refill+mailbox; blockers=%v", rd["blockers"])
	}
	prov, _ := rd["providers"].(map[string]interface{})
	if mb, _ := prov["mailbox"].(float64); mb < 1 {
		t.Fatalf("mailbox provider not counted: %v", prov)
	}
	if emailOTP, _ := prov["email_otp"].(float64); emailOTP < 1 {
		t.Fatalf("email_otp provider not counted: %v", prov)
	}
}

func TestRegistrationReadinessReportsProviderConfigErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	if code, body := grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{"type":"refill","enabled":true,"config":{"target":5,"threshold":2}}`); code != http.StatusOK {
		t.Fatalf("save refill policy = %d: %s", code, body)
	}
	if _, err := h.store.DB().ExecContext(context.Background(),
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('bad-provider','mailbox','tempmail','Broken',1,0,'{','{}',0,0)`); err != nil {
		t.Fatal(err)
	}

	rd := getReadiness(t, h)
	if rd["ready"] != false {
		t.Fatalf("expected not-ready with invalid provider config: %v", rd)
	}
	if !blockersContain(rd, "provider_settings") {
		t.Fatalf("expected provider_settings blocker, got %v", rd["blockers"])
	}
	if got, _ := rd["provider_error"].(string); !strings.Contains(got, "invalid config_json") {
		t.Fatalf("provider_error = %q, want invalid config_json", got)
	}
}

func TestRegistrationReadinessReportsIdentityMethodMismatch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	if code, body := grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{"type":"refill","enabled":true,"config":{"target":5,"threshold":2,"register_method":"protocol_v2","identity_mode":"phone"}}`); code != http.StatusOK {
		t.Fatalf("save refill policy = %d: %s", code, body)
	}

	rd := getReadiness(t, h)
	if rd["ready"] != false {
		t.Fatalf("expected not-ready with identity/method mismatch: %v", rd)
	}
	if !blockersContain(rd, "requires identity_mode=email") {
		t.Fatalf("expected identity mismatch blocker, got %v", rd["blockers"])
	}
}

func TestRegistrationReadinessReportsMissingGroup(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	if code, body := grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{"type":"refill","enabled":true,"config":{"target":5,"threshold":2,"group":"missing-group"}}`); code != http.StatusOK {
		t.Fatalf("save refill policy = %d: %s", code, body)
	}

	rd := getReadiness(t, h)
	if rd["ready"] != false {
		t.Fatalf("expected not-ready with missing group: %v", rd)
	}
	if !blockersContain(rd, "missing-group") {
		t.Fatalf("expected missing group blocker, got %v", rd["blockers"])
	}
}

func TestRegistrationReadinessReportsPoolReadErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE accounts`); err != nil {
		t.Fatal(err)
	}

	rd := h.app.registrationReadiness(context.Background())
	if rd["ready"] != false {
		t.Fatalf("expected not-ready with account pool read error: %v", rd)
	}
	if !blockersContain(rd, "账号池读取失败") {
		t.Fatalf("expected account pool read blocker, got %v", rd["blockers"])
	}
	if got, _ := rd["pool_error"].(string); got == "" {
		t.Fatalf("pool_error was empty: %v", rd)
	}
}

func TestAutoRefillUsesRegistrationDefaultGroup(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	regPool, _ := configureRegistrationEgressPools(t, h)

	h.app.autoRefill(context.Background(), &Policy{
		Enabled: true,
		Config:  map[string]interface{}{"target": 1, "threshold": 1},
	})

	var raw string
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT config_json FROM registration_jobs ORDER BY created_at DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode queued registration request: %v (%s)", err, raw)
	}
	if req["group_name"] != config.DefaultGroupName {
		t.Fatalf("auto-refill group_name = %q, want %q", req["group_name"], config.DefaultGroupName)
	}
	if req["registration_egress_pool_id"] != regPool {
		t.Fatalf("auto-refill registration_egress_pool_id = %q, want %q", req["registration_egress_pool_id"], regPool)
	}
	if req["runtime_egress_pool_id"] != "" {
		t.Fatalf("auto-refill runtime_egress_pool_id = %q, want ignored/empty", req["runtime_egress_pool_id"])
	}
	if req["egress_id"] != "" {
		t.Fatalf("auto-refill egress_id = %q, want empty so worker selects from registration pool", req["egress_id"])
	}
}

func TestAutomationPolicyReadErrorsAreVisible(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	if err := h.store.SetSetting(context.Background(), automationPoliciesKey, "{"); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/automation/policies", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("automation policies with invalid JSON = %d, want 500: %s", code, raw)
	}

	rd := getReadiness(t, h)
	if rd["ready"] != false {
		t.Fatalf("expected not-ready with invalid automation policy JSON: %v", rd)
	}
	if !blockersContain(rd, "automation_policies") {
		t.Fatalf("expected automation_policies blocker, got %v", rd["blockers"])
	}
	if got, _ := rd["policy_error"].(string); !strings.Contains(got, "invalid JSON") {
		t.Fatalf("policy_error = %q, want invalid JSON", got)
	}

	status, body, rawBody := getSettingsCenter(t, h, "?section=automation")
	if status != http.StatusOK {
		t.Fatalf("settings-center automation with invalid policies = %d: %s", status, rawBody)
	}
	automation := settingsSection(t, body, "automation")
	if got, _ := automation["policy_error"].(string); !strings.Contains(got, "invalid JSON") {
		t.Fatalf("settings-center policy_error = %q, want invalid JSON", got)
	}
}

func TestSettingsCenterAutomationReportsStatsErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE accounts`); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/automation/stats", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("automation stats with missing accounts = %d, want 500: %s", code, raw)
	}

	status, body, rawBody := getSettingsCenter(t, h, "?section=automation")
	if status != http.StatusOK {
		t.Fatalf("settings-center automation with missing accounts = %d: %s", status, rawBody)
	}
	automation := settingsSection(t, body, "automation")
	if got, _ := automation["stats_error"].(string); !strings.Contains(got, "no such table") {
		t.Fatalf("settings-center stats_error = %q, want no such table", got)
	}
	stats, ok := automation["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("settings-center stats type = %T, want object: %v", automation["stats"], automation)
	}
	if len(stats) != 0 {
		t.Fatalf("settings-center stats = %v, want empty object after read error", stats)
	}
}
