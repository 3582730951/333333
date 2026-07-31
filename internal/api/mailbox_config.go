package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"codex-account-pool/internal/registration/provider/mailbox"
	"codex-account-pool/internal/storage"
)

const (
	cloudflareMailboxAdapter = "cloudflare_temp_email"
	mailboxProbeTimeout      = 20 * time.Second
)

var mailboxProviderKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type cloudflareMailboxRequest struct {
	ProviderKey            string `json:"provider_key"`
	DisplayName            string `json:"display_name"`
	APIURL                 string `json:"api_url"`
	Domain                 string `json:"domain"`
	AdminToken             string `json:"admin_token"`
	Enabled                *bool  `json:"enabled"`
	DefaultForRegistration bool   `json:"default_for_registration"`
	DefaultForTeam         bool   `json:"default_for_team"`
}

type cloudflareMailboxProfile struct {
	ProviderKey            string                        `json:"provider_key"`
	DisplayName            string                        `json:"display_name"`
	APIURL                 string                        `json:"api_url"`
	Domain                 string                        `json:"domain"`
	Enabled                bool                          `json:"enabled"`
	AdminTokenConfigured   bool                          `json:"admin_token_configured"`
	DefaultForRegistration bool                          `json:"default_for_registration"`
	DefaultForTeam         bool                          `json:"default_for_team"`
	Health                 storage.MailboxProviderHealth `json:"health"`
	UpdatedAt              int64                         `json:"updated_at"`
}

func defaultCloudflareMailboxKey(domain string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(domain))))
	return "cf_" + hex.EncodeToString(digest[:6])
}

func validateMailboxEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errors.New("mailbox API URL must be an absolute URL without embedded credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("mailbox API URL must not contain query parameters or a fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return nil, errors.New("mailbox API URL must use HTTPS")
		}
	default:
		return nil, errors.New("mailbox API URL must use HTTPS")
	}
	host := strings.ToLower(strings.Trim(strings.TrimSpace(parsed.Hostname()), "[]"))
	if isLoopbackHost(host) && !strings.EqualFold(parsed.Scheme, "http") {
		return nil, errors.New("loopback mailbox API URLs are accepted only over HTTP for fixture checks")
	}
	if ip := net.ParseIP(host); ip != nil && !isLoopbackHost(host) &&
		(!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return nil, errors.New("mailbox API URL must use a public network address")
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, errors.New("mailbox API URL must not use a local hostname")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func normalizeCloudflareMailboxRequest(req cloudflareMailboxRequest) (cloudflareMailboxRequest, error) {
	req.ProviderKey = strings.ToLower(strings.TrimSpace(req.ProviderKey))
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.AdminToken = strings.TrimSpace(req.AdminToken)
	domain, err := storage.NormalizeMailboxDomain(req.Domain)
	if err != nil || domain == "" {
		if err == nil {
			err = errors.New("mailbox domain is required")
		}
		return req, err
	}
	req.Domain = domain
	parsed, err := validateMailboxEndpoint(req.APIURL)
	if err != nil {
		return req, err
	}
	req.APIURL = strings.TrimRight(parsed.String(), "/")
	if req.ProviderKey == "" {
		req.ProviderKey = defaultCloudflareMailboxKey(domain)
	}
	if !mailboxProviderKeyPattern.MatchString(req.ProviderKey) {
		return req, errors.New("mailbox provider key contains unsupported characters")
	}
	if req.DisplayName == "" {
		req.DisplayName = "Cloudflare · " + domain
	}
	if len(req.DisplayName) > 256 {
		return req, errors.New("mailbox display name exceeds 256 bytes")
	}
	return req, nil
}

func (s *Server) handleCloudflareMailboxConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		s.listCloudflareMailboxProfiles(w, r)
	case http.MethodPost, http.MethodPut:
		s.saveCloudflareMailboxProfile(w, r)
	case http.MethodDelete:
		s.deleteCloudflareMailboxProfile(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleCloudflareMailboxTest(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var req cloudflareMailboxRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var err error
	if strings.TrimSpace(req.ProviderKey) != "" && strings.TrimSpace(req.APIURL) == "" {
		req, err = s.loadCloudflareMailboxRequest(r.Context(), req.ProviderKey)
	} else {
		req, err = normalizeCloudflareMailboxRequest(req)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	started := time.Now()
	probeCtx, cancel := context.WithTimeout(r.Context(), mailboxProbeTimeout)
	defer cancel()
	client := &http.Client{Timeout: mailboxProbeTimeout}
	adapter := mailbox.NewNamedCloudflareTempEmailProvider(
		req.ProviderKey, req.APIURL, req.AdminToken, req.Domain, client,
	)
	address, _, mailboxID, probeErr := adapter.CreateEmail(probeCtx)
	if probeErr == nil && storage.EmailDomain(address) != req.Domain {
		probeErr = errors.New("mailbox provider returned an address outside the configured domain")
	}
	if mailboxID != "" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		_ = adapter.DeleteEmail(cleanupCtx, mailboxID)
		cleanupCancel()
	}
	latency := time.Since(started).Milliseconds()
	errorClass := mailboxProbeErrorClass(probeErr)
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	recordErr := s.store.RecordMailboxProviderHealth(recordCtx, req.ProviderKey, probeErr == nil, latency, errorClass)
	recordCancel()
	if recordErr != nil {
		writeError(w, http.StatusInternalServerError, recordErr)
		return
	}
	if probeErr != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": false, "provider_key": req.ProviderKey, "domain": req.Domain,
			"latency_ms": latency, "error_class": errorClass,
			"message": mailboxProbeMessage(errorClass),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "provider_key": req.ProviderKey, "domain": req.Domain,
		"address_preview": redactMailboxAddress(address), "latency_ms": latency,
		"message": "address creation and domain verification succeeded",
	})
}

func mailboxProbeErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "outside") || strings.Contains(text, "domain"):
		return "domain_mismatch"
	case strings.Contains(text, "http"):
		return "upstream_http"
	case strings.Contains(text, "decode") || strings.Contains(text, "json"):
		return "response_contract"
	default:
		return "connection_failed"
	}
}

func mailboxProbeMessage(class string) string {
	switch class {
	case "timeout":
		return "mailbox endpoint timed out"
	case "network":
		return "mailbox endpoint is unreachable"
	case "domain_mismatch":
		return "created address does not match the configured domain"
	case "upstream_http":
		return "mailbox endpoint rejected address creation"
	case "response_contract":
		return "mailbox endpoint response is incompatible"
	default:
		return "mailbox connection check failed"
	}
}

func redactMailboxAddress(address string) string {
	address = strings.TrimSpace(address)
	index := strings.LastIndexByte(address, '@')
	if index <= 1 {
		return address
	}
	local := address[:index]
	if len(local) > 4 {
		local = local[:2] + "••" + local[len(local)-2:]
	} else {
		local = "••"
	}
	return local + address[index:]
}

func (s *Server) listCloudflareMailboxProfiles(w http.ResponseWriter, r *http.Request) {
	defaultRegistration, _, err := s.store.GetSetting(r.Context(), "reg_default_mailbox")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defaultTeam, _, err := s.store.GetSetting(r.Context(), "team_default_mailbox_provider")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	healthByKey, err := s.store.ListMailboxProviderHealth(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := s.store.ReadDB().QueryContext(r.Context(), `
SELECT provider_key,display_name,enabled,config_json,auth_json,updated_at
FROM provider_settings
WHERE provider_type='mailbox'
ORDER BY priority DESC,display_name,provider_key`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	profiles := make([]cloudflareMailboxProfile, 0)
	for rows.Next() {
		var key, displayName, configJSON, authJSON string
		var enabled int
		var updatedAt int64
		if err := rows.Scan(&key, &displayName, &enabled, &configJSON, &authJSON, &updatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		config := map[string]interface{}{}
		normalizedKey := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		legacyCloudflareKey := normalizedKey == "cloudflare" || normalizedKey == "moemail" ||
			normalizedKey == "freemail" || normalizedKey == "cftempemail" || normalizedKey == "cfworker"
		if json.Unmarshal([]byte(configJSON), &config) != nil ||
			(!strings.EqualFold(strings.TrimSpace(fmt.Sprint(config["adapter"])), cloudflareMailboxAdapter) && !legacyCloudflareKey) {
			continue
		}
		credentials := storage.ProviderAuthMetadata(authJSON)
		profiles = append(profiles, cloudflareMailboxProfile{
			ProviderKey: key, DisplayName: displayName,
			APIURL:                 strings.TrimSpace(fmt.Sprint(config["api_url"])),
			Domain:                 strings.TrimSpace(fmt.Sprint(config["domain"])),
			Enabled:                enabled == 1,
			AdminTokenConfigured:   credentials["admin_token"] != nil,
			DefaultForRegistration: key == defaultRegistration,
			DefaultForTeam:         key == defaultTeam,
			Health:                 healthByKey[key], UpdatedAt: updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"profiles": profiles,
		"defaults": map[string]string{
			"registration": defaultRegistration,
			"team":         defaultTeam,
		},
		"deployment": map[string]interface{}{
			"recommended_adapter": cloudflareMailboxAdapter,
			"steps": []string{
				"Deploy a compatible Cloudflare Email Worker with Email Routing and D1.",
				"Set the Worker HTTPS URL, owned mail domain, and optional admin token here.",
				"Run the connection check, then make the profile the registration and team default.",
			},
			"references": []string{
				"https://github.com/dreamhunter2333/cloudflare_temp_email",
				"https://github.com/agenticmail/agenticmail",
			},
		},
	})
}

func (s *Server) saveCloudflareMailboxProfile(w http.ResponseWriter, r *http.Request) {
	var req cloudflareMailboxRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	normalized, err := normalizeCloudflareMailboxRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	enabled := true
	if normalized.Enabled != nil {
		enabled = *normalized.Enabled
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	var id, existingAuth string
	err = tx.QueryRowContext(r.Context(), `
SELECT id,auth_json FROM provider_settings
WHERE provider_type='mailbox' AND provider_key=?`,
		normalized.ProviderKey,
	).Scan(&id, &existingAuth)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	secrets := map[string]string{}
	if id != "" {
		secrets, err = s.store.OpenProviderAuthJSON("mailbox", normalized.ProviderKey, existingAuth)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if normalized.AdminToken != "" {
		secrets["admin_token"] = normalized.AdminToken
	}
	authJSON, err := s.store.SealProviderAuthJSON("mailbox", normalized.ProviderKey, secrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	configJSON, _ := json.Marshal(map[string]interface{}{
		"adapter":             cloudflareMailboxAdapter,
		"api_url":             normalized.APIURL,
		"domain":              normalized.Domain,
		"custom_domain":       true,
		"same_domain_capable": true,
		"compatibility":       "dreamhunter-v1",
	})
	now := storage.Now()
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	if id == "" {
		id = fmt.Sprintf("prov_%d", time.Now().UnixNano())
		_, err = tx.ExecContext(r.Context(), `
INSERT INTO provider_settings(
 id,provider_type,provider_key,display_name,enabled,priority,config_json,auth_json,created_at,updated_at
) VALUES(?,'mailbox',?,?,?,?,?,?,?,?)`,
			id, normalized.ProviderKey, normalized.DisplayName, enabledInt, 100,
			string(configJSON), authJSON, now, now,
		)
	} else {
		_, err = tx.ExecContext(r.Context(), `
UPDATE provider_settings
SET display_name=?,enabled=?,config_json=?,auth_json=?,updated_at=?
WHERE id=?`,
			normalized.DisplayName, enabledInt, string(configJSON), authJSON, now, id,
		)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if normalized.DefaultForRegistration {
		if _, err = tx.ExecContext(r.Context(), `
INSERT INTO settings(key,value,updated_at) VALUES('reg_default_mailbox',?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
			normalized.ProviderKey, now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else if _, err = tx.ExecContext(r.Context(), `
UPDATE settings SET value='',updated_at=?
WHERE key='reg_default_mailbox' AND value=?`,
		now, normalized.ProviderKey,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if normalized.DefaultForTeam {
		for key, value := range map[string]string{
			"team_default_mailbox_provider": normalized.ProviderKey,
			"team_default_mailbox_domain":   normalized.Domain,
		} {
			if _, err = tx.ExecContext(r.Context(), `
INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
				key, value, now,
			); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	} else {
		result, clearErr := tx.ExecContext(r.Context(), `
UPDATE settings SET value='',updated_at=?
WHERE key='team_default_mailbox_provider' AND value=?`,
			now, normalized.ProviderKey,
		)
		if clearErr != nil {
			writeError(w, http.StatusInternalServerError, clearErr)
			return
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			if _, clearErr = tx.ExecContext(r.Context(), `
UPDATE settings SET value='',updated_at=?
WHERE key='team_default_mailbox_domain'`,
				now,
			); clearErr != nil {
				writeError(w, http.StatusInternalServerError, clearErr)
				return
			}
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.store.InvalidateSettingsCache()
	if s.regHandler != nil {
		if err := s.regHandler.ReloadProviders(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"saved": true, "provider_key": normalized.ProviderKey,
		"domain": normalized.Domain, "admin_token_configured": len(secrets["admin_token"]) > 0,
	})
}

func (s *Server) deleteCloudflareMailboxProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProviderKey string `json:"provider_key"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	key := strings.ToLower(strings.TrimSpace(req.ProviderKey))
	if !mailboxProviderKeyPattern.MatchString(key) {
		writeError(w, http.StatusBadRequest, errors.New("valid mailbox provider key is required"))
		return
	}
	tx, err := s.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(),
		`DELETE FROM provider_settings WHERE provider_type='mailbox' AND provider_key=?`,
		key,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, errors.New("mailbox provider not found"))
		return
	}
	now := storage.Now()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE settings SET value='',updated_at=? WHERE key='reg_default_mailbox' AND value=?`,
		now, key,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	teamDefault, err := tx.ExecContext(r.Context(),
		`UPDATE settings SET value='',updated_at=? WHERE key='team_default_mailbox_provider' AND value=?`,
		now, key,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if affected, _ := teamDefault.RowsAffected(); affected > 0 {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE settings SET value='',updated_at=? WHERE key='team_default_mailbox_domain'`,
			now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.store.InvalidateSettingsCache()
	if s.regHandler != nil {
		_ = s.regHandler.ReloadProviders(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "provider_key": key})
}

func (s *Server) loadCloudflareMailboxRequest(ctx context.Context, providerKey string) (cloudflareMailboxRequest, error) {
	key := strings.ToLower(strings.TrimSpace(providerKey))
	if !mailboxProviderKeyPattern.MatchString(key) {
		return cloudflareMailboxRequest{}, errors.New("valid mailbox provider key is required")
	}
	var displayName, configJSON, authJSON string
	var enabled int
	err := s.store.ReadDB().QueryRowContext(ctx, `
SELECT display_name,enabled,config_json,auth_json
FROM provider_settings
WHERE provider_type='mailbox' AND provider_key=?`,
		key,
	).Scan(&displayName, &enabled, &configJSON, &authJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return cloudflareMailboxRequest{}, errors.New("mailbox provider not found")
	}
	if err != nil {
		return cloudflareMailboxRequest{}, err
	}
	config := map[string]interface{}{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return cloudflareMailboxRequest{}, errors.New("mailbox provider config is invalid")
	}
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(config["adapter"])), cloudflareMailboxAdapter) {
		return cloudflareMailboxRequest{}, errors.New("mailbox provider is not a Cloudflare adapter")
	}
	secrets, err := s.store.OpenProviderAuthJSON("mailbox", key, authJSON)
	if err != nil {
		return cloudflareMailboxRequest{}, err
	}
	enabledBool := enabled == 1
	return normalizeCloudflareMailboxRequest(cloudflareMailboxRequest{
		ProviderKey: key, DisplayName: displayName,
		APIURL: fmt.Sprint(config["api_url"]), Domain: fmt.Sprint(config["domain"]),
		AdminToken: secrets["admin_token"], Enabled: &enabledBool,
	})
}

func (s *Server) mailboxProviderDomain(ctx context.Context, providerKey string) (string, bool, error) {
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	if providerKey == "" {
		return "", false, nil
	}
	var configJSON string
	err := s.store.ReadDB().QueryRowContext(ctx, `
SELECT config_json FROM provider_settings
WHERE provider_type='mailbox' AND provider_key=? AND enabled=1`,
		providerKey,
	).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	config := map[string]interface{}{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return "", false, errors.New("mailbox provider config is invalid")
	}
	domain, err := storage.NormalizeMailboxDomain(fmt.Sprint(config["domain"]))
	if err != nil || domain == "" {
		return "", false, err
	}
	return domain, true, nil
}
