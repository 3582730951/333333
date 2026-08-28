package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type codexNativeIdentityCapture struct {
	Session string
	Thread  string
	Turn    string
	Window  string
	State   string
	Body    string
}

func enableCodexSessionMappingForTest(h *testHarness) {
	// Mapping is the upstream identity boundary and takes precedence over the legacy
	// stateless compatibility flag. Set every switch explicitly so focused tests do
	// not depend on configuration defaults.
	h.app.cfg.CodexStatelessPassthrough = false
	h.app.cfg.CodexSessionMappingEnabled = true
	h.app.cfg.CodexCPAStrict = true
}

func codexNativeNamespaceForTest(t *testing.T, keyHash, sessionID string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Session-Id", sessionID)
	return codexSessionNamespace(downstreamPolicy{KeyHash: keyHash}, request)
}

func TestCodexSessionNamespaceUsesNativeSessionWithoutExtraConfig(t *testing.T) {
	policy := downstreamPolicy{KeyHash: hashAPIKey("cap-shared-client-key")}
	requestA, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestB, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestA.Header.Set("Thread-Id", "same-visible-thread")
	requestB.Header.Set("Thread-Id", "same-visible-thread")
	requestA.Header.Set("Session-Id", "native-codex-session-a")
	requestB.Header.Set("Session-Id", "native-codex-session-b")

	namespaceA := codexSessionNamespace(policy, requestA)
	namespaceB := codexSessionNamespace(policy, requestB)
	if namespaceA == namespaceB {
		t.Fatalf("shared API key ignored Codex's native session identity: %q", namespaceA)
	}
	for _, namespace := range []string{namespaceA, namespaceB} {
		if strings.Contains(namespace, "native-codex-session-") {
			t.Fatalf("raw native session leaked into namespace: %q", namespace)
		}
	}
}

func TestCodexNativeNamespaceMigratesOnlyExactLegacyState(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	policy := downstreamPolicy{KeyHash: hashAPIKey("cap-native-migration")}
	legacyRequest, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	legacyNamespace := codexLegacySessionNamespace(policy, legacyRequest)
	legacy, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: legacyNamespace,
		Binding: storage.CodexSessionBinding{
			ID: "legacy-native-binding", TreeID: "legacy-native-tree",
			AccountID: "legacy-native-account", EgressID: storage.DefaultDirectEgressID,
			State: "active", RootSessionID: "upstream-root", ThreadID: "upstream-root",
		},
		Aliases: []storage.CodexSessionAlias{
			{Type: "root", Value: "same-visible-thread"},
			{Type: "response", Value: "legacy-exact-response"},
		},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"gpt","previous_response_id":"legacy-exact-response","input":"continue"}`)
	request, _ := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Thread-Id", "same-visible-thread")
	request.Header.Set("Session-Id", "native-session-after-upgrade")
	nativeNamespace := codexSessionNamespace(policy, request)
	if nativeNamespace == legacyNamespace {
		t.Fatalf("native namespace did not differ from key-only legacy namespace: %q", nativeNamespace)
	}
	mapping, err := h.app.resolveCodexSessionMapping(ctx, request, body, policy)
	if err != nil || mapping.binding == nil || mapping.binding.ID != legacy.ID {
		t.Fatalf("exact legacy response migration binding=%+v err=%v", mapping.binding, err)
	}

	migrated, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: nativeNamespace,
		Binding:   *mapping.binding,
		Aliases: []storage.CodexSessionAlias{
			{Type: "root", Value: "same-visible-thread"},
			{Type: "response", Value: "native-response"},
		},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := h.store.ResolveCodexSessionAliases(ctx, nativeNamespace, []storage.CodexSessionAlias{{Type: "response", Value: "native-response"}})
	if err != nil || resolved.ID != migrated.ID {
		t.Fatalf("migrated native response binding=%+v err=%v", resolved, err)
	}

	// A different zero-config Codex client cannot cross via the weak root alias.
	otherBody := []byte(`{"model":"gpt","input":"fresh"}`)
	otherRequest, _ := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(otherBody))
	otherRequest.Header.Set("Thread-Id", "same-visible-thread")
	otherRequest.Header.Set("Session-Id", "different-native-session")
	other, err := h.app.resolveCodexSessionMapping(ctx, otherRequest, otherBody, policy)
	if err != nil || other.binding != nil {
		t.Fatalf("weak legacy hierarchy crossed native namespaces binding=%+v err=%v", other.binding, err)
	}
}

func TestCodexDownstreamIdentityPrefersConcreteThreadOverWeakSession(t *testing.T) {
	headers := http.Header{}
	headers.Set("Thread-Id", "cli-thread-alpha")
	headers.Set("Session-Id", "shared-process-session")
	identity := codexDownstreamSessionIdentity(headers, []byte(`{"model":"gpt","conversation_id":"conversation-fallback"}`))
	if identity.RootID != "cli-thread-alpha" || identity.ThreadID != "cli-thread-alpha" ||
		identity.SessionID != "shared-process-session" || identity.durableSessionAlias() {
		t.Fatalf("weak session replaced concrete root: %+v", identity)
	}
	for _, alias := range identity.directAliases() {
		if alias.Type == "session" {
			t.Fatalf("weak process session became a durable alias: %+v", identity.directAliases())
		}
	}

	childHeaders := headers.Clone()
	childHeaders.Set("Thread-Id", "cli-child")
	childHeaders.Set("X-Codex-Parent-Thread-Id", "cli-thread-alpha")
	child := codexDownstreamSessionIdentity(childHeaders, []byte(`{"model":"gpt"}`))
	if child.RootID != "" || child.ThreadID != "cli-child" || child.ParentID != "cli-thread-alpha" {
		t.Fatalf("child claimed a weak root instead of resolving its parent: %+v", child)
	}

	sessionOnly := codexDownstreamSessionIdentity(nil, []byte(`{"model":"gpt","session_id":"session-only"}`))
	if sessionOnly.RootID != "session-only" || sessionOnly.ThreadID != "session-only" || !sessionOnly.durableSessionAlias() {
		t.Fatalf("session-only legacy client lost its usable root: %+v", sessionOnly)
	}
}

func TestCodexUpstreamAttemptPersistsAfterRequestCancellation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	accountID := h.importAccount(t, "cancelled-attempt", "upstream-cancelled-attempt", "access-cancelled-attempt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.app.recordCodexUpstreamAttemptBinding(ctx, storage.CodexSessionBinding{
		TreeID:    "tree-cancelled-attempt",
		AccountID: accountID,
		EgressID:  storage.DefaultDirectEgressID,
		Epoch:     1,
		ExpiresAt: storage.Now() + 60,
	}, scheduler.Lease{
		Account:    storage.Account{ID: accountID},
		RouteEpoch: 1,
	}, storage.EgressProfile{ID: storage.DefaultDirectEgressID}, "transport_attempted", 0)
	h.app.WaitForAsyncWrites()

	rows, err := h.store.ListCodexUpstreamAttemptDiagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.AccountID == accountID && row.EgressID == storage.DefaultDirectEgressID && row.State == "transport_attempted" {
			return
		}
	}
	t.Fatalf("cancelled request lost its upstream attempt record: %+v", rows)
}

func TestCodexUpstreamAttemptDoesNotBlockTerminalPathOnBusyWriter(t *testing.T) {
	h := newHarness(t, nil)
	accountID := h.importAccount(t, "queued-attempt", "upstream-queued-attempt", "access-queued-attempt")
	tx, err := h.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	returned := make(chan struct{})
	go func() {
		h.app.recordCodexUpstreamAttemptBinding(context.Background(), storage.CodexSessionBinding{
			TreeID: "tree-queued-attempt", AccountID: accountID, EgressID: storage.DefaultDirectEgressID, ExpiresAt: storage.Now() + 60,
		}, scheduler.Lease{Account: storage.Account{ID: accountID}}, storage.EgressProfile{ID: storage.DefaultDirectEgressID}, "response_headers", http.StatusOK)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(250 * time.Millisecond):
		_ = tx.Rollback()
		t.Fatal("diagnostic attempt blocked the request path behind SQLite's writer")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	h.app.WaitForAsyncWrites()
	rows, err := h.store.ListCodexUpstreamAttemptDiagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.AccountID == accountID && row.State == "response_headers" {
			return
		}
	}
	t.Fatalf("queued upstream attempt was not drained: %+v", rows)
}

func TestCodexSessionMappingSeparatesConcurrentCLIThreadsWithSharedWeakSession(t *testing.T) {
	type capture struct {
		kind, session, thread, body string
	}
	var (
		mu       sync.Mutex
		captures []capture
		starts   atomic.Int32
		release  = make(chan struct{})
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		kind := ""
		for _, candidate := range []string{"alpha-start", "beta-start", "alpha-next", "beta-next"} {
			if strings.Contains(string(raw), candidate) {
				kind = candidate
				break
			}
		}
		if strings.HasSuffix(kind, "-start") {
			if starts.Add(1) == 2 {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				t.Errorf("independent CLI roots did not reach upstream concurrently")
			}
		}
		mu.Lock()
		captures = append(captures, capture{
			kind: kind, session: r.Header.Get("Session-Id"),
			thread: r.Header.Get("Thread-Id"), body: string(raw),
		})
		mu.Unlock()
		responseID := "resp_" + strings.ReplaceAll(kind, "-", "_")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+responseID+`","object":"response","model":"gpt","status":"completed","output":[]}`)
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "shared-cli", "upstream-shared-cli", "access-shared-cli")
	const apiKey = "cap_shared_cli_isolation"
	if err := h.store.UpsertAPIKey(context.Background(), storage.APIKey{
		KeyHash: hashAPIKey(apiKey), Label: "shared-cli", GroupName: "cyber", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	post := func(thread, body string) (int, string, error) {
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Thread-Id", thread)
		req.Header.Set("Session-Id", "shared-process-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), readErr
	}
	runPair := func(cases []struct{ thread, body, response string }) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan string, len(cases))
		for _, tc := range cases {
			tc := tc
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, body, err := post(tc.thread, tc.body)
				if err != nil || status != http.StatusOK || !strings.Contains(body, tc.response) {
					errs <- fmt.Sprintf("thread=%s status=%d body=%s err=%v", tc.thread, status, body, err)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for failure := range errs {
			t.Error(failure)
		}
	}
	runPair([]struct{ thread, body, response string }{
		{"cli-thread-alpha", `{"model":"gpt","session_id":"shared-process-session","input":"alpha-start"}`, "resp_alpha_start"},
		{"cli-thread-beta", `{"model":"gpt","session_id":"shared-process-session","input":"beta-start"}`, "resp_beta_start"},
	})
	runPair([]struct{ thread, body, response string }{
		{"cli-thread-alpha", `{"model":"gpt","session_id":"shared-process-session","previous_response_id":"resp_alpha_start","input":"alpha-next"}`, "resp_alpha_next"},
		{"cli-thread-beta", `{"model":"gpt","session_id":"shared-process-session","previous_response_id":"resp_beta_start","input":"beta-next"}`, "resp_beta_next"},
	})

	mu.Lock()
	got := append([]capture(nil), captures...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("upstream calls=%d captures=%+v", len(got), got)
	}
	byKind := make(map[string]capture, len(got))
	for _, item := range got {
		byKind[item.kind] = item
	}
	alpha, beta := byKind["alpha-start"], byKind["beta-start"]
	if alpha.session == "" || beta.session == "" || alpha.session != alpha.thread || beta.session != beta.thread ||
		alpha.session == beta.session {
		t.Fatalf("independent CLI roots shared an upstream identity alpha=%+v beta=%+v", alpha, beta)
	}
	if byKind["alpha-next"].session != alpha.session || byKind["beta-next"].session != beta.session {
		t.Fatalf("continuations changed or crossed identities captures=%+v", got)
	}
	if strings.Contains(byKind["alpha-next"].body, "resp_beta_start") ||
		strings.Contains(byKind["beta-next"].body, "resp_alpha_start") {
		t.Fatalf("continuation state crossed CLI roots captures=%+v", got)
	}

	namespace := codexNativeNamespaceForTest(t, hashAPIKey(apiKey), "shared-process-session")
	alphaRows, alphaErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "root", Value: "cli-thread-alpha"})
	betaRows, betaErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "root", Value: "cli-thread-beta"})
	if alphaErr != nil || betaErr != nil || len(alphaRows) != 1 || len(betaRows) != 1 ||
		alphaRows[0].TreeID == betaRows[0].TreeID {
		t.Fatalf("durable roots were not isolated alpha=%+v/%v beta=%+v/%v", alphaRows, alphaErr, betaRows, betaErr)
	}
	if rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "session", Value: "shared-process-session"}); err == nil || len(rows) != 0 {
		t.Fatalf("weak shared session unexpectedly became durable rows=%+v err=%v", rows, err)
	}
}

