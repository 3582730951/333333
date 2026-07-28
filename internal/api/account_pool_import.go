package api

import (
	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

func (s *Server) accountPoolImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	key, ok := s.authorizePoolImportKey(w, r)
	if !ok {
		return
	}
	var req authJSONImportRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw := []byte(req.AuthJSON)
	if len(raw) == 0 && strings.TrimSpace(req.AuthJSONText) != "" {
		raw = []byte(req.AuthJSONText)
	}
	doc, err := authparse.ParseImportDocument(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if doc.Format != authparse.ImportFormatSingle {
		key.LastUsedAt = storage.Now()
		_ = s.store.UpsertAPIKey(r.Context(), key)
		s.adminImportAuthDocument(w, r, req, doc)
		return
	}
	parsed := doc.Entries[0].Parsed
	sessionCookie, err := normalizeImportedSessionCookie(req.SessionCookie, parsed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	warnings := importedAuthWarnings(parsed, sessionCookie)
	if req.Label == "" {
		req.Label = firstNonEmpty(parsed.Name, parsed.Email, parsed.UpstreamAccountID, parsed.AccountID)
	}
	if req.GroupName == "" {
		req.GroupName = s.cfg.DefaultGroup
	}
	account := storage.Account{
		ID:                parsed.AccountID,
		Label:             req.Label,
		GroupName:         req.GroupName,
		UpstreamAccountID: parsed.UpstreamAccountID,
		ChatGPTUserID:     parsed.ChatGPTUserID,
		Email:             parsed.Email,
		PlanType:          parsed.PlanType,
		Provider:          parsed.Provider,
		Status:            "active",
		IsFedramp:         parsed.IsFedramp,
	}
	if existing, err := s.findExistingImportedAccount(r.Context(), parsed); err == nil {
		updatedAccount, updated, updateErr := s.updateExistingExternalChatGPTTokens(r.Context(), existing, parsed, sessionCookie)
		if updateErr != nil {
			writeError(w, http.StatusBadRequest, updateErr)
			return
		}
		key.LastUsedAt = storage.Now()
		_ = s.store.UpsertAPIKey(r.Context(), key)
		status := "duplicate"
		if updated {
			status = "updated"
		}
		writeJSON(w, http.StatusOK, accountImportResponse{
			Account: updatedAccount, Duplicate: !updated, Updated: updated, ImportStatus: status,
			CredentialMode: parsed.CredentialMode, Warnings: warnings,
		})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	token := accountTokenFromParsed(parsed, parsed.RefreshToken)
	if strings.TrimSpace(account.Provider) == "" {
		if inferred := accountprovider.InferProviderFromToken(token); inferred != accountprovider.UnknownProvider {
			account.Provider = inferred
		}
	}
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.storeImportedSessionCookie(r.Context(), account.ID, sessionCookie); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bindImportedAccountPrimaryEgress(r.Context(), account.ID, requestedImportEgressID(req.EgressID, req.PrimaryEgressID)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	key.LastUsedAt = storage.Now()
	_ = s.store.UpsertAPIKey(r.Context(), key)
	writeJSON(w, http.StatusOK, accountImportResponse{
		Account: account, ImportStatus: "imported", CredentialMode: parsed.CredentialMode, Warnings: warnings,
	})
}

func (s *Server) authorizePoolImportKey(w http.ResponseWriter, r *http.Request) (storage.APIKey, bool) {
	plain := downstreamBearer(r)
	if !strings.HasPrefix(plain, "poolimp_") {
		writeError(w, http.StatusUnauthorized, errors.New("pool import key required"))
		return storage.APIKey{}, false
	}
	key, found, err := s.store.LookupAPIKey(r.Context(), hashAPIKey(plain))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return storage.APIKey{}, false
	}
	if !found || normalizeAPIKeyType(key.KeyType) != "pool_import" {
		writeError(w, http.StatusUnauthorized, errors.New("unknown pool import key"))
		return storage.APIKey{}, false
	}
	if !key.Enabled {
		writeError(w, http.StatusUnauthorized, errors.New("pool import key disabled"))
		return storage.APIKey{}, false
	}
	if key.ExpiresAt > 0 && storage.Now() > key.ExpiresAt {
		writeError(w, http.StatusUnauthorized, errors.New("pool import key expired"))
		return storage.APIKey{}, false
	}
	return key, true
}

func isPoolImportKeyPlain(plain string) bool {
	return strings.HasPrefix(strings.TrimSpace(plain), "poolimp_")
}
