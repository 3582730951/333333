package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCodexSessionMappingMigratesUnavailableBoundAccountWithDurableReplay(t *testing.T) {
	type upstreamCall struct {
		auth      string
		body      string
		turnState string
	}
	var mu sync.Mutex
	var calls []upstreamCall
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, upstreamCall{auth: r.Header.Get("Authorization"), body: string(raw), turnState: r.Header.Get("X-Codex-Turn-State")})
		call := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "bound-root-state")
			_, _ = io.WriteString(w, `{"id":"resp-bound-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-bound","call_id":"call-bound","name":"apply_patch","input":"{}"}]}`)
		case 2:
			if r.Header.Get("Authorization") != "Bearer access-bound-replacement" ||
				strings.Contains(string(raw), "previous_response_id") ||
				r.Header.Get("X-Codex-Turn-State") != "" ||
				!strings.Contains(string(raw), "start durable task") || !strings.Contains(string(raw), "continue durable task") ||
				!strings.Contains(string(raw), `"type":"custom_tool_call"`) || !strings.Contains(string(raw), `"type":"custom_tool_call_output"`) || !strings.Contains(string(raw), "kept tool result") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid durable migration"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"resp-bound-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	boundID := h.importAccount(t, "bound-origin", "upstream-bound-origin", "access-bound-origin")

	post := func(body string, state string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "bound-durable-root")
		req.Header.Set("Session-Id", "bound-durable-root")
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Clone(), string(payload)
	}

	if status, _, body := post(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"start durable task"}]}`, ""); status != http.StatusOK || !strings.Contains(body, "resp-bound-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	if err := h.store.SetAccountStatus(context.Background(), boundID, "disabled"); err != nil {
		t.Fatal(err)
	}
	// The replacement is intentionally added only after the origin is unavailable:
	// this is the refill path that previously returned a permanent 409.
	h.importAccount(t, "bound-replacement", "upstream-bound-replacement", "access-bound-replacement")
	h.app.scheduler.InvalidateAccountCache()

	status, headers, body := post(`{"model":"gpt-5.6-sol","previous_response_id":"resp-bound-root","input":[{"type":"custom_tool_call_output","call_id":"call-bound","output":"kept tool result"},{"role":"user","content":"continue durable task"}]}`, "bound-root-state")
	if status != http.StatusOK || !strings.Contains(body, "resp-bound-recovered") || headers.Get("X-MiCliProxy-Context-Status") != "rebuilt" {
		t.Fatalf("migration status=%d context=%q body=%s", status, headers.Get("X-MiCliProxy-Context-Status"), body)
	}

	mu.Lock()
	got := append([]upstreamCall(nil), calls...)
	mu.Unlock()
	if len(got) != 2 || got[0].auth != "Bearer access-bound-origin" || got[1].auth != "Bearer access-bound-replacement" {
		t.Fatalf("unexpected upstream route: %+v", got)
	}
	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-bound-recovered"})
	if err != nil || len(rows) != 1 || rows[0].AccountID == boundID || rows[0].State != "active" {
		t.Fatalf("recovered mapping rows=%+v err=%v", rows, err)
	}
}

func TestCodexSessionMappingRepairsUltraPreviousResponseNotFoundInPlace(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "ultra-root-state")
			_, _ = io.WriteString(w, `{"id":"resp-ultra-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ultra root"}]}]}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp-ultra-root' not found.\",\"param\":\"previous_response_id\"},\"status\":400}"},"status":400,"type":"error"}`)
		case 3:
			if strings.Contains(string(raw), "previous_response_id") || r.Header.Get("X-Codex-Turn-State") != "" ||
				!strings.Contains(string(raw), "ultra root turn") || !strings.Contains(string(raw), "ultra continuation") ||
				!strings.Contains(string(raw), `"effort":"max"`) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid ultra rebuild"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"resp-ultra-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "ultra-origin", "upstream-ultra-origin", "access-ultra-origin")

	post := func(body, state string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "ultra-root")
		req.Header.Set("Session-Id", "ultra-root")
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Clone(), string(payload)
	}

	if status, _, body := post(`{"model":"gpt-5.6-sol","reasoning":{"effort":"ultra"},"input":[{"role":"user","content":"ultra root turn"}]}`, ""); status != http.StatusOK || !strings.Contains(body, "resp-ultra-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	status, headers, body := post(`{"model":"gpt-5.6-sol","reasoning":{"effort":"ultra"},"previous_response_id":"resp-ultra-root","input":[{"role":"user","content":"ultra continuation"}]}`, "ultra-root-state")
	if status != http.StatusOK || !strings.Contains(body, "resp-ultra-recovered") || headers.Get("X-MiCliProxy-Context-Status") != "rebuilt" {
		t.Fatalf("ultra recovery status=%d context=%q body=%s", status, headers.Get("X-MiCliProxy-Context-Status"), body)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 3 || strings.Contains(got[2], "previous_response_id") {
		t.Fatalf("expected one fresh-root repair, calls=%d payloads=%v", len(got), got)
	}
}

func TestCodexSessionMappingRepairsStreamedPreviousResponseNotFound(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()

		switch call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Codex-Turn-State", "stream-root-state")
			_, _ = io.WriteString(w, `{"id":"resp-stream-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"stream root turn"}]}]}`)
		case 2:
			// The early SSE probe must recognize this before relaying response.created
			// or response.failed downstream, so a durable replay can replace the lost
			// native state in the same client request.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream-root\",\"object\":\"response\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"status\":400,\"response\":{\"id\":\"resp-stream-root\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp-stream-root' not found.\",\"param\":\"previous_response_id\"}}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 3:
			if strings.Contains(string(raw), "previous_response_id") || r.Header.Get("X-Codex-Turn-State") != "" ||
				!strings.Contains(string(raw), "stream root turn") || !strings.Contains(string(raw), "stream continuation") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid streamed durable rebuild"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream-recovered\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream-recovered\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[]}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "stream-recovery-origin", "upstream-stream-recovery-origin", "access-stream-recovery-origin")

	post := func(body, state string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "stream-recovery-root")
		req.Header.Set("Session-Id", "stream-recovery-root")
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Clone(), string(payload)
	}

	if status, _, body := post(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"stream root turn"}]}`, ""); status != http.StatusOK || !strings.Contains(body, "resp-stream-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	status, headers, body := post(`{"model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp-stream-root","input":[{"role":"user","content":"stream continuation"}]}`, "stream-root-state")
	if status != http.StatusOK || headers.Get("X-MiCliProxy-Context-Status") != "rebuilt" ||
		!strings.Contains(body, "resp-stream-recovered") || strings.Contains(body, "previous_response_not_found") {
		t.Fatalf("stream recovery status=%d context=%q body=%s", status, headers.Get("X-MiCliProxy-Context-Status"), body)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 3 || strings.Contains(got[2], "previous_response_id") {
		t.Fatalf("expected one fresh-root streamed repair, calls=%d payloads=%v", len(got), got)
	}
}
