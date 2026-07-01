// admin_resources.go holds the admin console's egress-profile, tenant/user/project,
// and observability (CF events, usage, usage-timeseries, quota) REST handlers.
// Extracted verbatim from server.go (no behavior change). Imports via goimports.
package api

import (
	"codex-account-pool/internal/proxyparse"
	"codex-account-pool/internal/storage"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) adminEgressBinding(w http.ResponseWriter, r *http.Request, accountID string) {
	switch r.Method {
	case http.MethodGet:
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, binding)
	case http.MethodPost:
		var req storage.AccountEgressBinding
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.AccountID = accountID
		if req.PrimaryEgressID == "" {
			writeError(w, http.StatusBadRequest, errors.New("primary_egress_id required"))
			return
		}
		if err := s.store.UpsertEgressBinding(r.Context(), req); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, req)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminBrowserRepair(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		EgressID     string `json:"egress_id"`
		UpstreamHost string `json:"upstream_host"`
		CookieHeader string `json:"cookie_header"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.CookieHeader == "" {
		writeError(w, http.StatusBadRequest, errors.New("cookie_header required"))
		return
	}
	binding, err := s.store.GetEgressBinding(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	egressID := firstNonEmpty(req.EgressID, binding.PrimaryEgressID)
	if err := s.upstream.ImportCookies(accountID, egressID, req.UpstreamHost, accountID+":"+egressID, req.CookieHeader); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = s.store.SetBindingCooldown(r.Context(), accountID, 0)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id": accountID,
		"egress_id":  egressID,
		"repaired":   true,
	})
}

func (s *Server) adminEgressProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		profiles, err := s.store.ListEgressProfiles(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, profiles)
	case http.MethodPost:
		// Accept either a full EgressProfile {type,endpoint,...} or the four-field
		// proxy form {type,host,port,username,password}; build the endpoint URL from
		// the fields when no explicit endpoint is given.
		var req struct {
			storage.EgressProfile
			Host         string `json:"host"`
			Port         string `json:"port"`
			Username     string `json:"username"`
			Password     string `json:"password"`
			DetectRegion *bool  `json:"detect_region"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		profile := req.EgressProfile
		if strings.TrimSpace(profile.Endpoint) == "" && strings.TrimSpace(req.Host) != "" {
			d, err := proxyparse.FromFields(req.Host, req.Port, req.Username, req.Password)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			profile.Endpoint = d.Endpoint(profile.Type)
		}
		if strings.TrimSpace(profile.ID) == "" {
			profile.ID = generatedID("egress")
		}
		if strings.TrimSpace(profile.Name) == "" {
			profile.Name = profile.ID
		}
		// Auto-detect region/exit-IP on create (best-effort; off for direct unless
		// explicitly requested) so the operator immediately sees where it exits.
		detect := profile.Type != "" && profile.Type != "direct"
		if req.DetectRegion != nil {
			detect = *req.DetectRegion
		}
		if detect {
			if pr, perr := s.upstream.ProbeEgress(r.Context(), profile); perr == nil {
				profile.Region = firstNonEmpty(pr.Country, profile.Region)
				profile.ExitIP = pr.IP
				profile.LatencyMillis = pr.LatencyMS
			}
		}
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, profile)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminEgressProfileAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/egress-profiles/"), "/")
	// Collection-level action: bulk import of "host:port:user:pass" lines.
	if rest == "import" {
		s.adminEgressImport(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	switch action {
	case "health-check":
		profile, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		profile.Health = "healthy"
		profile.CooldownUntil = 0
		profile.LatencyMillis = 0
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"egress_id": id, "health": "healthy"})
	case "detect-region":
		// Probe the geo endpoint THROUGH this egress; persist the discovered exit
		// region/IP/latency. Doubles as a real connectivity check.
		profile, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		pr, perr := s.upstream.ProbeEgress(r.Context(), profile)
		if perr != nil {
			writeError(w, http.StatusBadGateway, perr)
			return
		}
		profile.Region = firstNonEmpty(pr.Country, profile.Region)
		profile.ExitIP = pr.IP
		profile.LatencyMillis = pr.LatencyMS
		profile.Health = "healthy"
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"egress_id": id, "region": profile.Region, "exit_ip": profile.ExitIP,
			"city": pr.City, "latency_ms": pr.LatencyMS,
		})
	default:
		http.NotFound(w, r)
	}
}

