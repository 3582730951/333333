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

func TestRegisterProvidersBulkSavesRegistrarAndPreservesUnknownProviderConfig(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "node_registrar_config", `{"proxyUsername":"old-user","proxyPassword":"old-password","removedByReplace":"old"}`); err != nil {
		t.Fatal(err)
	}

	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"sms","key":"future_sms","display_name":"Future SMS","enabled":false,
			"config":{"service":"old","future_plugin_option":{"version":2}}}],
		"defaults": {"sms":"future_sms"},
		"registrar": {"phoneCountryCode":"US","proxyUsername":"","proxyPassword":""},
		"registrar_mode": "replace"
	}`)
	if code != http.StatusOK {
		t.Fatalf("atomic provider/registrar save status = %d: %s", code, body)
	}
	var response struct {
		Saved          int                  `json:"saved"`
		RegistrarSaved bool                 `json:"registrar_saved"`
		SettingsSaved  []settingsCenterDiff `json:"settings_saved"`
		ReloadOK       bool                 `json:"reload_ok"`
		Warning        string               `json:"warning"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode atomic save response: %v (%s)", err, body)
	}
	if response.Saved != 1 || !response.RegistrarSaved || !response.ReloadOK || response.Warning != "" || response.SettingsSaved == nil {
		t.Fatalf("unexpected atomic save response: %#v", response)
	}
	if strings.Contains(string(body), "old-user") || strings.Contains(string(body), "old-password") {
		t.Fatalf("atomic save response leaked registrar credentials: %s", body)
	}

	rawRegistrar, ok, err := h.store.GetSetting(ctx, "node_registrar_config")
	if err != nil || !ok {
		t.Fatalf("read stored registrar config: ok=%v err=%v", ok, err)
	}
	var registrar map[string]interface{}
	if err := json.Unmarshal([]byte(rawRegistrar), &registrar); err != nil {
		t.Fatal(err)
	}
	if registrar["phoneCountryCode"] != "US" || registrar["proxyUsername"] != "old-user" || registrar["proxyPassword"] != "old-password" {
		t.Fatalf("registrar replace did not preserve blank write-only credentials: %#v", registrar)
	}
	if _, exists := registrar["removedByReplace"]; exists {
		t.Fatalf("registrar replace retained non-secret stale field: %#v", registrar)
	}

	code, body = grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"sms","key":"future_sms","display_name":"Future SMS","enabled":false,
			"config":{"service":"new"}}]
	}`)
	if code != http.StatusOK {
		t.Fatalf("partial provider update status = %d: %s", code, body)
	}
	var configJSON string
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT config_json FROM provider_settings WHERE provider_type='sms' AND provider_key='future_sms'`).Scan(&configJSON); err != nil {
		t.Fatal(err)
	}
	var providerConfig map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &providerConfig); err != nil {
		t.Fatal(err)
	}
	if providerConfig["service"] != "new" || providerConfig["future_plugin_option"] == nil {
		t.Fatalf("partial provider update lost or failed to overwrite public config: %#v", providerConfig)
	}
}

func TestRegisterProvidersBulkProviderErrorIsScopedAndRollsBackRegistrar(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	const originalRegistrar = `{"phoneCountryCode":"GB"}`
	if err := h.store.SetSetting(ctx, "node_registrar_config", originalRegistrar); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx,
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('broken-auth','sms','herosms','Original',0,0,'{}','{',0,0)`); err != nil {
		t.Fatal(err)
	}

	const incomingSecret = "request-secret-must-not-leak"
	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"sms","key":"herosms","display_name":"Changed","enabled":false,
			"config":{"api_key":"`+incomingSecret+`"}}],
		"registrar": {"phoneCountryCode":"US"},
		"registrar_mode": "replace"
	}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("provider write failure status = %d, want 503: %s", code, body)
	}
	responseText := string(body)
	if !strings.Contains(responseText, `"code":"provider_save_failed"`) ||
		!strings.Contains(responseText, `"provider_type":"sms"`) ||
		!strings.Contains(responseText, `"provider_key":"herosms"`) {
		t.Fatalf("provider failure omitted safe identity: %s", body)
	}
	if strings.Contains(responseText, incomingSecret) || strings.Contains(responseText, "auth_json") || strings.Contains(responseText, "invalid character") {
		t.Fatalf("provider failure leaked credential/storage details: %s", body)
	}
	rawRegistrar, ok, err := h.store.GetSetting(ctx, "node_registrar_config")
	if err != nil || !ok || rawRegistrar != originalRegistrar {
		t.Fatalf("failed provider write partially committed registrar: value=%q ok=%v err=%v", rawRegistrar, ok, err)
	}
	var displayName string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT display_name FROM provider_settings WHERE id='broken-auth'`).Scan(&displayName); err != nil {
		t.Fatal(err)
	}
	if displayName != "Original" {
		t.Fatalf("failed provider write partially updated provider: %q", displayName)
	}
}

