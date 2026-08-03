package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/storage"
)

func TestDecodeRegistrationRequestAcceptsLegacyAndCamelAliases(t *testing.T) {
	req, err := decodeRegistrationRequest(strings.NewReader(`{
		"engine":"email",
		"total":"3",
		"groupName":"cyber",
		"registration_pool_id":"registration-pool",
		"identity":"mail",
		"mail_provider":"emailPool",
		"mailDomain":"example.test",
		"captcha_provider":"solver-a"
	}`))
	if err != nil {
		t.Fatalf("decodeRegistrationRequest: %v", err)
	}
	want := pipeline.RegisterRequest{
		Method: "email", Count: 3, GroupName: "cyber",
		RegistrationEgressPoolID: "registration-pool", IdentityMode: "mail",
		MailboxProvider: "emailPool", MailboxDomain: "example.test", CaptchaSolver: "solver-a",
	}
	if req.Method != want.Method || req.Count != want.Count || req.GroupName != want.GroupName ||
		req.RegistrationEgressPoolID != want.RegistrationEgressPoolID || req.IdentityMode != want.IdentityMode ||
		req.MailboxProvider != want.MailboxProvider || req.MailboxDomain != want.MailboxDomain ||
		req.CaptchaSolver != want.CaptchaSolver {
		t.Fatalf("decoded=%+v want=%+v", req, want)
	}
}

func TestLegacyEmailRegistrationConfigRoundTripsCanonicalSettings(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })

	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/email/config", `{
		"total":5,"groupName":"cyber","registration_pool_id":"pool-legacy","workers":6
	}`)
	if code != http.StatusOK {
		t.Fatalf("save legacy email config=%d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode legacy config response: %v", err)
	}
	if got["count"] != float64(5) || got["group_name"] != "cyber" ||
		got["egress_pool_id"] != "pool-legacy" || got["concurrency"] != float64(6) {
		t.Fatalf("legacy config response=%v", got)
	}
	settings := map[string]string{
		"reg_default_group": "cyber", "registration_egress_pool_id": "pool-legacy",
		"registration_concurrency": "6", "reg_default_mailbox": "email_pool",
	}
	for key, want := range settings {
		value, ok, err := h.store.GetSetting(context.Background(), key)
		if err != nil || !ok || value != want {
			t.Fatalf("setting %s=%q ok=%v err=%v, want %q", key, value, ok, err, want)
		}
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/register/email/config", "")
	if code != http.StatusOK {
		t.Fatalf("get legacy email config=%d: %s", code, raw)
	}
	if !strings.Contains(string(raw), `"compatibility":"unified_registration"`) {
		t.Fatalf("compatibility marker missing: %s", raw)
	}
}

func TestEmailPoolAcceptsStructuredLegacyImportAndQueryAliases(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	body := `{"groupName":"legacy-mail","accounts":[{
		"address":"legacy.one@example.test","password":"pw","clientId":"client-one","refreshToken":"refresh-one"
	}]}`
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/email-pool/import", body); code != http.StatusOK {
		t.Fatalf("structured import=%d: %s", code, raw)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/email-pool?page_size=5&q=legacy.one&state=idle", "")
	if code != http.StatusOK {
		t.Fatalf("legacy query aliases=%d: %s", code, raw)
	}
	var response struct {
		Accounts []map[string]interface{} `json:"accounts"`
		PageSize int                      `json:"pageSize"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.PageSize != 5 || len(response.Accounts) != 1 || response.Accounts[0]["email"] != "legacy.one@example.test" {
		t.Fatalf("legacy query response=%s", raw)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/register/providers/options", "")
	if code != http.StatusOK || !strings.Contains(string(raw), `"value":"email_pool"`) {
		t.Fatalf("email pool was not hot-wired into provider options: code=%d body=%s", code, raw)
	}
}

func TestEmailPoolEmptyListReturnsArray(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/email-pool", "")
	if code != http.StatusOK || !strings.Contains(string(raw), `"accounts":[]`) || strings.Contains(string(raw), `"accounts":null`) {
		t.Fatalf("empty email pool response=%d: %s", code, raw)
	}
}

