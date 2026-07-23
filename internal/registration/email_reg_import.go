package registration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/agentidentity"
	"codex-account-pool/internal/storage"
)

// ConvertToAgentIdentity generates an Ed25519 keypair, creates an agent runtime ID,
// and registers a task with OpenAI's agent identity API. Returns the credentials
// needed for the account pool.
func ConvertToAgentIdentity(ctx context.Context, httpClient *http.Client, accessToken string) (*agentidentity.Credentials, error) {
	// Generate Ed25519 keypair
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	privateKeyB64 := base64.StdEncoding.EncodeToString(der)

	// Create runtime ID from public key fingerprint
	runtimeID := fmt.Sprintf("agent-%x", pub[:12])

	creds := agentidentity.Credentials{
		RuntimeID:  runtimeID,
		PrivateKey: privateKeyB64,
	}

	// Validate
	if err := agentidentity.Validate(creds, false); err != nil {
		return nil, fmt.Errorf("validate credentials: %w", err)
	}

	// Register a task
	now := time.Now()
	regURL, regBody, err := agentidentity.BuildTaskRegistration(creds, agentidentity.AuthAPIBaseURL, now)
	if err != nil {
		return nil, fmt.Errorf("build task registration: %w", err)
	}

	// Execute the registration request
	req, err := http.NewRequestWithContext(ctx, "POST", regURL, strings.NewReader(string(regBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("task registration request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respBody)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, fmt.Errorf("task registration returned HTTP %d: %s", resp.StatusCode, msg)
	}

	taskID, err := agentidentity.ParseTaskRegistrationResponse(creds, respBody)
	if err != nil {
		return nil, fmt.Errorf("parse task registration: %w", err)
	}

	creds.TaskID = taskID
	return &creds, nil
}

// BuildAccountFromResult creates storage.Account and storage.AccountToken from a
// registration result and agent identity credentials.
func BuildAccountFromResult(result *RegisterResult, creds *agentidentity.Credentials, groupName string) (*storage.Account, *storage.AccountToken) {
	now := storage.Now()
	accountID := result.AccountID
	if accountID == "" {
		accountID = fmt.Sprintf("emreg_%x", now)
	}

	account := &storage.Account{
		ID:                accountID,
		Label:             result.Email,
		GroupName:         groupName,
		UpstreamAccountID: result.AccountID,
		Email:             result.Email,
		PlanType:          result.PlanType,
		Provider:          "codex",
		Status:            "active",
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	token := &storage.AccountToken{
		AccountID:       accountID,
		CredentialMode:  agentidentity.CredentialMode,
		AccessToken:     result.AccessToken,
		RefreshToken:    result.SessionToken,
		AgentRuntimeID:  creds.RuntimeID,
		AgentPrivateKey: creds.PrivateKey,
		AgentTaskID:     creds.TaskID,
		LastRefresh:     now,
		Scopes:          "openid email profile offline_access model.request",
	}

	return account, token
}

// BuildAuthJSONForImport creates auth.json formatted bytes suitable for
// importing via the existing pool import logic.
func BuildAuthJSONForImport(result *RegisterResult, creds *agentidentity.Credentials) ([]byte, error) {
	doc := map[string]interface{}{
		"auth_mode":      agentidentity.CredentialMode,
		"OPENAI_API_KEY": nil,
		"agent_identity": map[string]interface{}{
			"agent_runtime_id":              creds.RuntimeID,
			"agent_private_key":             creds.PrivateKey,
			"account_id":                    result.AccountID,
			"email":                         result.Email,
			"plan_type":                     result.PlanType,
			"chatgpt_account_is_fedramp":    false,
			"task_id":                       creds.TaskID,
		},
		"session_token": result.SessionToken,
		"cookies":       result.Cookies,
	}

	return json.Marshal(doc)
}

// BuildSub2APIExport creates a sub2api-compatible export document.
func BuildSub2APIExport(result *RegisterResult, creds *agentidentity.Credentials) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	doc := map[string]interface{}{
		"type":       "sub2api-data",
		"version":    1,
		"exported_at": now,
		"proxies":    []interface{}{},
		"accounts": []map[string]interface{}{
			{
				"name":     result.Email,
				"platform": "openai",
				"type":     "oauth",
				"credentials": map[string]interface{}{
					"auth_mode":       agentidentity.CredentialMode,
					"agent_runtime_id": creds.RuntimeID,
					"agent_private_key": creds.PrivateKey,
					"task_id":          creds.TaskID,
					"account_id":       result.AccountID,
					"chatgpt_account_id": result.AccountID,
					"email":            result.Email,
					"plan_type":        result.PlanType,
					"session_token":    result.SessionToken,
					"cookies":          result.Cookies,
				},
				"extra": map[string]interface{}{
					"email":              result.Email,
					"source":             "chatgpt_email_registration",
					"last_refresh":       now,
					"account_id":         result.AccountID,
					"has_session_token":  result.SessionToken != "",
					"has_cookies":        len(result.Cookies) > 0,
				},
				"concurrency":         10,
				"priority":            1,
				"rate_multiplier":     1,
				"auto_pause_on_expired": true,
			},
		},
		"auth_mode":       agentidentity.CredentialMode,
		"OPENAI_API_KEY":  nil,
		"agent_identity": map[string]interface{}{
			"agent_runtime_id":  creds.RuntimeID,
			"agent_private_key": creds.PrivateKey,
			"account_id":        result.AccountID,
			"email":             result.Email,
			"plan_type":         result.PlanType,
			"task_id":           creds.TaskID,
		},
	}

	return json.MarshalIndent(doc, "", "  ")
}
