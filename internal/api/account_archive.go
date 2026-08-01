package api

import (
	"archive/zip"
	"bytes"
	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/storage"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	accountBackupDocumentType      = "codex-account-pool-account"
	accountBackupDocumentVersion   = 1
	accountArchiveQueryBatchSize   = 500
	accountArchiveMaxUploadBytes   = 64 << 20
	accountArchiveMaxFormOverhead  = 1 << 20
	accountArchiveMaxJSONBytes     = 16 << 20
	accountArchiveMaxExpandedBytes = 256 << 20
	accountArchiveMaxFiles         = 50_000
)

var (
	errGeneratedAccountJSONTooLarge    = errors.New("generated account JSON exceeds the 16 MiB per-file import limit")
	errGeneratedAccountArchiveTooLarge = errors.New("generated account archive exceeds the 64 MiB import file limit")
)

type accountArchiveBoundedWriter struct {
	dst       io.Writer
	remaining int64
	err       error
}

func (w *accountArchiveBoundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type accountBackupTokenV1 struct {
	AuthMethod         string `json:"auth_method,omitempty"`
	CredentialMode     string `json:"credential_mode,omitempty"`
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	OpenAIAPIKey       string `json:"openai_api_key,omitempty"`
	IDTokenRaw         string `json:"id_token_raw,omitempty"`
	AgentRuntimeID     string `json:"agent_runtime_id,omitempty"`
	AgentPrivateKey    string `json:"agent_private_key,omitempty"`
	AgentTaskID        string `json:"agent_task_id,omitempty"`
	LastRefresh        int64  `json:"last_refresh,omitempty"`
	ExpiresAt          int64  `json:"expires_at,omitempty"`
	Scopes             string `json:"scopes,omitempty"`
	OAuthRateLimitTier string `json:"oauth_rate_limit_tier,omitempty"`
	CreatedAt          int64  `json:"created_at,omitempty"`
	UpdatedAt          int64  `json:"updated_at,omitempty"`
}

type accountBackupKiroCredentialsV1 struct {
	AuthMethod     string `json:"auth_method"`
	ClientID       string `json:"client_id,omitempty"`
	ClientSecret   string `json:"client_secret,omitempty"`
	ProfileARN     string `json:"profile_arn,omitempty"`
	AuthRegion     string `json:"auth_region,omitempty"`
	APIRegion      string `json:"api_region,omitempty"`
	MachineID      string `json:"machine_id,omitempty"`
	KiroAPIKey     string `json:"kiro_api_key,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	CredentialHash string `json:"credential_hash,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`
}

type accountBackupAntigravityCredentialsV1 struct {
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	CreatedAt    int64  `json:"created_at,omitempty"`
	UpdatedAt    int64  `json:"updated_at,omitempty"`
}

// accountBackupDocumentV1 deliberately emits plaintext credentials because it
// is a portable account backup. The endpoint is admin-only, sends no-store
// headers, and never logs the document.
type accountBackupDocumentV1 struct {
	Type                   string                                 `json:"type"`
	Version                int                                    `json:"version"`
	ExportedAt             int64                                  `json:"exported_at"`
	Account                storage.Account                        `json:"account"`
	Token                  accountBackupTokenV1                   `json:"token"`
	CustomProvider         *storage.CustomProvider                `json:"custom_provider,omitempty"`
	EgressProfiles         []storage.EgressProfile                `json:"egress_profiles,omitempty"`
	KiroCredentials        *accountBackupKiroCredentialsV1        `json:"kiro_credentials,omitempty"`
	AntigravityCredentials *accountBackupAntigravityCredentialsV1 `json:"antigravity_credentials,omitempty"`
	SessionCookie          *string                                `json:"session_cookie,omitempty"`
	InjectedCookies        []storage.InjectedCookie               `json:"injected_cookies,omitempty"`
	EgressBinding          *storage.AccountEgressBinding          `json:"egress_binding,omitempty"`
	Capabilities           []storage.ModelCapability              `json:"model_capabilities,omitempty"`
	ModelCatalogStatus     *storage.AccountModelCatalogStatus     `json:"model_catalog_status,omitempty"`
	CodexReauthConfig      *storage.AccountCodexReauthConfig      `json:"codex_reauth_config,omitempty"`
	GroupMemberships       []storage.AccountGroupMembership       `json:"group_memberships,omitempty"`
}

func backupDocumentFromStorage(backup storage.AccountBackup, exportedAt int64) accountBackupDocumentV1 {
	document := accountBackupDocumentV1{
		Type:               accountBackupDocumentType,
		Version:            accountBackupDocumentVersion,
		ExportedAt:         exportedAt,
		Account:            backup.Account,
		Token:              tokenToBackupV1(backup.Token),
		CustomProvider:     backup.CustomProvider,
		EgressProfiles:     backup.EgressProfiles,
		SessionCookie:      backup.SessionCookie,
		InjectedCookies:    backup.InjectedCookies,
		EgressBinding:      backup.EgressBinding,
		Capabilities:       backup.Capabilities,
		ModelCatalogStatus: backup.ModelCatalogStatus,
		CodexReauthConfig:  backup.CodexReauthConfig,
		GroupMemberships:   backup.GroupMemberships,
	}
	if item := backup.KiroCredentials; item != nil {
		document.KiroCredentials = &accountBackupKiroCredentialsV1{
			AuthMethod: item.AuthMethod, ClientID: item.ClientID, ClientSecret: item.ClientSecret,
			ProfileARN: item.ProfileARN, AuthRegion: item.AuthRegion, APIRegion: item.APIRegion,
			MachineID: item.MachineID, KiroAPIKey: item.KiroAPIKey, Endpoint: item.Endpoint,
			CredentialHash: item.CredentialHash, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
	}
	if item := backup.AntigravityCredentials; item != nil {
		document.AntigravityCredentials = &accountBackupAntigravityCredentialsV1{
			Email: item.Email, ProjectID: item.ProjectID, AccessToken: item.AccessToken,
			RefreshToken: item.RefreshToken, ExpiresAt: item.ExpiresAt, BaseURL: item.BaseURL,
			UserAgent: item.UserAgent, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
	}
	return document
}

func tokenToBackupV1(token storage.AccountToken) accountBackupTokenV1 {
	return accountBackupTokenV1{
		AuthMethod: token.AuthMethod, CredentialMode: token.CredentialMode,
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		OpenAIAPIKey: token.OpenAIAPIKey, IDTokenRaw: token.IDTokenRaw,
		AgentRuntimeID: token.AgentRuntimeID, AgentPrivateKey: token.AgentPrivateKey,
		AgentTaskID: token.AgentTaskID, LastRefresh: token.LastRefresh,
		ExpiresAt: token.ExpiresAt, Scopes: token.Scopes,
		OAuthRateLimitTier: token.OAuthRateLimitTier,
		CreatedAt:          token.CreatedAt, UpdatedAt: token.UpdatedAt,
	}
}

func tokenFromBackupV1(accountID string, token accountBackupTokenV1) storage.AccountToken {
	return storage.AccountToken{
		AccountID: accountID, AuthMethod: token.AuthMethod, CredentialMode: token.CredentialMode,
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		OpenAIAPIKey: token.OpenAIAPIKey, IDTokenRaw: token.IDTokenRaw,
		AgentRuntimeID: token.AgentRuntimeID, AgentPrivateKey: token.AgentPrivateKey,
		AgentTaskID: token.AgentTaskID, LastRefresh: token.LastRefresh,
		ExpiresAt: token.ExpiresAt, Scopes: token.Scopes,
		OAuthRateLimitTier: token.OAuthRateLimitTier,
		CreatedAt:          token.CreatedAt, UpdatedAt: token.UpdatedAt,
	}
}

func (document accountBackupDocumentV1) storageBackup() storage.AccountBackup {
	accountID := strings.TrimSpace(document.Account.ID)
	backup := storage.AccountBackup{
		Account: document.Account, Token: tokenFromBackupV1(accountID, document.Token),
		CustomProvider: document.CustomProvider, EgressProfiles: document.EgressProfiles,
		SessionCookie: document.SessionCookie, InjectedCookies: document.InjectedCookies,
		EgressBinding: document.EgressBinding, Capabilities: document.Capabilities,
		ModelCatalogStatus: document.ModelCatalogStatus,
		CodexReauthConfig:  document.CodexReauthConfig,
		GroupMemberships:   document.GroupMemberships,
	}
	if item := document.KiroCredentials; item != nil {
		backup.KiroCredentials = &storage.KiroCredentials{
			AccountID: accountID, AuthMethod: item.AuthMethod, ClientID: item.ClientID,
			ClientSecret: item.ClientSecret, ProfileARN: item.ProfileARN,
			AuthRegion: item.AuthRegion, APIRegion: item.APIRegion, MachineID: item.MachineID,
			KiroAPIKey: item.KiroAPIKey, Endpoint: item.Endpoint,
			CredentialHash: item.CredentialHash, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
	}
	if item := document.AntigravityCredentials; item != nil {
		backup.AntigravityCredentials = &storage.AntigravityCredentials{
			AccountID: accountID, Email: item.Email, ProjectID: item.ProjectID,
			AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
			ExpiresAt: item.ExpiresAt, BaseURL: item.BaseURL, UserAgent: item.UserAgent,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
	}
	return backup
}

func requestedAccountBackupIDs(r *http.Request) []string {
	values := append([]string(nil), r.URL.Query()["id"]...)
	values = append(values, r.URL.Query()["ids"]...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if _, duplicate := seen[item]; duplicate {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

func accountArchiveIDBatches(accountIDs []string) [][]string {
	if len(accountIDs) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(accountIDs)+accountArchiveQueryBatchSize-1)/accountArchiveQueryBatchSize)
	for start := 0; start < len(accountIDs); start += accountArchiveQueryBatchSize {
		end := start + accountArchiveQueryBatchSize
		if end > len(accountIDs) {
			end = len(accountIDs)
		}
		batches = append(batches, accountIDs[start:end])
	}
	return batches
}

func (s *Server) listAccountArchiveAccountsByIDs(r *http.Request, accountIDs []string) (map[string]storage.Account, error) {
	out := make(map[string]storage.Account, len(accountIDs))
	for _, batch := range accountArchiveIDBatches(accountIDs) {
		items, err := s.store.ListAccountsByIDs(r.Context(), batch)
		if err != nil {
			return nil, err
		}
		for id, item := range items {
			out[id] = item
		}
	}
	return out, nil
}

func encodeAccountBackupDocument(backup storage.AccountBackup, exportedAt int64) ([]byte, error) {
	var document bytes.Buffer
	writer := &accountArchiveBoundedWriter{
		dst:       &document,
		remaining: accountArchiveMaxJSONBytes,
		err:       errGeneratedAccountJSONTooLarge,
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(backupDocumentFromStorage(backup, exportedAt)); err != nil {
		return nil, err
	}
	return document.Bytes(), nil
}

func (s *Server) writeAccountsBackupDownload(w http.ResponseWriter, r *http.Request) {
	requestedIDs := requestedAccountBackupIDs(r)
	var accounts []storage.Account
	if len(requestedIDs) == 0 {
		items, err := s.store.ListAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		accounts = items
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	} else {
		items, err := s.listAccountArchiveAccountsByIDs(r, requestedIDs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		accounts = make([]storage.Account, 0, len(requestedIDs))
		for _, id := range requestedIDs {
			item, found := items[id]
			if !found {
				writeError(w, http.StatusNotFound, fmt.Errorf("account %q not found", id))
				return
			}
			accounts = append(accounts, item)
		}
	}
	if len(accounts) == 0 {
		writeError(w, http.StatusNotFound, errors.New("account pool is empty"))
		return
	}
	if len(accounts) > accountArchiveMaxFiles {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("account pool contains more than %d exportable accounts", accountArchiveMaxFiles))
		return
	}
	exportedAt := storage.Now()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if len(accounts) == 1 {
		backups, err := s.store.ExportAccountBackups(r.Context(), accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		raw, err := encodeAccountBackupDocument(backups[0], exportedAt)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errGeneratedAccountJSONTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(w, status, err)
			return
		}
		filename := accountBackupFilename(accounts[0], 0)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(filename))
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	archive, err := os.CreateTemp("", "codex-account-pool-accounts-*.zip")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	archiveName := archive.Name()
	if err := archive.Chmod(0o600); err != nil {
		_ = archive.Close()
		_ = os.Remove(archiveName)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// On Unix the open descriptor remains usable after unlink. Remove the
	// plaintext credential archive from the shared temporary directory
	// immediately; platforms that do not allow unlink-open fall back to the
	// deferred cleanup below.
	if err := os.Remove(archiveName); err == nil {
		archiveName = ""
	}
	defer func() {
		_ = archive.Close()
		if archiveName != "" {
			_ = os.Remove(archiveName)
		}
	}()
	archiveWriter := &accountArchiveBoundedWriter{
		dst: archive, remaining: accountArchiveMaxUploadBytes,
		err: errGeneratedAccountArchiveTooLarge,
	}
	zw := zip.NewWriter(archiveWriter)
	usedNames := make(map[string]struct{}, len(accounts))
	providerDefinitions := make(map[string]storage.CustomProvider)
	egressDefinitions := make(map[string]storage.EgressProfile)
	expandedBytes := int64(0)
	for batchStart := 0; batchStart < len(accounts); batchStart += accountArchiveQueryBatchSize {
		batchEnd := batchStart + accountArchiveQueryBatchSize
		if batchEnd > len(accounts) {
			batchEnd = len(accounts)
		}
		backups, exportErr := s.store.ExportAccountBackups(r.Context(), accounts[batchStart:batchEnd])
		if exportErr != nil {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, exportErr)
			return
		}
		if len(backups) != batchEnd-batchStart {
			_ = zw.Close()
			writeError(w, http.StatusInternalServerError, errors.New("account backup export returned an incomplete batch"))
			return
		}
		// ExportAccountBackups intentionally loads account rows in bounded batches.
		// Pin each repeated global definition to its first observation so a concurrent
		// model-discovery or egress-health update cannot make one ZIP contradict itself.
		for backupIndex := range backups {
			backup := &backups[backupIndex]
			if provider := backup.CustomProvider; provider != nil {
				if pinned, found := providerDefinitions[provider.ID]; found {
					copy := pinned
					backup.CustomProvider = &copy
				} else {
					providerDefinitions[provider.ID] = *provider
				}
			}
			for _, profile := range backup.EgressProfiles {
				if _, found := egressDefinitions[profile.ID]; !found {
					egressDefinitions[profile.ID] = profile
				}
			}
			referencedEgresses := make(map[string]struct{})
			addEgress := func(rawID string) {
				if id := strings.TrimSpace(rawID); id != "" {
					referencedEgresses[id] = struct{}{}
				}
			}
			if binding := backup.EgressBinding; binding != nil {
				addEgress(binding.PrimaryEgressID)
				for _, id := range binding.StandbyIDs() {
					addEgress(id)
				}
				addEgress(binding.SidecarEgressID)
			}
			for _, cookie := range backup.InjectedCookies {
				addEgress(cookie.EgressID)
			}
			if provider := backup.CustomProvider; provider != nil {
				for _, id := range provider.EgressIDs {
					addEgress(id)
				}
			}
			egressIDs := make([]string, 0, len(referencedEgresses))
			for id := range referencedEgresses {
				egressIDs = append(egressIDs, id)
			}
			sort.Strings(egressIDs)
			backup.EgressProfiles = backup.EgressProfiles[:0]
			for _, id := range egressIDs {
				profile, found := egressDefinitions[id]
				if !found {
					_ = zw.Close()
					writeError(w, http.StatusInternalServerError, fmt.Errorf("portable egress definition %q disappeared during export", id))
					return
				}
				backup.EgressProfiles = append(backup.EgressProfiles, profile)
			}
		}
		for batchIndex, backup := range backups {
			index := batchStart + batchIndex
			raw, encodeErr := encodeAccountBackupDocument(backup, exportedAt)
			if encodeErr != nil {
				_ = zw.Close()
				status := http.StatusInternalServerError
				if errors.Is(encodeErr, errGeneratedAccountJSONTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				writeError(w, status, encodeErr)
				return
			}
			expandedBytes += int64(len(raw))
			if expandedBytes > accountArchiveMaxExpandedBytes {
				_ = zw.Close()
				writeError(w, http.StatusRequestEntityTooLarge, errors.New("generated account archive exceeds the 256 MiB expanded import limit"))
				return
			}
			name := accountBackupFilename(accounts[index], index)
			if _, exists := usedNames[name]; exists {
				name = strings.TrimSuffix(name, ".json") + "-" + strconv.Itoa(index+1) + ".json"
			}
			usedNames[name] = struct{}{}
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetModTime(time.Unix(exportedAt, 0).UTC())
			entry, createErr := zw.CreateHeader(header)
			if createErr != nil {
				_ = zw.Close()
				status := http.StatusInternalServerError
				if errors.Is(createErr, errGeneratedAccountArchiveTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				writeError(w, status, createErr)
				return
			}
			if _, writeErr := entry.Write(raw); writeErr != nil {
				_ = zw.Close()
				status := http.StatusInternalServerError
				if errors.Is(writeErr, errGeneratedAccountArchiveTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				writeError(w, status, writeErr)
				return
			}
		}
	}
	if err := zw.Close(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errGeneratedAccountArchiveTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err)
		return
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispositionAttachment("account-pool-"+time.Unix(exportedAt, 0).UTC().Format("20060102-150405")+".zip"))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, archive)
}

func contentDispositionAttachment(filename string) string {
	filename = strings.NewReplacer(`"`, "_", "\r", "_", "\n", "_").Replace(filename)
	return mime.FormatMediaType("attachment", map[string]string{"filename": filename})
}

func accountBackupFilename(account storage.Account, index int) string {
	value := strings.TrimSpace(account.ID)
	if value == "" {
		value = strings.TrimSpace(account.Label)
	}
	var name strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			name.WriteRune(r)
		} else {
			name.WriteByte('_')
		}
		if name.Len() >= 96 {
			break
		}
	}
	safe := strings.Trim(name.String(), "._-")
	if safe == "" {
		safe = "account-" + strconv.Itoa(index+1)
	}
	return "account-" + safe + ".json"
}

func (s *Server) adminAccountsArchiveImport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	raw, filename, err := readAccountArchiveUpload(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	payloads, zipped, err := accountArchivePayloads(raw, filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	backups := make([]storage.AccountBackup, 0, len(payloads))
	formats := make(map[string]struct{})
	for index, payload := range payloads {
		items, format, err := s.decodeAccountImportPayload(payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("file %d: %w", index+1, err))
			return
		}
		formats[format] = struct{}{}
		backups = append(backups, items...)
	}
	if err := s.validateImportedAccountBackups(backups); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ids := make([]string, 0, len(backups))
	for _, backup := range backups {
		ids = append(ids, backup.Account.ID)
	}
	existing, err := s.listAccountArchiveAccountsByIDs(r, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.RestoreAccountBackups(r.Context(), backups); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
		s.scheduler.InvalidateEgressCache()
		s.scheduler.NotifyStateChanged()
	}
	replaced := 0
	resultAccounts := make([]map[string]string, 0, len(backups))
	for _, backup := range backups {
		if _, found := existing[backup.Account.ID]; found {
			replaced++
		}
		resultAccounts = append(resultAccounts, map[string]string{
			"id": backup.Account.ID, "label": backup.Account.Label,
			"provider": backup.Account.Provider, "status": "imported",
		})
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
			AccountID: backup.Account.ID, AccountLabel: backup.Account.Label,
			Action: "account_backup_import", State: "active",
			Reason: "complete account restore", Detail: "source=" + path.Base(filename),
		})
	}
	formatNames := make([]string, 0, len(formats))
	for name := range formats {
		formatNames = append(formatNames, name)
	}
	sort.Strings(formatNames)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"recognized": len(backups), "imported": len(backups) - replaced,
		"replaced": replaced, "files": len(payloads), "zip": zipped,
		"formats": formatNames, "accounts": resultAccounts,
	})
}

func readAccountArchiveUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, error) {
	return readAccountArchiveUploadWithLimits(w, r, accountArchiveMaxUploadBytes, accountArchiveMaxFormOverhead)
}

func readAccountArchiveUploadWithLimits(w http.ResponseWriter, r *http.Request, maxFileBytes, maxFormOverhead int64) ([]byte, string, error) {
	// The portable file itself may be exactly 64 MiB. Bound the multipart
	// envelope separately so normal boundaries and headers do not consume the
	// file allowance while hostile oversized form metadata remains bounded.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes+maxFormOverhead)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, "", errors.New("multipart/form-data upload required")
	}
	var raw []byte
	var filename string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				return nil, "", errors.New("multipart form overhead exceeds 1 MiB")
			}
			return nil, "", err
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
			_ = part.Close()
			continue
		}
		if raw != nil {
			_ = part.Close()
			return nil, "", errors.New("upload exactly one JSON or ZIP file")
		}
		filename = path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		raw, err = io.ReadAll(io.LimitReader(part, maxFileBytes+1))
		_ = part.Close()
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				return nil, "", errors.New("multipart form overhead exceeds 1 MiB")
			}
			return nil, "", err
		}
		if int64(len(raw)) > maxFileBytes {
			return nil, "", errors.New("account archive exceeds 64 MiB")
		}
	}
	if len(raw) == 0 {
		return nil, "", errors.New("uploaded account archive is empty")
	}
	if filename == "" {
		filename = "accounts.json"
	}
	return raw, filename, nil
}