func TestRegisterProvidersBulkReloadFailureReturnsCommittedWarning(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if _, err := h.store.DB().ExecContext(ctx,
		`INSERT INTO provider_settings(id, provider_type, provider_key, display_name, enabled, priority, config_json, auth_json, created_at, updated_at)
		 VALUES('broken-enabled','sms','broken','Broken',1,100,'{','{}',0,0)`); err != nil {
		t.Fatal(err)
	}

	code, body := grpReq(t, h, http.MethodPost, "/admin/register/providers", `{
		"providers": [{"type":"mailbox","key":"tempmail","display_name":"TempMail","enabled":false}],
		"registrar": {"phoneCountryCode":"CA"},
		"registrar_mode": "merge"
	}`)
	if code != http.StatusOK {
		t.Fatalf("committed save with reload failure status = %d: %s", code, body)
	}
	var response struct {
		Saved          int    `json:"saved"`
		RegistrarSaved bool   `json:"registrar_saved"`
		ReloadOK       bool   `json:"reload_ok"`
		Warning        string `json:"warning"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Saved != 1 || !response.RegistrarSaved || response.ReloadOK || response.Warning != providerReloadWarning {
		t.Fatalf("unexpected committed reload warning response: %#v", response)
	}
	if strings.Contains(string(body), "config_json") || strings.Contains(string(body), "broken-enabled") {
		t.Fatalf("reload warning leaked internal provider details: %s", body)
	}
	if count := providerRowsCount(t, h); count != 2 {
		t.Fatalf("reload failure discarded committed provider row: count=%d", count)
	}
	rawRegistrar, ok, err := h.store.GetSetting(ctx, "node_registrar_config")
	if err != nil || !ok || !strings.Contains(rawRegistrar, `"phoneCountryCode":"CA"`) {
		t.Fatalf("reload failure discarded committed registrar config: value=%q ok=%v err=%v", rawRegistrar, ok, err)
	}
}

func TestSettingsCenterRegistrarCredentialsAreWriteOnly(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	const oldUsername = "registrar-user-must-not-leak"
	const oldPassword = "registrar-password-must-not-leak"
	const oldHeroKey = "hero-api-key-must-not-leak"
	const oldBowerKey = "bower-api-key-must-not-leak"
	const oldRelayToken = "relay-token-must-not-leak"
	if err := h.store.SetSetting(ctx, "node_registrar_config", `{"proxyHost":"old.proxy","proxyUsername":"`+oldUsername+`","proxyPassword":"`+oldPassword+`","heroSmsApiKey":"`+oldHeroKey+`","smsBowerApiKey":"`+oldBowerKey+`","otpRelayToken":"`+oldRelayToken+`"}`); err != nil {
		t.Fatal(err)
	}

	code, body := grpReq(t, h, http.MethodGet, "/admin/settings-center?section=registrar", "")
	if code != http.StatusOK {
		t.Fatalf("registrar settings GET status = %d: %s", code, body)
	}
	if strings.Contains(string(body), oldUsername) || strings.Contains(string(body), oldPassword) ||
		strings.Contains(string(body), oldHeroKey) || strings.Contains(string(body), oldBowerKey) || strings.Contains(string(body), oldRelayToken) ||
		!strings.Contains(string(body), `"proxyUsername_configured":true`) ||
		!strings.Contains(string(body), `"proxyPassword_configured":true`) ||
		!strings.Contains(string(body), `"heroSmsApiKey_configured":true`) ||
		!strings.Contains(string(body), `"smsBowerApiKey_configured":true`) ||
		!strings.Contains(string(body), `"otpRelayToken_configured":true`) {
		t.Fatalf("registrar settings GET did not mask credentials: %s", body)
	}

	code, body = grpReq(t, h, http.MethodPost, "/admin/settings-center", `{
		"section":"registrar","mode":"replace",
		"values":{"proxyHost":"new.proxy","proxyUsername":"","proxyPassword":"","heroSmsApiKey":"","smsBowerApiKey":"","otpRelayToken":""}
	}`)
	if code != http.StatusOK {
		t.Fatalf("registrar blank credential replace status = %d: %s", code, body)
	}
	rawRegistrar, ok, err := h.store.GetSetting(ctx, "node_registrar_config")
	if err != nil || !ok {
		t.Fatalf("read registrar after replace: ok=%v err=%v", ok, err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal([]byte(rawRegistrar), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["proxyHost"] != "new.proxy" || stored["proxyUsername"] != oldUsername || stored["proxyPassword"] != oldPassword ||
		stored["heroSmsApiKey"] != oldHeroKey || stored["smsBowerApiKey"] != oldBowerKey || stored["otpRelayToken"] != oldRelayToken {
		t.Fatalf("blank registrar credential replace did not preserve stored values: %#v", stored)
	}

	const nextPassword = "replacement-password-must-not-leak"
	code, body = grpReq(t, h, http.MethodPost, "/admin/settings-center", `{
		"section":"registrar","mode":"merge","values":{"proxyPassword":"`+nextPassword+`"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("registrar credential update status = %d: %s", code, body)
	}
	if strings.Contains(string(body), oldPassword) || strings.Contains(string(body), nextPassword) ||
		!strings.Contains(string(body), `"configured":true`) || !strings.Contains(string(body), `"masked":"••••"`) {
		t.Fatalf("registrar credential diff leaked or omitted mask metadata: %s", body)
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
