package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestResolveDownstreamPolicyRejectsExpiredKeyAndMarksValidKeyUsed(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.RequireDownstreamKey = true
	ctx := context.Background()
	now := storage.Now()

	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash:     hashAPIKey("cap_valid_lifecycle_test"),
		Label:       "valid",
		GroupName:   "cyber",
		ForceModel:  "gpt-5.6-sol",
		ForceEffort: "low",
		Enabled:     true,
		ExpiresAt:   now + 3600,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash:     hashAPIKey("cap_expired_lifecycle_test"),
		Label:       "expired",
		GroupName:   "cyber",
		ForceModel:  "gpt-5.6-terra",
		ForceEffort: "high",
		Enabled:     true,
		ExpiresAt:   now - 1,
	}); err != nil {
		t.Fatal(err)
	}

	validReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	validReq.Header.Set("Authorization", "Bearer cap_valid_lifecycle_test")
	validW := httptest.NewRecorder()
	pol, ok := h.app.resolveDownstreamPolicy(validW, validReq)
	if !ok || !pol.Authed {
		t.Fatalf("valid key rejected: status=%d body=%s", validW.Code, validW.Body.String())
	}
	if pol.ForceModel != "gpt-5.6-sol" || pol.ForceEffort != "low" {
		t.Fatalf("valid key policy = model %q effort %q", pol.ForceModel, pol.ForceEffort)
	}
	valid, found, err := h.store.LookupAPIKey(ctx, hashAPIKey("cap_valid_lifecycle_test"))
	if err != nil || !found {
		t.Fatalf("lookup valid key: found=%v err=%v", found, err)
	}
	if valid.LastUsedAt < now {
		t.Fatalf("last_used_at = %d, want >= %d", valid.LastUsedAt, now)
	}

	expiredReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	expiredReq.Header.Set("Authorization", "Bearer cap_expired_lifecycle_test")
	expiredW := httptest.NewRecorder()
	if pol, ok := h.app.resolveDownstreamPolicy(expiredW, expiredReq); ok || pol.Authed {
		t.Fatalf("expired key authenticated: %+v", pol)
	}
	if expiredW.Code != http.StatusUnauthorized {
		t.Fatalf("expired key status = %d, want 401; body=%s", expiredW.Code, expiredW.Body.String())
	}
	expired, found, err := h.store.LookupAPIKey(ctx, hashAPIKey("cap_expired_lifecycle_test"))
	if err != nil || !found {
		t.Fatalf("lookup expired key: found=%v err=%v", found, err)
	}
	if expired.LastUsedAt != 0 {
		t.Fatalf("expired key last_used_at = %d, want 0", expired.LastUsedAt)
	}
}

func TestDownstreamKeyForcesModelAndEffortWithoutChangingUnforcedKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_override","object":"response","model":"captured","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	})
	h.importAccount(t, "override", "upstream-override", "access-override")
	ctx := context.Background()
	for _, key := range []storage.APIKey{
		{KeyHash: hashAPIKey("cap_unforced_override_test"), Label: "unforced", GroupName: "cyber", ProviderHint: "codex", Enabled: true},
		{KeyHash: hashAPIKey("cap_forced_override_test"), Label: "forced", GroupName: "cyber", ProviderHint: "codex", ForceModel: "gpt-5.6-terra", ForceEffort: "high", Enabled: true},
	} {
		if err := h.store.UpsertAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}

	call := func(key, model, effort string) map[string]interface{} {
		t.Helper()
		before := len(h.requests())
		body := `{"model":"` + model + `","reasoning":{"effort":"` + effort + `","summary":"auto"},"input":"return ok","stream":false}`
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("inference status = %d: %s", resp.StatusCode, raw)
		}
		requests := h.requests()
		if len(requests) != before+1 {
			t.Fatalf("captured requests = %d, want %d", len(requests), before+1)
		}
		var upstreamBody map[string]interface{}
		if err := json.Unmarshal([]byte(requests[len(requests)-1].Body), &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v: %s", err, requests[len(requests)-1].Body)
		}
		return upstreamBody
	}

	unforced := call("cap_unforced_override_test", "gpt-5.6-sol", "low")
	if unforced["model"] != "gpt-5.6-sol" {
		t.Fatalf("unforced model = %#v", unforced["model"])
	}
	if reasoning, _ := unforced["reasoning"].(map[string]interface{}); reasoning["effort"] != "low" {
		t.Fatalf("unforced effort = %#v", reasoning["effort"])
	}

	forced := call("cap_forced_override_test", "gpt-5.6-luna", "low")
	if forced["model"] != "gpt-5.6-terra" {
		t.Fatalf("forced model = %#v, want terra", forced["model"])
	}
	reasoning, _ := forced["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("forced reasoning = %#v, want high with preserved summary", reasoning)
	}
}
