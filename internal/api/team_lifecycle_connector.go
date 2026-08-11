package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/registration/teamflow"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

const (
	teamLifecycleResponseLimit = 1 << 20
	teamPersonalTokenTTL       = 60 * 24 * 60 * 60
)

var teamPersonalTokenScopes = []string{
	"chatgpt.workspace.feature.hermes.access",
	"chatgpt.workspace.feature.allow-codex-local-access.access",
}

// teamLifecycleRawDo is deliberately narrow so the production connector always
// uses the account's bound egress while focused tests can supply a deterministic
// in-memory upstream.
type teamLifecycleRawDo func(
	context.Context,
	storage.Account,
	string,
	string,
	http.Header,
	[]byte,
) (*upstream.Response, error)

type nativeTeamLifecycleConnector struct {
	server      *Server
	store       *storage.Store
	upstream    *upstream.Client
	origin      string
	backendBase string
	doRaw       teamLifecycleRawDo
}

func newNativeTeamLifecycleConnector(server *Server) *nativeTeamLifecycleConnector {
	connector := &nativeTeamLifecycleConnector{server: server}
	if server != nil {
		connector.store = server.store
		connector.upstream = server.upstream
		connector.origin, connector.backendBase = teamLifecycleEndpoints(server.cfg.UpstreamBaseURL)
	}
	if connector.origin == "" {
		connector.origin = "https://chatgpt.com"
	}
	if connector.backendBase == "" {
		connector.backendBase = connector.origin + "/backend-api"
	}
	return connector
}

func teamLifecycleEndpoints(configured string) (string, string) {
	parsed, err := url.Parse(strings.TrimSpace(configured))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://chatgpt.com", "https://chatgpt.com/backend-api"
	}
	origin := parsed.Scheme + "://" + parsed.Host
	return origin, origin + "/backend-api"
}

func (c *nativeTeamLifecycleConnector) boundRaw(
	ctx context.Context,
	actor storage.Account,
	method, rawURL string,
	headers http.Header,
	body []byte,
) (*upstream.Response, error) {
	if c == nil || c.store == nil || c.upstream == nil {
		return nil, teamflow.Permanent("team_transport_not_configured", errors.New("team lifecycle transport is not configured"))
	}
	binding, err := c.store.GetEgressBinding(ctx, actor.ID)
	if err != nil {
		return nil, teamflow.Permanent("team_egress_not_bound", errors.New("team lifecycle account egress is not bound"))
	}
	binding, err = c.store.EffectiveEgressBinding(ctx, binding)
	if err != nil {
		return nil, teamflow.Permanent("team_egress_unavailable", errors.New("team lifecycle account egress is unavailable"))
	}
	egress, err := c.store.ResolvePrimaryEgressBinding(ctx, binding)
	if err != nil {
		return nil, teamflow.Permanent("team_egress_unavailable", errors.New("team lifecycle account egress is unavailable"))
	}
	return c.upstream.DoRaw(
		ctx, egress, method, rawURL, headers, body,
		"team-lifecycle:"+actor.ID+":"+binding.CookieJarKey,
	)
}

func (c *nativeTeamLifecycleConnector) call(
	ctx context.Context,
	actor storage.Account,
	method, rawURL string,
	headers http.Header,
	body []byte,
) ([]byte, int, error) {
	doRaw := c.doRaw
	if doRaw == nil {
		doRaw = c.boundRaw
	}
	response, err := doRaw(ctx, actor, method, rawURL, headers, body)
	if err != nil {
		var classified *teamflow.ClassifiedError
		if errors.As(err, &classified) {
			return nil, 0, err
		}
		return nil, 0, teamflow.Retryable("team_transport_failed", err)
	}
	if response == nil || response.Body == nil {
		return nil, 0, teamflow.Retryable("team_invalid_response", errors.New("team lifecycle upstream returned no response body"))
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, teamLifecycleResponseLimit+1))
	if readErr != nil {
		return nil, response.StatusCode, teamflow.Retryable("team_response_read_failed", readErr)
	}
	if len(raw) > teamLifecycleResponseLimit {
		return nil, response.StatusCode, teamflow.Permanent("team_response_too_large", errors.New("team lifecycle response exceeded limit"))
	}
	return raw, response.StatusCode, nil
}

