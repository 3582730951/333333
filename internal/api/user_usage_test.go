package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// user_usage_test.go is the end-to-end guard for P3: a downstream call made with a
// user-owned api key attributes its usage to that user (usage_records.user_id), the
// user's /user/* console is scoped to their own keys + usage, and one user can never
// see or mutate another's. It reuses the DeepSeek custom-provider harness (deepseekMock
// / setupDeepSeek from custom_stream_test.go).

func getArray(t *testing.T, c *http.Client, url string) []map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var a []map[string]interface{}
	_ = json.Unmarshal(raw, &a)
	return a
}

func TestUserUsageAttributionAndIsolation(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	// A pooled DeepSeek account behind the mock upstream (admin/open-mode setup).
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	// First user becomes admin; treat as user A. Second user B is a normal user.
	a := jarClient(t)
	if resp, _ := doReq(t, a, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"a@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register A: %d", resp.StatusCode)
	}
	csrfA := csrfFor(t, a, h.pool.URL)
	// A creates a self-service downstream key.
	_, keyBody := doReq(t, a, http.MethodPost, h.pool.URL+"/user/api-keys", `{"label":"mykey"}`, map[string]string{csrfHeaderName: csrfA})
	keyA, _ := keyBody["key"].(string)
	if keyA == "" || !strings.HasPrefix(keyA, "cap_") {
		t.Fatalf("user key not issued: %v", keyBody)
	}

	// A makes an inference call WITH that key (Bearer). Model deepseek-chat routes to
	// the custom provider; the response carries usage, attributed to user A.
	plain := &http.Client{}
	if resp, _ := doReq(t, plain, http.MethodPost, h.pool.URL+"/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer " + keyA}); resp.StatusCode != http.StatusOK {
		t.Fatalf("inference with user key: %d", resp.StatusCode)
	}

	// A sees their own usage (dsChatResp usage = 5/2/7) attributed to deepseek-chat.
	h.app.WaitForAsyncWrites() // usage rows are written asynchronously; drain before reading
	usageA := getArray(t, a, h.pool.URL+"/user/usage")
	if len(usageA) == 0 {
		t.Fatalf("user A usage not attributed (empty)")
	}
	var total float64
	var sawModel bool
	for _, row := range usageA {
		total += row["total_tokens"].(float64)
		if row["model"] == "deepseek-chat" {
			sawModel = true
		}
	}
	if !sawModel || total != 7 {
		t.Fatalf("user A usage wrong: model=%v total=%v rows=%v", sawModel, total, usageA)
	}
	// A sees their own key.
	if keys := getArray(t, a, h.pool.URL+"/user/api-keys"); len(keys) != 1 {
		t.Fatalf("user A should see exactly their 1 key, got %d", len(keys))
	}

	// User B sees NOTHING of A's (isolation).
	b := jarClient(t)
	if resp, _ := doReq(t, b, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"b@x.io","password":"password123"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register B: %d", resp.StatusCode)
	}
	if usageB := getArray(t, b, h.pool.URL+"/user/usage"); len(usageB) != 0 {
		t.Fatalf("user B must not see A's usage, got %v", usageB)
	}
	if keysB := getArray(t, b, h.pool.URL+"/user/api-keys"); len(keysB) != 0 {
		t.Fatalf("user B must not see A's keys, got %v", keysB)
	}
	// B cannot delete A's key (404, not revealing it exists).
	csrfB := csrfFor(t, b, h.pool.URL)
	keyHashA, _ := keyBody["key_hash"].(string)
	if resp, _ := doReq(t, b, http.MethodDelete, h.pool.URL+"/user/api-keys/"+keyHashA, "", map[string]string{csrfHeaderName: csrfB}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("user B deleting A's key should 404, got %d", resp.StatusCode)
	}
	// And A's key still works → still listed.
	if keys := getArray(t, a, h.pool.URL+"/user/api-keys"); len(keys) != 1 {
		t.Fatalf("A's key should survive B's delete attempt, got %d", len(keys))
	}
}
