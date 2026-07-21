package api

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func sub2APIAgentPrivateKey(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func TestAgentIdentityMissingTaskRegistersAndPersistsThroughBoundEgress(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(der)
	registration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/runtime-register/task/register" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request struct {
			Timestamp string `json:"timestamp"`
			Signature string `json:"signature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
		}
		signature, _ := base64.StdEncoding.DecodeString(request.Signature)
		if _, err := time.Parse(time.RFC3339, request.Timestamp); err != nil || !ed25519.Verify(publicKey, []byte("runtime-register:"+request.Timestamp), signature) {
			t.Errorf("invalid registration signature timestamp=%q err=%v", request.Timestamp, err)
		}
		_, _ = w.Write([]byte(`{"task_id":"task-registered"}`))
	}))
	defer registration.Close()
	previousBaseURL := agentIdentityAuthAPIBaseURL
	agentIdentityAuthAPIBaseURL = registration.URL
	defer func() { agentIdentityAuthAPIBaseURL = previousBaseURL }()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) })
	account := storage.Account{ID: "agent-register", Label: "agent-register", GroupName: "cyber", Provider: "codex", Status: "active", UpstreamAccountID: "workspace-register", ChatGPTUserID: "user-register"}
	token := storage.AccountToken{AuthMethod: "oauth", CredentialMode: "agent_identity", AgentRuntimeID: "runtime-register", AgentPrivateKey: encodedKey}
	if err := h.store.UpsertAccount(context.Background(), account, token); err != nil {
		t.Fatal(err)
	}
	binding, _ := h.store.GetEgressBinding(context.Background(), account.ID)
	egress, _ := h.store.ResolvePrimaryEgressBinding(context.Background(), binding)
	recovered, err := h.app.ensureAgentIdentityTask(context.Background(), account, token, egress, binding.CookieJarKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.AgentTaskID != "task-registered" {
		t.Fatalf("task = %q", recovered.AgentTaskID)
	}
	persisted, err := h.store.GetToken(context.Background(), account.ID)
	if err != nil || persisted.AgentTaskID != "task-registered" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestAdminImportSub2APIDataImportsAgentIdentityProxyAndIsolatesErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	privateKey := sub2APIAgentPrivateKey(t)
	proxyURL, _ := url.Parse(h.upstream.URL)
	proxyHost, proxyPortText, _ := net.SplitHostPort(proxyURL.Host)
	proxyPort, _ := strconv.Atoi(proxyPortText)
	proxyKey := fmt.Sprintf("http|%s|%d|user|pass", proxyHost, proxyPort)
	payload := map[string]interface{}{
		"type": "sub2api-data", "version": 1, "exported_at": "2026-07-21T09:10:19Z",
		"proxies": []interface{}{map[string]interface{}{
			"proxy_key": proxyKey, "name": "sub proxy", "protocol": "http", "host": proxyHost, "port": proxyPort,
			"username": "user", "password": "pass", "status": "active",
		}},
		"accounts": []interface{}{
			map[string]interface{}{
				"name": "agent@example.com", "platform": "openai", "type": "oauth", "proxy_key": proxyKey,
				"credentials": map[string]interface{}{
					"auth_mode": "agentIdentity", "agent_runtime_id": "runtime-import", "agent_private_key": privateKey,
					"task_id": "task-import", "account_id": "account-import", "chatgpt_user_id": "user-import",
					"email": "agent@example.com", "plan_type": "k12",
				},
			},
			map[string]interface{}{"name": "wrong", "platform": "anthropic", "type": "oauth", "credentials": map[string]interface{}{"access_token": "must-not-import"}},
		},
	}
	requestBody, _ := json.Marshal(map[string]interface{}{"auth_json": payload, "group_name": "cyber"})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(requestBody))
	if code != http.StatusOK {
		t.Fatalf("import = %d: %s", code, raw)
	}
	if strings.Contains(string(raw), privateKey) {
		t.Fatal("import response leaked Agent Identity private key")
	}
	var result authDocumentImportResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Format != "sub2api-data" || result.Total != 2 || result.Imported != 1 || result.Failed != 1 || result.ProxyCreated != 1 {
		t.Fatalf("unexpected result: %+v body=%s", result, raw)
	}
	accountID := result.Items[0].AccountID
	if accountID == "" || result.Items[0].Action != "imported" || result.Items[1].Action != "failed" {
		t.Fatalf("unexpected items: %+v", result.Items)
	}
	token, err := h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if token.AuthMethod != "oauth" || token.CredentialMode != "agent_identity" || token.AgentRuntimeID != "runtime-import" || token.AgentPrivateKey != privateKey || token.AgentTaskID != "task-import" || token.AccessToken != "" {
		t.Fatalf("stored token shape is wrong: %+v", token)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil || !strings.HasPrefix(binding.PrimaryEgressID, "sub2api_") {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	profile, err := h.store.GetEgressProfile(context.Background(), binding.PrimaryEgressID)
	expectedEndpoint := "http://user:pass@" + net.JoinHostPort(proxyHost, proxyPortText)
	if err != nil || profile.Type != "http_proxy" || profile.Endpoint != expectedEndpoint {
		t.Fatalf("profile=%+v err=%v", profile, err)
	}
	code, accountsRaw := grpReq(t, h, http.MethodGet, "/admin/accounts", "")
	if code != http.StatusOK || strings.Contains(string(accountsRaw), privateKey) || !strings.Contains(string(accountsRaw), `"credential_mode":"agent_identity"`) {
		t.Fatalf("unsafe/incomplete account response: %d %s", code, accountsRaw)
	}
}

func TestAdminImportSub2APIDataSeparatesAgentUsersInSharedWorkspace(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	buildAccount := func(userID, privateKey string) map[string]interface{} {
		return map[string]interface{}{
			"name": userID + "@example.com", "platform": "openai", "type": "oauth",
			"credentials": map[string]interface{}{
				"auth_mode": "agentIdentity", "agent_runtime_id": "runtime-" + userID, "agent_private_key": privateKey,
				"task_id": "task-" + userID, "account_id": "shared-workspace", "chatgpt_user_id": userID,
			},
		}
	}
	payload := map[string]interface{}{
		"type": "sub2api-data", "version": 1, "proxies": []interface{}{},
		"accounts": []interface{}{
			buildAccount("user-one", sub2APIAgentPrivateKey(t)),
			buildAccount("user-two", sub2APIAgentPrivateKey(t)),
		},
	}
	requestBody, _ := json.Marshal(map[string]interface{}{"auth_json": payload, "group_name": "cyber"})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(requestBody))
	if code != http.StatusOK {
		t.Fatalf("import = %d: %s", code, raw)
	}
	var result authDocumentImportResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Duplicates != 0 || result.Failed != 0 || len(result.Items) != 2 {
		t.Fatalf("unexpected result: %+v body=%s", result, raw)
	}
	if result.Items[0].AccountID == "" || result.Items[0].AccountID == result.Items[1].AccountID {
		t.Fatalf("distinct Agent Identity users received duplicate account ids: %+v", result.Items)
	}
}

func TestAdminImportSub2APIDataKeepsLegacyAgentIdentityDuplicateScopedToUser(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	legacyKey := sub2APIAgentPrivateKey(t)
	legacy := storage.Account{
		ID: "legacy-agent-identity", Label: "legacy", GroupName: "cyber", Provider: "codex", Status: "active",
		UpstreamAccountID: "shared-workspace", ChatGPTUserID: "user-one",
	}
	if err := h.store.UpsertAccount(context.Background(), legacy, storage.AccountToken{
		AuthMethod: "oauth", CredentialMode: "agent_identity", AgentRuntimeID: "legacy-runtime", AgentPrivateKey: legacyKey,
	}); err != nil {
		t.Fatal(err)
	}
	buildAccount := func(userID, privateKey string) map[string]interface{} {
		return map[string]interface{}{
			"name": userID + "@example.com", "platform": "openai", "type": "oauth",
			"credentials": map[string]interface{}{
				"auth_mode": "agentIdentity", "agent_runtime_id": "runtime-" + userID, "agent_private_key": privateKey,
				"task_id": "task-" + userID, "account_id": "shared-workspace", "chatgpt_user_id": userID,
			},
		}
	}
	payload := map[string]interface{}{
		"type": "sub2api-data", "version": 1, "proxies": []interface{}{},
		"accounts": []interface{}{
			buildAccount("user-one", legacyKey),
			buildAccount("user-two", sub2APIAgentPrivateKey(t)),
		},
	}
	requestBody, _ := json.Marshal(map[string]interface{}{"auth_json": payload, "group_name": "cyber"})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(requestBody))
	if code != http.StatusOK {
		t.Fatalf("import = %d: %s", code, raw)
	}
	var result authDocumentImportResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.Duplicates != 1 || result.Failed != 0 {
		t.Fatalf("unexpected result: %+v body=%s", result, raw)
	}
	if result.Items[0].Action != "duplicate" || result.Items[0].AccountID != legacy.ID || result.Items[1].Action != "imported" {
		t.Fatalf("legacy/new split was not preserved: %+v", result.Items)
	}
}
