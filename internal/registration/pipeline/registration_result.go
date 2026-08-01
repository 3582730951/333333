package pipeline

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/registration/httpclient"
	"codex-account-pool/internal/storage"
)

type registrationCredential struct {
	LabelPrefix       string
	Email             string
	UpstreamAccountID string
	ChatGPTUserID     string
	AccessToken       string
	RefreshToken      string
	IDToken           string
	SessionToken      string
	LoginPassword     string
}

func registrationSessionCookie(sessionToken string) string {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" || len(sessionToken) > 64<<10 || strings.ContainsAny(sessionToken, "\r\n") {
		return ""
	}
	if strings.Contains(sessionToken, "=") {
		return sessionToken
	}
	return "__Secure-next-auth.session-token=" + sessionToken
}

func (p *Pipeline) updateWorkflow(ctx context.Context, req RegisterRequest, state, errorClass string) {
	if p == nil || p.store == nil || strings.TrimSpace(req.WorkflowItemID) == "" {
		return
	}
	_ = p.store.UpdateRegistrationWorkflowItem(ctx, req.WorkflowItemID, state, errorClass)
}

func (p *Pipeline) finalizeWorkflow(ctx context.Context, req RegisterRequest, state, errorClass string) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	persistCtx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	p.updateWorkflow(persistCtx, req, state, errorClass)
}

