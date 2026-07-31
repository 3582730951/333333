package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/registration/teamflow"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

type teamConnectorRequest struct {
	ActorID string
	Method  string
	URL     string
	Headers http.Header
	Body    string
}

func teamConnectorResponse(status int, payload interface{}) *upstream.Response {
	raw := []byte{}
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	return &upstream.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}

func seedTeamConnectorFixture(t *testing.T) (*testHarness, storage.TeamLifecycleWorkflow) {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: "parent", Email: "parent@example.test", Provider: "codex", Status: "active",
		UpstreamAccountID: "parent-space",
	}, storage.AccountToken{AccountID: "parent", AccessToken: "parent-access"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: "child", Email: "child@example.test", Provider: "codex", Status: "active",
		UpstreamAccountID: "personal-space",
	}, storage.AccountToken{AccountID: "child", AccessToken: "child-access", RefreshToken: "child-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSessionCookie(ctx, "child", "__Secure-next-auth.session-token=child-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertTeamWorkspace(ctx, storage.TeamWorkspace{
		ID: "workspace", Name: "Fixture", ParentAccountID: "parent",
		WorkspaceRef: "team-space", ConnectorKind: "native", MaxMembers: 10,
		Status: storage.TeamWorkspaceStatusActive, RequiredEmailDomain: "example.test",
		SameDomainRequired: true,
	}); err != nil {
		t.Fatal(err)
	}
	return h, storage.TeamLifecycleWorkflow{
		ID: "workflow", WorkspaceID: "workspace", ParentAccountID: "parent",
		ChildAccountID: "child", RequiredEmailDomain: "example.test",
	}
}

func TestNativeTeamLifecycleConnectorInvitePATImportAndRemove(t *testing.T) {
	h, workflow := seedTeamConnectorFixture(t)
	connector := newNativeTeamLifecycleConnector(h.app)
	connector.origin = "https://fixture.invalid"
	connector.backendBase = connector.origin + "/backend-api"

	var mu sync.Mutex
	requests := make([]teamConnectorRequest, 0)
	memberVisible := false
	connector.doRaw = func(
		_ context.Context,
		actor storage.Account,
		method, rawURL string,
		headers http.Header,
		body []byte,
	) (*upstream.Response, error) {
		mu.Lock()
		requests = append(requests, teamConnectorRequest{
			ActorID: actor.ID, Method: method, URL: rawURL,
			Headers: headers.Clone(), Body: string(body),
		})
		mu.Unlock()
		switch {
		case method == http.MethodGet && strings.Contains(rawURL, "/api/auth/session"):
			return teamConnectorResponse(http.StatusOK, map[string]interface{}{
				"accessToken":  "child-team-access",
				"sessionToken": "rotated-session",
				"account":      map[string]interface{}{"id": "team-space", "planType": "team"},
				"user":         map[string]interface{}{"id": "child-user", "email": "child@example.test"},
			}), nil
		case method == http.MethodPost && strings.HasSuffix(rawURL, "/wham/auth-credentials"):
			memberVisible = true
			return teamConnectorResponse(http.StatusOK, map[string]string{"access_token": "child-personal-access-token"}), nil
		case method == http.MethodGet && strings.Contains(rawURL, "/users"):
			if memberVisible {
				return teamConnectorResponse(http.StatusOK, map[string]interface{}{
					"items": []interface{}{map[string]interface{}{
						"id": "child-user", "email": "child@example.test",
					}},
				}), nil
			}
			return teamConnectorResponse(http.StatusOK, map[string]interface{}{"items": []interface{}{}}), nil
		case method == http.MethodGet && strings.HasSuffix(rawURL, "/invites"):
			return teamConnectorResponse(http.StatusOK, map[string]interface{}{"items": []interface{}{}}), nil
		case method == http.MethodPost && strings.HasSuffix(rawURL, "/invites"):
			return teamConnectorResponse(http.StatusOK, map[string]interface{}{"id": "invite"}), nil
		case method == http.MethodDelete && strings.Contains(rawURL, "/users/child-user"):
			memberVisible = false
			return teamConnectorResponse(http.StatusNoContent, nil), nil
		default:
			return teamConnectorResponse(http.StatusNotFound, map[string]string{"error": "fixture route"}), nil
		}
	}

	ctx := context.Background()
	inviteRef, err := connector.Invite(ctx, teamflow.Operation{
		Workflow: workflow, OperationKey: "workflow:inviting",
	})
	if err != nil || !strings.HasPrefix(inviteRef, "team-membership:") {
		t.Fatalf("Invite ref=%q err=%v", inviteRef, err)
	}
	credentialRef, err := connector.LoginWithCredential(ctx, teamflow.Operation{
		Workflow: workflow, OperationKey: "workflow:credential_login",
	})
	if err != nil || credentialRef != "account_auth_tokens:child:personal_access_token" {
		t.Fatalf("LoginWithCredential ref=%q err=%v", credentialRef, err)
	}
	token, err := h.store.GetTokenFresh(ctx, "child")
	if err != nil ||
		token.CredentialMode != accountprovider.CredentialModePersonalAccessToken ||
		token.OpenAIAPIKey != "child-personal-access-token" ||
		accountprovider.Credential("codex", token) != "child-personal-access-token" {
		t.Fatalf("persisted personal token metadata=%+v err=%v", token, err)
	}
	accountID, err := connector.ImportAccount(ctx, teamflow.Operation{
		Workflow: workflow, OperationKey: "workflow:importing",
	})
	if err != nil || accountID != "child" {
		t.Fatalf("ImportAccount id=%q err=%v", accountID, err)
	}
	if err := connector.RemoveMember(ctx, teamflow.Operation{
		Workflow: workflow, OperationKey: "workflow:removing",
	}); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	account, err := h.store.GetAccount(ctx, "child")
	if err != nil || account.UpstreamAccountID != "team-space" || account.ChatGPTUserID != "child-user" {
		t.Fatalf("imported team account=%+v err=%v", account, err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawInvite, sawExchange, sawPAT, sawRemove bool
	for _, request := range requests {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL, "/invites"):
			sawInvite = request.ActorID == "parent" &&
				request.Headers.Get("Authorization") == "Bearer parent-access" &&
				strings.Contains(request.Body, "child@example.test")
		case request.Method == http.MethodGet && strings.Contains(request.URL, "/api/auth/session"):
			sawExchange = request.ActorID == "child" &&
				strings.Contains(request.Headers.Get("Cookie"), "child-session")
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL, "/wham/auth-credentials"):
			sawPAT = request.ActorID == "child" &&
				request.Headers.Get("Authorization") == "Bearer child-team-access"
		case request.Method == http.MethodDelete:
			sawRemove = request.ActorID == "parent"
		}
		if strings.Contains(request.Body, "child-personal-access-token") {
			t.Fatalf("personal token leaked into a request body: %+v", request)
		}
	}
	if !sawInvite || !sawExchange || !sawPAT || !sawRemove {
		t.Fatalf("missing connector stages invite=%v exchange=%v pat=%v remove=%v requests=%+v",
			sawInvite, sawExchange, sawPAT, sawRemove, requests)
	}
}

