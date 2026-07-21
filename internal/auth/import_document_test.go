package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func agentPrivateKeyForTest(t *testing.T) string {
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

func TestParseSub2APIDataAgentIdentityAndIsolateBadAccounts(t *testing.T) {
	privateKey := agentPrivateKeyForTest(t)
	payload := map[string]interface{}{
		"type": "sub2api-data", "version": 1, "exported_at": "2026-07-21T09:10:19Z",
		"proxies": []interface{}{},
		"accounts": []interface{}{
			map[string]interface{}{
				"name": "agent@example.com", "platform": "openai", "type": "oauth", "concurrency": 10, "priority": 1,
				"credentials": map[string]interface{}{
					"auth_mode": "agentIdentity", "agent_runtime_id": "agent-runtime", "agent_private_key": privateKey,
					"task_id": "task-id", "account_id": "chatgpt-account", "chatgpt_user_id": "chatgpt-user",
					"email": "agent@example.com", "plan_type": "k12", "id_token": "synthetic.must.not.be.used",
				},
			},
			map[string]interface{}{"name": "wrong", "platform": "anthropic", "type": "oauth", "credentials": map[string]interface{}{"access_token": "secret"}},
		},
	}
	raw, _ := json.Marshal(payload)
	doc, err := ParseImportDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != ImportFormatSub2API || len(doc.Entries) != 2 {
		t.Fatalf("unexpected document: %+v", doc)
	}
	first := doc.Entries[0]
	if first.Err != nil || first.Parsed.CredentialMode != "agent_identity" || first.Parsed.AgentPrivateKey != privateKey {
		t.Fatalf("agent entry: %+v err=%v", first.Parsed, first.Err)
	}
	if first.Parsed.AccessToken != "" || first.Parsed.IDTokenRaw != "" {
		t.Fatalf("synthetic id_token became a bearer credential: %+v", first.Parsed)
	}
	if first.Parsed.UpstreamAccountID != "chatgpt-account" || first.Parsed.ChatGPTUserID != "chatgpt-user" || len(first.Warnings) == 0 {
		t.Fatalf("metadata/warnings missing: %+v", first)
	}
	if doc.Entries[1].Err == nil {
		t.Fatal("unsupported platform must fail only its own item")
	}
}

func TestParseSub2APIAgentIdentitySeparatesUsersInSharedWorkspace(t *testing.T) {
	firstKey := agentPrivateKeyForTest(t)
	secondKey := agentPrivateKeyForTest(t)
	buildEntry := func(userID, privateKey string) map[string]interface{} {
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
		"accounts": []interface{}{buildEntry("user-one", firstKey), buildEntry("user-two", secondKey)},
	}
	raw, _ := json.Marshal(payload)
	doc, err := ParseImportDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 2 || doc.Entries[0].Err != nil || doc.Entries[1].Err != nil {
		t.Fatalf("unexpected document: %+v", doc)
	}
	if doc.Entries[0].Parsed.AccountID == doc.Entries[1].Parsed.AccountID {
		t.Fatalf("users in a shared workspace were assigned the same account id: %q", doc.Entries[0].Parsed.AccountID)
	}
}

func TestParseSub2APIDataValidatesHeaderAndPrivateKey(t *testing.T) {
	for _, raw := range []string{
		`{"type":"sub2api-data","version":2,"proxies":[],"accounts":[]}`,
		`{"type":"sub2api-data","version":1,"proxies":[],"accounts":[{"name":"bad","platform":"openai","type":"oauth","credentials":{"auth_mode":"agentIdentity","agent_runtime_id":"runtime","agent_private_key":"not-base64","account_id":"account","chatgpt_user_id":"user"}}]}`,
	} {
		doc, err := ParseImportDocument([]byte(raw))
		if err == nil && (len(doc.Entries) == 0 || doc.Entries[0].Err == nil) {
			t.Fatalf("invalid document accepted: %s %+v", raw, doc)
		}
	}
}

func TestParseImportDocumentKeepsSingleAuthJSONCompatibility(t *testing.T) {
	doc, err := ParseImportDocument([]byte(`{"access_token":"access","refresh_token":"refresh","account_id":"account"}`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != ImportFormatSingle || len(doc.Entries) != 1 || doc.Entries[0].Parsed.AccessToken != "access" {
		t.Fatalf("unexpected document: %+v", doc)
	}
}

func TestParseSub2APIDataRegularOpenAIOAuth(t *testing.T) {
	raw := []byte(`{"type":"sub2api-data","version":1,"proxies":[],"accounts":[{"name":"oauth@example.com","platform":"openai","type":"oauth","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account","chatgpt_user_id":"user","email":"oauth@example.com"}}]}`)
	doc, err := ParseImportDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 1 || doc.Entries[0].Err != nil {
		t.Fatalf("unexpected document: %+v", doc)
	}
	parsed := doc.Entries[0].Parsed
	if parsed.AccessToken != "access" || parsed.RefreshToken != "refresh" || parsed.UpstreamAccountID != "account" || parsed.ChatGPTUserID != "user" {
		t.Fatalf("unexpected OAuth credentials: %+v", parsed)
	}
}