func (p *Pipeline) persistVerifiedRegistration(ctx context.Context, req RegisterRequest, candidate registrationCredential) (*storage.Account, error) {
	p.updateWorkflow(ctx, req, storage.RegistrationItemCredentialsObtained, "")
	parsed, err := authparse.ParseAccessToken(candidate.AccessToken, candidate.UpstreamAccountID)
	if err != nil {
		p.finalizeWorkflow(ctx, req, storage.RegistrationItemQuarantined, "credential_invalid")
		return nil, fmt.Errorf("registration credential validation failed: %w", err)
	}
	if got, want := strings.TrimSpace(parsed.UpstreamAccountID), strings.TrimSpace(candidate.UpstreamAccountID); want != "" && got != want {
		p.finalizeWorkflow(ctx, req, storage.RegistrationItemQuarantined, "remote_identity_mismatch")
		return nil, errors.New("registration remote account identity mismatch")
	}
	if got, want := strings.TrimSpace(parsed.ChatGPTUserID), strings.TrimSpace(candidate.ChatGPTUserID); want != "" && got != "" && got != want {
		p.finalizeWorkflow(ctx, req, storage.RegistrationItemQuarantined, "remote_identity_mismatch")
		return nil, errors.New("registration remote user identity mismatch")
	}
	if got, want := strings.TrimSpace(parsed.Email), strings.TrimSpace(candidate.Email); want != "" && got != "" && !strings.EqualFold(got, want) {
		p.finalizeWorkflow(ctx, req, storage.RegistrationItemQuarantined, "remote_identity_mismatch")
		return nil, errors.New("registration remote email identity mismatch")
	}

	p.updateWorkflow(ctx, req, storage.RegistrationItemRemoteAccountVerifying, "")
	if p.remoteVerificationRequired || req.Canary || strings.TrimSpace(req.ReadinessFingerprint) != "" {
		if err := p.verifyRegistrationLiveness(ctx, req, parsed.AccessToken, parsed.UpstreamAccountID); err != nil {
			p.finalizeWorkflow(ctx, req, storage.RegistrationItemQuarantined, "remote_liveness_failed")
			return nil, err
		}
	}

	now := time.Now().Unix()
	accountStatus := "active"
	labelPrefix := strings.TrimSpace(candidate.LabelPrefix)
	if req.Canary {
		accountStatus = "quarantined"
		labelPrefix = "canary-" + labelPrefix
	}
	account := storage.Account{
		ID:                parsed.AccountID,
		Label:             labelPrefix + firstNonEmpty(parsed.Email, candidate.Email, parsed.UpstreamAccountID),
		GroupName:         req.GroupName,
		UpstreamAccountID: parsed.UpstreamAccountID,
		ChatGPTUserID:     parsed.ChatGPTUserID,
		Email:             firstNonEmpty(parsed.Email, candidate.Email),
		PlanType:          parsed.PlanType,
		Provider:          "codex",
		Status:            accountStatus,
		IsFedramp:         parsed.IsFedramp,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	token := storage.AccountToken{
		AccountID:      account.ID,
		AuthMethod:     "oauth",
		CredentialMode: authparse.CredentialModeChatGPTAuthTokens,
		AccessToken:    parsed.AccessToken,
		RefreshToken:   strings.TrimSpace(candidate.RefreshToken),
		IDTokenRaw:     firstNonEmpty(strings.TrimSpace(candidate.IDToken), parsed.IDTokenRaw),
		LastRefresh:    now,
		ExpiresAt:      parsed.ExpiresAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	p.updateWorkflow(ctx, req, storage.RegistrationItemImportedProvisioning, "")
	if err := p.store.CommitRegistration(ctx, storage.RegistrationCommit{
		Account: account, Token: token, EgressID: req.EgressID, Method: req.Method,
		JobID: req.JobID, RecordID: req.RecordID, WorkflowItemID: req.WorkflowItemID,
		RemoteIdentityAlias: p.registrationIdentityAlias(parsed.AccountID),
		SessionCookie:       registrationSessionCookie(candidate.SessionToken),
		LoginPassword:       candidate.LoginPassword,
	}); err != nil {
		state := storage.RegistrationItemFailed
		class := "import_failed"
		if errors.Is(err, storage.ErrRegistrationIdentityExists) {
			state = storage.RegistrationItemQuarantined
			class = "duplicate_remote_identity"
		}
		p.finalizeWorkflow(ctx, req, state, class)
		return nil, fmt.Errorf("commit verified registration: %w", err)
	}
	return &account, nil
}

func (p *Pipeline) verifyRegistrationLiveness(ctx context.Context, req RegisterRequest, accessToken, upstreamAccountID string) error {
	if p.upstream == nil {
		return errors.New("registration verification transport unavailable")
	}
	egress, err := p.store.GetEgressProfile(ctx, req.EgressID)
	if err != nil {
		return errors.New("registration verification egress unavailable")
	}
	var client *http.Client
	if sidecar := p.upstream.SidecarEndpoint(); sidecar != "" && httpclient.SidecarHealthy(ctx, sidecar) {
		client = httpclient.NewSidecarClient(sidecar, egress.Endpoint, "registration_verify_"+req.WorkflowItemID)
	} else {
		client, err = p.upstream.EgressHTTPClient(egress)
		if err != nil || client == nil {
			return errors.New("registration verification transport unavailable")
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(verifyCtx, http.MethodGet, "https://chatgpt.com/backend-api/models", nil)
	if err != nil {
		return errors.New("registration verification request failed")
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("User-Agent", "Mozilla/5.0")
	if strings.TrimSpace(upstreamAccountID) != "" {
		httpReq.Header.Set("ChatGPT-Account-ID", upstreamAccountID)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return errors.New("registration remote liveness check failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("registration remote liveness rejected with status %d", resp.StatusCode)
	}
	var envelope interface{}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 256<<10))
	if err := decoder.Decode(&envelope); err != nil || envelope == nil {
		return errors.New("registration remote liveness returned an invalid response")
	}
	return nil
}

var registrationAliasBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func (p *Pipeline) registrationIdentityAlias(rawID string) string {
	if p == nil || p.cfg == nil || len(p.cfg.RuntimeDiagnosticAliasKey) < 32 || strings.TrimSpace(rawID) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, p.cfg.RuntimeDiagnosticAliasKey)
	_, _ = mac.Write([]byte("codex-pool-diagnostic-alias-v3\x00"))
	_, _ = mac.Write([]byte("account"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(rawID))
	return "ACC-" + registrationAliasBase32.EncodeToString(mac.Sum(nil)[:16])
}
