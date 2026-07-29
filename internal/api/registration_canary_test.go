package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/registration/pipeline"
)

func configureProtocolEmailProvider(t *testing.T, h *testHarness, token string) {
	t.Helper()
	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{
			"type":"mailbox","key":"cloudflare","display_name":"Mailbox","enabled":true,
			"config":{
				"api_url":"https://mail.example.test",
				"admin_token":"`+token+`",
				"domain":"example.test"
			}
		}]
	}`)
	if code != http.StatusOK {
		t.Fatalf("configure mailbox provider = %d: %s", code, body)
	}
}

func TestRegisterBatchRequiresCanaryForCurrentFingerprint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.regHandler.enabled = true
	registrationPoolID, _ := configureRegistrationEgressPools(t, h)
	configureProtocolEmailProvider(t, h, "mail-token-one")

	body := `{"count":1,"method":"protocol","identity_mode":"email","registration_egress_pool_id":"` + registrationPoolID + `"}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", body)
	if code != http.StatusConflict {
		t.Fatalf("batch before canary = %d, want 409: %s", code, raw)
	}

	req := pipeline.RegisterRequest{
		Method:                   "protocol",
		IdentityMode:             "email",
		RegistrationEgressPoolID: registrationPoolID,
	}
	readiness, err := h.app.regHandler.registrationMethodReadiness(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.Ready || readiness.CanaryReady {
		t.Fatalf("readiness before canary = %+v", readiness)
	}
	if err := h.store.RecordRegistrationCanary(
		context.Background(), "protocol", "passed", readiness.Fingerprint,
		"job-canary", "account-canary", "",
	); err != nil {
		t.Fatal(err)
	}
	// Keep this an API-gating test: the async worker fails immediately instead of
	// contacting the configured mailbox endpoint.
	h.app.regHandler.pipeline = pipeline.NewPipeline(h.store, nil, h.app.upstream, nil)

	code, raw = grpReq(t, h, http.MethodPost, "/admin/register/batch", body)
	if code != http.StatusAccepted {
		t.Fatalf("batch after matching canary = %d, want 202: %s", code, raw)
	}
}

func TestRegistrationCanaryInvalidatesWhenProviderCredentialChanges(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	registrationPoolID, _ := configureRegistrationEgressPools(t, h)
	configureProtocolEmailProvider(t, h, "mail-token-one")
	req := pipeline.RegisterRequest{
		Method:                   "protocol",
		IdentityMode:             "email",
		RegistrationEgressPoolID: registrationPoolID,
	}
	first, err := h.app.regHandler.registrationMethodReadiness(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Ready {
		t.Fatalf("initial method not ready: %+v", first)
	}
	if err := h.store.RecordRegistrationCanary(
		context.Background(), "protocol", "passed", first.Fingerprint,
		"job-canary", "account-canary", "",
	); err != nil {
		t.Fatal(err)
	}
	configureProtocolEmailProvider(t, h, "mail-token-two")
	second, err := h.app.regHandler.registrationMethodReadiness(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Ready || second.CanaryReady {
		t.Fatalf("changed provider credential retained stale canary: %+v", second)
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("provider credential change did not alter readiness fingerprint")
	}
}

func TestRegisterCanaryRouteIsAdminOnly(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.AdminToken = "canary-admin-token"
	resp, err := http.Get(h.pool.URL + "/admin/register/canary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated canary route = %d, want 401", resp.StatusCode)
	}
}
