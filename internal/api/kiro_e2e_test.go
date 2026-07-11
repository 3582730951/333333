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

	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
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
			if !bytes.Contains(requestBody, []byte(`"additionalModelRequestFields":{"thinking":{"type":"adaptive"}}`)) {
				t.Errorf("Kiro default thinking missing from request: %s", requestBody)
			}
			if bytes.Contains(requestBody, []byte("<thinking_mode>")) || bytes.Contains(requestBody, []byte("<system>")) {
				t.Errorf("legacy compatibility prompt leaked into request content: %s", requestBody)
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"hello from kiro"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":7,"outputTokens":3}`)))
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
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello from kiro")) {
		t.Fatalf("messages status=%d body=%s", resp.StatusCode, body)
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

func TestKiroRepeatedUnauthorizedInvalidatesAccount(t *testing.T) {
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
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || generateCalls != 2 {
		t.Fatalf("status=%d generate_calls=%d", resp.StatusCode, generateCalls)
	}
	accounts, _ := h.store.ListAccounts(context.Background())
	if len(accounts) != 1 || accounts[0].Status != "invalid" {
		t.Fatalf("accounts=%+v", accounts)
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
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":7,"outputTokens":2,"cacheReadTokens":0}`)))
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
	case <-time.After(time.Second):
		t.Fatal("Kiro stream buffered until completion")
	}
	close(releaseSecond)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rest, []byte(" second")) || !bytes.Contains(rest, []byte(`"cache_read_input_tokens":0`)) {
		t.Fatalf("terminal stream=%s", rest)
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
			metering := `{"inputTokens":10,"outputTokens":1,"cacheCreationInputTokens":5}`
			if len(generateBodies) == 2 {
				metering = `{"inputTokens":10,"outputTokens":1,"cacheReadInputTokens":5}`
			}
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(metering)))
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
	if len(generateBodies[0]) < 4096 {
		t.Fatalf("probe prefix too short to exercise realistic cache eligibility: %d bytes", len(generateBodies[0]))
	}
	if !bytes.Contains(probeBody, []byte(`"cache_capability":"hit_observed"`)) || !bytes.Contains(probeBody, []byte(`"cache_read_tokens":{"value":5,"present":true}`)) {
		t.Fatalf("probe did not report real metering: %s", probeBody)
	}
}

func TestKiroCacheSingleflightOnlyAfterReportedCapability(t *testing.T) {
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
	releaseFirst, waited := h.app.enterKiroCacheSingleflight(ctx, raw, affinity, lease, model)
	if waited {
		t.Fatal("first request unexpectedly waited")
	}
	type result struct {
		release func()
		waited  bool
	}
	second := make(chan result, 1)
	go func() {
		defer supervisor.Recover("kiro-singleflight-test")
		release, didWait := h.app.enterKiroCacheSingleflight(ctx, raw, affinity, lease, model)
		second <- result{release: release, waited: didWait}
	}()
	select {
	case <-second:
		t.Fatal("second request did not wait for the active stable-prefix flight")
	case <-time.After(40 * time.Millisecond):
	}
	releaseFirst()
	select {
	case got := <-second:
		if !got.waited {
			t.Fatal("second request was released without recording a wait")
		}
		got.release()
	case <-time.After(time.Second):
		t.Fatal("singleflight waiter did not resume")
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