func TestCodexSessionMappingKeepsNativeIdentityAndToolOutput(t *testing.T) {
	var mu sync.Mutex
	var captures []codexNativeIdentityCapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture := codexNativeIdentityCapture{
			Session: r.Header.Get("Session-Id"),
			Thread:  r.Header.Get("Thread-Id"),
			Turn:    turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")),
			Window:  r.Header.Get("X-Codex-Window-Id"),
			State:   r.Header.Get("X-Codex-Turn-State"),
			Body:    string(raw),
		}
		mu.Lock()
		captures = append(captures, capture)
		call := len(captures)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.Header().Set("X-Codex-Turn-State", "opaque-state-1")
			_, _ = w.Write([]byte(`{"id":"resp-map-1","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		w.Header().Set("X-Codex-Turn-State", "opaque-state-2")
		_, _ = w.Write([]byte(`{"id":"resp-map-2","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "mapping", "upstream-mapping", "access-mapping")

	post := func(body string, state string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "client-root-thread")
		req.Header.Set("Session-Id", "client-root-thread")
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := post(`{"model":"gpt","thread_id":"client-root-thread","session_id":"client-root-thread","input":[{"role":"user","content":"start"}]}`, "")
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || first.Header.Get("X-MiCliProxy-Context-Engine") != "cpa-v2" || first.Header.Get("X-Codex-Turn-State") != "opaque-state-1" || !bytes.Contains(firstBody, []byte("resp-map-1")) {
		t.Fatalf("first status=%d engine=%q turn_state=%q body=%s", first.StatusCode, first.Header.Get("X-MiCliProxy-Context-Engine"), first.Header.Get("X-Codex-Turn-State"), firstBody)
	}

	second := post(`{"model":"gpt","previous_response_id":"resp-map-1","input":[{"type":"custom_tool_call_output","call_id":"call-client-1","output":{"exact":900719925474099312345}}]}`, "opaque-state-1")
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || !bytes.Contains(secondBody, []byte("resp-map-2")) {
		t.Fatalf("second status=%d body=%s", second.StatusCode, secondBody)
	}

	mu.Lock()
	got := append([]codexNativeIdentityCapture(nil), captures...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d", len(got))
	}
	if got[0].Session == "" || got[0].Session != got[0].Thread || got[0].Thread == "client-root-thread" {
		t.Fatalf("root identity not internally mapped: %+v", got[0])
	}
	if strings.Contains(got[0].Body, "client-root-thread") {
		t.Fatalf("downstream session correlator leaked into upstream body: %s", got[0].Body)
	}
	if got[1].Session != got[0].Session || got[1].Thread != got[0].Thread || got[1].Turn == got[0].Turn {
		t.Fatalf("identity lifecycle mismatch first=%+v second=%+v", got[0], got[1])
	}
	if got[1].State != "opaque-state-1" || !strings.Contains(got[1].Body, `"previous_response_id":"resp-map-1"`) || !strings.Contains(got[1].Body, `"type":"custom_tool_call_output"`) || !strings.Contains(got[1].Body, "900719925474099312345") {
		t.Fatalf("state/tool output was not native: %+v", got[1])
	}
	if !strings.HasSuffix(got[0].Window, ":0") || got[1].Window != got[0].Window {
		t.Fatalf("window lifecycle first=%q second=%q", got[0].Window, got[1].Window)
	}

	namespace := codexNativeNamespaceForTest(t, "", "client-root-thread")
	rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-map-1"})
	if err != nil || len(rows) != 1 || rows[0].AccountID == "" {
		t.Fatalf("durable response mapping rows=%+v err=%v", rows, err)
	}
	if _, err := h.store.GetAffinityBinding(context.Background(), routing.ResponseAffinityKey("resp-map-1").Hash); !storage.NotFound(err) {
		t.Fatalf("raw response id leaked into legacy affinity binding: %v", err)
	}
}

func TestCodexSessionMappingUsesHTTPForPersistentHTTPChain(t *testing.T) {
	type capturedUpstreamCall struct {
		method  string
		upgrade string
		body    string
	}
	var mu sync.Mutex
	var calls []capturedUpstreamCall
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, capturedUpstreamCall{method: r.Method, upgrade: r.Header.Get("Upgrade"), body: string(raw)})
		call := len(calls)
		mu.Unlock()

		responseID := "resp-http-chain-root"
		if call > 1 {
			responseID = "resp-http-chain-next"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\""+responseID+"\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\""+responseID+"\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	accountID := h.importAccount(t, "http-chain", "upstream-http-chain", "access-http-chain")
	setTestCapability(t, h, accountID, "gpt-5.6-sol", 1024)

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "http-chain-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(payload)
	}
	if status, body := post(`{"model":"gpt-5.6-sol","stream":true,"input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-http-chain-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp-http-chain-root","input":"continue"}`); status != http.StatusOK || !strings.Contains(body, "resp-http-chain-next") {
		t.Fatalf("continuation status=%d body=%s", status, body)
	}
	mu.Lock()
	got := append([]capturedUpstreamCall(nil), calls...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d %#v", len(got), got)
	}
	for i, call := range got {
		if call.method != http.MethodPost || call.upgrade != "" {
			t.Fatalf("HTTP CPA turn %d unexpectedly used websocket: %#v", i, call)
		}
	}
	if !strings.Contains(got[1].body, `"previous_response_id":"resp-http-chain-root"`) {
		t.Fatalf("continuation lost native response id: %s", got[1].body)
	}
}

func TestCodexSessionMappingPersistsInstallationIDAcrossOSHints(t *testing.T) {
	var mu sync.Mutex
	installationIDs := []string{}
	userAgents := []string{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("upstream payload: %v", err)
		}
		var installationID string
		if err := json.Unmarshal(payload.ClientMetadata["x-codex-installation-id"], &installationID); err != nil || installationID == "" {
			t.Fatalf("mapped installation id=%q err=%v payload=%s", installationID, err, raw)
		}
		mu.Lock()
		installationIDs = append(installationIDs, installationID)
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		call := len(installationIDs)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"resp-installation-root","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-installation-next","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.IdentityOSSource = "downstream"
	h.importAccount(t, "installation", "upstream-installation", "access-installation")

	post := func(body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "installation-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// The root presents macOS; the tool-result turn explicitly presents Linux.
	// Without a persisted virtual installation identity those two OS hints derive
	// different devices and the upstream rejects the otherwise valid response id.
	first := post(`{"model":"gpt","input":[{"role":"user","content":"work in /Users/alice/project"}]}`)
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("root status=%d", first.StatusCode)
	}
	second := post(`{"model":"gpt","previous_response_id":"resp-installation-root","input":[{"type":"function_call_output","call_id":"call-installation","output":"Platform: linux"}]}`)
	_, _ = io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("continuation status=%d", second.StatusCode)
	}

	mu.Lock()
	got := append([]string(nil), installationIDs...)
	gotUserAgents := append([]string(nil), userAgents...)
	mu.Unlock()
	if len(got) != 2 || got[0] == "" || got[1] != got[0] {
		t.Fatalf("installation identity changed across strict continuation: %q", got)
	}
	if len(gotUserAgents) != 2 || gotUserAgents[0] == "" || gotUserAgents[1] != gotUserAgents[0] {
		t.Fatalf("device OS profile changed across strict continuation: %q", gotUserAgents)
	}
	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-installation-root"})
	if err != nil || len(rows) != 1 || rows[0].InstallationID != got[0] || rows[0].DeviceOSHint != "Mac OS" || !rows[0].DeviceOSHintSet {
		t.Fatalf("durable installation mapping rows=%+v err=%v", rows, err)
	}
}

func TestNativeCodexContinuePreservesInstructionSnapshotLitePrefix(t *testing.T) {
	original := setResponsesInstructions([]byte(`{"model":"gpt-5.6-sol","instructions":"must disappear","previous_response_id":"resp-old","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec","format":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"permissions developer"}],"exact":900719925474099312346},{"type":"message","role":"developer","content":[{"type":"input_text","text":"collaboration developer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"old user turn"}],"exact":900719925474099312345}]}`), "snapshotted administrator base")
	continued, err := nativeCodexContinueBody(original, "resp-latest")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(continued, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["previous_response_id"]) != `"resp-latest"` {
		t.Fatalf("previous response was not updated: %s", continued)
	}
	if _, exists := root["instructions"]; exists {
		t.Fatalf("Lite continuation regained top-level instructions: %s", continued)
	}
	var input []json.RawMessage
	if err := json.Unmarshal(root["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 5 || !strings.Contains(string(input[0]), `900719925474099312345`) ||
		!strings.Contains(string(input[1]), "snapshotted administrator base") ||
		!strings.Contains(string(input[2]), "permissions developer") || !strings.Contains(string(input[2]), `900719925474099312346`) ||
		!strings.Contains(string(input[3]), "collaboration developer") || strings.Contains(string(continued), "must disappear") ||
		strings.Contains(string(continued), "old user turn") {
		t.Fatalf("Lite continuation did not retain only its stable prefix: %s", continued)
	}
	if !strings.Contains(string(input[4]), `"text":"continue"`) || !strings.Contains(string(root["stream"]), "true") {
		t.Fatalf("native continuation input missing: %s", continued)
	}
}

func TestCodexSessionMappingPersistsTurnStateFromStreamMetadata(t *testing.T) {
	var calls atomic.Int32
	var resumedState string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.metadata\ndata: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-turn-state\":\"opaque-metadata-state\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-metadata-state\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-metadata-state\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
			return
		}
		resumedState = r.Header.Get("X-Codex-Turn-State")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-metadata-state-next","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "metadata-state", "upstream-metadata-state", "access-metadata-state")

	firstReq, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","stream":true,"input":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("Thread-Id", "metadata-root")
	first, err := http.DefaultClient.Do(firstReq)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || !bytes.Contains(firstBody, []byte("opaque-metadata-state")) || !bytes.Contains(firstBody, []byte("resp-metadata-state")) {
		t.Fatalf("metadata stream status=%d body=%s", first.StatusCode, firstBody)
	}

	secondReq, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","previous_response_id":"resp-metadata-state","input":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("X-Codex-Turn-State", "opaque-metadata-state")
	second, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || calls.Load() != 2 || resumedState != "opaque-metadata-state" || !bytes.Contains(secondBody, []byte("resp-metadata-state-next")) {
		t.Fatalf("metadata resume status=%d calls=%d upstream-state=%q body=%s", second.StatusCode, calls.Load(), resumedState, secondBody)
	}
}

func TestCodexSessionMappingAdvancesWindowAfterBodyTriggeredCompaction(t *testing.T) {
	var mu sync.Mutex
	var windows []string
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		windows = append(windows, r.Header.Get("X-Codex-Window-Id"))
		call := calls.Add(1)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-` + strconv.Itoa(int(call)) + `","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "compact-window", "upstream-compact-window", "access-compact-window")

	post := func(body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "compact-window-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, result)
		}
	}
	post(`{"model":"gpt","input":"before compact"}`)
	post(`{"model":"gpt","previous_response_id":"resp-compact-1","compaction_trigger":true,"input":"compact"}`)
	post(`{"model":"gpt","previous_response_id":"resp-compact-2","input":"after compact"}`)
	mu.Lock()
	got := append([]string(nil), windows...)
	mu.Unlock()
	if len(got) != 3 || !strings.HasSuffix(got[0], ":0") || got[1] != got[0] || !strings.HasSuffix(got[2], ":1") || strings.TrimSuffix(got[2], ":1") != strings.TrimSuffix(got[0], ":0") {
		t.Fatalf("compaction window lifecycle=%v", got)
	}
}

