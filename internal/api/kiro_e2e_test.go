package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func kiroEventFrame(headers map[string]string, payload []byte) []byte {
	var encoded bytes.Buffer
	for name, value := range headers {
		encoded.WriteByte(byte(len(name)))
		encoded.WriteString(name)
		encoded.WriteByte(7)
		_ = binary.Write(&encoded, binary.BigEndian, uint16(len(value)))
		encoded.WriteString(value)
	}
	total := 16 + encoded.Len() + len(payload)
	raw := make([]byte, total)
	binary.BigEndian.PutUint32(raw, uint32(total))
	binary.BigEndian.PutUint32(raw[4:], uint32(encoded.Len()))
	binary.BigEndian.PutUint32(raw[8:], crc32.ChecksumIEEE(raw[:8]))
	copy(raw[12:], encoded.Bytes())
	copy(raw[12+encoded.Len():], payload)
	binary.BigEndian.PutUint32(raw[total-4:], crc32.ChecksumIEEE(raw[:total-4]))
	return raw
}

func allowKiroTestEndpoint(t *testing.T, h *testHarness, endpoint string) {
	t.Helper()
	if err := h.store.SetSetting(context.Background(), "kiro_endpoint_allowlist", endpoint); err != nil {
		t.Fatal(err)
	}
}

func TestKiroImportAndMessagesEndToEnd(t *testing.T) {
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refreshToken":
			_, _ = w.Write([]byte(`{"accessToken":"kiro-access","refreshToken":"kiro-refresh-2","expiresIn":3600}`))
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimitWithPrecision":100,"currentUsageWithPrecision":1}]}`))
		case "/generateAssistantResponse":
			if r.Header.Get("Authorization") != "Bearer kiro-access" {
				t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			}
			requestBody, _ := io.ReadAll(r.Body)
			if !bytes.Contains(requestBody, []byte(`"thinking":{"type":"adaptive"}`)) || !bytes.Contains(requestBody, []byte(`"output_config":{"effort":"max"}`)) || !bytes.Contains(requestBody, []byte(`"max_tokens":64000`)) {
				t.Errorf("mandatory Kiro max-quality fields missing from request: %s", requestBody)
			}
			if bytes.Contains(requestBody, []byte("<thinking_mode>")) || bytes.Contains(requestBody, []byte("<system>")) {
				t.Errorf("legacy compatibility prompt leaked into request content: %s", requestBody)
			}
			if !bytes.Contains(requestBody, []byte(`"cachePoint":{"type":"default"}`)) {
				t.Errorf("auto mode did not send a planned Kiro cachePoint: %s", requestBody)
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"hello from kiro"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":7,"cacheReadInputTokens":5,"cacheWriteInputTokens":2,"outputTokens":3,"totalTokens":17}}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0.75}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]interface{}{"accounts": []interface{}{
		map[string]interface{}{"authMethod": "social", "refreshToken": "kiro-refresh", "endpoint": kiroMock.URL},
		map[string]interface{}{"authMethod": "idc", "refreshToken": "invalid-without-client"},
	}})
	payload, _ := json.Marshal(map[string]interface{}{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	resp, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	importBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(importBody, []byte(`"imported":1`)) || !bytes.Contains(importBody, []byte(`"failed":1`)) {
		t.Fatalf("import status=%d body=%s", resp.StatusCode, importBody)
	}
	accountsResp, err := http.Get(h.pool.URL + "/admin/accounts")
	if err != nil {
		t.Fatal(err)
	}
	accountsBody, _ := io.ReadAll(accountsResp.Body)
	accountsResp.Body.Close()
	if bytes.Contains(accountsBody, []byte("kiro-access")) || bytes.Contains(accountsBody, []byte("kiro-refresh")) {
		t.Fatalf("admin account response leaked Kiro credentials: %s", accountsBody)
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("Kiro accounts after import = %+v, err=%v", accounts, err)
	}
	if caps, err := h.app.probeAccountModels(context.Background(), accounts[0]); err != nil || len(caps) == 0 {
		t.Fatalf("built-in Kiro model probe used custom-provider path: caps=%+v err=%v", caps, err)
	}
	token, _ := h.store.GetToken(context.Background(), accounts[0].ID)
	if probe := h.app.probeAccountLiveness(context.Background(), accounts[0], token); probe.Err != nil || !probe.Alive {
		t.Fatalf("built-in Kiro health probe failed: %+v", probe)
	}

	requestBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pool-Provider", "kiro")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello from kiro")) || !bytes.Contains(body, []byte(`"input_tokens":7`)) || !bytes.Contains(body, []byte(`"cache_read_input_tokens":5`)) || !bytes.Contains(body, []byte(`"cache_creation_input_tokens":2`)) {
		t.Fatalf("messages status=%d body=%s", resp.StatusCode, body)
	}
	h.app.FlushWrites()
	var promptTokens, completionTokens, totalTokens, cacheRead, cacheCreation, cacheTotal int64
	var usageSource, rawUsage string
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_creation_tokens, cache_total_input_tokens, usage_source, raw_usage_json FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(
		&promptTokens, &completionTokens, &totalTokens, &cacheRead, &cacheCreation, &cacheTotal, &usageSource, &rawUsage); err != nil {
		t.Fatal(err)
	}
	if promptTokens != 7 || completionTokens != 3 || totalTokens != 17 || cacheRead != 5 || cacheCreation != 2 || cacheTotal != 14 || usageSource != "upstream" || !strings.Contains(rawUsage, `"kiro_credits":0.75`) {
		t.Fatalf("persisted Kiro metadata usage = prompt=%d output=%d total=%d read=%d write=%d cache_total=%d source=%s raw=%s", promptTokens, completionTokens, totalTokens, cacheRead, cacheCreation, cacheTotal, usageSource, rawUsage)
	}

	resp, err = http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	duplicateBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(duplicateBody, []byte(`"duplicate":1`)) {
		t.Fatalf("duplicate import body=%s", duplicateBody)
	}
}

