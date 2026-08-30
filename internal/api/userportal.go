package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func publicUserSessionID(tokenHash string) string {
	digest := sha256.Sum256([]byte("portal-session-v1\x00" + tokenHash))
	return hex.EncodeToString(digest[:16])
}

func (s *Server) handleUserSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	currentHash := ""
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		currentHash = hashAPIKey(cookie.Value)
	}
	sessions, err := s.store.ListUserSessions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if r.Method == http.MethodGet && strings.Trim(r.URL.Path, "/") == "user/sessions" {
		items := make([]map[string]interface{}, 0, len(sessions))
		for _, session := range sessions {
			userAgent := strings.TrimSpace(session.UserAgent)
			if len(userAgent) > 240 {
				userAgent = userAgent[:240]
			}
			items = append(items, map[string]interface{}{
				"id": publicUserSessionID(session.TokenHash), "current": session.TokenHash == currentHash,
				"user_agent": userAgent, "created_at": session.CreatedAt, "expires_at": session.ExpiresAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": items})
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	requestedID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/user/sessions/"), "/")
	if requestedID == "" || strings.Contains(requestedID, "/") {
		http.NotFound(w, r)
		return
	}
	ownedHash := ""
	for _, session := range sessions {
		if publicUserSessionID(session.TokenHash) == requestedID {
			ownedHash = session.TokenHash
			break
		}
	}
	if ownedHash == "" {
		http.NotFound(w, r)
		return
	}
	deleted, err := s.store.DeleteUserSessionOwned(r.Context(), ownedHash, u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !deleted {
		http.NotFound(w, r)
		return
	}
	if ownedHash == currentHash {
		clearCookie(w, sessionCookieName, s.requestIsHTTPS(r))
		clearCookie(w, csrfCookieName, s.requestIsHTTPS(r))
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "user_session_revoke", State: "success", Reason: "owner_requested", Detail: "user_id=" + u.ID + " session_id=" + requestedID})
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "current": ownedHash == currentHash})
}

type portalUsageCursor struct {
	At int64  `json:"at"`
	ID string `json:"id"`
}

func decodePortalUsageCursor(value string) (portalUsageCursor, error) {
	if strings.TrimSpace(value) == "" {
		return portalUsageCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 512 {
		return portalUsageCursor{}, errors.New("invalid usage cursor")
	}
	var cursor portalUsageCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.At <= 0 || cursor.ID == "" {
		return portalUsageCursor{}, errors.New("invalid usage cursor")
	}
	return cursor, nil
}

func encodePortalUsageCursor(row storage.UsageComponentRow) string {
	raw, _ := json.Marshal(portalUsageCursor{At: row.CreatedAt, ID: row.UsageEventID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Server) handleUserUsageEvents(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cursor, err := decodePortalUsageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if to <= 0 {
		to = time.Now().Unix()
	}
	if from <= 0 {
		from = to - 30*24*60*60
	}
	if from > to {
		writeError(w, http.StatusBadRequest, errors.New("from must not be after to"))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tier := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("service_tier")))
	if tier != "" && tier != "default" && tier != "fast" {
		writeError(w, http.StatusBadRequest, errors.New("service_tier must be default or fast"))
		return
	}
	components, hasMore, err := s.store.ListUserUsageComponents(r.Context(), storage.UserUsageComponentFilter{
		UserID: u.ID, Model: r.URL.Query().Get("model"), ServiceTier: tier, From: from, To: to,
		CursorAt: cursor.At, CursorEventID: cursor.ID, Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	eventIDs := make([]string, len(components))
	for index := range components {
		eventIDs[index] = components[index].UsageEventID
	}
	valuations, err := s.store.ListUsageValuationsByEventIDs(r.Context(), eventIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]map[string]interface{}, 0, len(components))
	for _, row := range components {
		items = append(items, map[string]interface{}{
			"usage_event_id":   row.UsageEventID,
			"models":           map[string]string{"requested": row.RequestedModel, "resolved": row.ResolvedModel, "observed": row.UpstreamModel},
			"service_tier":     map[string]string{"requested": row.RequestedServiceTier, "forwarded": row.ForwardedServiceTier, "observed": row.ObservedServiceTier, "billed": row.BilledServiceTier, "reason": row.BilledTierReason},
			"tokens":           map[string]interface{}{"input_total": row.InputTotal, "input_uncached": row.InputUncached, "cached_read": row.CachedRead, "cache_write": row.CacheWrite, "output_total": row.OutputTotal, "output_reasoning": row.OutputReasoning, "presence": row.FieldPresence},
			"settlement_state": row.SettlementState, "estimated": row.Estimated != 0, "integrity_error": row.IntegrityError,
			"valuations": valuations[row.UsageEventID], "created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
		})
	}
	nextCursor := ""
	if hasMore && len(components) > 0 {
		nextCursor = encodePortalUsageCursor(components[len(components)-1])
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items, "next_cursor": nextCursor, "has_more": hasMore, "from": from, "to": to})
}

func (s *Server) handleUserQuota(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now().Unix()
	from := now - 30*24*60*60
	summary, err := s.store.AccountValuationTotals(r.Context(), "", u.ID, from, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accuracy := "settled"
	if summary.UnavailableEvents > 0 {
		accuracy = "partial"
	} else if summary.ProvisionalEvents > 0 {
		accuracy = "estimated"
	}
	catalogs, err := s.store.ListPricingCatalogVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var activeCatalog interface{}
	for _, catalog := range catalogs {
		if catalog.Status == "active" {
			activeCatalog = catalog
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"period": map[string]int64{"from": from, "to": now}, "accuracy": accuracy,
		"valuation": summary, "catalog": activeCatalog, "updated_at": summary.UpdatedAt,
		"labels": map[string]string{"usd": "API list-price equivalent consumed", "credits": "ChatGPT Credits consumed when subscription evidence is available"},
	})
}

func (s *Server) handleUserOverview(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	keys, err := s.store.ListAPIKeysByUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().Unix()
	valuation, err := s.store.AccountValuationTotals(r.Context(), "", u.ID, now-7*24*60*60, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user": userView(u, "session"), "api_key_count": len(keys), "last_7d_valuation": valuation,
		"onboarding": map[string]bool{"create_key": len(keys) == 0, "copy_config": len(keys) == 0, "first_request": valuation.SettledEvents+valuation.ProvisionalEvents == 0},
		"updated_at": now,
	})
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