func TestCodexSessionMappingDoesNotAdvanceWindowAfterCompactContextFailure(t *testing.T) {
	var mu sync.Mutex
	var windows []string
	var paths []string
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		mu.Lock()
		windows = append(windows, r.Header.Get("X-Codex-Window-Id"))
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-compact-failure-root","object":"response","model":"gpt","status":"completed","output":[]}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","response":{"id":"resp-compact-failure","status":"failed","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too many tokens to compact"}}}`+"\n\n")
		case 3:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-compact-failure-next","object":"response","model":"gpt","status":"completed","output":[]}`)
		default:
			t.Fatalf("unexpected upstream call %d", call)
		}
	})
	enableCodexSessionMappingForTest(h)
	accountID := h.importAccount(t, "compact-failure-window", "upstream-compact-failure-window", "access-compact-failure-window")

	post := func(path, body string) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "compact-failure-window-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", path, resp.StatusCode, result)
		}
		return result
	}
	post("/v1/responses", `{"model":"gpt","input":"before compact"}`)
	failed := post("/v1/responses/compact", `{"model":"gpt","instructions":"compact","input":[{"role":"user","content":"full history"}],"tools":[],"parallel_tool_calls":false}`)
	if !bytes.Contains(failed, []byte(`"code":"context_length_exceeded"`)) {
		t.Fatalf("compact failure signal changed: %s", failed)
	}
	post("/v1/responses", `{"model":"gpt","previous_response_id":"resp-compact-failure-root","input":"after failed compact"}`)

	mu.Lock()
	gotWindows := append([]string(nil), windows...)
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotWindows) != 3 || !strings.HasSuffix(gotWindows[0], ":0") || gotWindows[1] != gotWindows[0] || gotWindows[2] != gotWindows[0] {
		t.Fatalf("failed compact advanced window generation: %v", gotWindows)
	}
	if len(gotPaths) != 3 || gotPaths[1] != "/backend-api/codex/responses/compact" || calls.Load() != 3 {
		t.Fatalf("compact failure retried or used wrong route: calls=%d paths=%v", calls.Load(), gotPaths)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil || account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("compact request error changed account health: %+v err=%v", account, err)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil || binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("compact request error changed egress binding: %+v err=%v", binding, err)
	}
}