func TestKiroAutoFirstConcreteRequestBootstrapsAndVerifiesCapability(t *testing.T) {
	var generateCalls int
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimitWithPrecision":100,"currentUsageWithPrecision":1}]}`))
		case "/generateAssistantResponse":
			generateCalls++
			body, _ := io.ReadAll(r.Body)
			healthProbe := bytes.Contains(body, []byte("Reply exactly OK"))
			model := "claude-opus-4.8"
			maxOutput := `"max_tokens":128000`
			content := "bootstrap ok"
			if healthProbe {
				model = "claude-sonnet-4.6"
				maxOutput = `"max_tokens":64000`
				content = "OK"
			}
			if !bytes.Contains(body, []byte(`"modelId":"`+model+`"`)) {
				t.Errorf("request did not use expected canonical Kiro model %s: %s", model, body)
			}
			for _, required := range []string{`"thinking":{"type":"adaptive"}`, `"output_config":{"effort":"max"}`, maxOutput} {
				if !bytes.Contains(body, []byte(required)) {
					t.Errorf("mandatory Kiro max-quality field %s missing: %s", required, body)
				}
			}
			if bytes.Contains(body, []byte(`"type":"disabled"`)) {
				t.Errorf("downstream disabled mandatory Kiro thinking: %s", body)
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			responsePayload, _ := json.Marshal(map[string]any{"modelId": model, "content": content})
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, responsePayload))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":4,"outputTokens":2}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "bootstrap-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("import status=%d", response.StatusCode)
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	account := accounts[0]
	// Reproduce a legacy transient generation auth failure that left a valid API
	// key excluded from scheduling even though UsageLimits still authenticates.
	if err := h.store.SetAccountStatus(context.Background(), account.ID, "invalid"); err != nil {
		t.Fatal(err)
	}
	endpointHash, err := kirowire.EndpointHash(kiroMock.URL, "us-east-1", []string{kiroMock.URL})
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.store.GetKiroRuntimeCapability(context.Background(), account.ID, endpointHash, "claude-opus-4.8")
	if err != nil || state.ModelState != "unknown" {
		t.Fatalf("runtime state before first request=%+v err=%v", state, err)
	}

	health, err := http.Post(h.pool.URL+"/admin/accounts/"+account.ID+"/health-test", "application/json", strings.NewReader(`{"confirm_cost":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var healthBody map[string]any
	if err := json.NewDecoder(health.Body).Decode(&healthBody); err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if healthBody["probe_scope"] != kiroHealthProbeScope || healthBody["model_checked"] != true || healthBody["model"] != "claude-sonnet-4.6" || healthBody["ready"] != true {
		t.Fatalf("Kiro health probe did not report both layers: %#v", healthBody)
	}
	recovered, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil || recovered.Status != "active" {
		t.Fatalf("successful Kiro health test did not recover stale invalid status: account=%+v err=%v", recovered, err)
	}
	auditRows, err := h.store.ListAuditLog(context.Background(), 10)
	if err != nil || len(auditRows) == 0 {
		t.Fatalf("health audit=%+v err=%v", auditRows, err)
	}
	var healthAudit *storage.AuditLogRow
	for i := range auditRows {
		if auditRows[i].Action == "health_test" {
			healthAudit = &auditRows[i]
			break
		}
	}
	if healthAudit == nil || !strings.Contains(healthAudit.Detail, "probe_scope="+kiroHealthProbeScope) || !strings.Contains(healthAudit.Detail, "model_checked=false") {
		t.Fatalf("Kiro health auth audit did not identify the two-layer scope: %+v", auditRows)
	}
	state, err = h.store.GetKiroRuntimeCapability(context.Background(), account.ID, endpointHash, "claude-opus-4.8")
	if err != nil || state.ModelState != "unknown" {
		t.Fatalf("health probe unexpectedly verified model: %+v err=%v", state, err)
	}

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","max_tokens":32,"thinking":{"type":"disabled"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("bootstrap ok")) {
		t.Fatalf("auto bootstrap status=%d body=%s", response.StatusCode, body)
	}
	if response.Header.Get("X-Pool-Resolved-Provider") != "kiro" || response.Header.Get("X-Pool-Resolved-Model") != "claude-opus-4.8" {
		t.Fatalf("resolved headers=%v", response.Header)
	}
	if response.Header.Get("X-Pool-Kiro-Thinking") != "adaptive" || response.Header.Get("X-Pool-Kiro-Effort") != "max" || response.Header.Get("X-Pool-Kiro-Max-Output-Tokens") != "128000" {
		t.Fatalf("mandatory Kiro quality headers=%v", response.Header)
	}
	compatibility := response.Header.Get("X-Pool-Kiro-Compatibility")
	if !strings.Contains(compatibility, kirowire.LossThinkingForcedAdaptive) || !strings.Contains(compatibility, kirowire.LossThinkingEffortForcedMax) {
		t.Fatalf("quality overrides not reported: %q", compatibility)
	}
	if generateCalls != 2 {
		t.Fatalf("generate calls=%d", generateCalls)
	}
	state, err = h.store.GetKiroRuntimeCapability(context.Background(), account.ID, endpointHash, "claude-opus-4.8")
	if err != nil || state.ModelState != "verified" || state.ThinkingState != "verified" {
		t.Fatalf("runtime state after first request=%+v err=%v", state, err)
	}
}

func TestKiroTwoJSONIdCImportEndToEnd(t *testing.T) {
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			if !bytes.Contains(raw, []byte(`"clientId":"client-placeholder"`)) || !bytes.Contains(raw, []byte(`"clientSecret":"secret-placeholder"`)) {
				t.Errorf("merged IdC credentials missing from refresh request: %s", raw)
			}
			_, _ = w.Write([]byte(`{"idToken":"idc-access","refreshToken":"idc-refresh-rotated","expiresIn":3600}`))
		case "/getUsageLimits":
			if r.Header.Get("Authorization") != "Bearer idc-access" {
				t.Errorf("IdC authorization=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO ENTERPRISE","usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimitWithPrecision":100,"currentUsageWithPrecision":1}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	tokenJSON, _ := json.Marshal(map[string]interface{}{
		"authMethod": "IdC", "provider": "Enterprise", "refreshToken": "idc-refresh", "clientIdHash": "hash-placeholder", "region": "us-east-1", "endpoint": kiroMock.URL,
	})
	clientJSON := `{"clientId":"client-placeholder","clientSecret":"secret-placeholder","expiresAt":"2099-01-01T00:00:00Z"}`
	payload, _ := json.Marshal(map[string]interface{}{
		"kiro_json_text": string(tokenJSON), "kiro_client_json_text": clientJSON, "group_name": "cyber", "egress_id": "egress_direct",
	})
	resp, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"imported":1`)) || bytes.Contains(body, []byte("secret-placeholder")) {
		t.Fatalf("two-JSON IdC import status=%d body=%s", resp.StatusCode, body)
	}
	accounts, _ := h.store.ListAccounts(context.Background())
	if len(accounts) != 1 || accounts[0].Provider != "kiro" {
		t.Fatalf("imported IdC accounts=%+v", accounts)
	}
	credentials, err := h.store.GetKiroCredentials(context.Background(), accounts[0].ID)
	if err != nil || credentials.ClientID != "client-placeholder" || credentials.ClientSecret != "secret-placeholder" {
		t.Fatalf("stored merged credentials=%+v err=%v", credentials, err)
	}
}

