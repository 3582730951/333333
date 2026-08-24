package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/cursorproxy"
)

func TestCursorAPIKeyAndBrowserAccountImports(t *testing.T) {
	accountsRoot := t.TempDir()
	t.Setenv("CODEX_CURSOR_ACCOUNTS_DIR", accountsRoot)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-cursor", `{
  "auth_method":"api_key", "api_key":"cursor-key-test", "label":"Cursor key"
}`)
	if code != http.StatusOK {
		t.Fatalf("Cursor API-key import = %d: %s", code, raw)
	}
	provider, found, err := h.store.GetCustomProvider(t.Context(), cursorproxy.ProviderID)
	if err != nil || !found || !provider.Enabled || provider.UpstreamProtocol != "responses" {
		t.Fatalf("Cursor provider = %+v found=%v err=%v", provider, found, err)
	}
	accounts, err := h.store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].Provider != cursorproxy.ProviderID || accounts[0].GroupName != "cursor" {
		t.Fatalf("Cursor API-key account = %+v err=%v", accounts, err)
	}
	token, err := h.store.GetToken(t.Context(), accounts[0].ID)
	if err != nil || accountprovider.EffectiveAuthMethod(cursorproxy.ProviderID, token) != accountprovider.AuthMethodAPIKey || token.OpenAIAPIKey != "cursor-key-test" || token.AccessToken == "" || token.AccessToken == token.OpenAIAPIKey {
		t.Fatalf("Cursor API-key token = %+v err=%v", token, err)
	}

	configDir := filepath.Join(accountsRoot, "browser-main")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "cli-config.json"), []byte(`{"authInfo":{"email":"cursor@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/accounts/import-cursor", `{
  "auth_method":"browser_account", "account_name":"browser-main", "group_name":"cursor-team"
}`)
	if code != http.StatusOK {
		t.Fatalf("Cursor browser-account import = %d: %s", code, raw)
	}
	accounts, err = h.store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("Cursor accounts = %+v err=%v", accounts, err)
	}
	var browserID string
	for _, account := range accounts {
		if account.GroupName == "cursor-team" {
			browserID = account.ID
		}
	}
	token, err = h.store.GetToken(t.Context(), browserID)
	if err != nil || token.CredentialMode != cursorproxy.CredentialBrowser || token.AgentRuntimeID != configDir || token.AccessToken == "" {
		t.Fatalf("Cursor browser token = %+v err=%v", token, err)
	}
}

func TestCursorImportNeverAcceptsPassword(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _ := grpReq(t, h, http.MethodPost, "/admin/accounts/import-cursor", `{
  "auth_method":"browser_account", "account_name":"main", "password":"must-not-store"
}`)
	if code != http.StatusBadRequest {
		t.Fatalf("password import status = %d, want 400", code)
	}
}

func TestCursorLoopbackHopNeverUsesAccountEgress(t *testing.T) {
	egress := cursorLoopbackEgress()
	if egress.ID != "cursor-loopback" || egress.Type != "direct" || egress.Endpoint != "" || egress.ChainProxy != "" {
		t.Fatalf("Cursor loopback egress = %+v", egress)
	}
}