// adminEgressImport bulk-creates proxy egress profiles from newline-separated
// "host:port:username:password" lines (the batch-import format). Region detection
// is off by default (one outbound probe per proxy would be slow on large lists);
// the operator can detect per-row afterwards.
func (s *Server) adminEgressImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Type         string `json:"type"`
		Lines        string `json:"lines"`
		DetectRegion *bool  `json:"detect_region"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Type) == "" {
		req.Type = "socks5h_proxy"
	}
	drafts, parseErrs := proxyparse.ParseLines(req.Lines)
	detect := req.DetectRegion != nil && *req.DetectRegion
	created := make([]storage.EgressProfile, 0, len(drafts))
	for _, d := range drafts {
		p := storage.EgressProfile{
			ID:             generatedID("egress"),
			Name:           firstNonEmpty(d.Host, "proxy"),
			Type:           req.Type,
			Endpoint:       d.Endpoint(req.Type),
			StreamCapable:  true,
			Health:         "healthy",
			MaxConcurrency: 16,
		}
		if detect {
			if pr, perr := s.upstream.ProbeEgress(r.Context(), p); perr == nil {
				p.Region = pr.Country
				p.ExitIP = pr.IP
				p.LatencyMillis = pr.LatencyMS
			}
		}
		if err := s.store.UpsertEgressProfile(r.Context(), p); err == nil {
			created = append(created, p)
		}
	}
	errStrs := make([]string, 0, len(parseErrs))
	for _, e := range parseErrs {
		errStrs = append(errStrs, e.Error())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(created), "created": created, "errors": errStrs,
	})
}

func (s *Server) adminTenants(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListTenants(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var item storage.Tenant
		if err := decodeJSONRequestBody(r.Body, &item, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if item.ID == "" {
			item.ID = generatedID("tenant")
		}
		if item.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("name required"))
			return
		}
		if err := s.store.UpsertTenant(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var req struct {
			Email    string `json:"email"`
			Name     string `json:"name"`
			Role     string `json:"role"`
			Status   string `json:"status"`
			Password string `json:"password"`
			TenantID string `json:"tenant_id"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		email := strings.TrimSpace(strings.ToLower(req.Email))
		if email == "" {
			writeError(w, http.StatusBadRequest, errors.New("email required"))
			return
		}
		if _, exists, err := s.store.GetUserByEmail(r.Context(), email); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		} else if exists {
			writeError(w, http.StatusConflict, errors.New("email already registered"))
			return
		}
		item := storage.User{ID: generatedID("usr"), Email: email, Name: strings.TrimSpace(req.Name), TenantID: strings.TrimSpace(req.TenantID), Role: "user", Status: "active"}
		if req.Role == "admin" {
			item.Role = "admin"
		}
		if req.Status == "disabled" {
			item.Status = "disabled"
		}
		if strings.TrimSpace(req.Password) != "" {
			if len(req.Password) < minPasswordLen {
				writeError(w, http.StatusBadRequest, errors.New("password too short"))
				return
			}
			ph, err := hashPassword(req.Password)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			item.PasswordHash = ph
		}
		if err := s.store.UpsertUser(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

// adminUserAction handles PATCH/DELETE /admin/users/{id}: change role/status/name,
// reset password, or delete a portal user. Guards against an admin locking
// themselves out (demoting/disabling/deleting their own account). A status change to
// disabled, a password reset, or a delete invalidates the target's sessions.
func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	u, ok, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	cur, _ := s.currentUser(r) // empty when acting via admin_token
	switch r.Method {
	case http.MethodDelete:
		if cur.ID == u.ID {
			writeError(w, http.StatusBadRequest, errors.New("cannot delete your own account"))
			return
		}
		if err := s.store.DeleteUser(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": id})
	case http.MethodPatch:
		var req struct {
			Role     *string `json:"role"`
			Status   *string `json:"status"`
			Name     *string `json:"name"`
			Password *string `json:"password"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		invalidate := false
		if req.Role != nil && (*req.Role == "admin" || *req.Role == "user") {
			u.Role = *req.Role
		}
		if req.Status != nil && (*req.Status == "active" || *req.Status == "disabled") {
			if *req.Status == "disabled" && u.Status != "disabled" {
				invalidate = true
			}
			u.Status = *req.Status
		}
		if req.Name != nil {
			u.Name = strings.TrimSpace(*req.Name)
		}
		if req.Password != nil && *req.Password != "" {
			if len(*req.Password) < minPasswordLen {
				writeError(w, http.StatusBadRequest, errors.New("password too short"))
				return
			}
			ph, err := hashPassword(*req.Password)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			u.PasswordHash = ph
			invalidate = true
		}
		// Self-lockout guard: an admin cannot demote or disable their own account.
		if cur.ID == u.ID && (u.Role != "admin" || u.Status != "active") {
			writeError(w, http.StatusBadRequest, errors.New("cannot demote or disable your own account"))
			return
		}
		if err := s.store.UpsertUser(r.Context(), u); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if invalidate {
			_ = s.store.DeleteUserSessionsForUser(r.Context(), u.ID)
		}
		writeJSON(w, http.StatusOK, u)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminProjects(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListProjects(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var item storage.Project
		if err := decodeJSONRequestBody(r.Body, &item, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if item.ID == "" {
			item.ID = generatedID("project")
		}
		if item.TenantID == "" || item.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("tenant_id and name required"))
			return
		}
		if err := s.store.UpsertProject(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminCFEvents(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListCFEvents(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if events == nil {
		events = []storage.CFEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

// adminUsage returns per-account token usage aggregates for the billing view.
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	summary, err := s.store.UsageSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// adminUsageTimeseries returns usage aggregated into fixed-width time buckets for
// the usage-over-time chart. Query params: since (epoch seconds, default last 24h)
// and bucket (seconds per bucket, default 1h).
func (s *Server) adminUsageTimeseries(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := storage.Now()
	bucket, _ := strconv.ParseInt(r.URL.Query().Get("bucket"), 10, 64)
	if bucket <= 0 {
		bucket = 3600
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if since <= 0 {
		since = now - 24*3600
	}
	buckets, err := s.store.UsageTimeseries(r.Context(), since, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"since": since, "bucket": bucket, "now": now, "buckets": buckets,
	})
}

// adminQuota returns the latest captured rate-limit / remaining-quota snapshot for
// every account, joined with the account label and provider, for the quota gauges.
// Each snapshot includes both the 5h (primary) and 7d (secondary) window data.
func (s *Server) adminQuota(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	snaps, err := s.store.ListAccountRateLimits(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accountIDs := make([]string, 0, len(snaps))
	for _, snap := range snaps {
		accountIDs = append(accountIDs, snap.AccountID)
	}
	labels, err := s.store.AccountLabelsByID(r.Context(), accountIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type secondaryWindow struct {
		UsedPercent        float64 `json:"used_percent"`
		RemainingTokens    int64   `json:"remaining_tokens"`
		LimitTokens        int64   `json:"limit_tokens"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
		ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	}
	type quotaView struct {
		storage.AccountRateLimit
		Label           string           `json:"label"`
		Secondary7d     *secondaryWindow `json:"secondary_7d,omitempty"`
		Secondary7dUsed float64          `json:"secondary_7d_used_pct"`
	}
	out := make([]quotaView, 0, len(snaps))
	for _, snap := range snaps {
		qv := quotaView{AccountRateLimit: snap, Label: labels[snap.AccountID]}
		if snap.Raw != "" {
			var detail struct {
				Secondary struct {
					UsedPercent        float64 `json:"used_percent"`
					RemainingTokens    int64   `json:"remaining_tokens"`
					LimitTokens        int64   `json:"limit_tokens"`
					LimitWindowSeconds int64   `json:"limit_window_seconds"`
					ResetAfterSeconds  int64   `json:"reset_after_seconds"`
				} `json:"secondary"`
			}
			if json.Unmarshal([]byte(snap.Raw), &detail) == nil && detail.Secondary.LimitWindowSeconds > 0 {
				qv.Secondary7d = &secondaryWindow{
					UsedPercent:        detail.Secondary.UsedPercent,
					RemainingTokens:    detail.Secondary.RemainingTokens,
					LimitTokens:        detail.Secondary.LimitTokens,
					LimitWindowSeconds: detail.Secondary.LimitWindowSeconds,
					ResetAfterSeconds:  detail.Secondary.ResetAfterSeconds,
				}
				qv.Secondary7dUsed = detail.Secondary.UsedPercent
			}
		}
		out = append(out, qv)
	}
	writeJSON(w, http.StatusOK, out)
}

// adminIdentity exposes the synthetic, account-bound virtual identity the relay
// presents upstream. All fields are generated (never the real client's), so the
// view is safe to surface and lets operators confirm each account looks like a
// single consistent machine.
