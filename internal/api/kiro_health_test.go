package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/storage"
)

const awsSuspensionFixture = `{"message":"Your AWS Builder ID temporarily is suspended. We have locked your account as a security precaution. To restore access, contact AWS Support at https://support.aws.amazon.com/ and complete verification of your identity."}`

func postKiroHealth(t *testing.T, h *testHarness, accountID string, confirm bool) (int, map[string]any) {
	t.Helper()
	body := `{}`
	if confirm {
		body = `{"confirm_cost":true}`
	}
	response, err := http.Post(h.pool.URL+"/admin/accounts/"+accountID+"/health-test", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, decoded
}

func TestKiroHealthTestRequiresCostConfirmationBeforeAnyProbe(t *testing.T) {
	var usageCalls, generateCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			usageCalls.Add(1)
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"usageLimit":100,"currentUsage":1}]}`))
		case "/generateAssistantResponse":
			generateCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, upstream.URL, "confirm-key")
	baselineUsage := usageCalls.Load()
	status, body := postKiroHealth(t, h, account.ID, false)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != "cost_confirmation_required" || usageCalls.Load() != baselineUsage || generateCalls.Load() != 0 {
		t.Fatalf("confirmation response=%v usage=%d/%d generate=%d", body, usageCalls.Load(), baselineUsage, generateCalls.Load())
	}
}

func TestKiroHealthAuthFailureSkipsInferenceProbe(t *testing.T) {
	var failAuth atomic.Bool
	var generateCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			if failAuth.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"invalid API key"}`))
				return
			}
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO"}`))
		case "/generateAssistantResponse":
			generateCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, upstream.URL, "auth-failure-key")
	failAuth.Store(true)
	status, body := postKiroHealth(t, h, account.ID, true)
	if status != http.StatusOK || body["alive"] != false || body["ready"] != false || body["reason"] != "invalid api key" {
		t.Fatalf("health status=%d body=%#v", status, body)
	}
	auth, _ := body["auth_probe"].(map[string]any)
	inference, _ := body["inference_probe"].(map[string]any)
	if auth["alive"] != false || auth["http_status"] != float64(http.StatusUnauthorized) || inference["checked"] != false || generateCalls.Load() != 0 {
		t.Fatalf("auth=%#v inference=%#v generate=%d", auth, inference, generateCalls.Load())
	}
}

