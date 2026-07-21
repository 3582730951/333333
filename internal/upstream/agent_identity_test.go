package upstream

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func upstreamAgentToken(t *testing.T) storage.AccountToken {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return storage.AccountToken{
		AuthMethod: "oauth", CredentialMode: "agent_identity", AgentRuntimeID: "runtime-upstream",
		AgentPrivateKey: base64.StdEncoding.EncodeToString(der), AgentTaskID: "task-upstream",
	}
}

func TestAgentIdentityAuthorizationIsSignedForHTTPAndWebSocket(t *testing.T) {
	client := NewClient(config.Default())
	token := upstreamAgentToken(t)
	spec := Request{
		Method: http.MethodPost, DownstreamPath: "/v1/responses", Body: []byte(`{"model":"gpt-5.5","input":"hi"}`),
		Account: storage.Account{ID: "agent-account", UpstreamAccountID: "workspace-upstream"}, Token: token,
	}
	headers := http.Header{}
	if err := client.applyCodexHeaders(headers, spec); err != nil {
		t.Fatal(err)
	}
	assertAgentAuthorization(t, headers.Get("Authorization"), "runtime-upstream", "task-upstream")
	if getHeaderFold(headers, "ChatGPT-Account-ID") != "workspace-upstream" {
		t.Fatalf("missing account header: %v", headers)
	}
	_, wsHeaders, _, err := client.prepareCodexResponsesWebSocket(spec)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentAuthorization(t, wsHeaders.Get("Authorization"), "runtime-upstream", "task-upstream")
}

func TestAgentIdentityMissingTaskFailsClosed(t *testing.T) {
	client := NewClient(config.Default())
	token := upstreamAgentToken(t)
	token.AgentTaskID = ""
	err := client.applyCodexHeaders(http.Header{}, Request{Token: token})
	if err == nil || !strings.Contains(err.Error(), "task id is missing") {
		t.Fatalf("error = %v", err)
	}
}

func assertAgentAuthorization(t *testing.T, authorization, runtimeID, taskID string) {
	t.Helper()
	if !strings.HasPrefix(authorization, "AgentAssertion ") {
		t.Fatalf("authorization = %q", authorization)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(authorization, "AgentAssertion "))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		RuntimeID string `json:"agent_runtime_id"`
		TaskID    string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.RuntimeID != runtimeID || envelope.TaskID != taskID {
		t.Fatalf("unexpected assertion envelope: %+v", envelope)
	}
}
