package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func providerRowsCount(t *testing.T, h *testHarness) int {
	t.Helper()
	var count int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM provider_settings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestRegisterProvidersBulkRejectsInvalidProviderWithoutPartialWrite(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [
			{"type":"mailbox","key":"tempmail","display_name":"TempMail","enabled":true},
			{"type":"","key":"bad","enabled":true}
		]
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid provider bulk status = %d, want 400: %s", code, body)
	}
	if count := providerRowsCount(t, h); count != 0 {
		t.Fatalf("invalid bulk wrote %d provider rows, want 0", count)
	}

	code, body = grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"unknown","key":"x","enabled":true}]
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown provider type status = %d, want 400: %s", code, body)
	}
	if count := providerRowsCount(t, h); count != 0 {
		t.Fatalf("unknown type wrote %d provider rows, want 0", count)
	}
}

func TestRegisterProvidersBulkSavesProvidersAndDefaultsAtomically(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [
			{"type":"mailbox","key":"tempmail","display_name":"TempMail","enabled":true,"priority":7,"config":{"api_url":"https://mail.test"}}
		],
		"defaults": {"mailbox":"tempmail","group":"default"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("provider bulk save status = %d: %s", code, body)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/providers", "")
	if code != http.StatusOK {
		t.Fatalf("provider list status = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode provider list: %v (%s)", err, raw)
	}
	providers, ok := got["providers"].([]interface{})
	if !ok || len(providers) != 1 {
		t.Fatalf("providers = %#v, want one provider", got["providers"])
	}
	row, ok := providers[0].(map[string]interface{})
	if !ok {
		t.Fatalf("provider row has unexpected shape: %#v", providers[0])
	}
	if row["type"] != "mailbox" || row["key"] != "tempmail" || row["enabled"] != true || row["priority"] != float64(7) {
		t.Fatalf("provider row not persisted correctly: %#v", row)
	}
	defaults, ok := got["defaults"].(map[string]interface{})
	if !ok {
		t.Fatalf("defaults missing from provider list: %#v", got)
	}
	if defaults["mailbox"] != "tempmail" || defaults["group"] != "default" {
		t.Fatalf("defaults not persisted: %#v", defaults)
	}
}

func TestRegisterProvidersAcceptsHotmailOTPEmailProvider(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"email","key":"hotmail_otp","display_name":"Hotmail OTP","enabled":true,
			"config":{"base_email":"owner@outlook.com","otp_url":"https://otp.example/read"}}]
	}`)
	if code != http.StatusOK {
		t.Fatalf("hotmail OTP provider save status = %d: %s", code, body)
	}
	var count int
	if err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM provider_settings WHERE provider_type='email' AND provider_key='hotmail_otp' AND enabled=1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("hotmail OTP provider count = %d, want 1", count)
	}
}

func TestRegisterProvidersNeverReturnsConfiguredSecrets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	const secret = "provider-api-key-must-not-leak"
	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"sms","key":"herosms","display_name":"HeroSMS","enabled":true,
			"config":{"api_key":"`+secret+`","service":"dr"}}]
	}`)
	if code != http.StatusOK {
		t.Fatalf("provider save status = %d: %s", code, body)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/providers", "")
	if code != http.StatusOK {
		t.Fatalf("provider list status = %d: %s", code, raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("provider list leaked configured secret: %s", raw)
	}
	if !strings.Contains(string(raw), `"api_key_configured":true`) ||
		!strings.Contains(string(raw), `"masked":"••••"`) {
		t.Fatalf("provider list omitted safe credential metadata: %s", raw)
	}
}

func TestRegisterProvidersReportsDefaultsReadErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	// The settings snapshot is warmed by harness setup; clear it so the next read hits
	// the now-dropped table and surfaces the read error this test asserts.
	h.store.InvalidateSettingsCache()

	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/providers", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("provider list with missing settings table = %d, want 503: %s", code, raw)
	}
	if !strings.Contains(string(raw), `"code":"service_unavailable"`) || strings.Contains(string(raw), "settings") {
		t.Fatalf("provider list error was not safely normalized: %s", raw)
	}
}

func TestRegisterProviderOptionsRejectsInvalidStoredConfig(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	if _, err := h.store.DB().ExecContext(context.Background(),
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('bad-provider','mailbox','tempmail','Broken',1,0,'{','{}',0,0)`); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/register/providers/options", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"code":"service_unavailable"`) {
		t.Fatalf("provider options with invalid config = %d, want safe 503: %s", code, raw)
	}
}