func teamHTTPError(operation string, status int) error {
	class := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(operation)), "-", "_")
	if class == "" {
		class = "team_request"
	}
	err := fmt.Errorf("%s returned HTTP %d", class, status)
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return teamflow.Retryable(class+"_retryable", err)
	}
	return teamflow.Permanent(class+"_rejected", err)
}

func teamAuthHeaders(token, workspace string) http.Header {
	headers := http.Header{
		"Accept":          []string{"application/json"},
		"Accept-Language": []string{"en-US,en;q=0.9"},
		"Origin":          []string{"https://chatgpt.com"},
		"Referer":         []string{"https://chatgpt.com/"},
		"User-Agent":      []string{"Mozilla/5.0 AppleWebKit/537.36 Chrome/145.0.0.0 Safari/537.36"},
	}
	if token = strings.TrimSpace(token); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if workspace = strings.TrimSpace(workspace); workspace != "" {
		headers.Set("chatgpt-account-id", workspace)
	}
	return headers
}

type teamConnectorContext struct {
	workspace    storage.TeamWorkspace
	workspaceRef string
	parent       storage.Account
	child        storage.Account
	childFound   bool
}

func (c *nativeTeamLifecycleConnector) resolveAccount(ctx context.Context, identity string) (storage.Account, error) {
	identity = strings.TrimSpace(identity)
	account, err := c.store.GetAccount(ctx, identity)
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storage.Account{}, err
	}
	return c.store.GetAccountByEmail(ctx, identity)
}

func (c *nativeTeamLifecycleConnector) operationContext(
	ctx context.Context,
	operation teamflow.Operation,
	requireChild bool,
) (teamConnectorContext, error) {
	if c == nil || c.store == nil {
		return teamConnectorContext{}, teamflow.Permanent("team_storage_not_configured", errors.New("team lifecycle storage is not configured"))
	}
	workflow := operation.Workflow
	workspace, err := c.store.GetTeamWorkspace(ctx, workflow.WorkspaceID)
	if err != nil {
		return teamConnectorContext{}, teamflow.Permanent("team_workspace_missing", err)
	}
	if workspace.ParentAccountID != strings.TrimSpace(workflow.ParentAccountID) {
		return teamConnectorContext{}, teamflow.Permanent("team_parent_mismatch", errors.New("team lifecycle parent does not match workspace"))
	}
	parent, err := c.store.GetAccount(ctx, workflow.ParentAccountID)
	if err != nil {
		return teamConnectorContext{}, teamflow.Permanent("team_parent_missing", err)
	}
	workspaceRef := strings.TrimSpace(workspace.WorkspaceRef)
	if workspaceRef == "" {
		workspaceRef = strings.TrimSpace(parent.UpstreamAccountID)
	}
	if workspaceRef == "" {
		return teamConnectorContext{}, teamflow.Permanent("team_workspace_reference_missing", errors.New("team workspace reference is empty"))
	}
	resolved := teamConnectorContext{
		workspace: workspace, workspaceRef: workspaceRef, parent: parent,
	}
	child, childErr := c.resolveAccount(ctx, workflow.ChildAccountID)
	if childErr == nil {
		resolved.child = child
		resolved.childFound = true
	} else if requireChild {
		return teamConnectorContext{}, teamflow.Permanent("team_child_missing", childErr)
	} else if !errors.Is(childErr, sql.ErrNoRows) {
		return teamConnectorContext{}, teamflow.Retryable("team_child_lookup_failed", childErr)
	}
	return resolved, nil
}

func (c *nativeTeamLifecycleConnector) parentToken(ctx context.Context, resolved teamConnectorContext) (storage.AccountToken, error) {
	token, err := c.store.GetToken(ctx, resolved.parent.ID)
	if err != nil {
		return storage.AccountToken{}, teamflow.Permanent("team_parent_credential_missing", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return storage.AccountToken{}, teamflow.Permanent("team_parent_credential_missing", errors.New("parent access credential is empty"))
	}
	if token.ExpiresAt > 0 && token.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
		return storage.AccountToken{}, teamflow.Permanent("team_parent_credential_expired", errors.New("parent access credential is expired"))
	}
	return token, nil
}