func TestKiroHealthAuthAliveInferenceSuspendedQuarantinesWithoutDeleting(t *testing.T) {
	var generateCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"usageLimit":100,"currentUsage":1}]}`))
		case "/generateAssistantResponse":
			generateCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(awsSuspensionFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, upstream.URL, "suspended-key")
	h.app.cfg.BanAutoDelete = true
	status, body := postKiroHealth(t, h, account.ID, true)
	if status != http.StatusOK || body["alive"] != false || body["ready"] != false || body["state"] != "banned" {
		t.Fatalf("health status=%d body=%#v", status, body)
	}
	auth, _ := body["auth_probe"].(map[string]any)
	inference, _ := body["inference_probe"].(map[string]any)
	if auth["alive"] != true || auth["http_status"] != float64(http.StatusOK) || inference["checked"] != true || inference["alive"] != false || inference["error_code"] != "kiro_account_suspended" {
		t.Fatalf("layered result auth=%#v inference=%#v", auth, inference)
	}
	if body["probe_scope"] != kiroHealthProbeScope || body["model_checked"] != true || generateCalls.Load() != 1 {
		t.Fatalf("scope/model result=%#v generate=%d", body, generateCalls.Load())
	}

	retained, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("suspended Kiro account was deleted: %v", err)
	}
	if retained.QuarantineUntil != kiroSuspensionQuarantineUntil || retained.QuarantineReason != kiroSuspensionQuarantineReason {
		t.Fatalf("suspension quarantine=%+v", retained)
	}
	if _, err := h.store.GetKiroCredentials(context.Background(), account.ID); err != nil {
		t.Fatalf("Kiro credentials were deleted: %v", err)
	}
	if capabilities, err := h.store.ListCapabilities(context.Background(), account.ID); err != nil || len(capabilities) == 0 {
		t.Fatalf("Kiro capabilities were deleted: %+v err=%v", capabilities, err)
	}

	clear, err := http.Post(h.pool.URL+"/admin/accounts/"+account.ID+"/clear-quarantine", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, clear.Body)
	clear.Body.Close()
	if clear.StatusCode != http.StatusConflict {
		t.Fatalf("manual clear status=%d, want 409", clear.StatusCode)
	}

	// The background/free auth probe can remain healthy, but must not clear an
	// inference suspension or spend another generation credit.
	token, _ := h.store.GetToken(context.Background(), account.ID)
	freeProbe := h.app.probeAccountLiveness(context.Background(), retained, token)
	afterFreeProbe, _ := h.store.GetAccount(context.Background(), account.ID)
	if !freeProbe.Ready || generateCalls.Load() != 1 || afterFreeProbe.QuarantineReason != kiroSuspensionQuarantineReason {
		t.Fatalf("free probe=%+v generate=%d account=%+v", freeProbe, generateCalls.Load(), afterFreeProbe)
	}
	// Re-importing the same credential is a free duplicate check. It must not
	// overwrite the inference suspension or send a generation probe.
	credential, _ := json.Marshal(map[string]any{"authMethod": "api_key", "kiroApiKey": "suspended-key", "endpoint": upstream.URL})
	payload, _ := json.Marshal(map[string]any{"kiro_json_text": string(credential), "group_name": "cyber", "egress_id": "egress_direct"})
	importResponse, err := http.Post(h.pool.URL+"/admin/accounts/import-kiro-json", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var importBody map[string]any
	if err := json.NewDecoder(importResponse.Body).Decode(&importBody); err != nil {
		importResponse.Body.Close()
		t.Fatal(err)
	}
	importResponse.Body.Close()
	afterImport, _ := h.store.GetAccount(context.Background(), account.ID)
	if importResponse.StatusCode != http.StatusOK || importBody["duplicate"] != float64(1) || generateCalls.Load() != 1 || afterImport.QuarantineReason != kiroSuspensionQuarantineReason {
		t.Fatalf("duplicate import status=%d body=%#v generate=%d account=%+v", importResponse.StatusCode, importBody, generateCalls.Load(), afterImport)
	}

	audits, _ := h.store.ListAuditLogForAccount(context.Background(), account.ID, 20)
	if !auditHasAction(audits, "kiro_user_suspended") || !auditHasAction(audits, "kiro_inference_probe") {
		t.Fatalf("suspension audits=%+v", audits)
	}
}

func TestKiroSuccessfulManualProbeRecoversSuspensionAndRecordsUsage(t *testing.T) {
	var suspended atomic.Bool
	suspended.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"usageLimit":100,"currentUsage":1}]}`))
		case "/generateAssistantResponse":
			requestBody, _ := io.ReadAll(r.Body)
			if !bytes.Contains(requestBody, []byte("Reply exactly OK")) || bytes.Contains(requestBody, []byte("cachePoint")) || bytes.Contains(requestBody, []byte(`"tools"`)) {
				t.Errorf("health probe was not minimal/public: %s", requestBody)
			}
			if suspended.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(awsSuspensionFixture))
				return
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"modelId":"claude-sonnet-4.6","content":"OK"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"inputTokens":6,"outputTokens":1,"totalTokens":7}}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0.25}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, upstream.URL, "recovery-key")
	h.app.cfg.BanAutoDelete = true
	_, first := postKiroHealth(t, h, account.ID, true)
	if first["quarantine_reason"] != kiroSuspensionQuarantineReason {
		t.Fatalf("first health did not suspend: %#v", first)
	}
	suspended.Store(false)
	status, recoveredBody := postKiroHealth(t, h, account.ID, true)
	if status != http.StatusOK || recoveredBody["alive"] != true || recoveredBody["ready"] != true || recoveredBody["quarantined"] != false {
		t.Fatalf("recovery status=%d body=%#v", status, recoveredBody)
	}
	recovered, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil || recovered.Status != "active" || recovered.QuarantineUntil != 0 || recovered.QuarantineReason != "" {
		t.Fatalf("recovered account=%+v err=%v", recovered, err)
	}
	h.app.WaitForAsyncWrites()
	var affinitySource, rawUsage string
	var credits float64
	var creditsPresent int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT affinity_source, raw_usage_json, kiro_credits, kiro_credits_present FROM usage_records WHERE account_id=? ORDER BY id DESC LIMIT 1`, account.ID).Scan(&affinitySource, &rawUsage, &credits, &creditsPresent); err != nil {
		t.Fatal(err)
	}
	if affinitySource != "kiro_health_probe" || !strings.Contains(rawUsage, `"probe_kind":"kiro_health_probe"`) || credits != 0.25 || creditsPresent != 1 {
		t.Fatalf("probe usage source=%q raw=%s credits=%g present=%d", affinitySource, rawUsage, credits, creditsPresent)
	}
	var affinities int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM affinity_bindings WHERE account_id=?`, account.ID).Scan(&affinities); err != nil || affinities != 0 {
		t.Fatalf("health probe persisted affinity count=%d err=%v", affinities, err)
	}
	audits, _ := h.store.ListAuditLogForAccount(context.Background(), account.ID, 30)
	if !auditHasAction(audits, "kiro_suspension_recovered") {
		t.Fatalf("recovery audit missing: %+v", audits)
	}
}

func TestKiroLiveInferenceSuspensionReturnsExplicitCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getUsageLimits":
			_, _ = w.Write([]byte(`{"subscriptionTitle":"KIRO PRO","usageBreakdownList":[{"usageLimit":100,"currentUsage":1}]}`))
		case "/generateAssistantResponse":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(awsSuspensionFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := importKiroEndpointForTest(t, h, upstream.URL, "live-suspend-key")
	h.app.cfg.BanAutoDelete = true

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(body, []byte(`"code":"kiro_account_suspended"`)) || !bytes.Contains(body, []byte("AWS Support")) {
		t.Fatalf("live suspension status=%d body=%s", response.StatusCode, body)
	}
	retained, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil || retained.QuarantineReason != kiroSuspensionQuarantineReason {
		t.Fatalf("live suspended account=%+v err=%v", retained, err)
	}
}

func TestKiroHealthProbeModelPrefersLowestCostVerifiedAdaptiveModel(t *testing.T) {
	model, ok := kiroHealthProbeModel([]string{"claude-opus-4.8", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4.5"}, "KIRO PRO")
	if !ok || model != "claude-sonnet-4.6" {
		t.Fatalf("model=%q ok=%v", model, ok)
	}
	model, ok = kiroHealthProbeModel(nil, "KIRO FREE")
	if !ok || model != "claude-sonnet-4.6" {
		t.Fatalf("fallback model=%q ok=%v", model, ok)
	}
}

func auditHasAction(rows []storage.AuditLogRow, action string) bool {
	for _, row := range rows {
		if row.Action == action {
			return true
		}
	}
	return false
}
