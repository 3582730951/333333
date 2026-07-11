package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestThinkingAdminErrorsUseJSONEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	h.app.cfg.AdminToken = "secret"

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		token       string
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "unauthorized",
			method:      http.MethodGet,
			path:        "/admin/thinking",
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "admin token required",
		},
		{
			name:        "method not allowed",
			method:      http.MethodPut,
			path:        "/admin/thinking",
			token:       "secret",
			wantStatus:  http.StatusMethodNotAllowed,
			wantMessage: "method not allowed",
		},
		{
			name:        "invalid save json",
			method:      http.MethodPost,
			path:        "/admin/thinking",
			body:        "{",
			token:       "secret",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid JSON",
		},
		{
			name:        "oversized save json",
			method:      http.MethodPost,
			path:        "/admin/thinking",
			body:        `{"enabled":true}` + strings.Repeat(" ", adminJSONBodyLimit),
			token:       "secret",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "request body too large",
		},
		{
			name:        "preview missing provider model",
			method:      http.MethodPost,
			path:        "/admin/thinking/preview",
			body:        "{}",
			token:       "secret",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "provider and model are required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := thinkingAdminReq(t, h, tc.method, tc.path, tc.body, tc.token)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%v", resp.StatusCode, tc.wantStatus, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
			assertErrorEnvelope(t, body, tc.wantMessage)
		})
	}
}

func thinkingAdminReq(t *testing.T, h *testHarness, method, path, body, token string) (*http.Response, map[string]interface{}) {
	t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.pool.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return resp, decoded
}

func assertErrorEnvelope(t *testing.T, body map[string]interface{}, wantMessage string) {
	t.Helper()

	errBody, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing JSON error envelope: %v", body)
	}
	if typ, _ := errBody["type"].(string); typ != "codex_pool_error" {
		t.Fatalf("error.type = %q, want codex_pool_error", typ)
	}
	msg, _ := errBody["message"].(string)
	if !strings.Contains(msg, wantMessage) {
		t.Fatalf("error.message = %q, want substring %q", msg, wantMessage)
	}
}
