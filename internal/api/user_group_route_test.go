package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func createRouteTestGroup(t *testing.T, h *testHarness, id string, targets []storage.TargetRef, rules []storage.ModelRoutingRule) {
	t.Helper()
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID:           id,
		Name:         id,
		Targets:      targets,
		ModelRouting: rules,
	}); err != nil {
		t.Fatalf("create user group %s: %v", id, err)
	}
}

func TestUserGroupAttemptWriterSpoolsAndCommitsWithoutChangingBody(t *testing.T) {
	dir := t.TempDir()
	budget := bodysource.NewBudget(64, 2<<20)
	recorder := httptest.NewRecorder()
	w := newUserGroupAttemptWriter(context.Background(), recorder, false, false, bodysource.CaptureOptions{
		MaxBytes: 2 << 20, MemoryThreshold: 64, TempDir: dir, Budget: budget,
	})
	payload := []byte(`{"id":"resp_spooled","status":"completed","output_text":"` + strings.Repeat("large-output-", 64<<10) + `"}`)
	w.WriteHeader(http.StatusOK)
	for offset := 0; offset < len(payload); {
		end := offset + 4093
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := w.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if !w.body.Spilled() {
		t.Fatal("large candidate response stayed in memory")
	}
	if got := w.CompletedResponseID(); got != "resp_spooled" {
		t.Fatalf("completed response id=%q", got)
	}
	w.Commit()
	if !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("committed body changed: got=%d want=%d", recorder.Body.Len(), len(payload))
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("candidate response budget leaked: %+v", snapshot)
	}
}

func TestUserGroupAttemptWriterFindsRetryMarkerAcrossSpoolChunks(t *testing.T) {
	w := newUserGroupAttemptWriter(context.Background(), httptest.NewRecorder(), false, false, bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 1, TempDir: t.TempDir(), Budget: bodysource.NewBudget(1, 1<<20),
	})
	w.WriteHeader(http.StatusBadRequest)
	for _, chunk := range []string{strings.Repeat("x", 32<<10) + "CAPABILITY_", "UNAVAILABLE"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !w.RetryableFailure() {
		t.Fatal("split retry marker was not detected")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchUserGroupRouteCandidatesFallsBackBeforeCommitAcrossEntrypoints(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "route-secondary"}); err != nil {
		t.Fatal(err)
	}
	targets := []storage.TargetRef{
		{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"},
		{Kind: storage.TargetKindAccountPoolGroup, ID: "route-secondary"},
	}

	tests := []struct {
		name       string
		path       string
		body       string
		headerName string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"gpt","messages":[]}`, headerName: "Thread-Id"},
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt","input":"hello"}`, headerName: "Thread-Id"},
		{name: "messages", path: "/v1/messages", body: `{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`, headerName: "X-Claude-Code-Session-Id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			groupID := "ug_route_" + test.name
			createRouteTestGroup(t, h, groupID, targets, nil)
			raw := []byte(test.body)
			req := httptest.NewRequest(http.MethodPost, test.path, nil)
			req.Header.Set(test.headerName, "session-"+test.name)
			pol := downstreamPolicy{UserGroupID: groupID}
			plan, err := resolveUserGroupRouteCandidates(req.Context(), h.store, pol, req, raw)
			if err != nil || len(plan.Candidates) != 2 {
				t.Fatalf("route plan = %+v, err=%v", plan, err)
			}

			recorder := httptest.NewRecorder()
			attempts := make([]storage.TargetRef, 0, 2)
			handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, pol, func(w http.ResponseWriter, candidate *http.Request) {
				target, ok := userGroupRouteOverride(candidate.Context())
				if !ok {
					t.Fatal("candidate request missing route override")
				}
				attempts = append(attempts, target)
				if len(attempts) == 1 {
					w.Header().Set("X-Failed-Target", target.ID)
					writePoolCodeError(w, http.StatusServiceUnavailable, "target_unavailable", "first target unavailable")
					return
				}
				w.Header().Set("X-Selected-Target", target.ID)
				writeJSON(w, http.StatusOK, map[string]string{"target": target.ID})
			})
			if !handled || len(attempts) != 2 {
				t.Fatalf("handled=%v attempts=%+v", handled, attempts)
			}
			if recorder.Code != http.StatusOK || recorder.Header().Get("X-Selected-Target") != attempts[1].ID {
				t.Fatalf("response code=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if recorder.Header().Get("X-Failed-Target") != "" {
				t.Fatalf("failed target headers leaked: %v", recorder.Header())
			}
			binding, found, err := h.store.GetUserGroupTargetBinding(context.Background(), groupID, plan.AffinityKey, "")
			if err != nil || !found || binding.Target != attempts[1] {
				t.Fatalf("fallback binding=%+v found=%v err=%v, want %+v", binding, found, err, attempts[1])
			}
		})
	}
}

