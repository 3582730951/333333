package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

func postProviderKeyImport(t *testing.T, h *testHarness, body map[string]interface{}) (*http.Response, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-key", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response, payload
}

func TestProviderAPIKeyImportRequiresCostConfirmationBeforeNetwork(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	response, body := postProviderKeyImport(t, h, map[string]interface{}{
		"provider_id": "codex", "api_key": "sk-upstream-secret", "confirm_cost": false,
	})
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte("cost_confirmation_required")) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if calls.Load() != 0 {
		t.Fatalf("cost confirmation failure made %d upstream request(s)", calls.Load())
	}
}

func TestBuiltInAPIKeyCannotBypassPaidImportThroughLegacyEndpoints(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	cases := []struct {
		path string
		body map[string]interface{}
	}{
		{path: "/admin/accounts/import-token", body: map[string]interface{}{"access_token": "sk-ant-api03-bypass"}},
		{path: "/admin/accounts/import-auth-json", body: map[string]interface{}{"auth_json": map[string]interface{}{"OPENAI_API_KEY": "sk-proj-bypass"}}},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(tc.body)
		response, err := http.Post(h.pool.URL+tc.path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || !bytes.Contains(payload, []byte("cost_confirmation_required")) {
			t.Fatalf("path=%s status=%d body=%s", tc.path, response.StatusCode, payload)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("legacy import bypass reached upstream %d time(s)", calls.Load())
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("legacy import bypass persisted accounts=%+v err=%v", accounts, err)
	}
}

func TestProviderAPIKeyFreeAuthFailureDoesNotSaveOrInfer(t *testing.T) {
	var inferenceCalls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			inferenceCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_api_key","code":"invalid_api_key"}}`)
	})
	response, body := postProviderKeyImport(t, h, map[string]interface{}{
		"provider_id": "codex", "api_key": "sk-invalid", "confirm_cost": true,
	})
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"auth_probe"`)) || !bytes.Contains(body, []byte(`"inference_probe"`)) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if inferenceCalls.Load() != 0 {
		t.Fatalf("authentication failure reached inference %d time(s)", inferenceCalls.Load())
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("failed authentication persisted accounts=%+v err=%v", accounts, err)
	}
}

func TestOpenAIPlatformAPIKeyImportUsesBoundMinimalProbeAndUsage(t *testing.T) {
	var modelsCalls, inferenceCalls atomic.Int64
	var h *testHarness
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-platform" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			modelsCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini","context_window":128000}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			inferenceCalls.Add(1)
			pending, err := h.store.GetAccount(context.Background(), customAccountID("codex", "sk-platform"))
			if err != nil || pending.QuarantineUntil <= storage.Now() || pending.QuarantineReason != providerAPIKeyInferenceProbePending {
				t.Errorf("account was routable during inference probe: account=%+v err=%v", pending, err)
			}
			raw, _ := io.ReadAll(r.Body)
			var request map[string]interface{}
			_ = json.Unmarshal(raw, &request)
			if request["input"] != "Reply exactly OK" || request["max_output_tokens"] != float64(8) || request["stream"] != false ||
				request["tools"] != nil || request["instructions"] != nil || request["store"] != nil || request["prompt_cache_key"] != nil {
				t.Errorf("non-minimal OpenAI inference body: %s", raw)
			}
			_, _ = io.WriteString(w, `{"id":"resp","model":"gpt-4o-mini","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}`)
		default:
			http.NotFound(w, r)
		}
	})
	response, body := postProviderKeyImport(t, h, map[string]interface{}{
		"provider_id": "codex", "api_key": "sk-platform", "label": "platform", "group_name": "cyber", "egress_id": "egress_direct", "confirm_cost": true,
	})
	if response.StatusCode != http.StatusOK || bytes.Contains(body, []byte("sk-platform")) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var result providerAPIKeyImportResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Ready || !result.AuthProbe.Alive || !result.InferenceProbe.Alive || result.InferenceProbe.Model != "gpt-4o-mini" || string(result.InferenceProbe.Usage) == "" {
		t.Fatalf("probe result=%+v body=%s", result, body)
	}
	if result.AuthMethod != accountprovider.AuthMethodAPIKey || result.BillingMode != accountprovider.BillingModePayAsYouGo || !result.APIKeyPresent || result.Quarantined {
		t.Fatalf("safe account metadata=%+v", result)
	}
	if modelsCalls.Load() != 1 || inferenceCalls.Load() != 1 {
		t.Fatalf("models=%d inference=%d", modelsCalls.Load(), inferenceCalls.Load())
	}
	token, err := h.store.GetToken(context.Background(), result.ID)
	if err != nil || token.AuthMethod != accountprovider.AuthMethodAPIKey || token.AccessToken != "sk-platform" {
		t.Fatalf("stored token method=%q err=%v", token.AuthMethod, err)
	}
	usageRows, err := h.store.UsageSummaryByAccountIDs(context.Background(), []string{result.ID})
	if err != nil || usageRows[result.ID].TotalTokens != 5 {
		t.Fatalf("usage=%+v err=%v", usageRows, err)
	}
	accountsResponse, err := http.Get(h.pool.URL + "/admin/accounts")
	if err != nil {
		t.Fatal(err)
	}
	accountsBody, _ := io.ReadAll(accountsResponse.Body)
	accountsResponse.Body.Close()
	if bytes.Contains(accountsBody, []byte("sk-platform")) || !bytes.Contains(accountsBody, []byte(`"auth_method":"api_key"`)) || !bytes.Contains(accountsBody, []byte(`"billing_mode":"pay_as_you_go"`)) || !bytes.Contains(accountsBody, []byte(`"api_key_present":true`)) {
		t.Fatalf("account management response was unsafe or incomplete: %s", accountsBody)
	}
}

