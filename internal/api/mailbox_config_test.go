package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/secretbox"
)

func TestCloudflareMailboxProfileEncryptedDefaultsAndProbe(t *testing.T) {
	const adminSecret = "cloudflare-admin-secret"
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/new_address" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-admin-auth") != adminSecret {
			t.Errorf("worker admin token=%q", r.Header.Get("x-admin-auth"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"jwt": "fixture-mailbox-jwt", "address": "fixturechild@example.test",
		})
	}))
	defer worker.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.AdminToken = "mailbox-config-admin"
	h.store.SetTokenEncryptionKey([]byte("mailbox-config-storage-key"))
	client := jarClient(t)
	headers := map[string]string{"Authorization": "Bearer mailbox-config-admin"}
	saveBody := `{
		"provider_key":"cf_team",
		"display_name":"Team Mail",
		"api_url":` + mailboxTestJSONString(worker.URL) + `,
		"domain":"Example.Test.",
		"admin_token":"` + adminSecret + `",
		"enabled":true,
		"default_for_registration":true,
		"default_for_team":true
	}`
	resp, saved := doReq(t, client, http.MethodPost, h.pool.URL+"/admin/email-pool/cloudflare", saveBody, headers)
	if resp.StatusCode != http.StatusOK || saved["provider_key"] != "cf_team" {
		t.Fatalf("save status=%d body=%v", resp.StatusCode, saved)
	}

	var authJSON string
	if err := h.store.DB().QueryRow(`
SELECT auth_json FROM provider_settings
WHERE provider_type='mailbox' AND provider_key='cf_team'`).Scan(&authJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(authJSON, adminSecret) || !strings.Contains(authJSON, secretbox.Prefix) {
		t.Fatalf("mailbox credential was not encrypted at rest: %s", authJSON)
	}
	for key, expected := range map[string]string{
		"reg_default_mailbox":           "cf_team",
		"team_default_mailbox_provider": "cf_team",
		"team_default_mailbox_domain":   "example.test",
	} {
		value, _, err := h.store.GetSetting(context.Background(), key)
		if err != nil || value != expected {
			t.Fatalf("setting %s=%q err=%v want=%q", key, value, err, expected)
		}
	}

	resp, probe := doReq(t, client, http.MethodPost, h.pool.URL+"/admin/email-pool/cloudflare/test", `{"provider_key":"cf_team"}`, headers)
	if resp.StatusCode != http.StatusOK || probe["ok"] != true ||
		strings.Contains(stringValue(probe["address_preview"]), "fixturechild") {
		t.Fatalf("probe status=%d body=%v", resp.StatusCode, probe)
	}
	resp, listed := doReq(t, client, http.MethodGet, h.pool.URL+"/admin/email-pool/cloudflare", "", headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%v", resp.StatusCode, listed)
	}
	raw, _ := json.Marshal(listed)
	if strings.Contains(string(raw), adminSecret) || !strings.Contains(string(raw), `"last_status":"healthy"`) {
		t.Fatalf("profile list leaked a secret or omitted health: %s", raw)
	}
}

func TestCloudflareMailboxUnsettingAndDeleteClearDefaults(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.AdminToken = "mailbox-config-admin"
	client := jarClient(t)
	headers := map[string]string{"Authorization": "Bearer mailbox-config-admin"}
	base := `{
		"provider_key":"cf_defaults",
		"display_name":"Defaults",
		"api_url":"http://127.0.0.1:8080",
		"domain":"example.test",
		"enabled":true,
		"default_for_registration":true,
		"default_for_team":true
	}`
	if resp, body := doReq(t, client, http.MethodPost, h.pool.URL+"/admin/email-pool/cloudflare", base, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("save status=%d body=%v", resp.StatusCode, body)
	}
	withoutDefaults := strings.ReplaceAll(strings.ReplaceAll(
		base, `"default_for_registration":true`, `"default_for_registration":false`,
	), `"default_for_team":true`, `"default_for_team":false`)
	if resp, body := doReq(t, client, http.MethodPut, h.pool.URL+"/admin/email-pool/cloudflare", withoutDefaults, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("unset status=%d body=%v", resp.StatusCode, body)
	}
	for _, key := range []string{
		"reg_default_mailbox", "team_default_mailbox_provider", "team_default_mailbox_domain",
	} {
		value, _, err := h.store.GetSetting(context.Background(), key)
		if err != nil || value != "" {
			t.Fatalf("setting %s=%q err=%v, want cleared", key, value, err)
		}
	}

	// Re-enable defaults and prove profile deletion clears the coupled team domain.
	if resp, body := doReq(t, client, http.MethodPut, h.pool.URL+"/admin/email-pool/cloudflare", base, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d body=%v", resp.StatusCode, body)
	}
	if resp, body := doReq(t, client, http.MethodDelete, h.pool.URL+"/admin/email-pool/cloudflare", `{"provider_key":"cf_defaults"}`, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d body=%v", resp.StatusCode, body)
	}
	for _, key := range []string{
		"reg_default_mailbox", "team_default_mailbox_provider", "team_default_mailbox_domain",
	} {
		value, _, err := h.store.GetSetting(context.Background(), key)
		if err != nil || value != "" {
			t.Fatalf("post-delete setting %s=%q err=%v", key, value, err)
		}
	}
}

func mailboxTestJSONString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
