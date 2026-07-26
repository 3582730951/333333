package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestCustomProvidersWithSameModelProduceDistinctProviderModelRows(t *testing.T) {
	const model = "shared-provider-model"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-attribution","object":"chat.completion","model":"`+model+`",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}
		}`)
	})
	if err := h.store.SetSetting(context.Background(), "usage_accuracy_cutover_at", "0"); err != nil {
		t.Fatal(err)
	}

	keys := map[string]string{}
	for _, providerID := range []string{"attribution-a", "attribution-b"} {
		if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
			ID: providerID, Name: strings.ToUpper(providerID), BaseURL: h.upstream.URL,
			UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
			Enabled:          true, Models: []string{model},
		}); err != nil {
			t.Fatal(err)
		}
		accountID := providerID + "-account"
		if err := h.store.UpsertAccount(t.Context(), storage.Account{
			ID: accountID, Label: accountID, GroupName: "cyber", Provider: providerID, Status: "active",
		}, storage.AccountToken{OpenAIAPIKey: "sk-" + providerID}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
			AccountID: accountID, ModelSlug: model, Source: "custom_usage_attribution_test",
		}}); err != nil {
			t.Fatal(err)
		}

		groupID := "ug-" + providerID
		createRouteTestGroup(t, h, groupID, []storage.TargetRef{{Kind: storage.TargetKindModelProvider, ID: providerID}}, nil)
		key := createTestAPIKeyForGroup(t, h, "cyber")
		if code, raw := grpReq(t, h, http.MethodPost, "/admin/api-keys/"+hashAPIKey(key)+"/user-group", `{"user_group_id":"`+groupID+`"}`); code != http.StatusOK {
			t.Fatalf("bind %s api key = %d: %s", providerID, code, raw)
		}
		keys[providerID] = key
	}
	h.app.scheduler.InvalidateAccountCache()

	for _, providerID := range []string{"attribution-a", "attribution-b"} {
		key := keys[providerID]
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "thread-"+providerID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s request status=%d body=%s", providerID, resp.StatusCode, raw)
		}
	}

	h.app.WaitForAsyncWrites()
	rows, err := h.store.UsageByProviderModelWindow(context.Background(), 0, time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, row := range rows {
		got[row.DimensionKey] = row.Requests
	}
	for _, key := range []string{"attribution-a::" + model, "attribution-b::" + model} {
		if got[key] != 1 {
			raw, _ := json.Marshal(rows)
			t.Fatalf("provider_model row %q requests=%d; rows=%s", key, got[key], raw)
		}
	}
}
