package api

import (
	"encoding/json"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func assertClaudeCodeIdentityWireShape(t *testing.T, raw, model string) {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		t.Fatalf("Claude identity body is not JSON: %v (%s)", err, raw)
	}
	if root["model"] != model {
		t.Fatalf("Claude identity body model=%#v want=%q: %s", root["model"], model, raw)
	}
	system, _ := root["system"].([]interface{})
	if len(system) < 2 {
		t.Fatalf("Claude identity body has no billing+identity prefix: %s", raw)
	}
	first, _ := system[0].(map[string]interface{})
	second, _ := system[1].(map[string]interface{})
	if !strings.Contains(wireTestString(first["text"]), "x-anthropic-billing-header: cc_version=2.1.226.503; cc_entrypoint=") ||
		!strings.HasPrefix(wireTestString(second["text"]), claudeCodeProbeIdentityLine) {
		t.Fatalf("Claude identity prefix is not native billing then SDK identity: %s", raw)
	}
	if _, ok := root["metadata"].(map[string]interface{}); !ok {
		t.Fatalf("Claude identity body omitted metadata.user_id: %s", raw)
	}
	previous := -1
	for _, key := range []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking", "context_management", "output_config", "stream"} {
		if _, present := root[key]; !present {
			continue
		}
		at := strings.Index(raw, `"`+key+`":`)
		if at <= previous {
			t.Fatalf("Claude identity root order differs at %q: %s", key, raw)
		}
		previous = at
	}
	if strings.Contains(raw, `\u003c`) || strings.Contains(raw, `\u003e`) || strings.Contains(raw, `\u0026`) {
		t.Fatalf("Claude identity body exposed Go HTML-safe escaping: %s", raw)
	}
}

func wireTestString(value interface{}) string {
	text, _ := value.(string)
	return text
}

func assertClaudeCodeProbeWireShape(t *testing.T, raw, model, promptText string, maxTokens int) {
	t.Helper()
	assertClaudeCodeIdentityWireShape(t, raw, model)
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		t.Fatalf("Claude probe body is not JSON: %v (%s)", err, raw)
	}
	if root["model"] != model || root["max_tokens"] != float64(maxTokens) || root["stream"] != false {
		t.Fatalf("Claude probe semantic shape is wrong: %s", raw)
	}
	messages, _ := root["messages"].([]interface{})
	if len(messages) != 1 {
		t.Fatalf("Claude probe messages are wrong: %s", raw)
	}
	message, _ := messages[0].(map[string]interface{})
	if message["role"] != "user" || message["content"] != promptText {
		t.Fatalf("Claude probe prompt changed: got=%#v want=%q body=%s", message, promptText, raw)
	}

	orderedKeys := []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "stream"}
	previous := -1
	for _, key := range orderedKeys {
		at := strings.Index(raw, `"`+key+`":`)
		if at < 0 || at <= previous {
			t.Fatalf("Claude probe root order does not match claude-cli: key=%q at=%d previous=%d body=%s", key, at, previous, raw)
		}
		previous = at
	}
	if !strings.Contains(raw, `"messages":[{"role":"user","content":`) {
		t.Fatalf("Claude probe message order/content differs from claude-cli: %s", raw)
	}
	for _, required := range []string{
		`x-anthropic-billing-header: cc_version=2.1.226.503; cc_entrypoint=sdk-cli;`,
		claudeCodeProbeIdentityLine,
		`"cache_control":{"type":"ephemeral"}`,
		`"tools":[]`,
		`"metadata":{"user_id":"{\"device_id\":`,
	} {
		if !strings.Contains(raw, required) {
			t.Fatalf("Claude probe omitted native identity fragment %q: %s", required, raw)
		}
	}
}

func TestClaudeCodeMinimalProbeBodyMatchesNativeIdentityAndJSONWire(t *testing.T) {
	h := newHarness(t, nil)
	account := storage.Account{ID: "claude-probe-wire", Provider: "claude"}
	body, _ := h.app.claudeCodeMinimalProbeBody(
		account,
		storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-probe"},
		storage.EgressProfile{ID: "probe-direct", Type: "direct"},
		"claude-sonnet-5",
		"Reply <probe>& OK",
		1,
	)
	assertClaudeCodeProbeWireShape(t, string(body), "claude-sonnet-5", "Reply <probe>& OK", 1)
}

func TestClaudeOAuthWireMatchesNativeClaudeCodeCapture(t *testing.T) {
	exchange, err := marshalClaudeOAuthJSON(claudeOAuthAuthorizationCodeGrant{
		GrantType: "authorization_code", Code: "code<>&", RedirectURI: config.DefaultClaudeOAuthRedirectURI,
		ClientID: config.DefaultClaudeOAuthClientID, CodeVerifier: "verifier", State: "state",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantExchange := `{"grant_type":"authorization_code","code":"code<>&","redirect_uri":"https://platform.claude.com/oauth/code/callback","client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e","code_verifier":"verifier","state":"state"}`
	if string(exchange) != wantExchange {
		t.Fatalf("authorization-code body = %s, want %s", exchange, wantExchange)
	}

	refresh, err := marshalClaudeOAuthJSON(claudeOAuthRefreshGrant{
		GrantType: "refresh_token", RefreshToken: "refresh<>&", ClientID: config.DefaultClaudeOAuthClientID,
		Scope: config.DefaultClaudeOAuthRefreshScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRefresh := `{"grant_type":"refresh_token","refresh_token":"refresh<>&","client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e","scope":"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"}`
	if string(refresh) != wantRefresh {
		t.Fatalf("refresh body = %s, want %s", refresh, wantRefresh)
	}

	headers := claudeOAuthHeaders()
	for name, want := range map[string]string{
		"Accept": claudeOAuthAccept, "Content-Type": "application/json",
		"User-Agent": claudeOAuthUserAgent, "Accept-Encoding": claudeOAuthAcceptEncoding,
		"Connection": "keep-alive",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, absent := range []string{"Anthropic-Beta", "Anthropic-Version", "X-App", "X-Stainless-Package-Version"} {
		if got := headers.Get(absent); got != "" {
			t.Errorf("%s = %q, want absent on native Axios OAuth call", absent, got)
		}
	}
}

func TestClaudeOAuthRefreshScopeMatchesNativeFirstPartyPolicy(t *testing.T) {
	got := claudeOAuthRefreshScope(
		"org:create_api_key user:plugins user:inference user:projects:read",
		config.DefaultClaudeOAuthScope,
		config.DefaultClaudeOAuthClientID,
	)
	want := config.DefaultClaudeOAuthRefreshScope + " user:plugins user:projects:read"
	if got != want {
		t.Fatalf("refresh scope = %q, want %q", got, want)
	}
	if strings.Contains(got, "org:create_api_key") {
		t.Fatal("normal Claude refresh retained login-only org:create_api_key scope")
	}
}

func TestClaudeAuthorizeURLMatchesNativeParameterOrder(t *testing.T) {
	cfg := config.Default()
	desc := oauthProviderDesc{
		provider: "claude", authURL: cfg.ClaudeOAuthAuthURL, tokenURL: cfg.ClaudeOAuthTokenURL,
		clientID: cfg.ClaudeOAuthClientID, redirectURI: cfg.ClaudeOAuthRedirectURI, scope: cfg.ClaudeOAuthScope,
	}
	got, err := desc.buildAuthorizeURLWithOptions("challenge", "state", oauthAuthorizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload&code_challenge=challenge&code_challenge_method=S256&state=state"
	if got != want {
		t.Fatalf("authorize URL = %s\nwant          = %s", got, want)
	}
}
