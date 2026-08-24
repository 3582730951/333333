package api

import (
	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/cursorproxy"
	"codex-account-pool/internal/storage"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var cursorAccountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) adminCursorModule(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	installError := ""
	if err := s.cursorProxy.Available(); err != nil {
		installError = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": cursorproxy.ProviderID, "installed": installError == "", "install_error": installError,
		"reference_project": cursorproxy.ReferenceProject, "reference_version": cursorproxy.ReferenceVersion,
		"reference_commit": cursorproxy.ReferenceCommit, "reviewed_head_commit": cursorproxy.ReviewedHeadCommit,
		"accounts_dir":  cursorAccountsDir(),
		"login_command": "cursor-api-proxy account login <account-name>",
		"auth_modes":    []string{"browser_account", "api_key"},
	})
}

func (s *Server) adminImportCursor(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		AuthMethod  string `json:"auth_method"`
		APIKey      string `json:"api_key"`
		AccountName string `json:"account_name"`
		Password    string `json:"password"`
		Label       string `json:"label"`
		GroupName   string `json:"group_name"`
		EgressID    string `json:"egress_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Password) != "" {
		writeError(w, http.StatusBadRequest, errors.New("Cursor passwords are entered only on Cursor's official browser login page and are never accepted or stored by the pool"))
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	if mode == "account" {
		mode = "browser_account"
	}
	var token storage.AccountToken
	var identity string
	var authLabel string
	switch mode {
	case "api_key":
		key := strings.TrimSpace(req.APIKey)
		if key == "" {
			writeError(w, http.StatusBadRequest, errors.New("api_key is required"))
			return
		}
		bridgeKey, err := newCursorBridgeKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		identity = key
		token = storage.AccountToken{AuthMethod: accountprovider.AuthMethodAPIKey, AccessToken: bridgeKey, OpenAIAPIKey: key, LastRefresh: storage.Now()}
		authLabel = "Cursor API Key"
	case "browser_account":
		name := strings.TrimSpace(req.AccountName)
		if !cursorAccountNamePattern.MatchString(name) {
			writeError(w, http.StatusBadRequest, errors.New("account_name must contain only letters, numbers, dot, underscore, or dash"))
			return
		}
		configDir := filepath.Join(cursorAccountsDir(), name)
		if err := validateCursorAccountConfig(configDir); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		bridgeKey, err := newCursorBridgeKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		identity = configDir
		token = storage.AccountToken{AuthMethod: accountprovider.AuthMethodAccessToken, CredentialMode: cursorproxy.CredentialBrowser,
			AccessToken: bridgeKey, AgentRuntimeID: configDir, LastRefresh: storage.Now()}
		authLabel = "Cursor Account " + name
	default:
		writeError(w, http.StatusBadRequest, errors.New("auth_method must be browser_account or api_key"))
		return
	}
	if err := s.ensureCursorProvider(r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	digest := sha256.Sum256([]byte(mode + "\x00" + identity))
	accountID := "cursor-" + hex.EncodeToString(digest[:])[:24]
	group := strings.TrimSpace(req.GroupName)
	if group == "" {
		group = "cursor"
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = authLabel
	}
	account := storage.Account{ID: accountID, Label: label, GroupName: group, Provider: cursorproxy.ProviderID, PlanType: "cursor", Status: "active"}
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bindImportedAccountPrimaryEgress(r.Context(), accountID, req.EgressID); err != nil {
		_ = s.store.DeleteAccount(r.Context(), accountID)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.seedImportedAccountCapabilities(r.Context(), account); err != nil {
		_ = s.store.DeleteAccount(r.Context(), accountID)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": account.ID, "label": account.Label, "group_name": account.GroupName,
		"provider": account.Provider, "auth_method": token.AuthMethod,
		"billing_mode":    accountprovider.BillingMode(account.Provider, token),
		"api_key_present": mode == "api_key", "ready": true,
	})
}

func (s *Server) ensureCursorProvider(r *http.Request) error {
	if existing, found, err := s.store.GetCustomProvider(r.Context(), cursorproxy.ProviderID); err != nil {
		return err
	} else if found {
		if existing.Enabled {
			return nil
		}
		existing.Enabled = true
		return s.store.UpsertCustomProvider(r.Context(), existing)
	}
	return s.store.UpsertCustomProvider(r.Context(), storage.CustomProvider{
		ID: cursorproxy.ProviderID, Name: "Cursor Agent Proxy",
		// The request path replaces this documented local endpoint with the
		// selected account's lazy sidecar URL before opening the wire request.
		BaseURL:          "http://127.0.0.1:8765/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		TransportProfile: storage.CustomProviderTransportGeneric,
		Enabled:          true, AutoDiscoverModels: false,
		Models:        []string{"auto", "default"},
		ModelMappings: map[string]string{"cursor": "auto", "cursor/auto": "auto", "cursor-auto": "auto"},
	})
}

func cursorAccountsDir() string {
	if configured := strings.TrimSpace(os.Getenv("CODEX_CURSOR_ACCOUNTS_DIR")); configured != "" {
		return filepath.Clean(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Clean(".cursor-api-proxy/accounts")
	}
	return filepath.Join(home, ".cursor-api-proxy", "accounts")
}

func validateCursorAccountConfig(configDir string) error {
	root := filepath.Clean(cursorAccountsDir())
	clean := filepath.Clean(configDir)
	relative, err := filepath.Rel(root, clean)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return errors.New("Cursor account directory is outside the managed accounts root")
	}
	info, err := os.Stat(filepath.Join(clean, "cli-config.json"))
	if err != nil || info.IsDir() {
		return errors.New("Cursor account is not logged in; run cursor-api-proxy account login <account-name> first")
	}
	return nil
}

func newCursorBridgeKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "cursor-bridge-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func cursorProxyEgressURL(egress storage.EgressProfile) string {
	if strings.EqualFold(strings.TrimSpace(egress.Type), storage.CurlCFFISidecarEgressType) {
		return strings.TrimSpace(egress.ChainProxy)
	}
	switch strings.ToLower(strings.TrimSpace(egress.Type)) {
	case "", "direct":
		return ""
	default:
		return strings.TrimSpace(egress.Endpoint)
	}
}

// cursorLoopbackEgress is used only for the pool -> local bridge hop. The
// selected account egress is already passed to the child process through
// cursorProxyEgressURL; reusing it here would proxy 127.0.0.1 through an
// external HTTP/SOCKS/sidecar endpoint and either break the request or expose
// the bridge address and secret outside the host.
func cursorLoopbackEgress() storage.EgressProfile {
	return storage.EgressProfile{ID: "cursor-loopback", Type: "direct"}
}
