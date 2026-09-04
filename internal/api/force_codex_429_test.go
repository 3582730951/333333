package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/storage"
)

func forceCodex429AdminReq(t *testing.T, h *testHarness, method, path, body string) (int, []byte) {
	t.Helper()
	return adminJSONReq(t, h, method, path, body)
}

func importForceCodex429Account(t *testing.T, h *testHarness, label, upstreamAccountID, accessToken string) string {
	t.Helper()
	payload := map[string]interface{}{
		"label": label,
		"auth_json": map[string]interface{}{
			"OPENAI_API_KEY": accessToken,
			"tokens": map[string]interface{}{
				"access_token":  accessToken,
				"refresh_token": "refresh-" + label,
				"account_id":    upstreamAccountID,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	code, response := forceCodex429AdminReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(raw))
	if code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", code, response)
	}
	var account storage.Account
	if err := json.Unmarshal(response, &account); err != nil {
		t.Fatal(err)
	}
	return account.ID
}

func TestAdminAccountForceCodex429AreIndividual(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := importForceCodex429Account(t, h, "fc-first", "up-fc-first", "access-fc-first")
	second := importForceCodex429Account(t, h, "fc-second", "up-fc-second", "access-fc-second")

	code, body := forceCodex429AdminReq(t, h, http.MethodPost, "/admin/accounts/"+first+"/force-codex-429", `{"force_codex_429":true}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"force_codex_429":true`)) {
		t.Fatalf("enable status=%d body=%s", code, body)
	}
	firstAccount, err := h.store.GetAccount(context.Background(), first)
	if err != nil || !firstAccount.ForceCodex429 {
		t.Fatalf("first account force=%v err=%v", firstAccount.ForceCodex429, err)
	}
	secondAccount, err := h.store.GetAccount(context.Background(), second)
	if err != nil || secondAccount.ForceCodex429 {
		t.Fatalf("second account leaked override=%v err=%v", secondAccount.ForceCodex429, err)
	}
	if h.app.confirmForceCodex429(context.Background(), first) {
		t.Fatal("first pending 429 signal must not confirm")
	}
	cooldownUntil := storage.Now() + 600
	if err := h.store.SetBindingCooldown(context.Background(), first, cooldownUntil); err != nil {
		t.Fatal(err)
	}

	code, body = forceCodex429AdminReq(t, h, http.MethodPatch, "/admin/accounts/"+first+"/force-codex-429", `{"force_codex_429":false}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"force_codex_429":false`)) {
		t.Fatalf("disable status=%d body=%s", code, body)
	}
	firstAccount, err = h.store.GetAccount(context.Background(), first)
	if err != nil || firstAccount.ForceCodex429 {
		t.Fatalf("first account should be restored: %+v err=%v", firstAccount, err)
	}
	h.app.forceCodex429Mu.Lock()
	_, pending := h.app.forceCodex429Counts[first]
	h.app.forceCodex429Mu.Unlock()
	if pending {
		t.Fatal("disabling the guard must clear only its pending confirmation streak")
	}
	binding, err := h.store.GetEgressBinding(context.Background(), first)
	if err != nil || binding.CooldownUntil != cooldownUntil {
		t.Fatalf("disabling guard changed authoritative cooldown: binding=%+v err=%v", binding, err)
	}
}

func TestAdminAccountForceCodex429RejectsIneligibleAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableAdmin(t, h)
	ctx := context.Background()
	cases := []struct {
		name    string
		account storage.Account
		token   storage.AccountToken
	}{
		{
			name:    "api_key",
			account: storage.Account{ID: "force-api-key", Label: "force-api-key", GroupName: "cyber", Provider: "codex", Status: "active"},
			token:   storage.AccountToken{AuthMethod: "api_key", OpenAIAPIKey: "sk-force-api-key"},
		},
		{
			name:    "other_provider",
			account: storage.Account{ID: "force-claude", Label: "force-claude", GroupName: "cyber", Provider: "claude", Status: "active"},
			token:   storage.AccountToken{AuthMethod: "oauth", AccessToken: "claude-oauth", RefreshToken: "claude-refresh"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.store.UpsertAccount(ctx, tc.account, tc.token); err != nil {
				t.Fatal(err)
			}
			code, body := forceCodex429AdminReq(t, h, http.MethodPost, "/admin/accounts/"+tc.account.ID+"/force-codex-429", `{"force_codex_429":true}`)
			if code != http.StatusBadRequest {
				t.Fatalf("enable status=%d body=%s", code, body)
			}
			stored, err := h.store.GetAccount(ctx, tc.account.ID)
			if err != nil || stored.ForceCodex429 {
				t.Fatalf("ineligible account was silently saved: account=%+v err=%v", stored, err)
			}
		})
	}
}

func TestAdminAccountForceCodex429AllowsOpenAIOAuth(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableAdmin(t, h)
	account := storage.Account{ID: "force-openai", Label: "force-openai", GroupName: "cyber", Provider: "openai", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AuthMethod: "oauth", AccessToken: "openai-oauth", RefreshToken: "openai-refresh"}); err != nil {
		t.Fatal(err)
	}
	code, body := forceCodex429AdminReq(t, h, http.MethodPost, "/admin/accounts/"+account.ID+"/force-codex-429", `{"force_codex_429":true}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"force_codex_429":true`)) {
		t.Fatalf("OpenAI OAuth enable status=%d body=%s", code, body)
	}
}