func TestCodexSessionMappingCommitsConcurrentAnonymousRootsWithUniqueResponses(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: response.completed
data: {"type":"response.completed","response":{"id":"resp-concurrent-`+strconv.Itoa(int(call))+`","object":"response","model":"gpt","status":"completed","output":[]}}

data: [DONE]

`)
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "concurrent-roots", "upstream-concurrent-roots", "access-concurrent-roots")

	const requests = 32
	errs := make(chan error, requests)
	var group sync.WaitGroup
	for index := 0; index < requests; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","stream":true,"prompt_cache_key":"concurrent-`+strconv.Itoa(index)+`","input":"hello"}`))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				errs <- readErr
			} else if closeErr != nil {
				errs <- closeErr
			} else if resp.StatusCode != http.StatusOK {
				errs <- errors.New(resp.Status)
			}
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	active, retired, err := h.app.store.CodexSessionMappingMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active != requests || retired != 0 {
		t.Fatalf("mapping counts active=%d retired=%d want_active=%d", active, retired, requests)
	}
}

func TestCodexSessionMappingUsesOneIdentityAcrossDownstreamWebSocketTurns(t *testing.T) {
	var mu sync.Mutex
	var upstreamHeaders http.Header
	var payloads []map[string]interface{}
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		upstreamHeaders = r.Header.Clone()
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		for index, responseID := range []string{"resp-ws-map-1", "resp-ws-map-2"} {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				t.Fatalf("read upstream websocket payload %d: %v", index, readErr)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode upstream websocket payload: %v\n%s", err, raw)
			}
			mu.Lock()
			payloads = append(payloads, payload)
			mu.Unlock()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+responseID+`","object":"response","model":"gpt","status":"in_progress"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+responseID+`","object":"response","model":"gpt","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`))
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "ws-map", "upstream-ws-map", "access-ws-map")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream websocket dial: %v body=%s", err, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	sendAndReadTerminal := func(raw string, responseID string) {
		t.Helper()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			_, event, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(event, []byte(responseID)) {
				t.Fatalf("downstream websocket event missing response id %q: %s", responseID, event)
			}
		}
	}
	sendAndReadTerminal(`{"type":"response.create","model":"gpt","thread_id":"client-ws-root","session_id":"client-ws-root","input":"start","stream":true}`, "resp-ws-map-1")
	sendAndReadTerminal(`{"type":"response.create","model":"gpt","previous_response_id":"resp-ws-map-1","input":"resume","stream":true}`, "resp-ws-map-2")

	mu.Lock()
	headers := upstreamHeaders.Clone()
	gotPayloads := append([]map[string]interface{}(nil), payloads...)
	mu.Unlock()
	if len(gotPayloads) != 2 {
		t.Fatalf("upstream websocket payloads=%d", len(gotPayloads))
	}
	sessionID, threadID := headers.Get("Session-Id"), headers.Get("Thread-Id")
	if sessionID == "" || sessionID != threadID || sessionID == "client-ws-root" || !isCodexUUIDv7(sessionID) {
		t.Fatalf("upstream websocket root headers session=%q thread=%q", sessionID, threadID)
	}
	metadata := func(payload map[string]interface{}) map[string]interface{} {
		value, _ := payload["client_metadata"].(map[string]interface{})
		return value
	}
	firstMetadata, secondMetadata := metadata(gotPayloads[0]), metadata(gotPayloads[1])
	firstSession, _ := firstMetadata["session_id"].(string)
	firstThread, _ := firstMetadata["thread_id"].(string)
	secondSession, _ := secondMetadata["session_id"].(string)
	secondThread, _ := secondMetadata["thread_id"].(string)
	firstTurn := turnIDFromMetadata(firstMetadata)
	secondTurn := turnIDFromMetadata(secondMetadata)
	if firstSession != sessionID || firstThread != threadID || secondSession != sessionID || secondThread != threadID || !isCodexUUIDv7(firstTurn) || !isCodexUUIDv7(secondTurn) || firstTurn == secondTurn {
		t.Fatalf("websocket identity metadata first=%+v second=%+v headers=%v", firstMetadata, secondMetadata, headers)
	}
	if _, present := gotPayloads[0]["thread_id"]; present {
		t.Fatalf("downstream root thread leaked as top-level upstream parameter: %+v", gotPayloads[0])
	}
	if rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-ws-map-2"}); err != nil || len(rows) != 1 || rows[0].RootSessionID != sessionID || rows[0].ThreadID != threadID {
		t.Fatalf("websocket terminal mapping rows=%+v err=%v", rows, err)
	}
}

func turnIDFromMetadata(metadata map[string]interface{}) string {
	raw, _ := metadata["x-codex-turn-metadata"].(string)
	return turnIDFromHeader(raw)
}

func isCodexUUIDv7(value string) bool {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Version() == 7
}

func TestCodexSessionMappingRejectsUnknownStateBeforeUpstream(t *testing.T) {
	called := false
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	enableCodexSessionMappingForTest(h)
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","previous_response_id":"resp-unknown","input":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("codex_session_mapping_unidentified")) || called {
		t.Fatalf("status=%d called=%v body=%s", resp.StatusCode, called, body)
	}
}

func TestCodexSessionMappingPassesToolOutput400WithoutRotation(t *testing.T) {
	var calls atomic.Int32
	var secondBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-tool-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
			return
		}
		secondBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"No tool output found for custom tool call call-native."},"status":400,"type":"error"}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "tool-400", "upstream-tool-400", "access-tool-400")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "tool-400-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-tool-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	status, body := post(`{"model":"gpt","previous_response_id":"resp-tool-origin","input":[{"type":"custom_tool_call_output","call_id":"call-native","output":{"exact":900719925474099312345}}]}`)
	if status != http.StatusBadRequest || calls.Load() != 2 || !strings.Contains(body, "No tool output found") || strings.Contains(body, "response.failed") {
		t.Fatalf("native tool 400 status=%d calls=%d body=%s", status, calls.Load(), body)
	}
	if !strings.Contains(secondBody, `"type":"custom_tool_call_output"`) || !strings.Contains(secondBody, "900719925474099312345") {
		t.Fatalf("tool result was changed before upstream: %s", secondBody)
	}
	resolved, err := h.store.ResolveCodexSessionAliases(context.Background(), "unauthenticated", []storage.CodexSessionAlias{{Type: "response", Value: "resp-tool-origin"}})
	if err != nil || resolved.State != "active" {
		t.Fatalf("ordinary tool 400 must retain active epoch binding=%+v err=%v", resolved, err)
	}
}

func TestCodexSessionMappingRecoversOnlyAfterUpstreamContextLoss(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"previous_response_not_found","message":"Previous response resp-retired-origin was not found."}}`))
		case 3:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-retired-recovered","object":"response","model":"gpt","status":"completed","output":[]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "retire", "upstream-retire", "access-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","previous_response_id":"resp-retired-origin","input":"resume"}`); status != http.StatusOK || !strings.Contains(body, "resp-retired-recovered") {
		t.Fatalf("context loss was not recovered status=%d body=%s", status, body)
	}
	oldRows, oldErr := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-retired-origin"})
	newRows, newErr := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-retired-recovered"})
	if calls.Load() != 3 || oldErr != nil || len(oldRows) != 1 || oldRows[0].State != "retired" || newErr != nil || len(newRows) != 1 || newRows[0].State != "active" {
		t.Fatalf("recovery calls=%d old=%+v old_err=%v new=%+v new_err=%v", calls.Load(), oldRows, oldErr, newRows, newErr)
	}
}

func TestCodexDurableRootRecoversWhenBoundAccountIsAlreadyBenched(t *testing.T) {
	const (
		accountAToken = "access-bound-health-a"
		accountBToken = "access-bound-health-b"
	)
	var calls atomic.Int32
	var violationMu sync.Mutex
	var violation string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			if auth != accountAToken || bytes.Contains(raw, []byte(`"previous_response_id"`)) {
				violationMu.Lock()
				violation = fmt.Sprintf("root auth=%q body=%s", auth, raw)
				violationMu.Unlock()
			}
			_, _ = io.WriteString(w, `{"id":"resp-bound-health-root","object":"response","model":"gpt","status":"completed","output":[]}`)
		case 2:
			if auth != accountBToken || bytes.Contains(raw, []byte(`"previous_response_id"`)) || !bytes.Contains(raw, []byte("stable-bound-health-context")) {
				violationMu.Lock()
				violation = fmt.Sprintf("recovery auth=%q body=%s", auth, raw)
				violationMu.Unlock()
			}
			_, _ = io.WriteString(w, `{"id":"resp-bound-health-recovered","object":"response","model":"gpt","status":"completed","output":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"unexpected upstream call"}`)
		}
	})
	enableCodexSessionMappingForTest(h)
	accountA := h.importAccount(t, "bound-health-a", "upstream-bound-health-a", accountAToken)

	post := func(body string) (*http.Response, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", "bound-health-root")
		req.Header.Set("Thread-Id", "bound-health-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, result
	}

	root, rootBody := post(`{"model":"gpt","input":"stable-bound-health-context"}`)
	if root.StatusCode != http.StatusOK || !bytes.Contains(rootBody, []byte("resp-bound-health-root")) {
		t.Fatalf("root status=%d body=%s", root.StatusCode, rootBody)
	}
	h.importAccount(t, "bound-health-b", "upstream-bound-health-b", accountBToken)
	if err := h.store.BenchBindingForRecheck(context.Background(), accountA, storage.Now()+300); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	recovered, recoveredBody := post(`{"model":"gpt","previous_response_id":"resp-bound-health-root","input":"continue"}`)
	if recovered.StatusCode != http.StatusOK || recovered.Header.Get("X-MiCliProxy-Context-Status") != "rebuilt" ||
		!bytes.Contains(recoveredBody, []byte("resp-bound-health-recovered")) || calls.Load() != 2 {
		t.Fatalf("recovery status=%d context=%q calls=%d body=%s", recovered.StatusCode,
			recovered.Header.Get("X-MiCliProxy-Context-Status"), calls.Load(), recoveredBody)
	}
	violationMu.Lock()
	gotViolation := violation
	violationMu.Unlock()
	if gotViolation != "" {
		t.Fatal(gotViolation)
	}
}