func TestUserGroupHTTPHandlerFallsBackAcrossModelProviders(t *testing.T) {
	const (
		groupID = "ug_http_provider_fallback"
		model   = "http-provider-fallback-model"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/primary/responses":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Primary-Failure", "must-not-leak")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"upstream_error","message":"primary target unavailable"}}`)
		case "/secondary/responses":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_fallback","object":"response","model":"`+model+`","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"served by secondary"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	})
	targets := []storage.TargetRef{
		{Kind: storage.TargetKindModelProvider, ID: "http-primary"},
		{Kind: storage.TargetKindModelProvider, ID: "http-secondary"},
	}
	for index, provider := range []storage.CustomProvider{
		{
			ID: "http-primary", Name: "HTTP primary", BaseURL: h.upstream.URL + "/primary",
			UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{model},
		},
		{
			ID: "http-secondary", Name: "HTTP secondary", BaseURL: h.upstream.URL + "/secondary",
			UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{model},
		},
	} {
		if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
			t.Fatal(err)
		}
		accountID := provider.ID + "-account"
		if err := h.store.UpsertAccount(t.Context(), storage.Account{
			ID: accountID, Label: accountID, GroupName: "cyber", Provider: provider.ID, Status: "active",
		}, storage.AccountToken{OpenAIAPIKey: "sk-" + provider.ID}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
			AccountID: accountID, ModelSlug: model, Source: "http_route_test",
		}}); err != nil {
			t.Fatal(err)
		}
		if index >= len(targets) {
			t.Fatal("provider/target setup mismatch")
		}
	}
	createRouteTestGroup(t, h, groupID, targets, []storage.ModelRoutingRule{{
		Model: model,
		Tiers: [][]storage.TargetRef{{targets[0]}, {targets[1]}},
	}})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"provider fallback","user_group_id":"`+groupID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create API key for user group = %d: %s", code, raw)
	}
	var keyPayload map[string]interface{}
	if err := json.Unmarshal(raw, &keyPayload); err != nil {
		t.Fatalf("decode API key response: %v (%s)", err, raw)
	}
	key, _ := keyPayload["key"].(string)
	if key == "" {
		t.Fatalf("API key response missing key: %s", raw)
	}

	body := `{"model":"` + model + `","input":"fallback please"}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "http-provider-fallback-root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "served by secondary") {
		t.Fatalf("fallback response status=%d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-Primary-Failure") != "" || strings.Contains(string(raw), "primary target unavailable") {
		t.Fatalf("primary failure leaked downstream: headers=%v body=%s", resp.Header, raw)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Path != "/primary/responses" || requests[1].Path != "/secondary/responses" {
		t.Fatalf("upstream attempts = %+v, want primary then secondary", requests)
	}
	var responseBody map[string]interface{}
	if err := json.Unmarshal(raw, &responseBody); err != nil || responseBody["id"] != "resp_fallback" {
		t.Fatalf("decode final response: err=%v body=%s", err, raw)
	}
	affinityRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	affinityRequest.Header.Set("Thread-Id", "http-provider-fallback-root")
	affinity := routing.ExtractAffinityKey(affinityRequest, []byte(body))
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target != targets[1] {
		t.Fatalf("persisted fallback binding=%+v found=%v err=%v, want %+v", binding, found, err, targets[1])
	}
}

func TestDispatchUserGroupRouteCandidatesNeverReplaysCommittedStream(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "stream-secondary"}); err != nil {
		t.Fatal(err)
	}
	targets := []storage.TargetRef{
		{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"},
		{Kind: storage.TargetKindAccountPoolGroup, ID: "stream-secondary"},
	}
	createRouteTestGroup(t, h, "ug_stream_commit", targets, nil)
	raw := []byte(`{"model":"gpt","stream":true,"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Thread-Id", "stream-root")
	recorder := httptest.NewRecorder()
	attempts := 0
	h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{UserGroupID: "ug_stream_commit"}, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: committed\n\n"))
	})
	if attempts != 1 || recorder.Code != http.StatusOK || recorder.Body.String() != "data: committed\n\n" {
		t.Fatalf("attempts=%d code=%d body=%q", attempts, recorder.Code, recorder.Body.String())
	}
}

