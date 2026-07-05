package api

import (
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
	"database/sql"
	"encoding/json"
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
	var req struct {
		Label        string          `json:"label"`
		GroupName    string          `json:"group_name"`
		AuthJSON     json.RawMessage `json:"auth_json"`
		AuthJSONText string          `json:"auth_json_text"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw := []byte(req.AuthJSON)
	if len(raw) == 0 && strings.TrimSpace(req.AuthJSONText) != "" {
		raw = []byte(req.AuthJSONText)
	}
	parsed, err := authparse.ParseAuthJSON(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
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
	if existing, err := s.store.GetAccount(r.Context(), account.ID); err == nil {
		key.LastUsedAt = storage.Now()
		_ = s.store.UpsertAPIKey(r.Context(), key)
		writeJSON(w, http.StatusOK, accountImportResponse{Account: existing, Duplicate: true, ImportStatus: "duplicate"})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	lastRefresh := parsed.LastRefresh
	if lastRefresh == 0 {
		lastRefresh = storage.Now()
	}
	token := storage.AccountToken{
		AccessToken:        parsed.AccessToken,
		RefreshToken:       parsed.RefreshToken,
		OpenAIAPIKey:       parsed.OpenAIAPIKey,
		IDTokenRaw:         parsed.IDTokenRaw,
		LastRefresh:        lastRefresh,
		ExpiresAt:          parsed.ExpiresAt,
		Scopes:             strings.Join(parsed.Scopes, " "),
		OAuthRateLimitTier: parsed.OAuthRateLimitTier,
	}
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	key.LastUsedAt = storage.Now()
	_ = s.store.UpsertAPIKey(r.Context(), key)
	writeJSON(w, http.StatusOK, accountImportResponse{Account: account, ImportStatus: "imported"})
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