func TestKiroGenericUnauthorizedIsNotReplayedOrInvalidated(t *testing.T) {
	var generateCalls int
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"usageLimitWithPrecision":100}]}`))
		case "/generateAssistantResponse":
			generateCalls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid credential"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]interface{}{"authMethod": "api_key", "kiroApiKey": "invalid-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]interface{}{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	resp, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pool-Provider", "kiro")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || generateCalls != 1 ||
		!bytes.Contains(body, []byte("invalid credential")) {
		t.Fatalf("status=%d generate_calls=%d body=%s", resp.StatusCode, generateCalls, body)
	}
	accounts, _ := h.store.ListAccounts(context.Background())
	if len(accounts) != 1 || accounts[0].Status != "active" {
		t.Fatalf("accounts=%+v", accounts)
	}
}

func TestKiroForbiddenIsNotReplayedOrInvalidated(t *testing.T) {
	var generateCalls int
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			generateCalls++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"temporarily unavailable"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, kiroMock.URL, "transient-forbidden-key")
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || generateCalls != 1 ||
		!bytes.Contains(body, []byte("temporarily unavailable")) {
		t.Fatalf("forbidden response was replayed or lost: status=%d calls=%d body=%s", response.StatusCode, generateCalls, body)
	}
	stored, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil || stored.Status != "active" {
		t.Fatalf("forbidden response invalidated API key: account=%+v err=%v", stored, err)
	}
}

func TestKiroAccessTokenExpiredRequiresExplicitExpiryMarker(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"code": "expired_token"}`),
		[]byte(`{"error":"token_expired"}`),
		[]byte(`{"message":"Access token has expired"}`),
	} {
		if !kiroAccessTokenExpired(body) {
			t.Fatalf("explicit expiry marker was not detected: %s", body)
		}
	}
	for _, body := range [][]byte{
		[]byte(`{"message":"invalid credential"}`),
		[]byte(`{"code":"invalid_token"}`),
		[]byte(`{"message":"not authorized"}`),
	} {
		if kiroAccessTokenExpired(body) {
			t.Fatalf("generic auth failure was treated as expiry: %s", body)
		}
	}
}

func TestKiroRateLimitForwardsRetryAfterWithoutReplaying(t *testing.T) {
	var generateCalls int
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			generateCalls++
			w.Header().Set("Retry-After", "37")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"quota temporarily unavailable"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, kiroMock.URL, "rate-limited-key")
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "37" ||
		generateCalls != 1 || !bytes.Contains(body, []byte("quota temporarily unavailable")) {
		t.Fatalf("status=%d retry_after=%q calls=%d body=%s", response.StatusCode, response.Header.Get("Retry-After"), generateCalls, body)
	}
	stored, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil || stored.Status != "active" {
		t.Fatalf("rate limit invalidated account: account=%+v err=%v", stored, err)
	}
}