func TestDispatchUserGroupRouteCandidatesDoesNotReplayServerSideState(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "state-secondary"}); err != nil {
		t.Fatal(err)
	}
	targets := []storage.TargetRef{
		{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"},
		{Kind: storage.TargetKindAccountPoolGroup, ID: "state-secondary"},
	}
	createRouteTestGroup(t, h, "ug_stateful", targets, nil)
	raw := []byte(`{"model":"gpt","previous_response_id":"resp_existing","input":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	attempts := 0
	h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{UserGroupID: "ug_stateful"}, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writePoolCodeError(w, http.StatusServiceUnavailable, "target_unavailable", "target unavailable")
	})
	if attempts != 1 || recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stateful request replayed: attempts=%d code=%d body=%s", attempts, recorder.Code, recorder.Body.String())
	}
}

func TestUserGroupRouteUsesModelExceptionWithoutReplacingRootBinding(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, provider := range []storage.CustomProvider{
		{ID: "route-main", Name: "Route Main", BaseURL: "https://main.example/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{"model-main"}},
		{ID: "route-child", Name: "Route Child", BaseURL: "https://child.example/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{"model-child"}},
	} {
		if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
			t.Fatal(err)
		}
	}
	createRouteTestGroup(t, h, "ug_model_exception", []storage.TargetRef{
		{Kind: storage.TargetKindModelProvider, ID: "route-main"},
		{Kind: storage.TargetKindModelProvider, ID: "route-child"},
	}, nil)
	pol := downstreamPolicy{UserGroupID: "ug_model_exception"}

	root := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	root.Header.Set("Thread-Id", "shared-root")
	rootRaw := []byte(`{"model":"model-main","input":"root"}`)
	_, rootProvider, err := resolveUserGroupRoute(root.Context(), h.store, pol, root, rootRaw)
	if err != nil || rootProvider != "custom:route-main" {
		t.Fatalf("root route provider=%q err=%v", rootProvider, err)
	}

	child := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	child.Header.Set("X-Codex-Parent-Thread-Id", "shared-root")
	childRaw := []byte(`{"model":"model-child","input":"child"}`)
	_, childProvider, err := resolveUserGroupRoute(child.Context(), h.store, pol, child, childRaw)
	if err != nil || childProvider != "custom:route-child" {
		t.Fatalf("child route provider=%q err=%v", childProvider, err)
	}

	rootPlan, err := resolveUserGroupRouteCandidates(root.Context(), h.store, pol, root, rootRaw)
	if err != nil {
		t.Fatal(err)
	}
	base, found, err := h.store.GetUserGroupTargetBinding(t.Context(), pol.UserGroupID, rootPlan.AffinityKey, "")
	if err != nil || !found || base.Target.ID != "route-main" {
		t.Fatalf("root binding=%+v found=%v err=%v", base, found, err)
	}
	exception, found, err := h.store.GetUserGroupTargetBinding(t.Context(), pol.UserGroupID, rootPlan.AffinityKey, "model-child")
	if err != nil || !found || exception.Target.ID != "route-child" {
		t.Fatalf("model exception=%+v found=%v err=%v", exception, found, err)
	}
}

func TestOrderUserGroupTiersNeverCrossesPriorityBoundary(t *testing.T) {
	tiers := [][]storage.TargetRef{
		{
			{Kind: storage.TargetKindAccountPoolGroup, ID: "first-a"},
			{Kind: storage.TargetKindAccountPoolGroup, ID: "first-b"},
		},
		{{Kind: storage.TargetKindAccountPoolGroup, ID: "last"}},
	}
	for _, seed := range []string{"one", "two", "three", "four"} {
		ordered := orderUserGroupTiers(tiers, seed)
		if len(ordered) != 3 || ordered[2].ID != "last" {
			t.Fatalf("seed %q crossed tier boundary: %+v", seed, ordered)
		}
	}
}
