package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/routing"
)

func TestCodexHostedToolPassthroughPreservesPathQueryAuthAndMultipartBoundary(t *testing.T) {
	type observed struct {
		path        string
		query       string
		method      string
		contentType string
		auth        string
		body        string
	}
	var mu sync.Mutex
	var calls []observed
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		value, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, observed{
			path:        r.URL.Path,
			query:       r.URL.RawQuery,
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			auth:        r.Header.Get("Authorization"),
			body:        string(value),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h.importAccount(t, "codex-hosted", "", "access-codex-hosted")

	search, _ := http.NewRequest(
		http.MethodPost,
		h.pool.URL+"/v1/alpha/search?mode=live",
		strings.NewReader(`{"queries":[{"q":"release notes"}]}`),
	)
	search.Header.Set("Content-Type", "application/json")
	searchResponse, err := http.DefaultClient.Do(search)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, searchResponse.Body)
	searchResponse.Body.Close()
	if searchResponse.StatusCode != http.StatusOK {
		t.Fatalf("search status=%d", searchResponse.StatusCode)
	}

	multipart := "--pool-boundary\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nedit\r\n--pool-boundary--\r\n"
	edit, _ := http.NewRequest(
		http.MethodPost,
		h.pool.URL+"/v1/images/edits",
		strings.NewReader(multipart),
	)
	edit.Header.Set("Content-Type", "multipart/form-data; boundary=pool-boundary")
	editResponse, err := http.DefaultClient.Do(edit)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, editResponse.Body)
	editResponse.Body.Close()
	if editResponse.StatusCode != http.StatusOK {
		t.Fatalf("image edit status=%d", editResponse.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("hosted-tool calls=%d: %+v", len(calls), calls)
	}
	if calls[0].path != "/backend-api/codex/alpha/search" ||
		calls[0].query != "mode=live" ||
		calls[0].method != http.MethodPost ||
		calls[0].contentType != "application/json" ||
		calls[0].auth != "Bearer access-codex-hosted" ||
		!strings.Contains(calls[0].body, "release notes") {
		t.Fatalf("search passthrough drift: %+v", calls[0])
	}
	if calls[1].path != "/backend-api/codex/images/edits" ||
		calls[1].contentType != "multipart/form-data; boundary=pool-boundary" ||
		calls[1].auth != "Bearer access-codex-hosted" ||
		calls[1].body != multipart {
		t.Fatalf("image edit passthrough drift: %+v", calls[1])
	}
}

func TestCodexCreatedResourceBindsItsLifecycleToOneAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"file_pool_bound","object":"file"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"file_pool_bound","status":"ready"}`))
	})
	h.importAccount(t, "codex-resource-a", "upstream-a", "access-codex-resource-a")
	h.importAccount(t, "codex-resource-b", "upstream-b", "access-codex-resource-b")

	create, _ := http.NewRequest(
		http.MethodPost,
		h.pool.URL+"/v1/files",
		strings.NewReader(`{"purpose":"assistants"}`),
	)
	create.Header.Set("Content-Type", "application/json")
	createResponse, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, createResponse.Body)
	createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusOK {
		t.Fatalf("create status=%d", createResponse.StatusCode)
	}

	affinity := routing.AffinityFromKey("codex_resource:files:file_pool_bound", "codex_resource")
	binding, err := h.store.GetAffinityBinding(context.Background(), affinity.Hash)
	if err != nil || binding.AccountID == "" || binding.Provider != "codex" {
		t.Fatalf("created resource binding=%+v err=%v", binding, err)
	}

	getResponse, err := http.Get(h.pool.URL + "/v1/files/file_pool_bound")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, getResponse.Body)
	getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d", getResponse.StatusCode)
	}

	requests := h.requests()
	if len(requests) < 2 {
		t.Fatalf("upstream requests=%+v", requests)
	}
	if requests[len(requests)-2].Auth == "" ||
		requests[len(requests)-2].Auth != requests[len(requests)-1].Auth {
		t.Fatalf("resource moved accounts: create=%+v get=%+v", requests[len(requests)-2], requests[len(requests)-1])
	}
}