func TestKiroStreamingForwardsFirstSemanticFrameBeforeUpstreamCompletes(t *testing.T) {
	firstWritten := make(chan struct{})
	releaseSecond := make(chan struct{})
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"first"}`)))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(firstWritten)
			<-releaseSecond
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":" second"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":7,"outputTokens":2,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"totalTokens":9}}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0.25}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "kiro-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "stream-session")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-firstWritten:
	case <-time.After(time.Second):
		t.Fatal("upstream did not write first frame")
	}
	if response.Header.Get("X-Pool-Resolved-Provider") != "kiro" || response.Header.Get("X-Pool-Resolved-Model") != "claude-sonnet-4.6" {
		t.Fatalf("resolved headers=%v", response.Header)
	}
	reader := bufio.NewReader(response.Body)
	firstDelta := make(chan string, 1)
	go func() {
		var seen strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			seen.WriteString(line)
			if strings.Contains(seen.String(), "first") || readErr != nil {
				firstDelta <- seen.String()
				return
			}
		}
	}()
	select {
	case got := <-firstDelta:
		if !strings.Contains(got, "first") {
			t.Fatalf("first downstream data=%q", got)
		}
		if strings.Contains(got, `"input_tokens":0`) {
			t.Fatalf("stream waited for metadata by emitting a fake zero instead of a provisional estimate: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Kiro stream buffered until completion")
	}
	close(releaseSecond)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rest, []byte(" second")) || !bytes.Contains(rest, []byte(`"input_tokens":7`)) || !bytes.Contains(rest, []byte(`"cache_read_input_tokens":0`)) || !bytes.Contains(rest, []byte(`"cache_creation_input_tokens":0`)) {
		t.Fatalf("terminal stream=%s", rest)
	}
}

func TestKiroStreamingReassemblesIDlessObjectToolFrames(t *testing.T) {
	const toolID = "toolu_bdrk_01EEWdCWVs59fnqL8QSH9cxw"
	upstream := bytes.Join([][]byte{
		kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"name":"Fetch","toolUseId":"`+toolID+`"}`)),
		kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"input":{"url":"https://www.elsevier.support/example","prompt":"Extract the answer"}}`)),
		kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"stop":true}`)),
	}, nil)

	var downstream bytes.Buffer
	emitter := newKiroAnthropicEmitter(&downstream, nil, "claude-opus-4.8", "msg_tool")
	data, err := streamKiroResponse(bytes.NewReader(upstream), nil, emitter)
	if err != nil {
		t.Fatal(err)
	}
	emitter.finish(data)
	body := downstream.String()
	if len(data.Tools) != 1 || data.Tools[0].ID != toolID {
		t.Fatalf("tools=%+v", data.Tools)
	}
	if strings.Count(body, `"type":"tool_use"`) != 1 || !strings.Contains(body, `"id":"`+toolID+`"`) {
		t.Fatalf("tool block was split or lost:\n%s", body)
	}
	if !strings.Contains(body, `"partial_json":"{\"url\":\"https://www.elsevier.support/example\",\"prompt\":\"Extract the answer\"}"`) {
		t.Fatalf("complete object input was not forwarded:\n%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected downstream error:\n%s", body)
	}
}