func accountArchivePayloads(raw []byte, filename string) ([][]byte, bool, error) {
	isZIP := len(raw) >= 4 && bytes.Equal(raw[:4], []byte{'P', 'K', 3, 4})
	if !isZIP {
		if len(raw) > accountArchiveMaxJSONBytes {
			return nil, false, errors.New("account JSON exceeds 16 MiB")
		}
		return [][]byte{raw}, false, nil
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, true, fmt.Errorf("invalid ZIP: %w", err)
	}
	if len(reader.File) > accountArchiveMaxFiles {
		return nil, true, fmt.Errorf("ZIP contains more than %d files", accountArchiveMaxFiles)
	}
	isDiagnosticArchive := accountArchiveLooksLikeDiagnostics(reader)
	payloads := make([][]byte, 0, len(reader.File))
	var expanded uint64
	entryNames := make(map[string]struct{}, len(reader.File))
	for _, file := range reader.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		clean := path.Clean(name)
		if strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(name, "/") {
			return nil, true, fmt.Errorf("ZIP entry %q has an unsafe path", file.Name)
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !strings.EqualFold(path.Ext(clean), ".json") {
			if isDiagnosticArchive {
				return nil, true, accountArchiveDiagnosticsUploadError()
			}
			return nil, true, fmt.Errorf("ZIP entry %q is not JSON", file.Name)
		}
		if strings.EqualFold(path.Base(clean), "manifest.json") {
			continue
		}
		nameKey := strings.ToLower(clean)
		if _, duplicate := entryNames[nameKey]; duplicate {
			return nil, true, fmt.Errorf("ZIP contains duplicate entry %q", file.Name)
		}
		entryNames[nameKey] = struct{}{}
		if file.UncompressedSize64 > accountArchiveMaxJSONBytes {
			return nil, true, fmt.Errorf("ZIP entry %q exceeds 16 MiB", file.Name)
		}
		expanded += file.UncompressedSize64
		if expanded > accountArchiveMaxExpandedBytes {
			return nil, true, errors.New("ZIP expands beyond 256 MiB")
		}
		entry, err := file.Open()
		if err != nil {
			return nil, true, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(entry, accountArchiveMaxJSONBytes+1))
		closeErr := entry.Close()
		if readErr != nil {
			return nil, true, readErr
		}
		if closeErr != nil {
			return nil, true, closeErr
		}
		if len(payload) > accountArchiveMaxJSONBytes {
			return nil, true, fmt.Errorf("ZIP entry %q exceeds 16 MiB", file.Name)
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) == 0 {
		if isDiagnosticArchive {
			return nil, true, accountArchiveDiagnosticsUploadError()
		}
		return nil, true, fmt.Errorf("ZIP %q contains no account JSON files", filename)
	}
	return payloads, true, nil
}

func accountArchiveDiagnosticsUploadError() error {
	return &PublicError{
		Code:    "account_archive_is_diagnostics",
		Message: "上传的是诊断包 ZIP，不是账号池备份。请在账号池页面点击「一键导出全部」或「一键导出所选」得到 account-pool*.zip 后再导入。",
	}
}

func accountArchiveLooksLikeDiagnostics(reader *zip.Reader) bool {
	for _, file := range reader.File {
		name := strings.ReplaceAll(file.Name, "\\", "/")
		clean := path.Clean(name)
		if strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(name, "/") {
			continue
		}
		if !strings.EqualFold(path.Base(clean), "manifest.json") || file.FileInfo().IsDir() || file.UncompressedSize64 > 1<<20 {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(entry, 1<<20))
		closeErr := entry.Close()
		if readErr != nil || closeErr != nil {
			continue
		}
		var manifest struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(payload, &manifest) == nil && strings.HasPrefix(manifest.Format, "codex-pool-diagnostics") {
			return true
		}
		if bytes.Contains(payload, []byte("codex-pool-diagnostics-v")) {
			return true
		}
	}
	return false
}

func (s *Server) decodeAccountImportPayload(raw []byte) ([]storage.AccountBackup, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, "", errors.New("account JSON is empty")
	}
	if trimmed[0] == '[' {
		var values []json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, "", err
		}
		if len(values) == 0 {
			return nil, "", errors.New("account JSON array is empty")
		}
		if allBackupDocuments(values) {
			out := make([]storage.AccountBackup, 0, len(values))
			for _, value := range values {
				item, err := decodeAccountBackupDocument(value)
				if err != nil {
					return nil, "", err
				}
				out = append(out, item)
			}
			return out, "pool-account-v1-array", nil
		}
		if allLegacyAccountRows(values) {
			out := make([]storage.AccountBackup, 0, len(values))
			for _, value := range values {
				item, err := s.decodeLegacyAccountRow(value)
				if err != nil {
					return nil, "", err
				}
				out = append(out, item)
			}
			return out, "legacy-pool-json-v0", nil
		}
		var first map[string]interface{}
		if json.Unmarshal(values[0], &first) == nil && looksLikeKiroImport(first) {
			items, err := s.backupsFromKiroJSON(trimmed)
			return items, "kiro-json", err
		}
	}

	var root map[string]interface{}
	if err := json.Unmarshal(trimmed, &root); err == nil {
		if stringValue(root["type"]) == accountBackupDocumentType || root["version"] != nil && root["account"] != nil {
			item, err := decodeAccountBackupDocument(trimmed)
			if err != nil {
				return nil, "", err
			}
			return []storage.AccountBackup{item}, "pool-account-v1", nil
		}
		if looksLikeLegacyAccountRow(root) {
			item, err := s.decodeLegacyAccountRow(trimmed)
			if err != nil {
				return nil, "", err
			}
			return []storage.AccountBackup{item}, "legacy-pool-json-v0", nil
		}
		if looksLikeAntigravityImport(root) {
			item, err := s.backupFromAntigravityJSON(root)
			if err != nil {
				return nil, "", err
			}
			return []storage.AccountBackup{item}, "antigravity-json", nil
		}
		if looksLikeKiroImport(root) {
			items, err := s.backupsFromKiroJSON(trimmed)
			return items, "kiro-json", err
		}
		if looksLikeProviderAPIKeyImport(root) {
			item, err := s.backupFromProviderAPIKeyJSON(root)
			if err != nil {
				return nil, "", err
			}
			return []storage.AccountBackup{item}, "provider-api-key", nil
		}
	}
	doc, err := authparse.ParseImportDocument(normalizeLegacyAuthExportJSON(trimmed))
	if err != nil {
		return nil, "", err
	}
	out := make([]storage.AccountBackup, 0, len(doc.Entries))
	for _, entry := range doc.Entries {
		if entry.Err != nil {
			return nil, "", fmt.Errorf("account %d: %w", entry.Index, entry.Err)
		}
		out = append(out, s.backupFromParsedAuth(entry.Parsed, entry.Name))
	}
	return out, doc.Format, nil
}

