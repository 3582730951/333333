package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"

	"github.com/google/uuid"
)

type teamWorkspaceRequest struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	ParentAccountID     string `json:"parent_account_id"`
	WorkspaceRef        string `json:"workspace_ref"`
	ConnectorKind       string `json:"connector_kind"`
	MaxMembers          int    `json:"max_members"`
	Status              string `json:"status"`
	MailboxProviderKey  string `json:"mailbox_provider_key"`
	RequiredEmailDomain string `json:"required_email_domain"`
	SameDomainRequired  *bool  `json:"same_domain_required"`
}

type teamLifecycleCreateRequest struct {
	IdempotencyKey         string   `json:"idempotency_key"`
	WorkspaceID            string   `json:"workspace_id"`
	ParentAccountID        string   `json:"parent_account_id"`
	ChildAccountID         string   `json:"child_account_id"`
	ReplacementMethod      string   `json:"replacement_method"`
	RotateThresholdBPS     int      `json:"rotate_threshold_bps"`
	RotateThresholdPercent *float64 `json:"rotate_threshold_percent"`
	MaxAttempts            int      `json:"max_attempts"`
	ShadowMode             *bool    `json:"shadow_mode"`
}

type teamLifecycleSetupBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Href    string `json:"href"`
}

type teamLifecycleSetupReadiness struct {
	Ready                    bool                        `json:"ready"`
	WorkspaceCreateReady     bool                        `json:"workspace_create_ready"`
	CycleCreateReady         bool                        `json:"cycle_create_ready"`
	ParentAccounts           int                         `json:"parent_accounts"`
	MailboxProfiles          int                         `json:"mailbox_profiles"`
	MailboxDefaultConfigured bool                        `json:"mailbox_default_configured"`
	MailboxHealthy           bool                        `json:"mailbox_healthy"`
	RegistrationReady        bool                        `json:"registration_ready"`
	RegistrationMethod       string                      `json:"registration_method,omitempty"`
	Workspaces               int                         `json:"workspaces"`
	Blockers                 []teamLifecycleSetupBlocker `json:"blockers"`
}

func (s *Server) teamLifecycleSetupReadiness(ctx context.Context) (teamLifecycleSetupReadiness, error) {
	readiness := teamLifecycleSetupReadiness{Blockers: make([]teamLifecycleSetupBlocker, 0, 4)}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return readiness, err
	}
	for _, account := range accounts {
		providerName := strings.ToLower(strings.TrimSpace(account.Provider))
		if account.Status != "active" || strings.TrimSpace(account.UpstreamAccountID) == "" ||
			(providerName != "" && providerName != "codex") {
			continue
		}
		token, tokenErr := s.store.GetToken(ctx, account.ID)
		if errors.Is(tokenErr, sql.ErrNoRows) {
			continue
		}
		if tokenErr != nil {
			return readiness, tokenErr
		}
		if strings.TrimSpace(token.AccessToken) != "" &&
			(token.ExpiresAt == 0 || token.ExpiresAt > storage.Now()+60) {
			readiness.ParentAccounts++
		}
	}

	defaultMailbox := s.settingString(ctx, "team_default_mailbox_provider", "")
	defaultDomain := s.settingString(ctx, "team_default_mailbox_domain", "")
	rows, err := s.store.ReadDB().QueryContext(ctx, `
SELECT provider_key,config_json FROM provider_settings
WHERE provider_type='mailbox' AND enabled=1
ORDER BY provider_key`)
	if err != nil {
		return readiness, err
	}
	for rows.Next() {
		var providerKey, configJSON string
		if err := rows.Scan(&providerKey, &configJSON); err != nil {
			rows.Close()
			return readiness, err
		}
		config := map[string]interface{}{}
		if json.Unmarshal([]byte(configJSON), &config) != nil ||
			!strings.EqualFold(strings.TrimSpace(fmt.Sprint(config["adapter"])), cloudflareMailboxAdapter) {
			continue
		}
		domain, domainErr := storage.NormalizeMailboxDomain(fmt.Sprint(config["domain"]))
		if domainErr != nil || domain == "" {
			continue
		}
		readiness.MailboxProfiles++
		if providerKey == defaultMailbox && domain == defaultDomain {
			readiness.MailboxDefaultConfigured = true
		}
	}
	if err := rows.Close(); err != nil {
		return readiness, err
	}
	if defaultMailbox != "" {
		health, found, healthErr := s.store.GetMailboxProviderHealth(ctx, defaultMailbox)
		if healthErr != nil {
			return readiness, healthErr
		}
		readiness.MailboxHealthy = found && health.LastStatus == "healthy"
	}

	workspaces, err := s.store.ListTeamWorkspaces(ctx, 1000)
	if err != nil {
		return readiness, err
	}
	readiness.Workspaces = len(workspaces)
	registration := s.registrationReadiness(ctx)
	registrationEnabled, _ := registration["registration_enabled"].(bool)
	methodReadiness, _ := registration["method_readiness"].(registrationMethodReadiness)
	readiness.RegistrationMethod = methodReadiness.Method
	readiness.RegistrationReady = registrationEnabled && methodReadiness.Ready && methodReadiness.CanaryReady

	if readiness.ParentAccounts == 0 {
		readiness.Blockers = append(readiness.Blockers, teamLifecycleSetupBlocker{
			Code: "parent_account", Message: "添加一个带 Team workspace ID 和有效凭据的母号", Href: "/accounts",
		})
	}
	if !readiness.MailboxDefaultConfigured {
		readiness.Blockers = append(readiness.Blockers, teamLifecycleSetupBlocker{
			Code: "mailbox_default", Message: "连接自建邮箱，并设为团队默认邮箱", Href: "/email-pool/cloudflare",
		})
	} else if !readiness.MailboxHealthy {
		readiness.Blockers = append(readiness.Blockers, teamLifecycleSetupBlocker{
			Code: "mailbox_health", Message: "运行团队默认邮箱的连接检测", Href: "/email-pool/cloudflare",
		})
	}
	if !readiness.RegistrationReady {
		readiness.Blockers = append(readiness.Blockers, teamLifecycleSetupBlocker{
			Code: "registration", Message: "完成自动注册配置并通过单账号 canary", Href: "/registration",
		})
	}
	if readiness.Workspaces == 0 {
		readiness.Blockers = append(readiness.Blockers, teamLifecycleSetupBlocker{
			Code: "workspace", Message: "创建第一个 Team 空间", Href: "#new-workspace",
		})
	}
	readiness.WorkspaceCreateReady = readiness.ParentAccounts > 0 && readiness.MailboxDefaultConfigured && readiness.MailboxHealthy
	readiness.CycleCreateReady = readiness.WorkspaceCreateReady && readiness.Workspaces > 0 && readiness.RegistrationReady
	readiness.Ready = readiness.CycleCreateReady
	return readiness, nil
}

