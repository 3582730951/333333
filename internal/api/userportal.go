package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// userportal.go is the end-user self-service surface (session-authenticated, owner-
// scoped): manage MY downstream api keys, view MY usage, and edit my profile/password.
// Everything is gated by requireUser (session + CSRF on unsafe methods) and filtered to
// the calling user's id, so one user can never see or touch another's keys/usage.

// handleUserKeys: GET list my keys / POST create a key owned by me (plaintext once).
func (s *Server) handleUserKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := s.store.ListAPIKeysByUser(r.Context(), u.ID)
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
			Label       string `json:"label"`
			ForceModel  string `json:"force_model"`
			ForceEffort string `json:"force_effort"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plain, err := generateAPIKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		key := storage.APIKey{
			KeyHash: hashAPIKey(plain), Label: strings.TrimSpace(req.Label),
			GroupName: s.cfg.DefaultGroup, ForceModel: strings.TrimSpace(req.ForceModel),
			ForceEffort: normalizeEffort(req.ForceEffort), Enabled: true, UserID: u.ID, Secret: plain,
		}
		if err := s.store.UpsertAPIKey(r.Context(), key); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"key": plain, "key_hash": key.KeyHash, "label": key.Label, "force_model": key.ForceModel, "enabled": true})
	default:
		methodNotAllowed(w)
	}
}

// handleUserKeyAction: PATCH/DELETE /user/api-keys/{hash} — only on a key the caller owns.
func (s *Server) handleUserKeyAction(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	hash := strings.Trim(strings.TrimPrefix(r.URL.Path, "/user/api-keys/"), "/")
	if hash == "" || strings.Contains(hash, "/") {
		http.NotFound(w, r)
		return
	}
	key, found, err := s.store.LookupAPIKey(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Not found OR not owned by this user → 404 (never reveal another user's key).
	if !found || key.UserID != u.ID {
		http.NotFound(w, r)
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
		var req struct {
			Label       *string `json:"label"`
			ForceModel  *string `json:"force_model"`
			ForceEffort *string `json:"force_effort"`
			Enabled     *bool   `json:"enabled"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Label != nil {
			key.Label = strings.TrimSpace(*req.Label)
		}
		if req.ForceModel != nil {
			key.ForceModel = strings.TrimSpace(*req.ForceModel)
		}
		if req.ForceEffort != nil {
			key.ForceEffort = normalizeEffort(*req.ForceEffort)
		}
		if req.Enabled != nil {
			key.Enabled = *req.Enabled
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

// handleUserUsage: GET my per-model usage rollup.
func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.store.UsageByUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []storage.UserUsageRow{}
	}
	w.Header().Set("X-MiCliProxy-Usage-Accuracy-Cutover", strconv.FormatInt(s.store.UsageAccuracyCutover(r.Context()), 10))
	writeJSON(w, http.StatusOK, rows)
}

// handleUserUsageTimeseries: GET my usage bucketed over time.
func (s *Server) handleUserUsageTimeseries(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	bucket, _ := strconv.ParseInt(r.URL.Query().Get("bucket"), 10, 64)
	buckets, err := s.store.UsageTimeseriesByUser(r.Context(), u.ID, since, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if buckets == nil {
		buckets = []storage.UsageBucket{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"buckets": buckets})
}

// handleUserProfile: PATCH my display name and/or password. Changing the password
// requires the current one (when set) and rotates all of this user's sessions, then
// re-issues a session for the current request so the caller stays logged in.
func (s *Server) handleUserProfile(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Name        *string `json:"name"`
		OldPassword string  `json:"old_password"`
		NewPassword string  `json:"new_password"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name != nil {
		u.Name = strings.TrimSpace(*req.Name)
	}
	rotate := false
	if strings.TrimSpace(req.NewPassword) != "" {
		if len(req.NewPassword) < minPasswordLen {
			writeError(w, http.StatusBadRequest, errors.New("new password too short"))
			return
		}
		if u.PasswordHash != "" && !verifyPassword(req.OldPassword, u.PasswordHash) {
			writeError(w, http.StatusUnauthorized, errors.New("current password is incorrect"))
			return
		}
		ph, err := hashPassword(req.NewPassword)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		u.PasswordHash = ph
		rotate = true
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rotate {
		_ = s.store.DeleteUserSessionsForUser(r.Context(), u.ID)
		_ = s.startSession(w, r, u)
	}
	writeJSON(w, http.StatusOK, userView(u, "session"))
}