func TestKiroAutoSessionNeverSwitchesBoundAccount(t *testing.T) {
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"ok"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":2,"outputTokens":1}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	for _, key := range []string{"kiro-key-one", "kiro-key-two"} {
		credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": key, "endpoint": kiroMock.URL})
		payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
		response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	endpointHash, err := kirowire.EndpointHash(kiroMock.URL, "us-east-1", []string{kiroMock.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if _, err := h.store.ObserveKiroCapability(context.Background(), account.ID, endpointHash, "claude-sonnet-4.6", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
			t.Fatal(err)
		}
	}
	send := func() (*http.Response, []byte) {
		request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"turn"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Claude-Code-Session-Id", "immutable-auto-session")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response, body
	}
	first, body := send()
	if first.StatusCode != http.StatusOK || first.Header.Get("X-Pool-Resolved-Provider") != "kiro" {
		t.Fatalf("first status=%d headers=%v body=%s", first.StatusCode, first.Header, body)
	}
	affinity := routing.ExtractClaudeTrueAffinityKey(&http.Request{Header: http.Header{"X-Claude-Code-Session-Id": {"immutable-auto-session"}}}, nil)
	bound, err := h.store.GetAffinityBinding(context.Background(), affinity.Hash)
	if err != nil || bound.Provider != "kiro" || bound.Model != "claude-sonnet-4.6" || bound.EgressID == "" {
		t.Fatalf("binding=%+v err=%v", bound, err)
	}
	if err := h.store.SetAccountStatus(context.Background(), bound.AccountID, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	second, body := send()
	if second.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"code":"bound_account_unavailable"`)) {
		t.Fatalf("bound session switched or wrong error: status=%d body=%s", second.StatusCode, body)
	}
}

func TestKiroCacheProbeRequiresCostConfirmationAndUsesStableRequest(t *testing.T) {
	var generateBodies [][]byte
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			generateBodies = append(generateBodies, append([]byte(nil), body...))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"OK"}`)))
			metadata := `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":1,"cacheReadInputTokens":0,"cacheWriteInputTokens":5,"totalTokens":16}}`
			if len(generateBodies)%2 == 0 {
				metadata = `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":1,"cacheReadInputTokens":5,"cacheWriteInputTokens":0,"totalTokens":16}}`
			}
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(metadata)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0.5}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "probe-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	accounts, _ := h.store.ListAccounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("accounts=%+v", accounts)
	}
	probeURL := h.pool.URL + "/admin/accounts/" + accounts[0].ID + "/kiro/cache-probe"
	response, err = http.Post(probeURL, "application/json", strings.NewReader(`{"model":"claude-sonnet-4-6","confirm_cost":false}`))
	if err != nil {
		t.Fatal(err)
	}
	denied, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(generateBodies) != 0 || !bytes.Contains(denied, []byte("cost_confirmation_required")) {
		t.Fatalf("unconfirmed probe status=%d calls=%d body=%s", response.StatusCode, len(generateBodies), denied)
	}
	response, err = http.Post(probeURL, "application/json", strings.NewReader(`{"model":"claude-sonnet-4-6","confirm_cost":true}`))
	if err != nil {
		t.Fatal(err)
	}
	probeBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(generateBodies) != 2 || !bytes.Equal(generateBodies[0], generateBodies[1]) {
		t.Fatalf("probe status=%d calls=%d stable=%v body=%s", response.StatusCode, len(generateBodies), len(generateBodies) == 2 && bytes.Equal(generateBodies[0], generateBodies[1]), probeBody)
	}
	if len(generateBodies[0]) < 32*1024 {
		t.Fatalf("probe prefix too short to exercise realistic cache eligibility: %d bytes", len(generateBodies[0]))
	}
	if !bytes.Contains(generateBodies[0], []byte(`"cachePoint":{"type":"default"}`)) {
		t.Fatalf("probe did not send a real cachePoint: %s", generateBodies[0])
	}
	for _, required := range []string{`"thinking":{"type":"adaptive"}`, `"output_config":{"effort":"max"}`, `"max_tokens":64000`} {
		if !bytes.Contains(generateBodies[0], []byte(required)) {
			t.Fatalf("cache probe disabled mandatory Kiro quality field %s: %s", required, generateBodies[0])
		}
	}
	if !bytes.Contains(probeBody, []byte(`"cache_capability":"hit_observed"`)) || !bytes.Contains(probeBody, []byte(`"cache_read_tokens":{"value":5,"present":true}`)) || !bytes.Contains(probeBody, []byte(`"cache_verified":true`)) || !bytes.Contains(probeBody, []byte(`"cache_reuse_observed":true`)) || !bytes.Contains(probeBody, []byte(`"cache_reuse_state":"verified"`)) || !bytes.Contains(probeBody, []byte(`"cache_reuse_evidence":"token_metadata"`)) || !bytes.Contains(probeBody, []byte(`"cache_evidence":"token_metadata"`)) || !bytes.Contains(probeBody, []byte(`"credits":{"value":0.5,"present":true}`)) {
		t.Fatalf("probe did not report real metering: %s", probeBody)
	}
	capabilityStates, err := h.store.ListKiroRuntimeCapabilities(context.Background(), accounts[0].ID)
	var persistedReuse *storage.KiroRuntimeCapability
	for i := range capabilityStates {
		if capabilityStates[i].Model == "claude-sonnet-4.6" {
			persistedReuse = &capabilityStates[i]
			break
		}
	}
	if err != nil || persistedReuse == nil || persistedReuse.CacheReuseState != "verified" || persistedReuse.CacheReuseEvidence != "token_metadata" || persistedReuse.CacheReuseProbedAt == 0 {
		t.Fatalf("cache probe evidence was not persisted: states=%+v err=%v", capabilityStates, err)
	}
	if !bytes.Contains(probeBody, []byte(`"thinking":"adaptive"`)) || !bytes.Contains(probeBody, []byte(`"effort":"max"`)) || !bytes.Contains(probeBody, []byte(`"max_output_tokens":64000`)) {
		t.Fatalf("cache probe response omitted mandatory quality controls: %s", probeBody)
	}
	response, err = http.Post(probeURL, "application/json", strings.NewReader(`{"model":"claude-sonnet-4-6","confirm_cost":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(generateBodies) != 4 || !bytes.Equal(generateBodies[2], generateBodies[3]) || bytes.Equal(generateBodies[0], generateBodies[2]) {
		t.Fatalf("repeated probe did not use a fresh stable pair: status=%d calls=%d", response.StatusCode, len(generateBodies))
	}
}

func TestKiroCacheProbeCreditsOnlyEvidenceRequiresMaterialReduction(t *testing.T) {
	attempt := func(value float64, present bool) kiroCacheProbeAttempt {
		return kiroCacheProbeAttempt{Credits: kirowire.MeteredFloat{Value: value, Present: present}}
	}
	percent, observed := kiroCacheProbeCreditEvidence([]kiroCacheProbeAttempt{attempt(0.20, true), attempt(0.10, true)})
	if !observed || percent != 50 {
		t.Fatalf("material credit reduction percent=%v observed=%v", percent, observed)
	}
	if percent, observed = kiroCacheProbeCreditEvidence([]kiroCacheProbeAttempt{attempt(0.20, true), attempt(0.19, true)}); observed || percent != 5 {
		t.Fatalf("noise-level credit reduction percent=%v observed=%v", percent, observed)
	}
	if _, observed = kiroCacheProbeCreditEvidence([]kiroCacheProbeAttempt{attempt(0.20, true), attempt(0, false)}); observed {
		t.Fatal("missing credits became cache evidence")
	}
}

func TestKiroCachePointRejectionFallsBackOnceAndPersistsUnsupported(t *testing.T) {
	var bodies [][]byte
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, append([]byte(nil), body...))
			if bytes.Contains(body, []byte(`"cachePoint"`)) {
				http.Error(w, `{"message":"cachePoint is not supported"}`, http.StatusUnprocessableEntity)
				return
			}
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"ok"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":4,"outputTokens":1,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"totalTokens":5}}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "fallback-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	accounts, _ := h.store.ListAccounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("accounts=%+v", accounts)
	}
	send := func() []byte {
		request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","system":"`+strings.Repeat("stable ", 800)+`","messages":[{"role":"user","content":"hello"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Pool-Provider", "kiro")
		request.Header.Set("X-Claude-Code-Session-Id", "cachepoint-fallback")
		resp, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		return body
	}
	first := send()
	if len(bodies) != 2 || !bytes.Contains(bodies[0], []byte(`"cachePoint"`)) || bytes.Contains(bodies[1], []byte(`"cachePoint"`)) {
		t.Fatalf("fallback requests=%d bodies=%q", len(bodies), bodies)
	}
	if !bytes.Contains(first, []byte(`"input_tokens":4`)) {
		t.Fatalf("fallback response lost metadata usage: %s", first)
	}
	endpointHash, _ := kirowire.EndpointHash(kiroMock.URL, "us-east-1", []string{kiroMock.URL})
	capabilityState, err := h.store.GetKiroRuntimeCapability(context.Background(), accounts[0].ID, endpointHash, "claude-sonnet-4.6")
	if err != nil || capabilityState.CachePointState != "unsupported" {
		t.Fatalf("cachePoint capability=%+v err=%v", capabilityState, err)
	}
	_ = send()
	if len(bodies) != 3 || bytes.Contains(bodies[2], []byte(`"cachePoint"`)) {
		t.Fatalf("unsupported cachePoint was retried on next request: %q", bodies)
	}
}

func TestKiroContentLengthErrorPreservesOutputAndRequestsClientCompaction(t *testing.T) {
	var bodies [][]byte
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, append([]byte(nil), body...))
			if bytes.Contains(body, []byte(`"max_tokens":64000`)) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))
				return
			}
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"ok"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":20,"outputTokens":1,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"totalTokens":21}}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, kiroMock.URL, "context-retry-key")
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"preserve this request"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "context-output-retry")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(bodies) != 1 ||
		!bytes.Contains(responseBody, []byte(`"code":"context_length_exceeded"`)) {
		t.Fatalf("status=%d requests=%d body=%s", response.StatusCode, len(bodies), responseBody)
	}
	if !bytes.Contains(bodies[0], []byte(`"max_tokens":64000`)) ||
		!bytes.Contains(bodies[0], []byte("preserve this request")) ||
		response.Header.Get("X-MiCliProxy-Context-Status") != "compact_required" ||
		response.Header.Get("X-MiCliProxy-Auto-Compact") != "client_retry" {
		t.Fatalf("context signal changed the request or lost diagnostics: headers=%v body=%s", response.Header, bodies[0])
	}
	endpointHash, _ := kirowire.EndpointHash(kiroMock.URL, "us-east-1", []string{kiroMock.URL})
	state, stateErr := h.store.GetKiroRuntimeCapability(context.Background(), account.ID, endpointHash, "claude-sonnet-4.6")
	if stateErr != nil || state.CachePointState == "unsupported" {
		t.Fatalf("content error poisoned cachePoint state: state=%+v err=%v", state, stateErr)
	}
}

func TestKiroContentLengthErrorNeverDropsHistoryOrReplays(t *testing.T) {
	var bodies [][]byte
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, append([]byte(nil), body...))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	_ = importKiroEndpointForTest(t, h, kiroMock.URL, "history-retry-key")
	payload, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("old-context-", 4_000)},
			map[string]any{"role": "assistant", "content": "old-answer"},
			map[string]any{"role": "user", "content": "recent-context"},
			map[string]any{"role": "assistant", "content": "recent-answer"},
			map[string]any{"role": "user", "content": "current-context"},
		},
	})
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	request.Header.Set("X-Claude-Code-Session-Id", "context-history-retry")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(bodies) != 1 ||
		!bytes.Contains(responseBody, []byte(`"code":"context_length_exceeded"`)) {
		t.Fatalf("status=%d requests=%d body=%s", response.StatusCode, len(bodies), responseBody)
	}
	for _, required := range [][]byte{
		[]byte("old-context-"),
		[]byte("old-answer"),
		[]byte("recent-context"),
		[]byte("recent-answer"),
		[]byte("current-context"),
	} {
		if !bytes.Contains(bodies[0], required) {
			t.Fatalf("original request lost %q: %s", required, bodies[0])
		}
	}
	if response.Header.Get("X-Pool-Kiro-History-Messages-Dropped") != "" ||
		response.Header.Get("X-MiCliProxy-Context-Status") != "compact_required" {
		t.Fatalf("history was silently trimmed or compaction signal missing: %v", response.Header)
	}
}

func TestKiroCatalogOpusClaudeCodeCompactionUsesExtendedWindow(t *testing.T) {
	var compactRequests int
	var fullHistoryForwarded bool
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("Your task is to create a detailed summary of the conversation so far")) {
				compactRequests++
				fullHistoryForwarded = bytes.Contains(body, []byte("historic-context-")) && bytes.Contains(body, []byte("tail-marker-before-compaction"))
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"compact summary"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":278032,"outputTokens":120,"totalTokens":278152}}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, kiroMock.URL, "compact-window-key")
	if !strings.Contains(strings.ToUpper(account.PlanType), "PRO") {
		t.Fatalf("imported Kiro plan=%q, want paid plan evidence", account.PlanType)
	}
	endpointHash, err := kirowire.EndpointHash(kiroMock.URL, "us-east-1", []string{kiroMock.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ObserveKiroCapability(context.Background(), account.ID, endpointHash, "claude-opus-4.8", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	history := strings.Repeat("historic-context-", 65_000) + "tail-marker-before-compaction"
	request := func(model, historyContent, system, finalInstruction, session string) (*http.Response, []byte) {
		payload, _ := json.Marshal(map[string]any{
			"model":  model,
			"system": system,
			"messages": []any{
				map[string]any{"role": "user", "content": historyContent},
				map[string]any{"role": "assistant", "content": "previous answer"},
				map[string]any{"role": "user", "content": finalInstruction},
			},
		})
		req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Pool-Provider", "kiro")
		req.Header.Set("X-Claude-Code-Session-Id", session)
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	// Runtime success and a paid-looking plan string are not entitlement evidence.
	// Until the complete account/region catalog says 1M, an oversized request fails
	// locally without contacting the upstream.
	ordinary, ordinaryBody := request("claude-opus-4-8", history, "You are Claude Code", "continue ordinary work", "ordinary-over-200k")
	if ordinary.StatusCode != http.StatusBadRequest || !bytes.Contains(ordinaryBody, []byte(`"code":"context_length_exceeded"`)) {
		t.Fatalf("request without live 1M catalog was accepted: status=%d headers=%v body=%s", ordinary.StatusCode, ordinary.Header, ordinaryBody)
	}

	capabilityKey, governanceKey := kirowire.KiroCapabilityKey(endpointHash, "us-east-1", "")
	now := storage.Now()
	if err := h.store.ReplaceKiroModelCatalog(context.Background(), storage.KiroProbeState{
		AccountID: account.ID, CapabilityKey: capabilityKey, Region: "us-east-1",
		EndpointHash: endpointHash, GovernanceKey: governanceKey, Source: "kiro_live_catalog",
		Generation: now, ExpiresAt: now + 21600, PageCount: 1, Complete: true,
	}, []storage.KiroModelDescriptor{{
		AccountID: account.ID, CapabilityKey: capabilityKey,
		UpstreamID: "claude-opus-4.8", PublicID: "claude-opus-4.8",
		MaxInputTokens: 1_000_000, Complete: true, Source: "kiro_live_catalog",
		Generation: now, ObservedAt: now, ExpiresAt: now + 21600,
	}}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	ordinary, ordinaryBody = request("claude-opus-4-8[1m]", history, "You are Claude Code", "continue ordinary work", "ordinary-over-200k-catalog")
	if ordinary.StatusCode != http.StatusOK || ordinary.Header.Get("X-Pool-Kiro-Context-Window") != "1000000" || !bytes.Contains(ordinaryBody, []byte(`"type":"message"`)) {
		t.Fatalf("catalog-backed ordinary request did not use 1M: status=%d headers=%v body=%s", ordinary.StatusCode, ordinary.Header, ordinaryBody)
	}

	compactSystem := "You are a helpful AI assistant tasked with summarizing conversations."
	compactInstruction := "Your task is to create a detailed summary of the conversation so far, preserving every technical decision."
	// Catalog evidence is authoritative regardless of marketing plan text.
	if err := h.store.SetAccountPlanType(context.Background(), account.ID, "KIRO FREE"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	freeResponse, freeBody := request("claude-opus-4-8[1m]", history, compactSystem, compactInstruction, "compact-free")
	if freeResponse.StatusCode != http.StatusOK || !bytes.Contains(freeBody, []byte("compact summary")) {
		t.Fatalf("catalog-backed compact status=%d body=%s", freeResponse.StatusCode, freeBody)
	}
	if freeResponse.Header.Get("X-Pool-Kiro-Context-Window") != "1000000" || compactRequests != 1 || !fullHistoryForwarded {
		t.Fatalf("compact did not use lossless extended window: headers=%v requests=%d full_history=%v", freeResponse.Header, compactRequests, fullHistoryForwarded)
	}
	overflowHistory := strings.Repeat("overflow-context-", 280_000)
	overflowResponse, overflowBody := request("claude-opus-4-8[1m]", overflowHistory, compactSystem, compactInstruction+"\n\nREMINDER: Do NOT call any tools. Respond with plain text only.", "compact-over-1m")
	var overflowEnvelope struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(overflowBody, &overflowEnvelope); err != nil {
		t.Fatalf("decode overflow compaction response: %v body=%s", err, overflowBody)
	}
	if overflowResponse.StatusCode != http.StatusBadRequest || overflowResponse.Header.Get("X-MiCliProxy-Context-Status") != "compact_failed" || overflowResponse.Header.Get("X-MiCliProxy-Auto-Compact") != "client_retry" || overflowEnvelope.Type != "error" || overflowEnvelope.Error.Type != "invalid_request_error" || overflowEnvelope.Error.Code != "context_length_exceeded" || !strings.HasPrefix(overflowEnvelope.Error.Message, "Prompt is too long:") || !strings.Contains(overflowEnvelope.Error.Message, "tokens > 1000000") || !strings.Contains(overflowEnvelope.Error.Message, "automatically retry compaction") || strings.Contains(overflowEnvelope.Error.Message, "请运行 /compact") {
		t.Fatalf("overflow compaction did not request Claude Code partial retry: status=%d headers=%v body=%s", overflowResponse.StatusCode, overflowResponse.Header, overflowBody)
	}
	caps, err := h.store.ListCapabilities(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	contextVerified := false
	for _, capRow := range caps {
		if canonical, ok := capability.KiroCanonicalModel(capRow.ModelSlug); ok && canonical == "claude-opus-4.8" && capRow.Context1MState == capability.Context1MSupported {
			contextVerified = true
		}
	}
	if !contextVerified {
		t.Fatalf("successful compact did not persist 1M runtime evidence: %+v", caps)
	}
}

func importKiroEndpointForTest(t *testing.T, h *testHarness, endpoint, apiKey string) storage.Account {
	t.Helper()
	allowKiroTestEndpoint(t, h, endpoint)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": apiKey, "endpoint": endpoint})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Kiro import status=%d body=%s", response.StatusCode, body)
	}
	accounts, err := h.store.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("Kiro accounts=%+v err=%v", accounts, err)
	}
	return accounts[0]
}

func TestKiroCachePointsNeverSerializeConcurrentDownstreamCalls(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	account := storage.Account{ID: "kiro-flight", Label: "kiro", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	credentials := storage.KiroCredentials{AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "key", APIRegion: "us-east-1"}
	if err := h.store.UpsertKiroCredentials(ctx, credentials); err != nil {
		t.Fatal(err)
	}
	binding, _ := h.store.GetEgressBinding(ctx, account.ID)
	egress, _ := h.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	endpointHash, _ := kirowire.EndpointHash("", "us-east-1", nil)
	model := "claude-sonnet-4.6"
	if _, err := h.store.ObserveKiroCapability(ctx, account.ID, endpointHash, model, storage.KiroCapabilityObservation{ModelSucceeded: true, CacheReadPresent: true}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"claude-sonnet-4-6","system":"` + strings.Repeat("stable prefix ", 200) + `","messages":[{"role":"user","content":"current"}]}`)
	affinity := routing.AffinityFromKey("flight-session", "test")
	lease := scheduler.Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: model}
	if mode := h.app.effectiveKiroConfig(ctx).KiroCacheMode; mode != "auto" {
		t.Fatalf("kiro cache mode=%q", mode)
	}
	if capability, err := h.store.GetKiroRuntimeCapability(ctx, account.ID, endpointHash, model); err != nil || capability.CacheCapability != "reported" {
		t.Fatalf("cache capability=%+v err=%v", capability, err)
	}
	if prefix := routing.AnthropicStablePromptPrefixHash(raw); prefix == "" {
		t.Fatal("stable prompt prefix hash is empty")
	}
	releaseFirst, waited := h.app.enterKiroCacheSingleflight(ctx, raw, affinity, lease, model, 1)
	if waited {
		t.Fatal("first request unexpectedly waited")
	}
	releaseSecond, waitedSecond := h.app.enterKiroCacheSingleflight(ctx, raw, affinity, lease, model, 1)
	if waitedSecond {
		t.Fatal("second same-prefix request was proxy-paced")
	}
	releaseSecond()
	releaseFirst()
}