func TestConfirmForceCodex429Window(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id := "confirm-account"
	if err := h.store.UpsertAccount(context.Background(), storage.Account{ID: id, Label: id, GroupName: "default", Provider: "codex", Status: "active"}, storage.AccountToken{AuthMethod: "oauth", AccessToken: "confirm-token"}); err != nil {
		t.Fatal(err)
	}
	if h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("first 429 should not confirm")
	}
	if !h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("second 429 within the window should confirm")
	}
	// Reset is used by the admin-disable path and must remove the durable state
	// without touching any existing authoritative cooldown.
	h.app.clearForceCodex429Confirmation(id)
	if h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("429 after reset should restart the count")
	}
	if !h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("second 429 after reset should confirm")
	}
}

func TestCodex429GuardRuntimeKillSwitchDefaultsOff(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if h.app.codex429GuardRuntimeEnabled(context.Background()) {
		t.Fatal("Codex 429 Guard runtime kill switch must default to false")
	}
	field, ok := configFieldByKey("codex_429_guard_runtime_enabled")
	if !ok || field.Type != fieldBool || field.Effect != effectHot {
		t.Fatalf("runtime kill switch config field=%+v found=%v", field, ok)
	}
	if err := h.store.SetSetting(context.Background(), "codex_429_guard_runtime_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if !h.app.codex429GuardRuntimeEnabled(context.Background()) {
		t.Fatal("runtime kill switch did not honor its hot setting")
	}
}

func TestForceCodex429InjectsPairOnlyWhenRuntimeGuardAndAccountAreEnabled(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string][]byte{} // account ID → upstream request body
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		// The Authorization header survives the relay unchanged (see the
		// rate-limit-controls retry test), so it keys each upstream body by account.
		bodies[r.Header.Get("Authorization")] = append([]byte(nil), raw...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-fc","object":"response","model":"gpt","status":"completed","output":[]}`))
	})

	enabled := importForceCodex429Account(t, h, "fc-on", "up-fc-on", "access-fc-on")
	_ = importForceCodex429Account(t, h, "fc-off", "up-fc-off", "access-fc-off")
	// Enable through the admin endpoint so the scheduler account cache is
	// refreshed exactly as in production; a direct store write would leave the
	// short-TTL cache serving a stale snapshot without the flag.
	if code, body := forceCodex429AdminReq(t, h, http.MethodPost, "/admin/accounts/"+enabled+"/force-codex-429", `{"force_codex_429":true}`); code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", code, body)
	}
	if h.app.codex429GuardRuntimeEnabled(context.Background()) {
		t.Fatal("runtime guard must remain disabled until its global setting is enabled")
	}
	if err := h.store.SetSetting(context.Background(), "codex_429_guard_runtime_enabled", "true"); err != nil {
		t.Fatal(err)
	}

	// The harness does not tag requests by account, so route via the token header
	// to distinguish the two upstream calls in the captured bodies map.
	post := func(accessToken string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":[{"type":"message","role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			payload, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, payload)
		}
	}
	post("access-fc-on")
	post("access-fc-off")

	inputItems := func(body []byte) int {
		t.Helper()
		var root struct {
			Input []map[string]interface{} `json:"input"`
		}
		if err := json.Unmarshal(body, &root); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		return len(root.Input)
	}

	mu.Lock()
	enabledBody, okOn := bodies["Bearer access-fc-on"]
	disabledBody, okOff := bodies["Bearer access-fc-off"]
	mu.Unlock()
	if !okOn || !okOff {
		t.Fatalf("upstream saw enabled=%v disabled=%v", okOn, okOff)
	}
	if got := inputItems(enabledBody); got != 3 {
		t.Fatalf("enabled account should carry 3 input items (1 msg + synthetic pair), got %d: %s", got, enabledBody)
	}
	if got := inputItems(disabledBody); got != 1 {
		t.Fatalf("disabled account should carry 1 input item, got %d: %s", got, disabledBody)
	}
}

func postCodex429TestResponse(t *testing.T, h *testHarness, accessToken string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":[{"type":"message","role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, body
}

func TestCodex429BaselineConfirmationWorksWithoutAccountGuard(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"limited"}}`))
	})
	accountID := importForceCodex429Account(t, h, "baseline-confirm", "up-baseline-confirm", "access-baseline-confirm")
	if err := h.store.SetSetting(context.Background(), "codex_429_guard_runtime_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil || account.ForceCodex429 {
		t.Fatalf("baseline account should not need the account guard: %+v err=%v", account, err)
	}
	before := storage.Now()
	resp, body := postCodex429TestResponse(t, h, "access-baseline-confirm")
	if resp.StatusCode < http.StatusBadRequest {
		t.Fatalf("confirmed upstream 429 unexpectedly became success: status=%d body=%s", resp.StatusCode, body)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("confirmation wire attempts=%d, want exactly 2", gotCalls)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil || binding.CooldownUntil <= before {
		t.Fatalf("confirmed 429 did not install a bounded cooldown: binding=%+v before=%d err=%v", binding, before, err)
	}
	if binding.RecheckPending {
		t.Fatal("confirmed capacity cooldown must not set recheck_pending")
	}
}

func TestCodex429ConfirmationSecondNon429UsesNormalErrorWithoutThirdAttempt(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"bad request"}}`))
	})
	_ = importForceCodex429Account(t, h, "second-non429", "up-second-non429", "access-second-non429")
	if err := h.store.SetSetting(context.Background(), "codex_429_guard_runtime_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	resp, body := postCodex429TestResponse(t, h, "access-second-non429")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("second non-429 must retain normal classifier result: status=%d body=%s", resp.StatusCode, body)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("second non-429 triggered extra same-account attempts=%d", gotCalls)
	}
}
