package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/storage"
)

// generateAPIKey returns a new random downstream api key in plaintext. It is
// returned to the caller at creation and also stored encrypted at rest so the
// admin/owner can later copy the key and its one-click install command.
func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cap_" + hex.EncodeToString(buf), nil
}

func generatePoolImportKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "poolimp_" + hex.EncodeToString(buf), nil
}

func normalizeAPIKeyType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "downstream", "cap", "api":
		return "downstream"
	case "pool_import", "pool-import", "poolimp", "account_pool_import", "account-pool-import":
		return "pool_import"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

// adminAPIKeys manages downstream api keys: their routing group and the forced
// model / reasoning-effort override the key imposes regardless of what the
// client requested (the "强制改写下游使用的模型和推理强度" requirement). This is the
// missing control surface for the force_model/force_effort columns that
// resolveDownstreamPolicy already consumes.
//
//	GET  /admin/api-keys           -> list keys, including encrypted-at-rest secret when available
//	POST /admin/api-keys           -> create; returns and stores the plaintext key
func (s *Server) adminAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := s.store.ListAPIKeys(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if keys == nil {
			keys = []storage.APIKey{}
		}
		writeJSON(w, http.StatusOK, keys)
	case http.MethodPost:
		var req struct {
			Label        string `json:"label"`
			KeyType      string `json:"key_type"`
			GroupName    string `json:"group_name"`
			UserGroupID  string `json:"user_group_id"`
			ForceModel   string `json:"force_model"`
			ForceEffort  string `json:"force_effort"`
			ProviderHint string `json:"provider_hint"`
			Enabled      *bool  `json:"enabled"`
			ExpiresAt    int64  `json:"expires_at"`
			TenantID     string `json:"tenant_id"`
			ProjectID    string `json:"project_id"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		keyType := normalizeAPIKeyType(req.KeyType)
		var plain string
		var err error
		if keyType == "pool_import" {
			plain, err = generatePoolImportKey()
		} else {
			plain, err = generateAPIKey()
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		providerHint, ok := normalizeProviderHint(req.ProviderHint)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("provider_hint must be auto, codex, claude, kiro, or custom:<provider_id>"))
			return
		}
		userGroupID := strings.TrimSpace(req.UserGroupID)
		groupName := strings.TrimSpace(req.GroupName)
		if keyType == "pool_import" && userGroupID != "" {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_routing", "pool_import keys can only select an account pool group")
			return
		}
		if keyType == "pool_import" && (strings.TrimSpace(req.ForceModel) != "" || strings.TrimSpace(req.ForceEffort) != "") {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_policy", "pool_import keys cannot set model or reasoning overrides")
			return
		}
		if keyType != "pool_import" && groupName != "" {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_routing", "downstream inference keys can only select a user group")
			return
		}
		if keyType == "pool_import" && groupName != "" {
			if _, groupErr := s.store.GetGroup(r.Context(), groupName); groupErr != nil {
				writeError(w, http.StatusBadRequest, errors.New("account pool group not found"))
				return
			}
		}
		if keyType != "pool_import" && userGroupID != "" {
			if _, found, lookupErr := s.store.GetUserGroup(r.Context(), userGroupID); lookupErr != nil {
				writeError(w, http.StatusInternalServerError, lookupErr)
				return
			} else if !found {
				writeError(w, http.StatusBadRequest, errors.New("user_group not found"))
				return
			}
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		key := storage.APIKey{
			KeyHash:      hashAPIKey(plain),
			Label:        strings.TrimSpace(req.Label),
			KeyType:      keyType,
			GroupName:    groupName,
			UserGroupID:  userGroupID,
			ForceModel:   strings.TrimSpace(req.ForceModel),
			ForceEffort:  normalizeEffort(req.ForceEffort),
			ProviderHint: providerHint,
			Enabled:      enabled,
			ExpiresAt:    req.ExpiresAt,
			TenantID:     strings.TrimSpace(req.TenantID),
			ProjectID:    strings.TrimSpace(req.ProjectID),
			Secret:       plain,
		}
		if err := s.store.UpsertAPIKey(r.Context(), key); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"key":           plain,
			"key_hash":      key.KeyHash,
			"label":         key.Label,
			"key_type":      key.KeyType,
			"group_name":    key.GroupName,
			"user_group_id": key.UserGroupID,
			"force_model":   key.ForceModel,
			"force_effort":  key.ForceEffort,
			"provider_hint": key.ProviderHint,
			"enabled":       key.Enabled,
			"expires_at":    key.ExpiresAt,
			"last_used_at":  key.LastUsedAt,
		})
	default:
		methodNotAllowed(w)
	}
}

// adminAPIKeyAction handles PATCH/DELETE /admin/api-keys/{key_hash}. PATCH
// updates the routing group / forced model / forced effort / label / enabled
// flag in place; DELETE removes the key. Legacy rows created before api_keys.secret
// stay unrecoverable and should be rotated to regain copy/install buttons.
func (s *Server) adminAPIKeyAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	hash := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api-keys/"), "/")
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	// Dispatch sub-paths: /admin/api-keys/{hash}/user-group
	if idx := strings.Index(hash, "/"); idx >= 0 {
		keyHash := hash[:idx]
		sub := hash[idx+1:]
		if sub == "user-group" {
			s.adminAPIKeySetUserGroup(w, r, keyHash)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteAPIKey(r.Context(), hash); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": hash})
	case http.MethodPatch:
		key, found, err := s.store.LookupAPIKey(r.Context(), hash)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Label        *string `json:"label"`
			KeyType      *string `json:"key_type"`
			GroupName    *string `json:"group_name"`
			UserGroupID  *string `json:"user_group_id"`
			ForceModel   *string `json:"force_model"`
			ForceEffort  *string `json:"force_effort"`
			ProviderHint *string `json:"provider_hint"`
			Enabled      *bool   `json:"enabled"`
			ExpiresAt    *int64  `json:"expires_at"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		nextKeyType := key.KeyType
		if req.KeyType != nil {
			nextKeyType = normalizeAPIKeyType(*req.KeyType)
		}
		if nextKeyType == "pool_import" && req.UserGroupID != nil && strings.TrimSpace(*req.UserGroupID) != "" {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_routing", "pool_import keys can only select an account pool group")
			return
		}
		if nextKeyType == "pool_import" && ((req.ForceModel != nil && strings.TrimSpace(*req.ForceModel) != "") || (req.ForceEffort != nil && strings.TrimSpace(*req.ForceEffort) != "")) {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_policy", "pool_import keys cannot set model or reasoning overrides")
			return
		}
		if nextKeyType != "pool_import" && req.GroupName != nil && strings.TrimSpace(*req.GroupName) != "" {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "invalid_key_routing", "downstream inference keys can only select a user group")
			return
		}
		if nextKeyType == "pool_import" && req.GroupName != nil && strings.TrimSpace(*req.GroupName) != "" {
			if _, groupErr := s.store.GetGroup(r.Context(), strings.TrimSpace(*req.GroupName)); groupErr != nil {
				writeError(w, http.StatusBadRequest, errors.New("account pool group not found"))
				return
			}
		}
		if nextKeyType != "pool_import" && req.UserGroupID != nil {
			userGroupID := strings.TrimSpace(*req.UserGroupID)
			if userGroupID != "" {
				if _, ok, lookupErr := s.store.GetUserGroup(r.Context(), userGroupID); lookupErr != nil {
					writeError(w, http.StatusInternalServerError, lookupErr)
					return
				} else if !ok {
					writeError(w, http.StatusBadRequest, errors.New("user_group not found"))
					return
				}
			}
			key.UserGroupID = userGroupID
		}
		if req.KeyType != nil && nextKeyType != key.KeyType {
			if nextKeyType == "pool_import" {
				key.UserGroupID = ""
				key.ForceModel = ""
				key.ForceEffort = ""
			} else {
				key.GroupName = ""
			}
		}
		if req.Label != nil {
			key.Label = strings.TrimSpace(*req.Label)
		}
		if req.KeyType != nil {
			key.KeyType = normalizeAPIKeyType(*req.KeyType)
		}
		if req.GroupName != nil {
			key.GroupName = strings.TrimSpace(*req.GroupName)
		}
		if req.ForceModel != nil {
			key.ForceModel = strings.TrimSpace(*req.ForceModel)
		}
		if req.ForceEffort != nil {
			key.ForceEffort = normalizeEffort(*req.ForceEffort)
		}
		if req.ProviderHint != nil {
			hint, ok := normalizeProviderHint(*req.ProviderHint)
			if !ok {
				writeError(w, http.StatusBadRequest, errors.New("provider_hint must be auto, codex, claude, kiro, or custom:<provider_id>"))
				return
			}
			key.ProviderHint = hint
		}
		if req.Enabled != nil {
			key.Enabled = *req.Enabled
		}
		if req.ExpiresAt != nil {
			key.ExpiresAt = *req.ExpiresAt
		}
		if err := s.store.UpsertAPIKey(r.Context(), key); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, key)
	default:
		methodNotAllowed(w)
	}
}
