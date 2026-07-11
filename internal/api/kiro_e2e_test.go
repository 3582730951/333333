package api

import (
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