func TestNativeTeamLifecycleCredentialRejectionFallsBackToOAuth(t *testing.T) {
	h, workflow := seedTeamConnectorFixture(t)
	_ = h.store.SetSessionCookie(context.Background(), "child", "")
	connector := newNativeTeamLifecycleConnector(h.app)
	connector.doRaw = func(
		_ context.Context,
		_ storage.Account,
		method, rawURL string,
		_ http.Header,
		_ []byte,
	) (*upstream.Response, error) {
		if method == http.MethodPost && strings.HasSuffix(rawURL, "/wham/auth-credentials") {
			return teamConnectorResponse(http.StatusForbidden, map[string]string{"error": "fixture"}), nil
		}
		return teamConnectorResponse(http.StatusOK, map[string]interface{}{"items": []interface{}{}}), nil
	}
	_, err := connector.LoginWithCredential(context.Background(), teamflow.Operation{
		Workflow: workflow, OperationKey: "workflow:credential_login",
	})
	var fallback *teamflow.OAuthFallbackError
	if !errors.As(err, &fallback) {
		t.Fatalf("error=%T %v, want OAuthFallbackError", err, err)
	}
}

func TestRegisteredReplacementEnqueuesNextExecuteWorkflow(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(context.Background(), storage.Account{
		ID: "parent", Email: "parent@example.test", Provider: "codex", Status: "active",
	}, storage.AccountToken{AccountID: "parent", AccessToken: "parent-access"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(context.Background(), storage.Account{
		ID: "child", Email: "child@example.test", Provider: "codex", Status: "active",
	}, storage.AccountToken{AccountID: "child", AccessToken: "child-access"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTeamWorkspace(context.Background(), storage.TeamWorkspace{
		ID: "workspace", Name: "Fixture", ParentAccountID: "parent", WorkspaceRef: "team-space",
		ConnectorKind: "native", MaxMembers: 10, Status: storage.TeamWorkspaceStatusActive,
		RequiredEmailDomain: "example.test", SameDomainRequired: true,
	}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{store: store}
	req := pipeline.RegisterRequest{
		MailboxProvider:                 "cf_team",
		MailboxDomain:                   "example.test",
		TeamLifecycleSourceWorkflowID:   "previous",
		TeamLifecycleWorkspaceID:        "workspace",
		TeamLifecycleParentAccountID:    "parent",
		TeamLifecycleReplacementMethod:  "protocol_v2",
		TeamLifecycleRotateThresholdBPS: 100,
		TeamLifecycleMaxAttempts:        7,
	}
	if err := handler.enqueueRegisteredTeamLifecycle(context.Background(), req, "child"); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListTeamLifecycleWorkflows(context.Background(), "workspace", "", 20)
	if err != nil || len(items) != 1 {
		t.Fatalf("workflows=%+v err=%v", items, err)
	}
	item := items[0]
	if item.ShadowMode || item.ChildAccountID != "child" || item.MailboxProviderKey != "cf_team" ||
		item.RequiredEmailDomain != "example.test" || item.RotateThresholdBPS != 100 ||
		item.MaxAttempts != 7 {
		t.Fatalf("unexpected replacement workflow: %+v", item)
	}
	// The deterministic idempotency key makes a retried registration handoff a
	// no-op rather than a second invite loop.
	if err := handler.enqueueRegisteredTeamLifecycle(context.Background(), req, "child"); err != nil {
		t.Fatal(err)
	}
	items, _ = store.ListTeamLifecycleWorkflows(context.Background(), "workspace", "", 20)
	if len(items) != 1 {
		t.Fatalf("duplicate handoff created %d workflows", len(items))
	}
}