func TestCodexSessionMappingGoalResumeStartsFreshRootAfterContextLoss(t *testing.T) {
	type capturedCall struct {
		body         string
		session      string
		thread       string
		turnState    string
		turnMetadata string
	}
	var mu sync.Mutex
	var calls []capturedCall
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, capturedCall{
			body:         string(raw),
			session:      r.Header.Get("Session-Id"),
			thread:       r.Header.Get("Thread-Id"),
			turnState:    r.Header.Get("X-Codex-Turn-State"),
			turnMetadata: r.Header.Get("X-Codex-Turn-Metadata"),
		})
		call := len(calls)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "stale-goal-turn-state")
			_, _ = w.Write([]byte(`{"id":"resp-goal-resume-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"previous_response_not_found","message":"Previous response resp-goal-resume-origin was not found."}}`))
		case 3:
			w.Header().Set("X-Codex-Turn-State", "fresh-goal-turn-state")
			_, _ = w.Write([]byte(`{"id":"resp-goal-resume-fresh","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 4:
			_, _ = w.Write([]byte(`{"id":"resp-goal-resume-followup","object":"response","model":"gpt","status":"completed","output":[]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "goal-resume", "upstream-goal-resume", "access-goal-resume")

	post := func(body, thread, session, state, metadata string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", thread)
		if session != "" {
			req.Header.Set("Session-Id", session)
		}
		if state != "" {
			req.Header.Set("X-Codex-Turn-State", state)
		}
		if metadata != "" {
			req.Header.Set("X-Codex-Turn-Metadata", metadata)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Clone(), string(result)
	}

	if status, _, body := post(`{"model":"gpt","input":"start"}`, "goal-resume-root", "goal-resume-root", "", ""); status != http.StatusOK || !strings.Contains(body, "resp-goal-resume-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	resumeMetadata := `{"thread_id":"goal-resume-root","session_id":"goal-resume-root","turn_state":"stale-goal-turn-state","request_kind":"turn"}`
	resumeBody := `{"model":"gpt","previous_response_id":"resp-goal-resume-origin","turn_state":"stale-goal-turn-state","session_id":"goal-resume-root","client_metadata":{"x-codex-turn-state":"stale-goal-turn-state","session_id":"goal-resume-root","x-codex-turn-metadata":"{\"thread_id\":\"goal-resume-root\",\"session_id\":\"goal-resume-root\",\"turn_state\":\"stale-goal-turn-state\",\"request_kind\":\"turn\"}"},"input":[{"role":"user","content":[{"type":"input_text","text":"resume"}],"exact":900719925474099312345}]}`
	status, headers, body := post(resumeBody, "goal-resume-root", "goal-resume-root", "stale-goal-turn-state", resumeMetadata)
	if status != http.StatusOK || !strings.Contains(body, "resp-goal-resume-fresh") || headers.Get("X-MiCliProxy-Context-Status") != "rebuilt" {
		t.Fatalf("goal resume fresh root status=%d headers=%v body=%s", status, headers, body)
	}
	if status, _, body := post(`{"model":"gpt","previous_response_id":"resp-goal-resume-fresh","input":"follow up"}`, "goal-resume-root", "goal-resume-root", "fresh-goal-turn-state", ""); status != http.StatusOK || !strings.Contains(body, "resp-goal-resume-followup") {
		t.Fatalf("fresh root continuation status=%d body=%s", status, body)
	}

	mu.Lock()
	got := append([]capturedCall(nil), calls...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("upstream calls=%d", len(got))
	}
	if strings.Contains(got[2].body, `"previous_response_id":"resp-goal-resume-origin"`) || got[2].turnState != "" || strings.Contains(got[2].turnMetadata, "stale-goal-turn-state") {
		t.Fatalf("fresh root retained stale upstream state: %+v", got[2])
	}
	if !strings.Contains(got[2].body, "900719925474099312345") {
		t.Fatalf("fresh root changed untouched input number: %s", got[2].body)
	}
	if got[0].session == "" || got[2].session == "" || got[0].session == got[2].session || got[0].thread == got[2].thread {
		t.Fatalf("fresh root did not allocate a new native identity old=%+v new=%+v", got[0], got[2])
	}
	if !strings.Contains(got[3].body, `"previous_response_id":"resp-goal-resume-fresh"`) {
		t.Fatalf("follow-up did not bind to fresh root: %s", got[3].body)
	}
}

func TestCodexSessionMappingRetiresOnSoft200PreviousResponseNotFound(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-soft-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			// The backend sometimes returns this error envelope with HTTP 200. The
			// semantic status is nested in error.message and must still rotate CPA.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp-soft-retired-origin' not found.\",\"param\":\"previous_response_id\"},\"status\":400}"},"status":400,"type":"error"}`))
		case 3:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-soft-recovered","object":"response","model":"gpt","status":"completed","output":[]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "soft-retire", "upstream-soft-retire", "access-soft-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "soft-retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-soft-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","previous_response_id":"resp-soft-retired-origin","input":"resume"}`); status != http.StatusOK || !strings.Contains(body, "resp-soft-recovered") || calls.Load() != 3 {
		t.Fatalf("soft context recovery status=%d calls=%d body=%s", status, calls.Load(), body)
	}
}

func TestCodexSessionMappingRetiresAfterNativeStreamContextLoss(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-stream-retired-origin","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"status\":400,\"response\":{\"id\":\"resp-stream-retired-origin\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"type\":\"previous_response_not_found\",\"message\":\"Previous response was not found.\"}}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream-context-recovered\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\r\n\r\n")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "stream-retire", "upstream-stream-retire", "access-stream-retire")

	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "stream-retire-root")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(result)
	}
	if status, body := post(`{"model":"gpt","input":"start"}`); status != http.StatusOK || !strings.Contains(body, "resp-stream-retired-origin") {
		t.Fatalf("initial status=%d body=%s", status, body)
	}
	if status, body := post(`{"model":"gpt","stream":true,"previous_response_id":"resp-stream-retired-origin","input":"resume"}`); status != http.StatusOK || !strings.Contains(body, "resp-stream-context-recovered") || strings.Contains(body, "previous_response_not_found") || strings.Contains(body, "codex_native_continue_failed") || calls.Load() != 3 {
		t.Fatalf("native stream context recovery status=%d calls=%d body=%s", status, calls.Load(), body)
	}
}

func TestCodexSessionMappingEOFUsesOneNativeContinue(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var sessions []string
	var turns []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		sessions = append(sessions, r.Header.Get("Session-Id")+"/"+r.Header.Get("Thread-Id"))
		turns = append(turns, turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-eof-source\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-eof-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-eof-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "eof", "upstream-eof", "access-eof")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"original turn must not be replayed"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("resp-eof-final")) || bytes.Contains(body, []byte("codex_native_continue_failed")) {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, body)
	}
	if got := bytes.Count(body, []byte("data: [DONE]")); got != 1 {
		t.Fatalf("truncated stream DONE leaked before native continuation: count=%d body=%s", got, body)
	}
	mu.Lock()
	gotBodies := append([]string(nil), bodies...)
	gotSessions := append([]string(nil), sessions...)
	gotTurns := append([]string(nil), turns...)
	mu.Unlock()
	if len(gotBodies) != 2 || gotSessions[0] == "" || gotSessions[0] != gotSessions[1] || gotTurns[0] == "" || gotTurns[0] == gotTurns[1] {
		t.Fatalf("native continue identity bodies=%d sessions=%v turns=%v", len(gotBodies), gotSessions, gotTurns)
	}
	var continuation map[string]interface{}
	if err := json.Unmarshal([]byte(gotBodies[1]), &continuation); err != nil {
		t.Fatal(err)
	}
	if continuation["previous_response_id"] != "resp-eof-source" || strings.Contains(gotBodies[1], "original turn must not be replayed") {
		t.Fatalf("continuation must use only native upstream state: %s", gotBodies[1])
	}
	input, _ := continuation["input"].([]interface{})
	if len(input) != 1 || !strings.Contains(gotBodies[1], h.app.autoContinueText(context.Background())) {
		t.Fatalf("continuation input=%v body=%s", input, gotBodies[1])
	}
	rows, err := h.store.FindCodexSessionAlias(context.Background(), "unauthenticated", storage.CodexSessionAlias{Type: "response", Value: "resp-eof-final"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("continuation terminal not mapped: rows=%+v err=%v", rows, err)
	}
}

func TestCodexSessionMappingQuietLongPollUsesKeepalive(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatal("a live upstream long poll must not trigger native continuation")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-quiet-native\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Longer than the configured heartbeat interval. Strict CPA must start the
		// relay immediately instead of holding response.created in a retry probe.
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-quiet-native\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.StreamKeepAliveSeconds = 1
	h.importAccount(t, "quiet-native", "upstream-quiet-native", "access-quiet-native")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || !bytes.Contains(body, []byte("response.in_progress")) || !bytes.Contains(body, []byte("response.completed")) || bytes.Contains(body, []byte("codex_native_continue_failed")) {
		t.Fatalf("quiet strict stream status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func TestCodexSessionMappingEOFPendingClientToolDoesNotContinue(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatalf("pending client tool call must not trigger native continue")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-pending-tool\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call-pending\",\"name\":\"patch\",\"input\":\"{}\"}}\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "pending", "upstream-pending", "access-pending")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || !bytes.Contains(body, []byte(`"code":"server_error"`)) || !bytes.Contains(body, []byte(publicRetryMessage)) || bytes.Count(body, []byte("data: [DONE]")) != 1 {
		t.Fatalf("pending tool EOF status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func TestCodexSessionMappingCreatesDistinctChildAndForkTrees(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	root, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: "unauthenticated",
		Binding: storage.CodexSessionBinding{
			ID: "root-binding", TreeID: "root-tree", AccountID: "account-root", EgressID: storage.DefaultDirectEgressID, State: "active",
			InstallationID:  "install-root-device",
			DeviceOSHint:    "Mac OS",
			DeviceOSHintSet: true,
			RootSessionID:   "019f0000-0000-7000-8000-000000000101",
			ThreadID:        "019f0000-0000-7000-8000-000000000101",
		},
		Aliases: []storage.CodexSessionAlias{{Type: "root", Value: "client-root"}, {Type: "session", Value: "client-root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := scheduler.Lease{Account: storage.Account{ID: "account-root"}, Egress: storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}}

	childRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child"}`))
	childMapping, err := h.app.resolveCodexSessionMapping(ctx, childRequest, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := childMapping.instructionTreeID(); got != root.TreeID {
		t.Fatalf("child instruction tree=%q, want parent tree %q", got, root.TreeID)
	}
	child, err := childMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if child.InstallationID != root.InstallationID || child.DeviceOSHint != root.DeviceOSHint || child.SessionID != root.RootSessionID || child.ThreadID == child.SessionID || child.ParentThreadID != root.ThreadID || !strings.HasSuffix(child.WindowID(), ":0") {
		t.Fatalf("child snapshot=%+v root=%+v", child, root)
	}
	if err := h.app.commitCodexSessionMapping(ctx, childMapping, lease, lease.Egress, "resp-child", "", false); err != nil {
		t.Fatal(err)
	}
	repeatChild, err := h.app.resolveCodexSessionMapping(ctx, childRequest, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child again"}`), downstreamPolicy{})
	if err != nil || repeatChild.binding == nil || repeatChild.binding.ThreadID != child.ThreadID {
		t.Fatalf("self-contained child turn must reuse its branch binding=%+v err=%v", repeatChild.binding, err)
	}
	childResume, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","previous_response_id":"resp-child","input":"next"}`))
	resumedChild, err := h.app.resolveCodexSessionMapping(ctx, childResume, []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","previous_response_id":"resp-child","input":"next"}`), downstreamPolicy{})
	if err != nil || resumedChild.binding == nil || resumedChild.binding.ThreadID != child.ThreadID {
		t.Fatalf("child stateful resume binding=%+v err=%v", resumedChild.binding, err)
	}

	// Some clients omit session_id on a child follow-up and identify it solely by
	// thread_id + parent_thread_id. That must still become a child branch rather
	// than a second root session.
	noSessionBody := []byte(`{"model":"gpt","thread_id":"client-child-no-session","parent_thread_id":"client-root","input":"child"}`)
	noSessionRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(noSessionBody))
	noSessionMapping, err := h.app.resolveCodexSessionMapping(ctx, noSessionRequest, noSessionBody, downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	noSessionChild, err := noSessionMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil || noSessionChild.InstallationID != root.InstallationID || noSessionChild.DeviceOSHint != root.DeviceOSHint || noSessionChild.SessionID != root.RootSessionID || noSessionChild.ThreadID == noSessionChild.SessionID || noSessionChild.ParentThreadID != root.ThreadID {
		t.Fatalf("child without session snapshot=%+v err=%v", noSessionChild, err)
	}
	if err := h.app.commitCodexSessionMapping(ctx, noSessionMapping, lease, lease.Egress, "resp-child-no-session", "", false); err != nil {
		t.Fatal(err)
	}
	repeatNoSession, err := h.app.resolveCodexSessionMapping(ctx, noSessionRequest, noSessionBody, downstreamPolicy{})
	if err != nil || repeatNoSession.binding == nil || repeatNoSession.binding.ThreadID != noSessionChild.ThreadID {
		t.Fatalf("child without session did not persist branch binding=%+v err=%v", repeatNoSession.binding, err)
	}

	forkRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork"}`))
	forkMapping, err := h.app.resolveCodexSessionMapping(ctx, forkRequest, []byte(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := forkMapping.instructionTreeID(); got != "" {
		t.Fatalf("fork must create a fresh instruction tree, got inherited %q", got)
	}
	fork, err := forkMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if fork.SessionID == root.RootSessionID || fork.SessionID != fork.ThreadID || fork.ParentThreadID != "" || fork.ForkedFromThreadID != child.ThreadID || !strings.HasSuffix(fork.WindowID(), ":0") {
		t.Fatalf("fork snapshot=%+v child=%+v root=%+v", fork, child, root)
	}
	if err := h.app.commitCodexSessionMapping(ctx, forkMapping, lease, lease.Egress, "resp-fork", "", false); err != nil {
		t.Fatal(err)
	}
	// Codex retains the original fork marker on later self-contained turns. That
	// marker identifies ancestry, not a request to create a fresh fork every time;
	// the second turn must reuse the durable fork root and commit another response
	// alias without colliding with its own root alias.
	repeatForkBody := []byte(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork follow-up"}`)
	repeatForkRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(repeatForkBody))
	repeatForkMapping, err := h.app.resolveCodexSessionMapping(ctx, repeatForkRequest, repeatForkBody, downstreamPolicy{})
	if err != nil || repeatForkMapping.binding == nil || repeatForkMapping.binding.ID != forkMapping.binding.ID {
		t.Fatalf("persistent fork marker did not reuse its root binding=%+v err=%v", repeatForkMapping.binding, err)
	}
	repeatFork, err := repeatForkMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil || repeatFork.SessionID != fork.SessionID || repeatFork.ThreadID != fork.ThreadID || repeatFork.ForkedFromThreadID != fork.ForkedFromThreadID {
		t.Fatalf("persistent fork marker changed upstream identity=%+v err=%v", repeatFork, err)
	}
	if err := h.app.commitCodexSessionMapping(ctx, repeatForkMapping, lease, lease.Egress, "resp-fork-follow-up", "", false); err != nil {
		t.Fatalf("persistent fork marker collided during terminal commit: %v", err)
	}
	if _, err := h.store.RetireCodexSessionTree(ctx, root.ID, root.Epoch); err != nil {
		t.Fatal(err)
	}
	newRootRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", strings.NewReader(`{"model":"gpt","thread_id":"client-root","session_id":"client-root","input":"fresh root"}`))
	newRootMapping, err := h.app.resolveCodexSessionMapping(ctx, newRootRequest, []byte(`{"model":"gpt","thread_id":"client-root","session_id":"client-root","input":"fresh root"}`), downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := newRootMapping.instructionTreeID(); got != "" {
		t.Fatalf("new root after retirement inherited old instruction tree %q", got)
	}
}

func TestCodexTerminalCommitPreservesResponseAliasAcrossLegacyHierarchyConflict(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	const downstreamRoot = "client-root-with-legacy-owner"
	body := []byte(`{"model":"gpt","session_id":"` + downstreamRoot + `","thread_id":"` + downstreamRoot + `","input":"fresh turn"}`)
	request, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := h.app.resolveCodexSessionMapping(ctx, request, body, downstreamPolicy{})
	if err != nil || mapping.binding != nil {
		t.Fatalf("fresh mapping=%+v err=%v", mapping.binding, err)
	}
	lease := scheduler.Lease{
		Account: storage.Account{ID: "account-new"},
		Egress:  storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"},
	}
	snapshot, err := mapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil || snapshot == nil {
		t.Fatalf("identity snapshot=%+v err=%v", snapshot, err)
	}

	// Simulate an older concurrently active worker winning the hierarchy alias
	// after this request resolved but before its successful terminal was persisted.
	competing, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: "unauthenticated",
		Binding: storage.CodexSessionBinding{
			ID: "legacy-winner", TreeID: "legacy-tree", AccountID: "account-legacy", EgressID: storage.DefaultDirectEgressID,
			State: "active", RootSessionID: "legacy-internal-root", ThreadID: "legacy-internal-root",
		},
		Aliases: []storage.CodexSessionAlias{
			{Type: "root", Value: downstreamRoot},
			{Type: "session", Value: downstreamRoot},
			{Type: "response", Value: "resp-legacy-winner"},
		},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.app.commitCodexSessionMapping(cancelled, mapping, lease, lease.Egress, "resp-new-terminal", "state-new-terminal", false); err != nil {
		t.Fatalf("terminal commit must retain its unique state aliases: %v", err)
	}
	resolved, err := h.store.ResolveCodexSessionAliases(ctx, "unauthenticated", []storage.CodexSessionAlias{
		{Type: "response", Value: "resp-new-terminal"},
		{Type: "turn_state", Value: "state-new-terminal"},
	})
	if err != nil || resolved.ID == competing.ID || resolved.AccountID != lease.Account.ID {
		t.Fatalf("new terminal alias resolved=%+v err=%v competing=%+v", resolved, err, competing)
	}
	root, err := h.store.ResolveCodexSessionAliases(ctx, "unauthenticated", []storage.CodexSessionAlias{{Type: "root", Value: downstreamRoot}})
	if err != nil || root.ID != competing.ID {
		t.Fatalf("legacy hierarchy owner changed root=%+v err=%v", root, err)
	}
	var audits int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='codex_session_hierarchy_alias_conflict' AND reason='legacy_active_owner'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("hierarchy conflict audit count=%d err=%v", audits, err)
	}
}

func turnIDFromHeader(raw string) string {
	var metadata map[string]interface{}
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return ""
	}
	value, _ := metadata["turn_id"].(string)
	return value
}

// TestCodexSafetyRotationFreshRequestDetachesRetiredChain verifies the single
// post-rotation turn strips every root-state pointer (previous_response_id,
// session_id, thread/fork headers, embedded turn metadata) so upstream sees a
// brand-new session even though the downstream mapping is preserved.
func TestCodexSafetyRotationFreshRequestDetachesRetiredChain(t *testing.T) {
	body := []byte(`{
		"model":"gpt",
		"previous_response_id":"resp-safety-1",
		"session_id":"retired-session",
		"turn_state":{"turn_id":"t1"},
		"client_metadata":{"x-codex-turn-state":"opaque"},
		"input":"resume"
	}`)
	header := http.Header{}
	header.Set("Session-Id", "retired-session")
	header.Set("X-Codex-Parent-Thread-Id", "retired-thread")
	header.Set("X-Codex-Turn-State", "opaque-header")
	header.Set("X-Codex-Turn-Metadata", `{"turn_id":"t1","session_id":"retired-session","x-codex-turn-state":"opaque"}`)

	out, outHeader, ok := codexSafetyRotationFreshRequest(body, header)
	if !ok {
		t.Fatal("detach strip declined a stateful request")
	}
	if gjson.GetBytes(out, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id survived detach: %s", out)
	}
	if gjson.GetBytes(out, "session_id").Exists() || gjson.GetBytes(out, "turn_state").Exists() {
		t.Fatalf("retired session/turn state survived detach: %s", out)
	}
	if gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("embedded turn metadata survived detach: %s", out)
	}
	if outHeader.Get("Session-Id") != "" || outHeader.Get("X-Codex-Parent-Thread-Id") != "" || outHeader.Get("X-Codex-Turn-State") != "" {
		t.Fatalf("retired identity headers survived detach: %v", outHeader)
	}
	if strings.Contains(outHeader.Get("X-Codex-Turn-Metadata"), "retired-session") || strings.Contains(outHeader.Get("X-Codex-Turn-Metadata"), "opaque") {
		t.Fatalf("chain-pointer keys survived header turn metadata: %v", outHeader.Get("X-Codex-Turn-Metadata"))
	}
	if !strings.Contains(string(out), `"input":"resume"`) {
		t.Fatalf("payload input was lost during detach: %s", out)
	}
}

func TestCodexResponseSafetyBufferedDetectsControlField(t *testing.T) {
	if !codexResponseSafetyBuffered([]byte(`{"id":"r","status":"completed","safety_buffering":{"detail":"mod"}}`)) {
		t.Fatal("safety_buffering control field not detected")
	}
	if codexResponseSafetyBuffered([]byte(`{"id":"r","status":"completed","output":[]}`)) {
		t.Fatal("plain completed response misdetected as safety-buffered")
	}
}

// TestCodexSessionMappingRotatesUpstreamSessionOnSafetyBuffering is the end-to-end
// contract behind the user-group toggle: when upstream withholds content via the
// Responses safety_buffering field, the committed binding rotates to a fresh
// upstream session id while the downstream Thread-Id aliases keep resolving to the
// same binding; the immediately following turn detaches from the retired chain and
// then clears the marker.
func TestCodexSessionMappingRotatesUpstreamSessionOnSafetyBuffering(t *testing.T) {
	runSafetyRotationFlow(t, true)
}

// TestCodexSessionMappingKeepsUpstreamSessionWithoutSafetyToggle is the negative
// control: with the user-group toggle off, a safety_buffering terminal commits the
// binding normally and the upstream session id is never rotated.
func TestCodexSessionMappingKeepsUpstreamSessionWithoutSafetyToggle(t *testing.T) {
	runSafetyRotationFlow(t, false)
}

func runSafetyRotationFlow(t *testing.T, toggleOn bool) {
	t.Helper()
	var mu sync.Mutex
	var upstreamSessions []string
	var previousResponseID string
	var calls atomic.Int32

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		mu.Lock()
		upstreamSessions = append(upstreamSessions, r.Header.Get("Session-Id"))
		if call == 2 {
			previousResponseID = string(gjson.ParseBytes(raw).Get("previous_response_id").String())
		}
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-safety-1\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-safety-1\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1},\"safety_buffering\":{\"detail\":\"mod\"}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-safety-2","object":"response","model":"gpt","status":"completed","output":[]}`))
	})

	enableCodexSessionMappingForTest(h)
	if toggleOn {
		h.app.cfg.SafetySessionRotationGroups = map[string]bool{"ug_safety_rotation": true}
	}

	const plain = "cap_safety_rotation"
	const userGroupID = "ug_safety_rotation"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: userGroupID, Name: "Safety Rotation",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(plain), Label: "safety-rotation", UserGroupID: userGroupID, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.importAccount(t, "safety", "upstream-safety", "access-safety")

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+plain)
		req.Header.Set("Thread-Id", "safety-thread-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	namespace := "key:" + hashAPIKey(plain)

	// First turn: upstream withholds content via safety_buffering. With the toggle
	// on, the binding must rotate to a fresh upstream session id while downstream
	// aliases stay intact. Without it, the binding keeps the original session id.
	firstStatus, firstBody := post(t, `{"model":"gpt","stream":true,"input":"start"}`)
	if firstStatus != http.StatusOK || !strings.Contains(firstBody, "resp-safety-1") {
		t.Fatalf("first turn status=%d body=%s", firstStatus, firstBody)
	}

	rows, err := h.store.FindCodexSessionAlias(t.Context(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-safety-1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("binding after safety terminal rows=%d err=%v", len(rows), err)
	}
	afterTerminal := rows[0]
	mu.Lock()
	firstUpstreamSession := ""
	if len(upstreamSessions) > 0 {
		firstUpstreamSession = upstreamSessions[0]
	}
	mu.Unlock()
	if firstUpstreamSession == "" {
		t.Fatal("first upstream request carried no session id")
	}
	if toggleOn {
		if afterTerminal.SafetyRotatedAt == 0 {
			t.Fatalf("rotation marker was not set: %+v", afterTerminal)
		}
		if afterTerminal.RootSessionID == firstUpstreamSession {
			t.Fatalf("binding did not rotate: session=%s", afterTerminal.RootSessionID)
		}
	} else {
		if afterTerminal.SafetyRotatedAt != 0 {
			t.Fatalf("rotation marker set without the toggle: %+v", afterTerminal)
		}
		if afterTerminal.RootSessionID != firstUpstreamSession {
			t.Fatalf("binding rotated without the toggle: %s != %s", afterTerminal.RootSessionID, firstUpstreamSession)
		}
	}

	// Second turn: resolves the same binding (downstream aliases unchanged). With the
	// toggle on it detaches from the retired chain — no previous_response_id — and
	// clears the rotation marker. Without it the resume passes previous_response_id
	// through untouched.
	secondStatus, secondBody := post(t, `{"model":"gpt","previous_response_id":"resp-safety-1","input":"resume"}`)
	if secondStatus != http.StatusOK || !strings.Contains(secondBody, "resp-safety-2") {
		t.Fatalf("second turn status=%d body=%s", secondStatus, secondBody)
	}
	mu.Lock()
	secondUpstreamSession := ""
	if len(upstreamSessions) > 1 {
		secondUpstreamSession = upstreamSessions[1]
	}
	mu.Unlock()
	if secondUpstreamSession == "" {
		t.Fatal("second upstream request carried no session id")
	}
	if toggleOn {
		if secondUpstreamSession != afterTerminal.RootSessionID {
			t.Fatalf("second upstream session=%s != rotated binding session=%s", secondUpstreamSession, afterTerminal.RootSessionID)
		}
		if secondUpstreamSession == firstUpstreamSession {
			t.Fatalf("upstream session did not change across the rotation: %s", secondUpstreamSession)
		}
		if previousResponseID != "" {
			t.Fatalf("post-rotation turn leaked previous_response_id upstream: %q", previousResponseID)
		}
	} else {
		if secondUpstreamSession != firstUpstreamSession {
			t.Fatalf("upstream session changed without the toggle: %s != %s", secondUpstreamSession, firstUpstreamSession)
		}
		if previousResponseID != "resp-safety-1" {
			t.Fatalf("resume turn lost previous_response_id without the toggle: %q", previousResponseID)
		}
	}

	afterSecond, err := h.store.ResolveCodexSessionAliases(t.Context(), namespace, []storage.CodexSessionAlias{{Type: "root", Value: "safety-thread-1"}})
	if err != nil {
		t.Fatalf("downstream alias lost after the flow: %v", err)
	}
	if afterSecond.ID != afterTerminal.ID || afterSecond.RootSessionID != afterTerminal.RootSessionID {
		t.Fatalf("downstream mapping moved: binding=%+v original=%+v", afterSecond, afterTerminal)
	}
	if toggleOn && afterSecond.SafetyRotatedAt != 0 {
		t.Fatalf("rotation marker not cleared after the detach turn: %+v", afterSecond)
	}
}

// TestCodexSessionMappingSafetyRotationPreservesChildBranch pins the fix for
// "session corrupt" continuations after a safety-buffered child-branch turn. A
// child branch (ThreadID != RootSessionID, ParentThreadID set) cannot rotate its
// RootSessionID: the upstream thread tree would then name a thread id that belongs
// to a retired session, and the next continuation surfaces as a corrupt session.
// The rotation must instead open a fresh child thread under the same parent while
// the root session id stays untouched. A fork root (ThreadID == RootSessionID)
// still rotates root and thread together and keeps its ancestry marker.
func TestCodexSessionMappingSafetyRotationPreservesChildBranch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()

	root, err := h.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace: "unauthenticated",
		Binding: storage.CodexSessionBinding{
			ID: "root-binding", TreeID: "root-tree", AccountID: "account-root", EgressID: storage.DefaultDirectEgressID, State: "active",
			InstallationID:  "install-root-device",
			DeviceOSHint:    "Mac OS",
			DeviceOSHintSet: true,
			RootSessionID:   "019f0000-0000-7000-8000-000000000101",
			ThreadID:        "019f0000-0000-7000-8000-000000000101",
		},
		Aliases: []storage.CodexSessionAlias{{Type: "root", Value: "client-root"}, {Type: "session", Value: "client-root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := scheduler.Lease{Account: storage.Account{ID: "account-root"}, Egress: storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}}

	childBody := []byte(`{"model":"gpt","thread_id":"client-child","session_id":"client-root","parent_thread_id":"client-root","input":"child"}`)
	childRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(childBody))

	childMapping, err := h.app.resolveCodexSessionMapping(ctx, childRequest, childBody, downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	childSnapshot, err := childMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	preThreadID := childSnapshot.ThreadID
	if childSnapshot.SessionID != root.RootSessionID || preThreadID == childSnapshot.SessionID || childSnapshot.ParentThreadID != root.ThreadID {
		t.Fatalf("child snapshot=%+v root=%+v", childSnapshot, root)
	}

	// First child turn carries safety_buffering under the user-group toggle.
	childMapping.rotateUpstreamSessionOnSafety = true
	childMapping.noteSafetyBuffering()
	if err := h.app.commitCodexSessionMapping(ctx, childMapping, lease, lease.Egress, "resp-child-safety", "", false); err != nil {
		t.Fatal(err)
	}
	committed := childMapping.binding
	if committed == nil {
		t.Fatal("child safety turn did not commit a binding")
	}
	if committed.RootSessionID != root.RootSessionID {
		t.Fatalf("child rotation changed the root session id: %s != %s", committed.RootSessionID, root.RootSessionID)
	}
	if committed.ParentThreadID != root.ThreadID {
		t.Fatalf("child rotation changed the parent thread id: %s != %s", committed.ParentThreadID, root.ThreadID)
	}
	if committed.ThreadID == preThreadID || committed.ThreadID == committed.RootSessionID {
		t.Fatalf("child rotation must open a fresh child thread: pre=%s committed=%+v", preThreadID, committed)
	}
	if committed.SafetyRotatedAt == 0 {
		t.Fatalf("child rotation marker not set: %+v", committed)
	}

	// The following turn detaches (marker cleared) and must reuse the rotated branch.
	resumeMapping, err := h.app.resolveCodexSessionMapping(ctx, childRequest, childBody, downstreamPolicy{})
	if err != nil || resumeMapping.binding == nil || resumeMapping.binding.ThreadID != committed.ThreadID {
		t.Fatalf("post-rotation child resume binding=%+v err=%v", resumeMapping.binding, err)
	}
	resumeSnapshot, err := resumeMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if resumeSnapshot.SessionID != root.RootSessionID || resumeSnapshot.ThreadID != committed.ThreadID {
		t.Fatalf("post-rotation upstream identity does not follow the rotated branch: %+v", resumeSnapshot)
	}
	resumeMapping.rotateUpstreamSessionOnSafety = true
	resumeMapping.markSafetyDetached()
	if err := h.app.commitCodexSessionMapping(ctx, resumeMapping, lease, lease.Egress, "resp-child-safety-2", "", false); err != nil {
		t.Fatal(err)
	}
	afterDetach := resumeMapping.binding
	if afterDetach == nil || afterDetach.SafetyRotatedAt != 0 {
		t.Fatalf("detach turn did not clear the rotation marker: %+v", afterDetach)
	}
	if afterDetach.ThreadID != committed.ThreadID || afterDetach.RootSessionID != root.RootSessionID {
		t.Fatalf("detach turn re-rotated the branch: %+v", afterDetach)
	}

	// Control: a fork root (ThreadID == RootSessionID) still rotates root+thread
	// together and keeps its ancestry marker.
	forkBody := []byte(`{"model":"gpt","thread_id":"fork-client-root","forked_from_thread_id":"client-child","input":"fork"}`)
	forkRequest, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", bytes.NewReader(forkBody))
	forkMapping, err := h.app.resolveCodexSessionMapping(ctx, forkRequest, forkBody, downstreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	forkSnapshot, err := forkMapping.identitySnapshot(h.app.identitySecret(), lease, "Linux")
	if err != nil {
		t.Fatal(err)
	}
	if forkSnapshot.SessionID != forkSnapshot.ThreadID {
		t.Fatalf("fork root precondition violated: %+v", forkSnapshot)
	}
	forkMapping.rotateUpstreamSessionOnSafety = true
	forkMapping.noteSafetyBuffering()
	if err := h.app.commitCodexSessionMapping(ctx, forkMapping, lease, lease.Egress, "resp-fork-safety", "", false); err != nil {
		t.Fatal(err)
	}
	forkCommitted := forkMapping.binding
	if forkCommitted == nil || forkCommitted.SafetyRotatedAt == 0 {
		t.Fatalf("fork safety turn did not rotate: %+v", forkCommitted)
	}
	if forkCommitted.RootSessionID == forkSnapshot.SessionID || forkCommitted.ThreadID != forkCommitted.RootSessionID {
		t.Fatalf("fork root rotation inconsistent: pre=%+v committed=%+v", forkSnapshot, forkCommitted)
	}
	if forkCommitted.ForkedFromThreadID != forkSnapshot.ForkedFromThreadID {
		t.Fatalf("fork rotation dropped the ancestry marker: pre=%+v committed=%+v", forkSnapshot, forkCommitted)
	}
}