func TestAnthropicAPIKeyInferenceFailureQuarantinesUntilConfirmedRecovery(t *testing.T) {
	var failInference atomic.Bool
	failInference.Store(true)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-platform" || r.Header.Get("Authorization") != "" {
			t.Errorf("anthropic auth headers x-api-key=%q authorization=%q", r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Anthropic-Beta")), "oauth") {
			t.Errorf("API key request carried OAuth beta: %q", r.Header.Get("Anthropic-Beta"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-3-5-haiku-latest","max_input_tokens":200000}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			raw, _ := io.ReadAll(r.Body)
			assertClaudeCodeProbeWireShape(t, string(raw), "claude-3-5-haiku-latest", "Reply exactly OK", 8)
			if bytes.Contains(raw, []byte(`"thinking"`)) || bytes.Contains(raw, []byte(`"context_management"`)) {
				t.Errorf("non-minimal Anthropic inference body: %s", raw)
			}
			if failInference.Load() {
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"billing_error"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"msg","model":"claude-3-5-haiku-latest","usage":{"input_tokens":4,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	})
	response, body := postProviderKeyImport(t, h, map[string]interface{}{
		"provider_id": "claude", "api_key": "sk-ant-platform", "label": "console", "group_name": "cyber", "confirm_cost": true,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var imported providerAPIKeyImportResponse
	if err := json.Unmarshal(body, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Ready || !imported.Quarantined || !strings.HasPrefix(imported.QuarantineReason, "provider_api_key_inference_probe_failed:") {
		t.Fatalf("failed inference was not preserved in quarantine: %+v", imported)
	}
	token, err := h.store.GetToken(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freeProbe := h.app.probeAccountLiveness(context.Background(), imported.Account, token); !freeProbe.Alive || freeProbe.ModelChecked {
		t.Fatalf("background API-key probe was not free auth-only: %+v", freeProbe)
	}
	stillQuarantined, err := h.store.GetAccount(context.Background(), imported.ID)
	if err != nil || stillQuarantined.QuarantineUntil <= storage.Now() {
		t.Fatalf("free background probe recovered inference quarantine: %+v err=%v", stillQuarantined, err)
	}

	withoutConfirmation, raw := postAccountActionJSON(t, h, imported.ID, "health-test", map[string]interface{}{})
	if withoutConfirmation != http.StatusBadRequest || !bytes.Contains(raw, []byte("cost_confirmation_required")) {
		t.Fatalf("unconfirmed health test status=%d body=%s", withoutConfirmation, raw)
	}
	failInference.Store(false)
	status, raw := postAccountActionJSON(t, h, imported.ID, "health-test", map[string]interface{}{"confirm_cost": true})
	if status != http.StatusOK {
		t.Fatalf("confirmed health test status=%d body=%s", status, raw)
	}
	account, err := h.store.GetAccount(context.Background(), imported.ID)
	if err != nil || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("successful manual inference did not recover account=%+v err=%v", account, err)
	}
}

func postAccountActionJSON(t *testing.T, h *testHarness, accountID, action string, body map[string]interface{}) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/admin/accounts/"+accountID+"/"+action, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, payload
}