func childEmail(operation teamflow.Operation, resolved teamConnectorContext) string {
	if resolved.childFound && strings.TrimSpace(resolved.child.Email) != "" {
		return strings.ToLower(strings.TrimSpace(resolved.child.Email))
	}
	return strings.ToLower(strings.TrimSpace(operation.Workflow.ChildAccountID))
}

func teamStableRef(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + ":" + hex.EncodeToString(sum[:12])
}

func decodeTeamJSON(raw []byte) (interface{}, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]interface{}{}, nil
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("team lifecycle upstream returned malformed JSON")
	}
	return value, nil
}

func teamObjects(value interface{}, depth int) []map[string]interface{} {
	if depth > 6 {
		return nil
	}
	switch typed := value.(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(typed))
		for _, item := range typed {
			if object, ok := item.(map[string]interface{}); ok {
				out = append(out, object)
			}
		}
		return out
	case map[string]interface{}:
		for _, key := range []string{"items", "users", "members", "invites", "data"} {
			if nested, ok := typed[key]; ok {
				if items := teamObjects(nested, depth+1); len(items) > 0 {
					return items
				}
			}
		}
	}
	return nil
}

func teamMapString(object map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, nestedKey := range []string{"user", "member", "invite", "account"} {
		if nested, ok := object[nestedKey].(map[string]interface{}); ok {
			if value := teamMapString(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

func teamFindIdentity(raw []byte, email string) (string, bool, error) {
	value, err := decodeTeamJSON(raw)
	if err != nil {
		return "", false, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, item := range teamObjects(value, 0) {
		candidate := strings.ToLower(teamMapString(item, "email", "email_address"))
		if candidate == email {
			return teamMapString(item, "id", "user_id", "member_id", "invite_id"), true, nil
		}
	}
	return "", false, nil
}

func (c *nativeTeamLifecycleConnector) listTeamCollection(
	ctx context.Context,
	resolved teamConnectorContext,
	parentToken, collection string,
) ([]byte, error) {
	headers := teamAuthHeaders(parentToken, resolved.workspaceRef)
	rawURL := c.backendBase + "/accounts/" + url.PathEscape(resolved.workspaceRef) + "/" + collection
	if collection == "users" {
		rawURL += "?limit=100&offset=0"
	}
	raw, status, err := c.call(ctx, resolved.parent, http.MethodGet, rawURL, headers, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, teamHTTPError("team_"+collection, status)
	}
	return raw, nil
}

func (c *nativeTeamLifecycleConnector) findMemberOrInvite(
	ctx context.Context,
	resolved teamConnectorContext,
	parentToken, email string,
) (string, bool, error) {
	for _, collection := range []string{"users", "invites"} {
		raw, err := c.listTeamCollection(ctx, resolved, parentToken, collection)
		if err != nil {
			return "", false, err
		}
		id, found, err := teamFindIdentity(raw, email)
		if err != nil {
			return "", false, teamflow.Retryable("team_membership_response_invalid", err)
		}
		if found {
			return id, true, nil
		}
	}
	return "", false, nil
}

func (c *nativeTeamLifecycleConnector) Invite(ctx context.Context, operation teamflow.Operation) (string, error) {
	resolved, err := c.operationContext(ctx, operation, false)
	if err != nil {
		return "", err
	}
	email := childEmail(operation, resolved)
	if storage.EmailDomain(email) == "" {
		return "", teamflow.Permanent("team_child_email_missing", errors.New("child email is required"))
	}
	if required := strings.TrimSpace(operation.Workflow.RequiredEmailDomain); required != "" &&
		storage.EmailDomain(email) != required {
		return "", teamflow.Permanent("team_child_domain_mismatch", errors.New("child email does not match team domain"))
	}
	token, err := c.parentToken(ctx, resolved)
	if err != nil {
		return "", err
	}
	if _, found, lookupErr := c.findMemberOrInvite(ctx, resolved, token.AccessToken, email); lookupErr != nil {
		return "", lookupErr
	} else if found {
		return teamStableRef("team-membership", resolved.workspaceRef, email), nil
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"email_addresses": []string{email},
		"role":            "standard-user",
		"resend_emails":   true,
	})
	headers := teamAuthHeaders(token.AccessToken, resolved.workspaceRef)
	headers.Set("Content-Type", "application/json")
	headers.Set("Idempotency-Key", operation.OperationKey)
	rawURL := c.backendBase + "/accounts/" + url.PathEscape(resolved.workspaceRef) + "/invites"
	_, status, callErr := c.call(ctx, resolved.parent, http.MethodPost, rawURL, headers, payload)
	if callErr != nil {
		return "", callErr
	}
	if status < 200 || status >= 300 {
		if status == http.StatusConflict {
			if _, found, lookupErr := c.findMemberOrInvite(ctx, resolved, token.AccessToken, email); lookupErr == nil && found {
				return teamStableRef("team-membership", resolved.workspaceRef, email), nil
			}
		}
		return "", teamHTTPError("team_invite", status)
	}
	return teamStableRef("team-membership", resolved.workspaceRef, email), nil
}

func normalizeSessionCookie(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64<<10 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	if strings.Contains(value, "=") {
		return value
	}
	return "__Secure-next-auth.session-token=" + value
}

func nestedMap(value map[string]interface{}, key string) map[string]interface{} {
	nested, _ := value[key].(map[string]interface{})
	return nested
}

func firstMapString(value map[string]interface{}, keys ...string) string {
	return teamMapString(value, keys...)
}

func (c *nativeTeamLifecycleConnector) exchangeWorkspaceSession(
	ctx context.Context,
	resolved teamConnectorContext,
	token storage.AccountToken,
) (storage.Account, storage.AccountToken, error) {
	cookie, err := c.store.GetSessionCookie(ctx, resolved.child.ID)
	if err != nil {
		return resolved.child, token, teamflow.Retryable("team_session_lookup_failed", err)
	}
	cookie = normalizeSessionCookie(cookie)
	if cookie == "" {
		return resolved.child, token, nil
	}
	query := url.Values{
		"exchange_workspace_token": []string{"true"},
		"workspace_id":             []string{resolved.workspaceRef},
		"reason":                   []string{"setCurrentAccount"},
	}
	headers := teamAuthHeaders("", resolved.workspaceRef)
	headers.Set("Cookie", cookie)
	raw, status, callErr := c.call(
		ctx, resolved.child, http.MethodGet,
		c.origin+"/api/auth/session?"+query.Encode(),
		headers, nil,
	)
	if callErr != nil {
		return resolved.child, token, callErr
	}
	if status < 200 || status >= 300 {
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			return resolved.child, token, teamflow.FallbackToOAuth(fmt.Errorf("workspace session exchange returned HTTP %d", status))
		}
		return resolved.child, token, teamHTTPError("team_workspace_exchange", status)
	}
	decoded, decodeErr := decodeTeamJSON(raw)
	if decodeErr != nil {
		return resolved.child, token, teamflow.Retryable("team_workspace_response_invalid", decodeErr)
	}
	root, ok := decoded.(map[string]interface{})
	if !ok {
		return resolved.child, token, teamflow.Retryable("team_workspace_response_invalid", errors.New("workspace session response is not an object"))
	}
	accountData := nestedMap(root, "account")
	userData := nestedMap(root, "user")
	workspaceRef := firstMapString(accountData, "id", "account_id")
	if workspaceRef == "" {
		workspaceRef = firstMapString(root, "account_id", "workspace_id")
	}
	if workspaceRef != resolved.workspaceRef {
		return resolved.child, token, teamflow.FallbackToOAuth(errors.New("workspace session did not select the requested team"))
	}
	accessToken := firstMapString(root, "accessToken", "access_token")
	if accessToken == "" {
		return resolved.child, token, teamflow.FallbackToOAuth(errors.New("workspace session returned no access credential"))
	}
	account := resolved.child
	account.UpstreamAccountID = resolved.workspaceRef
	if userID := firstMapString(userData, "id", "user_id"); userID != "" {
		account.ChatGPTUserID = userID
	}
	if email := firstMapString(userData, "email"); email != "" {
		account.Email = email
	}
	if plan := firstMapString(accountData, "planType", "plan_type"); plan != "" {
		account.PlanType = plan
	}
	token.AccessToken = accessToken
	token.AuthMethod = accountprovider.AuthMethodOAuth
	token.LastRefresh = storage.Now()
	if err := c.store.UpsertAccount(ctx, account, token); err != nil {
		return resolved.child, token, teamflow.Retryable("team_workspace_persist_failed", err)
	}
	if sessionToken := normalizeSessionCookie(firstMapString(root, "sessionToken", "session_token")); sessionToken != "" {
		if err := c.store.SetSessionCookie(ctx, account.ID, sessionToken); err != nil {
			return resolved.child, token, teamflow.Retryable("team_session_persist_failed", err)
		}
	}
	return account, token, nil
}

func personalAccessTokenFromJSON(raw []byte) string {
	decoded, err := decodeTeamJSON(raw)
	if err != nil {
		return ""
	}
	var visit func(interface{}, int) string
	visit = func(value interface{}, depth int) string {
		if depth > 6 {
			return ""
		}
		switch typed := value.(type) {
		case map[string]interface{}:
			for _, key := range []string{"access_token", "personal_access_token", "token"} {
				if token, ok := typed[key].(string); ok && strings.TrimSpace(token) != "" {
					return strings.TrimSpace(token)
				}
			}
			for _, nested := range typed {
				if token := visit(nested, depth+1); token != "" {
					return token
				}
			}
		case []interface{}:
			for _, nested := range typed {
				if token := visit(nested, depth+1); token != "" {
					return token
				}
			}
		}
		return ""
	}
	return visit(decoded, 0)
}

func (c *nativeTeamLifecycleConnector) createPersonalAccessToken(
	ctx context.Context,
	operation teamflow.Operation,
	resolved teamConnectorContext,
	account storage.Account,
	token storage.AccountToken,
) (string, error) {
	if strings.EqualFold(token.CredentialMode, accountprovider.CredentialModePersonalAccessToken) &&
		strings.TrimSpace(token.OpenAIAPIKey) != "" {
		return "account_auth_tokens:" + account.ID + ":personal_access_token", nil
	}
	accessToken := strings.TrimSpace(token.AccessToken)
	if accessToken == "" {
		return "", teamflow.FallbackToOAuth(errors.New("child access credential is empty"))
	}
	nameHash := sha256.Sum256([]byte(operation.OperationKey))
	payload, _ := json.Marshal(map[string]interface{}{
		"name":   "pool-" + hex.EncodeToString(nameHash[:6]),
		"scopes": teamPersonalTokenScopes,
		"ttl":    teamPersonalTokenTTL,
	})
	headers := teamAuthHeaders(accessToken, resolved.workspaceRef)
	headers.Set("Content-Type", "application/json")
	headers.Set("Idempotency-Key", operation.OperationKey)
	raw, status, callErr := c.call(
		ctx, account, http.MethodPost,
		c.backendBase+"/wham/auth-credentials",
		headers, payload,
	)
	if callErr != nil {
		return "", callErr
	}
	if status < 200 || status >= 300 {
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			return "", teamflow.FallbackToOAuth(fmt.Errorf("personal access token request returned HTTP %d", status))
		}
		return "", teamHTTPError("team_personal_access_token", status)
	}
	personalToken := personalAccessTokenFromJSON(raw)
	if personalToken == "" {
		return "", teamflow.FallbackToOAuth(errors.New("personal access token response contained no credential"))
	}
	account.UpstreamAccountID = resolved.workspaceRef
	token.OpenAIAPIKey = personalToken
	token.CredentialMode = accountprovider.CredentialModePersonalAccessToken
	token.AuthMethod = accountprovider.AuthMethodAccessToken
	if err := c.store.UpsertAccount(ctx, account, token); err != nil {
		return "", teamflow.Retryable("team_personal_access_token_persist_failed", err)
	}
	return "account_auth_tokens:" + account.ID + ":personal_access_token", nil
}

func (c *nativeTeamLifecycleConnector) LoginWithCredential(ctx context.Context, operation teamflow.Operation) (string, error) {
	resolved, err := c.operationContext(ctx, operation, true)
	if err != nil {
		return "", err
	}
	token, err := c.store.GetToken(ctx, resolved.child.ID)
	if err != nil {
		return "", teamflow.FallbackToOAuth(err)
	}
	if strings.EqualFold(token.CredentialMode, accountprovider.CredentialModePersonalAccessToken) &&
		strings.TrimSpace(token.OpenAIAPIKey) != "" {
		return "account_auth_tokens:" + resolved.child.ID + ":personal_access_token", nil
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		if strings.TrimSpace(token.OpenAIAPIKey) != "" {
			return "account_auth_tokens:" + resolved.child.ID + ":access_token", nil
		}
		return "", teamflow.FallbackToOAuth(errors.New("child access credential is empty"))
	}
	account, token, exchangeErr := c.exchangeWorkspaceSession(ctx, resolved, token)
	if exchangeErr != nil {
		return "", exchangeErr
	}
	return c.createPersonalAccessToken(ctx, operation, resolved, account, token)
}

func (c *nativeTeamLifecycleConnector) OAuthLogin(ctx context.Context, operation teamflow.Operation) (teamflow.OAuthResult, error) {
	resolved, err := c.operationContext(ctx, operation, true)
	if err != nil {
		return teamflow.OAuthResult{}, err
	}
	if c.server == nil {
		return teamflow.OAuthResult{}, teamflow.Permanent("team_oauth_not_configured", errors.New("team OAuth runner is not configured"))
	}
	cfg, found, err := c.store.GetCodexReauthConfig(ctx, resolved.child.ID)
	if err != nil {
		return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_config_lookup_failed", err)
	}
	if !found {
		cfg = storage.AccountCodexReauthConfig{
			AccountID:  resolved.child.ID,
			LoginEmail: resolved.child.Email,
		}
	}
	cfg.TargetWorkspaceID = resolved.workspaceRef
	cfg.AutoEnabled = true
	cfg.LastStatus = "team_oauth_queued"
	if err := c.store.UpsertCodexReauthConfig(ctx, cfg); err != nil {
		return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_config_persist_failed", err)
	}
	cookie, _ := c.store.GetSessionCookie(ctx, resolved.child.ID)
	if strings.TrimSpace(cfg.Password) == "" && strings.TrimSpace(cfg.OTPURL) == "" && strings.TrimSpace(cookie) == "" {
		return teamflow.OAuthResult{}, teamflow.Permanent("team_oauth_login_material_missing", errors.New("team OAuth login material is missing"))
	}
	job, _, err := c.store.EnqueueCodexReauthJob(ctx, resolved.child.ID, "team_lifecycle:"+operation.OperationKey)
	if err != nil {
		return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_enqueue_failed", err)
	}
	_, status, runErr := c.server.runCodexReauthJob(ctx, job.ID)
	if runErr != nil {
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_failed", runErr)
		}
		return teamflow.OAuthResult{}, teamflow.Permanent("team_oauth_rejected", runErr)
	}
	account, err := c.store.GetAccount(ctx, resolved.child.ID)
	if err != nil {
		return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_account_reload_failed", err)
	}
	token, err := c.store.GetTokenFresh(ctx, resolved.child.ID)
	if err != nil {
		return teamflow.OAuthResult{}, teamflow.Retryable("team_oauth_credential_reload_failed", err)
	}
	if ref, patErr := c.createPersonalAccessToken(ctx, operation, resolved, account, token); patErr == nil {
		return teamflow.OAuthResult{CredentialRef: ref}, nil
	}
	return teamflow.OAuthResult{
		CredentialRef: "account_auth_tokens:" + resolved.child.ID + ":oauth",
		// The reauth worker performs add_phone only when the OAuth page asks for
		// it, including virtual-number acquisition and SMS verification.
		PhoneRequired: false,
	}, nil
}

func (c *nativeTeamLifecycleConnector) VerifyPhone(ctx context.Context, operation teamflow.Operation) (string, error) {
	resolved, err := c.operationContext(ctx, operation, true)
	if err != nil {
		return "", err
	}
	token, err := c.store.GetTokenFresh(ctx, resolved.child.ID)
	if err != nil || strings.TrimSpace(accountprovider.Credential("codex", token)) == "" {
		return "", teamflow.Retryable("team_phone_verification_pending", errors.New("phone verification has not produced a credential"))
	}
	return "account_auth_tokens:" + resolved.child.ID + ":oauth", nil
}

func (c *nativeTeamLifecycleConnector) memberIdentity(
	ctx context.Context,
	resolved teamConnectorContext,
	email string,
) (string, bool, error) {
	parentToken, err := c.parentToken(ctx, resolved)
	if err != nil {
		return "", false, err
	}
	raw, err := c.listTeamCollection(ctx, resolved, parentToken.AccessToken, "users")
	if err != nil {
		return "", false, err
	}
	id, found, err := teamFindIdentity(raw, email)
	if err != nil {
		return "", false, teamflow.Retryable("team_members_response_invalid", err)
	}
	return id, found, nil
}

func (c *nativeTeamLifecycleConnector) ImportAccount(ctx context.Context, operation teamflow.Operation) (string, error) {
	resolved, err := c.operationContext(ctx, operation, true)
	if err != nil {
		return "", err
	}
	email := childEmail(operation, resolved)
	if required := strings.TrimSpace(operation.Workflow.RequiredEmailDomain); required != "" &&
		storage.EmailDomain(email) != required {
		return "", teamflow.Permanent("team_child_domain_mismatch", errors.New("child account is outside the required team domain"))
	}
	token, err := c.store.GetTokenFresh(ctx, resolved.child.ID)
	if err != nil || strings.TrimSpace(accountprovider.Credential("codex", token)) == "" {
		return "", teamflow.Retryable("team_import_credential_missing", errors.New("child credential is not ready"))
	}
	memberID, found, err := c.memberIdentity(ctx, resolved, email)
	if err != nil {
		return "", err
	}
	if !found {
		return "", teamflow.Retryable("team_membership_not_settled", errors.New("child is not visible in team membership yet"))
	}
	account := resolved.child
	account.UpstreamAccountID = resolved.workspaceRef
	if memberID != "" {
		account.ChatGPTUserID = memberID
	}
	if err := c.store.UpsertAccount(ctx, account, token); err != nil {
		return "", teamflow.Retryable("team_import_persist_failed", err)
	}
	return account.ID, nil
}

func (c *nativeTeamLifecycleConnector) RemoveMember(ctx context.Context, operation teamflow.Operation) error {
	resolved, err := c.operationContext(ctx, operation, true)
	if err != nil {
		return err
	}
	email := childEmail(operation, resolved)
	memberID, found, err := c.memberIdentity(ctx, resolved, email)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if strings.TrimSpace(memberID) == "" {
		return teamflow.Permanent("team_member_reference_missing", errors.New("team member response omitted the member id"))
	}
	parentToken, err := c.parentToken(ctx, resolved)
	if err != nil {
		return err
	}
	headers := teamAuthHeaders(parentToken.AccessToken, resolved.workspaceRef)
	headers.Set("Idempotency-Key", operation.OperationKey)
	rawURL := c.backendBase + "/accounts/" + url.PathEscape(resolved.workspaceRef) +
		"/users/" + url.PathEscape(memberID)
	_, status, callErr := c.call(ctx, resolved.parent, http.MethodDelete, rawURL, headers, nil)
	if callErr != nil {
		return callErr
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return teamHTTPError("team_remove_member", status)
	}
	return nil
}

var _ teamflow.RemoteConnector = (*nativeTeamLifecycleConnector)(nil)
