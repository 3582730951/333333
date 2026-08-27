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

func TestAdminAccountForceCodex429AreIndividual(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := h.importAccount(t, "fc-first", "up-fc-first", "access-fc-first")
	second := h.importAccount(t, "fc-second", "up-fc-second", "access-fc-second")

	code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+first+"/force-codex-429", `{"force_codex_429":true}`)
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

	code, body = grpReq(t, h, http.MethodPatch, "/admin/accounts/"+first+"/force-codex-429", `{"force_codex_429":false}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"force_codex_429":false`)) {
		t.Fatalf("disable status=%d body=%s", code, body)
	}
	firstAccount, err = h.store.GetAccount(context.Background(), first)
	if err != nil || firstAccount.ForceCodex429 {
		t.Fatalf("first account should be restored: %+v err=%v", firstAccount, err)
	}
}

func TestConfirmForceCodex429Window(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id := "confirm-account"
	if h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("first 429 should not confirm")
	}
	if !h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("second 429 within the window should confirm")
	}
	// Expire the window by rewinding the recorded window start; the next 429 must
	// restart the count at one instead of confirming again.
	h.app.forceCodex429Mu.Lock()
	st := h.app.forceCodex429Counts[id]
	if st == nil {
		h.app.forceCodex429Mu.Unlock()
		t.Fatal("no confirmation state recorded")
	}
	st.windowStart = storage.Now() - forceCodex429ConfirmWindowSecs - 1
	h.app.forceCodex429Mu.Unlock()
	if h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("429 after window expiry should restart the count")
	}
	if !h.app.confirmForceCodex429(context.Background(), id) {
		t.Fatal("second 429 in the fresh window should confirm")
	}
}

func TestForceCodex429InjectsPairOnlyOnEnabledOAuthAccount(t *testing.T) {
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

	enabled := h.importAccount(t, "fc-on", "up-fc-on", "access-fc-on")
	_ = h.importAccount(t, "fc-off", "up-fc-off", "access-fc-off")
	// Enable through the admin endpoint so the scheduler account cache is
	// refreshed exactly as in production; a direct store write would leave the
	// short-TTL cache serving a stale snapshot without the flag.
	if code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+enabled+"/force-codex-429", `{"force_codex_429":true}`); code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", code, body)
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