func TestEmailPoolOmittedOrAllStatusPreservesLegacyUnfilteredList(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	for index, status := range []string{"idle", "in_use", "used", "error"} {
		if err := h.store.InsertEmailAccount(ctx, storage.EmailAccount{
			ID: fmt.Sprintf("compat-email-%d", index), Email: fmt.Sprintf("compat-%d@example.test", index),
			Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/admin/email-pool", "/admin/email-pool?status=all", "/admin/email-pool?state=any"} {
		code, raw := grpReq(t, h, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("unfiltered list %s=%d: %s", path, code, raw)
		}
		var response struct {
			Accounts []map[string]interface{} `json:"accounts"`
			Total    int                      `json:"total"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		if response.Total != 4 || len(response.Accounts) != 4 {
			t.Fatalf("unfiltered list %s narrowed rows: %s", path, raw)
		}
	}
}

func TestRegistrationRuntimeSettingsPreferCanonicalAndFallbackToLegacy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	ctx := context.Background()
	if err := h.store.SetSettings(ctx, map[string]string{
		"email_registration_enabled":         "true",
		"email_registration_timeout_seconds": "41",
		"email_registration_concurrency":     "2",
	}); err != nil {
		t.Fatal(err)
	}
	if !h.app.regHandler.registrationEnabled(ctx) || h.app.regHandler.resolveRegistrationTimeout(ctx) != 41*time.Second {
		t.Fatalf("legacy runtime settings were not resolved")
	}
	if err := h.store.SetSettings(ctx, map[string]string{
		"registration_enabled": "false", "registration_timeout": "57", "registration_concurrency": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if h.app.regHandler.registrationEnabled(ctx) || h.app.regHandler.resolveRegistrationTimeout(ctx) != 57*time.Second || h.app.regHandler.resolveConcurrency(ctx) != 1 {
		t.Fatalf("canonical runtime settings did not win")
	}
}

func TestRegistrationConfigRegistryIncludesRuntimeCompatibilityFields(t *testing.T) {
	keys := map[string]bool{}
	for _, field := range configFields() {
		keys[field.Key] = true
	}
	for _, key := range []string{"registration_enabled", "registration_timeout", "registration_default_group"} {
		if !keys[key] {
			t.Fatalf("config field %q missing", key)
		}
	}
}

func TestAutomationPoliciesReadLegacyEnvelopeAndWriteCanonicalShape(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	legacy := `{"policies":[{"policyType":"auto-refill","active":true,"settings":{
		"desiredCount":12,"minAccounts":4,"engine":"email","identity":"mail",
		"groupName":"cyber","registrationPoolId":"pool-old","mailProvider":"emailPool"
	}}]}`
	if err := h.store.SetSetting(context.Background(), automationPoliciesKey, legacy); err != nil {
		t.Fatal(err)
	}
	policies, err := h.app.loadPoliciesWithError(context.Background())
	if err != nil {
		t.Fatalf("load legacy policies: %v", err)
	}
	refill := policies[PolicyTypeRefill]
	if refill == nil || !refill.Enabled {
		t.Fatalf("legacy refill=%+v policies=%+v", refill, policies)
	}
	if refill.Config["target"] != float64(12) || refill.Config["threshold"] != float64(4) ||
		refill.Config["register_method"] != "protocol_v2" || refill.Config["identity_mode"] != "email" ||
		refill.Config["registration_egress_pool_id"] != "pool-old" || refill.Config["mailbox_provider"] != "email_pool" {
		t.Fatalf("canonical config=%+v", refill.Config)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{
		"policy_type":"pool-refill","active":true,"options":{"poolSize":20,"refillThreshold":8,"method":"turbo"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("save aliased policy=%d: %s", code, raw)
	}
	stored, ok, err := h.store.GetSetting(context.Background(), automationPoliciesKey)
	if err != nil || !ok {
		t.Fatalf("stored policy ok=%v err=%v", ok, err)
	}
	if strings.Contains(stored, "pool-refill") || strings.Contains(stored, "poolSize") ||
		!strings.Contains(stored, `"refill"`) || !strings.Contains(stored, `"register_method":"browser_v3"`) {
		t.Fatalf("policy was not canonically written: %s", stored)
	}
}

func TestTurboRegistrationCompatibilityKeepsDraftsAndMapsConfig(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	code, raw := grpReq(t, h, http.MethodPatch, "/admin/turbo-gpt-register/config", `{
		"enabled":true,"workers":2,"timeout_seconds":77,"group_name":"cyber",
		"registration_pool_id":"pool-old","engine":"email","mail_provider":"emailPool"
	}`)
	if code != http.StatusOK {
		t.Fatalf("legacy turbo config=%d: %s", code, raw)
	}
	for key, want := range map[string]string{
		"registration_enabled": "true", "registration_concurrency": "2", "registration_timeout": "77",
		"registration_default_group": "cyber", "registration_egress_pool_id": "pool-old",
		"default_register_method": "protocol_v2", "default_mailbox_provider": "email_pool",
	} {
		got, ok, err := h.store.GetSetting(context.Background(), key)
		if err != nil || !ok || got != want {
			t.Fatalf("canonical turbo setting %s=%q ok=%v err=%v, want %q", key, got, ok, err, want)
		}
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/turbo-gpt-register/jobs", `{
		"fullName":"Legacy Operator","mailDomain":"example.test","auto_import":true,"start":false,
		"config":{"engine":"turbo","groupName":"cyber"}
	}`)
	if code != http.StatusCreated {
		t.Fatalf("legacy turbo draft=%d: %s", code, raw)
	}
	var draft storage.TurboGPTRegisterJob
	if err := json.Unmarshal(raw, &draft); err != nil || !strings.HasPrefix(draft.ID, "tgr_") || draft.Status != "pending" {
		t.Fatalf("legacy turbo draft=%+v err=%v body=%s", draft, err, raw)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/turbo-gpt-register/jobs", "")
	if code != http.StatusOK || !strings.Contains(string(raw), draft.ID) || !strings.Contains(string(raw), `"compatibility":"unified_registration"`) {
		t.Fatalf("legacy turbo list=%d: %s", code, raw)
	}
}