func (s *Server) teamLifecycleReady(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminAllowed(w, r) {
		return false
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("team lifecycle storage is not initialized"))
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func (s *Server) handleTeamLifecycleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !s.teamLifecycleReady(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := s.store.ListTeamWorkspaces(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})

	case http.MethodPost:
		var request teamWorkspaceRequest
		if err := decodeJSONRequestBody(r.Body, &request, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(request.ID) == "" {
			request.ID = "teamws_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		if strings.TrimSpace(request.ConnectorKind) == "" {
			request.ConnectorKind = "native"
		}
		// The selected parent already stores the opaque upstream workspace identifier.
		// Keep workspace_ref as an advanced override instead of making operators copy it.
		if strings.TrimSpace(request.WorkspaceRef) == "" {
			parent, err := s.store.GetAccount(r.Context(), strings.TrimSpace(request.ParentAccountID))
			if err != nil {
				writeError(w, http.StatusBadRequest, errors.New("select a parent account from the account pool"))
				return
			}
			request.WorkspaceRef = strings.TrimSpace(parent.UpstreamAccountID)
			if request.WorkspaceRef == "" {
				writeError(w, http.StatusBadRequest, errors.New("selected parent account has no upstream workspace id; fill the advanced workspace reference"))
				return
			}
		}
		if strings.TrimSpace(request.MailboxProviderKey) == "" {
			if value, ok, err := s.store.GetSetting(r.Context(), "team_default_mailbox_provider"); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			} else if ok {
				request.MailboxProviderKey = value
			}
		}
		if strings.TrimSpace(request.RequiredEmailDomain) == "" {
			if value, ok, err := s.mailboxProviderDomain(r.Context(), request.MailboxProviderKey); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			} else if ok {
				request.RequiredEmailDomain = value
			} else if value, ok, err := s.store.GetSetting(r.Context(), "team_default_mailbox_domain"); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			} else if ok {
				request.RequiredEmailDomain = value
			}
		}
		sameDomainRequired := true
		if request.SameDomainRequired != nil {
			sameDomainRequired = *request.SameDomainRequired
		}
		item, err := s.store.UpsertTeamWorkspace(r.Context(), storage.TeamWorkspace{
			ID: request.ID, Name: request.Name, ParentAccountID: request.ParentAccountID,
			WorkspaceRef: request.WorkspaceRef, ConnectorKind: request.ConnectorKind,
			MaxMembers: request.MaxMembers, Status: request.Status,
			MailboxProviderKey:  request.MailboxProviderKey,
			RequiredEmailDomain: request.RequiredEmailDomain,
			SameDomainRequired:  sameDomainRequired,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)

	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleTeamLifecycleWorkflows(w http.ResponseWriter, r *http.Request) {
	if !s.teamLifecycleReady(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := s.store.ListTeamLifecycleWorkflows(
			r.Context(),
			r.URL.Query().Get("workspace_id"),
			r.URL.Query().Get("state"),
			limit,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})

	case http.MethodPost:
		var request teamLifecycleCreateRequest
		if err := decodeJSONRequestBody(r.Body, &request, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(request.IdempotencyKey) == "" {
			request.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		}
		if request.RotateThresholdPercent != nil {
			value := *request.RotateThresholdPercent
			if value <= 0 || value > 100 {
				writeError(w, http.StatusBadRequest, errors.New("rotate_threshold_percent must be within (0,100]"))
				return
			}
			request.RotateThresholdBPS = int(value*100 + 0.5)
		}
		// The production connector is now complete and fail-closed, so a newly
		// created lifecycle executes by default. Operators can still request an
		// explicit shadow plan for dry-run review.
		shadow := false
		if request.ShadowMode != nil {
			shadow = *request.ShadowMode
		}
		item, created, err := s.store.CreateTeamLifecycleWorkflow(
			r.Context(),
			storage.CreateTeamLifecycleWorkflowInput{
				IdempotencyKey:     request.IdempotencyKey,
				WorkspaceID:        request.WorkspaceID,
				ParentAccountID:    request.ParentAccountID,
				ChildAccountID:     request.ChildAccountID,
				ReplacementMethod:  request.ReplacementMethod,
				RotateThresholdBPS: request.RotateThresholdBPS,
				MaxAttempts:        request.MaxAttempts,
				ShadowMode:         shadow,
			},
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if s.teamLifecycle != nil {
			s.teamLifecycle.Wake()
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]interface{}{"created": created, "workflow": item})

	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleTeamLifecycleWorkflowAction(w http.ResponseWriter, r *http.Request) {
	if !s.teamLifecycleReady(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/team-lifecycle/workflows/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, storage.ErrTeamLifecycleNotFound)
		return
	}
	workflowID := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		item, err := s.store.GetTeamLifecycleWorkflow(r.Context(), workflowID)
		if err != nil {
			writeTeamLifecycleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
		return
	}

	switch parts[1] {
	case "events":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := s.store.ListTeamLifecycleEvents(r.Context(), workflowID, limit)
		if err != nil {
			writeTeamLifecycleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})

	case "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		item, err := s.store.CancelTeamLifecycleWorkflow(r.Context(), workflowID)
		if err != nil {
			writeTeamLifecycleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, item)

	case "retry":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		item, err := s.store.RetryTeamLifecycleWorkflow(r.Context(), workflowID)
		if err != nil {
			writeTeamLifecycleError(w, err)
			return
		}
		if s.teamLifecycle != nil {
			s.teamLifecycle.Wake()
		}
		writeJSON(w, http.StatusOK, item)

	default:
		writeError(w, http.StatusNotFound, storage.ErrTeamLifecycleNotFound)
	}
}

func (s *Server) handleTeamLifecycleStats(w http.ResponseWriter, r *http.Request) {
	if !s.teamLifecycleReady(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	counts, err := s.store.TeamLifecycleStateCounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	readiness, err := s.teamLifecycleSetupReadiness(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"states":                   counts,
		"readiness":                readiness,
		"default_shadow_mode":      true,
		"credential_persistence":   "encrypted_account_reference",
		"lease_heartbeat":          true,
		"rotation_threshold_bps":   100,
		"same_domain_default":      true,
		"default_mailbox_provider": s.settingString(r.Context(), "team_default_mailbox_provider", ""),
		"default_mailbox_domain":   s.settingString(r.Context(), "team_default_mailbox_domain", ""),
	})
}

func writeTeamLifecycleError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, storage.ErrTeamLifecycleNotFound):
		status = http.StatusNotFound
	case errors.Is(err, storage.ErrTeamLifecycleVersionConflict),
		errors.Is(err, storage.ErrTeamLifecycleLeaseMismatch):
		status = http.StatusConflict
	}
	writeError(w, status, err)
}
