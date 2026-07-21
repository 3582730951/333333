package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/agentidentity"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

var (
	agentIdentityAuthAPIBaseURL = agentidentity.AuthAPIBaseURL
	agentIdentityTaskLocks      sync.Map // account id -> *sync.Mutex
)

func agentIdentityCredentials(token storage.AccountToken) agentidentity.Credentials {
	return agentidentity.Credentials{
		RuntimeID: token.AgentRuntimeID, PrivateKey: token.AgentPrivateKey, TaskID: token.AgentTaskID,
	}
}

// ensureAgentIdentityTask registers a task when one is absent, or rotates the
// exact task rejected by the upstream. Re-reading under a per-account lock avoids
// duplicate registrations across concurrent inference/model/quota requests.
func (s *Server) ensureAgentIdentityTask(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile, cookieJarKey, expectedTaskID string) (storage.AccountToken, error) {
	if !accountprovider.IsAgentIdentity(token) {
		return token, nil
	}
	current := strings.TrimSpace(token.AgentTaskID)
	expectedTaskID = strings.TrimSpace(expectedTaskID)
	if current != "" && (expectedTaskID == "" || current != expectedTaskID) {
		return token, nil
	}
	candidate := &sync.Mutex{}
	actual, _ := agentIdentityTaskLocks.LoadOrStore(account.ID, candidate)
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		return token, fmt.Errorf("agent identity task lock is invalid")
	}
	lock.Lock()
	defer lock.Unlock()

	fresh, err := s.store.GetToken(ctx, account.ID)
	if err != nil {
		return token, err
	}
	if !accountprovider.IsAgentIdentity(fresh) {
		return token, fmt.Errorf("agent identity credentials are unavailable")
	}
	current = strings.TrimSpace(fresh.AgentTaskID)
	if current != "" && (expectedTaskID == "" || current != expectedTaskID) {
		return fresh, nil
	}

	registrationURL, body, err := agentidentity.BuildTaskRegistration(agentIdentityCredentials(fresh), agentIdentityAuthAPIBaseURL, time.Now())
	if err != nil {
		return token, err
	}
	headers := http.Header{"Content-Type": []string{"application/json"}, "Accept": []string{"application/json"}}
	resp, err := s.upstream.DoRaw(ctx, egress, http.MethodPost, registrationURL, headers, body, cookieJarKey)
	if err != nil {
		return token, fmt.Errorf("agent task registration request failed: %w", err)
	}
	raw, err := upstream.DrainAndClose(resp.Body)
	if err != nil {
		return token, fmt.Errorf("agent task registration response failed: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return token, agentidentity.RegistrationStatusError(resp.StatusCode)
	}
	newTaskID, err := agentidentity.ParseTaskRegistrationResponse(agentIdentityCredentials(fresh), raw)
	if err != nil {
		return token, err
	}
	fresh.AgentTaskID = newTaskID
	if err := s.store.UpdateToken(ctx, fresh); err != nil {
		return token, err
	}
	action := "agent_identity_task_registered"
	reason := "missing_task_id"
	if expectedTaskID != "" {
		action = "agent_identity_task_recovered"
		reason = "upstream_rejected_task"
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: account.Label, Action: action,
		State: "active", Reason: reason, Detail: "credential_mode=agent_identity",
	})
	s.scheduler.NotifyStateChanged()
	return fresh, nil
}

func redactAgentIdentityError(token storage.AccountToken, body []byte) []byte {
	if !accountprovider.IsAgentIdentity(token) {
		return body
	}
	return agentidentity.RedactSensitive(body, agentIdentityCredentials(token))
}

func isAgentIdentityToken(token storage.AccountToken) bool {
	return accountprovider.IsAgentIdentity(token)
}

func isInvalidAgentIdentityTask(status int, body []byte, token storage.AccountToken) bool {
	return accountprovider.IsAgentIdentity(token) && agentidentity.InvalidTaskResponse(status, body)
}

func agentIdentityAuthorization(token storage.AccountToken) (string, error) {
	return agentidentity.BuildAssertion(agentIdentityCredentials(token), time.Now())
}