func decodeAccountBackupDocument(raw []byte) (storage.AccountBackup, error) {
	var header struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return storage.AccountBackup{}, err
	}
	if header.Type != accountBackupDocumentType {
		return storage.AccountBackup{}, fmt.Errorf("unsupported account backup type %q", header.Type)
	}
	if header.Version != accountBackupDocumentVersion {
		return storage.AccountBackup{}, fmt.Errorf("unsupported account backup version %d", header.Version)
	}
	var document accountBackupDocumentV1
	if err := json.Unmarshal(raw, &document); err != nil {
		return storage.AccountBackup{}, err
	}
	if strings.TrimSpace(document.Account.ID) == "" {
		return storage.AccountBackup{}, errors.New("account backup has no account.id")
	}
	return document.storageBackup(), nil
}

func allBackupDocuments(values []json.RawMessage) bool {
	for _, value := range values {
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(value, &header) != nil || header.Type != accountBackupDocumentType {
			return false
		}
	}
	return true
}

type legacyAccountExportRow struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Label        string `json:"label"`
	GroupName    string `json:"group_name"`
	Provider     string `json:"provider"`
	Status       string `json:"status"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	OpenAIAPIKey string `json:"openai_api_key"`
}

func allLegacyAccountRows(values []json.RawMessage) bool {
	for _, value := range values {
		var root map[string]interface{}
		if json.Unmarshal(value, &root) != nil || !looksLikeLegacyAccountRow(root) {
			return false
		}
	}
	return true
}

func looksLikeLegacyAccountRow(root map[string]interface{}) bool {
	return strings.TrimSpace(stringValue(root["id"])) != "" &&
		(root["group_name"] != nil || root["status"] != nil || root["provider"] != nil) &&
		(root["access_token"] != nil || root["refresh_token"] != nil || root["openai_api_key"] != nil)
}

func (s *Server) decodeLegacyAccountRow(raw []byte) (storage.AccountBackup, error) {
	var row legacyAccountExportRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return storage.AccountBackup{}, err
	}
	row.ID = strings.TrimSpace(row.ID)
	if row.ID == "" {
		return storage.AccountBackup{}, errors.New("legacy account export has no id")
	}
	provider := strings.ToLower(strings.TrimSpace(row.Provider))
	token := storage.AccountToken{
		AccountID: row.ID, AccessToken: row.AccessToken, RefreshToken: row.RefreshToken,
		OpenAIAPIKey: row.OpenAIAPIKey, LastRefresh: storage.Now(),
	}
	if provider == "" {
		provider = accountprovider.InferProviderFromToken(token)
		if provider == accountprovider.UnknownProvider {
			provider = "codex"
		}
	}
	token.AuthMethod = accountprovider.EffectiveAuthMethod(provider, token)
	account := storage.Account{
		ID: row.ID, Label: firstNonEmpty(row.Label, row.Email, row.ID),
		GroupName: firstNonEmpty(row.GroupName, s.cfg.DefaultGroup),
		Email:     row.Email, Provider: provider, Status: firstNonEmpty(row.Status, "active"),
	}
	backup := storage.AccountBackup{
		Account: account, Token: token, Capabilities: staticCapabilitiesForBackup(account),
		GroupMemberships: []storage.AccountGroupMembership{{
			AccountID: row.ID, GroupName: account.GroupName, IsPrimary: true, CreatedAt: storage.Now(),
		}},
	}
	if provider == "kiro" {
		method := "social"
		secret := row.RefreshToken
		if row.OpenAIAPIKey != "" {
			method = "api_key"
			secret = row.OpenAIAPIKey
		}
		sum := sha256.Sum256([]byte(method + "\x00\x00" + secret))
		backup.KiroCredentials = &storage.KiroCredentials{
			AccountID: row.ID, AuthMethod: method,
			AuthRegion: firstNonEmpty(s.cfg.KiroDefaultAuthRegion, "us-east-1"),
			APIRegion:  firstNonEmpty(s.cfg.KiroDefaultAPIRegion, "us-east-1"),
			KiroAPIKey: row.OpenAIAPIKey, CredentialHash: hex.EncodeToString(sum[:]),
		}
	}
	return backup, nil
}

func looksLikeKiroImport(root map[string]interface{}) bool {
	if accounts, ok := lookup(root, "accounts").([]interface{}); ok && len(accounts) > 0 {
		for _, candidate := range accounts {
			if item, ok := candidate.(map[string]interface{}); ok && looksLikeKiroImport(item) {
				return true
			}
		}
	}
	for _, name := range []string{"kiroApiKey", "kiro_api_key", "authMethod", "clientId", "clientSecret", "profileArn", "machineId"} {
		if lookup(root, name) != nil {
			return true
		}
	}
	if credentials, ok := lookup(root, "credentials").(map[string]interface{}); ok {
		return looksLikeKiroImport(credentials)
	}
	return false
}

func looksLikeProviderAPIKeyImport(root map[string]interface{}) bool {
	return strings.TrimSpace(stringValue(root["provider_id"])) != "" &&
		strings.TrimSpace(stringValue(root["api_key"])) != ""
}

func looksLikeAntigravityImport(root map[string]interface{}) bool {
	return strings.EqualFold(stringValue(root["provider"]), "antigravity") &&
		strings.TrimSpace(stringValue(root["project_id"])) != "" &&
		strings.TrimSpace(stringValue(root["access_token"])) != ""
}

func (s *Server) backupFromAntigravityJSON(root map[string]interface{}) (storage.AccountBackup, error) {
	parsed, err := authparse.ParseOAuthAntigravity(
		stringValue(root["access_token"]), stringValue(root["refresh_token"]),
		stringValue(root["email"]), stringValue(root["project_id"]),
		parseKiroExpiry(root["expires_at"]), nil,
	)
	if err != nil {
		return storage.AccountBackup{}, err
	}
	parsed.AntigravityBaseURL = stringValue(root["base_url"])
	parsed.AntigravityUserAgent = firstNonEmpty(stringValue(root["user_agent"]), parsed.AntigravityUserAgent)
	return s.backupFromParsedAuth(parsed, stringValue(root["label"])), nil
}

func (s *Server) backupFromProviderAPIKeyJSON(root map[string]interface{}) (storage.AccountBackup, error) {
	provider := slugify(stringValue(root["provider_id"]))
	apiKey := strings.TrimSpace(stringValue(root["api_key"]))
	if provider == "" || apiKey == "" {
		return storage.AccountBackup{}, errors.New("provider_id and api_key are required")
	}
	accountID := customAccountID(provider, apiKey)
	group := firstNonEmpty(stringValue(root["group_name"]), s.cfg.DefaultGroup, "cyber")
	account := storage.Account{
		ID: accountID, Label: firstNonEmpty(stringValue(root["label"]), provider),
		GroupName: group, Provider: provider, PlanType: "api", Status: "active",
	}
	token := storage.AccountToken{
		AccountID: accountID, AuthMethod: accountprovider.AuthMethodAPIKey,
		AccessToken: apiKey, OpenAIAPIKey: apiKey, LastRefresh: storage.Now(),
	}
	return storage.AccountBackup{
		Account: account, Token: token, Capabilities: staticCapabilitiesForBackup(account),
		GroupMemberships: []storage.AccountGroupMembership{{
			AccountID: accountID, GroupName: group, IsPrimary: true, CreatedAt: storage.Now(),
		}},
	}, nil
}

func (s *Server) backupsFromKiroJSON(raw []byte) ([]storage.AccountBackup, error) {
	items, err := parseKiroImportJSON(raw)
	if err != nil {
		return nil, err
	}
	out := make([]storage.AccountBackup, 0, len(items))
	for index, item := range items {
		if item.ParseError != "" {
			return nil, fmt.Errorf("Kiro account %d: %s", index+1, item.ParseError)
		}
		hash := kiroCredentialHash(item)
		accountID := "kiro-" + hash[:24]
		account := storage.Account{
			ID: accountID, Label: firstNonEmpty(item.Email, "Kiro "+hash[:8]),
			GroupName: firstNonEmpty(s.cfg.DefaultGroup, "cyber"), Email: item.Email, PlanType: item.Plan,
			Provider: "kiro", Status: "active",
		}
		memberships := []storage.AccountGroupMembership{{
			AccountID: accountID, GroupName: account.GroupName, IsPrimary: true, CreatedAt: storage.Now(),
		}}
		if account.GroupName != "kiro" {
			memberships = append(memberships, storage.AccountGroupMembership{
				AccountID: accountID, GroupName: "kiro", IsPrimary: false, CreatedAt: storage.Now(),
			})
		}
		token := storage.AccountToken{
			AccountID: accountID, AccessToken: item.AccessToken,
			RefreshToken: item.RefreshToken, ExpiresAt: item.ExpiresAt,
			LastRefresh: storage.Now(),
		}
		out = append(out, storage.AccountBackup{
			Account: account, Token: token,
			KiroCredentials: &storage.KiroCredentials{
				AccountID: accountID, AuthMethod: item.AuthMethod, ClientID: item.ClientID,
				ClientSecret: item.ClientSecret, ProfileARN: item.ProfileARN,
				AuthRegion: firstNonEmpty(item.AuthRegion, s.cfg.KiroDefaultAuthRegion, "us-east-1"),
				APIRegion:  firstNonEmpty(item.APIRegion, s.cfg.KiroDefaultAPIRegion, "us-east-1"),
				MachineID:  item.MachineID, KiroAPIKey: item.APIKey,
				Endpoint: item.Endpoint, CredentialHash: hash,
			},
			Capabilities:     staticCapabilitiesForBackup(account),
			GroupMemberships: memberships,
		})
	}
	return out, nil
}

func (s *Server) backupFromParsedAuth(parsed authparse.ParsedAuth, label string) storage.AccountBackup {
	group := firstNonEmpty(s.cfg.DefaultGroup, "cyber")
	account := storage.Account{
		ID: parsed.AccountID, Label: firstNonEmpty(label, parsed.Name, parsed.Email, parsed.UpstreamAccountID, parsed.AccountID),
		GroupName: group, UpstreamAccountID: parsed.UpstreamAccountID,
		ChatGPTUserID: parsed.ChatGPTUserID, Email: parsed.Email, PlanType: parsed.PlanType,
		Provider: parsed.Provider, Status: "active", IsFedramp: parsed.IsFedramp,
	}
	token := accountTokenFromParsed(parsed, parsed.RefreshToken)
	token.AccountID = account.ID
	if strings.TrimSpace(account.Provider) == "" {
		account.Provider = accountprovider.InferProviderFromToken(token)
		if account.Provider == accountprovider.UnknownProvider {
			account.Provider = "codex"
		}
	}
	if token.AuthMethod == "" {
		token.AuthMethod = accountprovider.EffectiveAuthMethod(account.Provider, token)
	}
	backup := storage.AccountBackup{
		Account: account, Token: token, Capabilities: staticCapabilitiesForBackup(account),
		GroupMemberships: []storage.AccountGroupMembership{{
			AccountID: account.ID, GroupName: group, IsPrimary: true, CreatedAt: storage.Now(),
		}},
	}
	if parsed.SessionCookie != "" {
		cookie := parsed.SessionCookie
		backup.SessionCookie = &cookie
	}
	if account.Provider == "antigravity" {
		backup.AntigravityCredentials = &storage.AntigravityCredentials{
			AccountID: account.ID, Email: account.Email, ProjectID: parsed.AntigravityProjectID,
			AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken,
			ExpiresAt: parsed.ExpiresAt, BaseURL: parsed.AntigravityBaseURL,
			UserAgent: parsed.AntigravityUserAgent,
		}
	}
	return backup
}

func staticCapabilitiesForBackup(account storage.Account) []storage.ModelCapability {
	switch strings.ToLower(strings.TrimSpace(account.Provider)) {
	case "claude":
		return capability.StaticClaudeModels(account.ID)
	case "kiro":
		return capability.StaticKiroModels(account.ID)
	case "", "codex":
		return capability.StaticCodexModels(account.ID)
	default:
		return nil
	}
}

func (s *Server) validateImportedAccountBackups(backups []storage.AccountBackup) error {
	if len(backups) == 0 {
		return errors.New("account archive contains no accounts")
	}
	if len(backups) > accountArchiveMaxFiles {
		return fmt.Errorf("account archive contains more than %d accounts", accountArchiveMaxFiles)
	}
	seen := make(map[string]struct{}, len(backups))
	for index := range backups {
		backup := &backups[index]
		id := strings.TrimSpace(backup.Account.ID)
		if id == "" {
			return fmt.Errorf("account %d has no id", index+1)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("account archive contains duplicate id %q", id)
		}
		seen[id] = struct{}{}
		backup.Account.ID = id
		if strings.TrimSpace(backup.Account.Label) == "" {
			backup.Account.Label = firstNonEmpty(backup.Account.Email, id)
		}
		if strings.TrimSpace(backup.Account.Status) == "" {
			backup.Account.Status = "active"
		}
		primaryGroup := strings.TrimSpace(backup.Account.GroupName)
		membershipSeen := make(map[string]struct{}, len(backup.GroupMemberships)+1)
		primaryCount := 0
		for membershipIndex := range backup.GroupMemberships {
			membership := &backup.GroupMemberships[membershipIndex]
			membership.GroupName = strings.TrimSpace(membership.GroupName)
			if membership.GroupName == "" {
				return fmt.Errorf("account %q has an empty group membership", id)
			}
			if _, duplicate := membershipSeen[membership.GroupName]; duplicate {
				return fmt.Errorf("account %q has duplicate group membership %q", id, membership.GroupName)
			}
			membershipSeen[membership.GroupName] = struct{}{}
			if membership.IsPrimary {
				primaryCount++
				if membership.GroupName != primaryGroup {
					return fmt.Errorf("account %q primary membership does not match group_name", id)
				}
			}
		}
		if primaryGroup != "" {
			if _, found := membershipSeen[primaryGroup]; !found {
				backup.GroupMemberships = append(backup.GroupMemberships, storage.AccountGroupMembership{
					AccountID: id, GroupName: primaryGroup, IsPrimary: true, CreatedAt: backup.Account.CreatedAt,
				})
				primaryCount++
			}
			if primaryCount != 1 {
				return fmt.Errorf("account %q must have exactly one primary group membership", id)
			}
		} else if primaryCount != 0 {
			return fmt.Errorf("account %q has a primary membership but no group_name", id)
		}
		if strings.TrimSpace(backup.Token.AuthMethod) == "" && backup.KiroCredentials == nil {
			backup.Token.AuthMethod = accountprovider.EffectiveAuthMethod(backup.Account.Provider, backup.Token)
		}
		if backup.SessionCookie != nil {
			if len(*backup.SessionCookie) > maxImportedSessionCookieBytes || strings.ContainsAny(*backup.SessionCookie, "\r\n") {
				return fmt.Errorf("account %q has an invalid session cookie", id)
			}
		}
		for _, cookie := range backup.InjectedCookies {
			if len(cookie.CookieHeader) > maxImportedSessionCookieBytes || strings.ContainsAny(cookie.CookieHeader, "\r\n") {
				return fmt.Errorf("account %q has an invalid injected cookie", id)
			}
		}
		if credentials := backup.KiroCredentials; credentials != nil {
			credentials.AuthMethod = normalizeKiroAuthMethod(credentials.AuthMethod)
			item := kiroImportItem{
				AuthMethod: credentials.AuthMethod, RefreshToken: backup.Token.RefreshToken,
				AccessToken: backup.Token.AccessToken, ClientID: credentials.ClientID,
				ClientSecret: credentials.ClientSecret, ProfileARN: credentials.ProfileARN,
				AuthRegion: credentials.AuthRegion, APIRegion: credentials.APIRegion,
				MachineID: credentials.MachineID, APIKey: credentials.KiroAPIKey,
				Endpoint: credentials.Endpoint, ExpiresAt: backup.Token.ExpiresAt,
			}
			if err := validateKiroItem(item); err != nil {
				return fmt.Errorf("account %q Kiro credentials: %w", id, err)
			}
			if _, err := kirowire.ValidateEndpoint(credentials.Endpoint, firstNonEmpty(credentials.APIRegion, s.cfg.KiroDefaultAPIRegion, "us-east-1"), s.cfg.KiroEndpointAllowlist); err != nil {
				return fmt.Errorf("account %q Kiro endpoint: %w", id, err)
			}
			if strings.TrimSpace(credentials.CredentialHash) == "" {
				credentials.CredentialHash = kiroCredentialHash(item)
			}
		}
		hasGenericCredential := strings.TrimSpace(backup.Token.AccessToken) != "" ||
			strings.TrimSpace(backup.Token.RefreshToken) != "" ||
			strings.TrimSpace(backup.Token.OpenAIAPIKey) != "" ||
			strings.TrimSpace(backup.Token.AgentPrivateKey) != ""
		hasProviderCredential := backup.KiroCredentials != nil &&
			(strings.TrimSpace(backup.KiroCredentials.KiroAPIKey) != "" ||
				strings.TrimSpace(backup.KiroCredentials.ClientSecret) != "" ||
				strings.TrimSpace(backup.Token.RefreshToken) != "") ||
			backup.AntigravityCredentials != nil &&
				(strings.TrimSpace(backup.AntigravityCredentials.AccessToken) != "" ||
					strings.TrimSpace(backup.AntigravityCredentials.RefreshToken) != "")
		if !hasGenericCredential && !hasProviderCredential {
			return fmt.Errorf("account %q has no restorable credential", id)
		}
	}
	if err := storage.ValidateAndNormalizeAccountBackupDefinitions(backups); err != nil {
		return err
	}
	for _, backup := range backups {
		if provider := backup.CustomProvider; provider != nil {
			if err := validateCustomProviderBaseURL(provider.BaseURL); err != nil {
				return fmt.Errorf("custom provider %q: %w", provider.ID, err)
			}
			for index, route := range provider.Routes {
				baseURL := strings.TrimSpace(route.BaseURL)
				if baseURL == "" {
					baseURL = provider.BaseURL
				}
				if err := validateCustomProviderBaseURL(baseURL); err != nil {
					return fmt.Errorf("custom provider %q route %d: %w", provider.ID, index+1, err)
				}
			}
		}
	}
	return nil
}

// The historical ?format=auth export used lower-case openai_api_key while the
// original auth.json convention used OPENAI_API_KEY. Normalize only this alias
// before passing the document through the established auth parser.
func normalizeLegacyAuthExportJSON(raw []byte) []byte {
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	var normalize func(interface{})
	normalize = func(candidate interface{}) {
		switch item := candidate.(type) {
		case []interface{}:
			for _, child := range item {
				normalize(child)
			}
		case map[string]interface{}:
			if item["OPENAI_API_KEY"] == nil {
				if legacy, found := item["openai_api_key"]; found {
					item["OPENAI_API_KEY"] = legacy
				}
			}
			for _, child := range item {
				normalize(child)
			}
		}
	}
	normalize(value)
	normalized, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return normalized
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