func TestFinalizeKiroUsageCreditsOnlyUsesExplicitEstimate(t *testing.T) {
	data := kirowire.ResponseData{Text: "ok", Metering: kirowire.KiroMetering{Credits: kirowire.MeteredFloat{Value: 0, Present: true}}}
	finalizeKiroUsage(&data, kirowire.Conversion{EstimatedInputTokens: 42})
	if data.UsageSource != kirowire.UsageSourceEstimated || data.InputTokens != 42 || data.OutputTokens <= 0 || data.Metering.InputTokens.Present {
		t.Fatalf("credits-only fallback was not clearly estimated: %+v", data)
	}
}

func TestKiroCountTokensIsExplicitlyEstimatedAndDoesNotGenerate(t *testing.T) {
	generateCalls := 0
	kiroMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			generateCalls++
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"unexpected"}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kiroMock.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	allowKiroTestEndpoint(t, h, kiroMock.URL)
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "count-key", "endpoint": kiroMock.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	response, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"count me"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || generateCalls != 0 || response.Header.Get("X-Pool-Usage-Source") != "estimated" || !bytes.Contains(body, []byte(`"estimated":true`)) || !bytes.Contains(body, []byte(`"usage_source":"estimated"`)) {
		t.Fatalf("Kiro count_tokens status=%d generate=%d headers=%v body=%s", response.StatusCode, generateCalls, response.Header, body)
	}
}
