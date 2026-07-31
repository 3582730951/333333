package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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

func setUserGroupCapacityTestIntervals(t *testing.T, timeout, poll, heartbeat, safetyRetry time.Duration) {
	t.Helper()
	oldTimeout := userGroupCapacityWaitTimeout
	oldPoll := userGroupCapacityPollInterval
	oldHeartbeat := userGroupCapacityHeartbeatInterval
	oldSafetyRetry := userGroupCapacitySafetyRetryInterval
	userGroupCapacityWaitTimeout = timeout
	userGroupCapacityPollInterval = poll
	userGroupCapacityHeartbeatInterval = heartbeat
	userGroupCapacitySafetyRetryInterval = safetyRetry
	t.Cleanup(func() {
		userGroupCapacityWaitTimeout = oldTimeout
		userGroupCapacityPollInterval = oldPoll
		userGroupCapacityHeartbeatInterval = oldHeartbeat
		userGroupCapacitySafetyRetryInterval = oldSafetyRetry
	})
}

func TestUserGroupCustomTargetSupportsDownstreamModelMappingSource(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	provider := storage.CustomProvider{
		ID:      "mapped-user-group-provider",
		Name:    "Mapped User Group Provider",
		BaseURL: "https://relay.example/v1",
		Enabled: true,
		Models:  []string{"relay-internal-model"},
		ModelMappings: map[string]string{
			"client-visible-model": "relay-internal-model",
			"team-*":               "relay-team-model",
		},
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	target := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: provider.ID}
	for _, model := range []string{"client-visible-model", "team-coding"} {
		if !userGroupTargetSupportsModel(t.Context(), h.store, target, model) {
			t.Fatalf("mapped downstream model %q was filtered before custom provider routing", model)
		}
	}
	if userGroupTargetSupportsModel(t.Context(), h.store, target, "unrelated-model") {
		t.Fatal("unrelated model unexpectedly matched custom provider")
	}
}

func TestUserGroupTargetFamilyPolicySkipsOnlySelectedAccountPool(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "family-policy-secondary"}); err != nil {
		t.Fatal(err)
	}
	primary := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}
	secondary := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "family-policy-secondary"}
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: "ug_family_policy", Name: "family-policy",
		Targets:                 []storage.TargetRef{primary, secondary},
		BlockClaudeTargetGroups: []string{primary.ID},
	}); err != nil {
		t.Fatal(err)
	}
	pol := downstreamPolicy{UserGroupID: "ug_family_policy", Group: "cyber"}

	claudeRaw := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`)
	claudePlan, err := resolveUserGroupRouteCandidates(t.Context(), h.store, pol, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), claudeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(claudePlan.Candidates) != 1 || claudePlan.Candidates[0] != secondary {
		t.Fatalf("Claude candidates=%+v, want only secondary", claudePlan.Candidates)
	}

	gptRaw := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	gptPlan, err := resolveUserGroupRouteCandidates(t.Context(), h.store, pol, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), gptRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(gptPlan.Candidates) != 2 || !userGroupTargetsContain(gptPlan.Candidates, primary) || !userGroupTargetsContain(gptPlan.Candidates, secondary) {
		t.Fatalf("GPT candidates=%+v, want both account pools", gptPlan.Candidates)
	}
}

func TestUserGroupTrafficFallbackRewritesModelAndTriesOrderedGroups(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, name := range []string{"traffic-fallback-primary", "traffic-fallback-first", "traffic-fallback-second"} {
		if err := h.store.CreateGroup(t.Context(), storage.Group{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	firstID := "ug_traffic_fallback_first"
	secondID := "ug_traffic_fallback_second"
	sourceID := "ug_traffic_fallback_source"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: firstID, Name: firstID,
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "traffic-fallback-first"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: secondID, Name: secondID,
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "traffic-fallback-second"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: sourceID, Name: sourceID,
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "traffic-fallback-primary"}},
		TrafficFallbackGroups: storage.TrafficFallbackGroups{
			GPT: []string{firstID, secondID},
		},
		TrafficFallbackModelMappings: []storage.TrafficFallbackModelMapping{
			{Family: "gpt", SourceModel: "gpt-5.*", TargetUserGroupID: firstID, TargetModel: "gpt-wildcard"},
			{Family: "gpt", SourceModel: "gpt-5.6-sol", TargetUserGroupID: firstID, TargetModel: "gpt-5.5"},
			{Family: "gpt", SourceModel: "*", TargetUserGroupID: secondID, TargetModel: "gpt-5.4"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`{"model":"gpt-5.6-sol","input":"fallback","stream":false}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	recorder := httptest.NewRecorder()
	targetModels := make(map[string]string)
	targetOrder := make([]string, 0, 3)
	var dispatch func(http.ResponseWriter, *http.Request)
	dispatch = func(w http.ResponseWriter, candidate *http.Request) {
		if target, forced := userGroupRouteOverride(candidate.Context()); forced {
			body, err := io.ReadAll(candidate.Body)
			if err != nil {
				t.Fatal(err)
			}
			targetOrder = append(targetOrder, target.ID)
			targetModels[target.ID] = routing.Model(body)
			switch target.ID {
			case "traffic-fallback-primary", "traffic-fallback-first":
				writePoolCodeError(w, http.StatusServiceUnavailable, "capability_unavailable", "fixture target unavailable")
			case "traffic-fallback-second":
				writeJSON(w, http.StatusOK, map[string]string{"id": "resp_cross_group_fallback", "status": "completed"})
			default:
				t.Fatalf("unexpected target: %+v", target)
			}
			return
		}
		fallbackPolicy, ok := h.app.resolveDownstreamPolicy(w, candidate)
		if !ok {
			return
		}
		body, err := io.ReadAll(candidate.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !h.app.dispatchUserGroupRouteCandidates(w, candidate, body, body, fallbackPolicy, dispatch) {
			t.Fatalf("fallback policy %q did not dispatch", fallbackPolicy.UserGroupID)
		}
	}

	handled := h.app.dispatchUserGroupRouteCandidates(
		recorder,
		request,
		raw,
		raw,
		downstreamPolicy{UserGroupID: sourceID, Group: "cyber"},
		dispatch,
	)
	if !handled || recorder.Code != http.StatusOK {
		t.Fatalf("handled=%v status=%d body=%s", handled, recorder.Code, recorder.Body.String())
	}
	wantOrder := []string{"traffic-fallback-primary", "traffic-fallback-first", "traffic-fallback-second"}
	if !reflect.DeepEqual(targetOrder, wantOrder) {
		t.Fatalf("target order=%v, want %v", targetOrder, wantOrder)
	}
	if targetModels["traffic-fallback-first"] != "gpt-5.5" {
		t.Fatalf("exact mapping did not outrank wildcard: models=%v", targetModels)
	}
	if targetModels["traffic-fallback-second"] != "gpt-5.4" {
		t.Fatalf("second fallback model=%q, want gpt-5.4", targetModels["traffic-fallback-second"])
	}
	if recorder.Header().Get("X-Pool-Fallback-Group") != secondID ||
		recorder.Header().Get("X-Pool-Fallback-Model") != "gpt-5.4" ||
		recorder.Header().Get("X-Pool-Fallback-From-Group") != sourceID {
		t.Fatalf("fallback diagnostics headers=%v", recorder.Header())
	}
}

func TestTrafficFallbackCandidatesCoverClaudeGeminiAndManualModels(t *testing.T) {
	group := storage.UserGroup{
		TrafficFallbackGroups: storage.TrafficFallbackGroups{
			Claude: []string{"ug_claude_fallback"},
			Gemini: []string{"ug_gemini_fallback"},
			GPT:    []string{"ug_manual_fallback"},
		},
		TrafficFallbackModelMappings: []storage.TrafficFallbackModelMapping{
			{Family: "claude", SourceModel: "claude-*", TargetUserGroupID: "ug_claude_fallback", TargetModel: "claude-sonnet-4-5"},
			{Family: "gemini", SourceModel: "*", TargetUserGroupID: "ug_gemini_fallback", TargetModel: "gemini-3-flash"},
			{Family: "gpt", SourceModel: "vendor-manual", TargetUserGroupID: "ug_manual_fallback", TargetModel: "gpt-5.5"},
		},
	}
	cases := []struct {
		model, groupID, targetModel, family string
	}{
		{"claude-opus-5", "ug_claude_fallback", "claude-sonnet-4-5", "claude"},
		{"gemini-3-pro", "ug_gemini_fallback", "gemini-3-flash", "gemini"},
		{"vendor-manual", "ug_manual_fallback", "gpt-5.5", "gpt"},
	}
	for _, tc := range cases {
		candidates := trafficFallbackCandidates(group, tc.model, []string{"ug_source"})
		if len(candidates) != 1 ||
			candidates[0].UserGroupID != tc.groupID ||
			candidates[0].TargetModel != tc.targetModel ||
			candidates[0].Family != tc.family {
			t.Fatalf("%s candidates=%+v", tc.model, candidates)
		}
	}
	if candidates := trafficFallbackCandidates(group, "claude-opus-5", []string{
		"one", "two", "three", "four", "five", "six", "seven", "eight",
	}); len(candidates) != 0 {
		t.Fatalf("fallback depth guard returned candidates: %+v", candidates)
	}
}

func TestUserGroupTrafficFallbackDoesNotReplayServerSideState(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "stateful-fallback-primary"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "stateful-fallback-target"}); err != nil {
		t.Fatal(err)
	}
	fallbackID := "ug_stateful_fallback_target"
	sourceID := "ug_stateful_fallback_source"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: fallbackID, Name: fallbackID,
		Targets: []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "stateful-fallback-target"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: sourceID, Name: sourceID,
		Targets:               []storage.TargetRef{{Kind: storage.TargetKindAccountPoolGroup, ID: "stateful-fallback-primary"}},
		TrafficFallbackGroups: storage.TrafficFallbackGroups{GPT: []string{fallbackID}},
		TrafficFallbackModelMappings: []storage.TrafficFallbackModelMapping{{
			Family: "gpt", SourceModel: "*", TargetUserGroupID: fallbackID, TargetModel: "gpt-5.5",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp_existing","input":[]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	attempts := 0
	h.app.dispatchUserGroupRouteCandidates(recorder, request, raw, raw, downstreamPolicy{UserGroupID: sourceID}, func(w http.ResponseWriter, candidate *http.Request) {
		attempts++
		if _, fallback := trafficFallbackExecutionFromContext(candidate.Context()); fallback {
			t.Fatal("stateful request entered traffic fallback")
		}
		writePoolCodeError(w, http.StatusServiceUnavailable, "capability_unavailable", "fixture unavailable")
	})
	if attempts != 1 || recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stateful traffic fallback attempts=%d status=%d body=%s", attempts, recorder.Code, recorder.Body.String())
	}
}

func TestUserGroupPolicyTransfersStaleModelBindingToEligiblePool(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "policy-transfer-secondary"}); err != nil {
		t.Fatal(err)
	}
	primary := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}
	secondary := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "policy-transfer-secondary"}
	const (
		groupID = "ug_policy_transfer"
		model   = "claude-opus-5"
		thread  = "policy-transfer-root"
	)
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: groupID, Name: "policy-transfer",
		Targets:                 []storage.TargetRef{primary, secondary},
		BlockClaudeTargetGroups: []string{primary.ID},
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"continue"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Claude-Code-Session-Id", thread)
	affinity := routing.ExtractClaudeAffinityKey(req, raw)
	for _, binding := range []storage.UserGroupTargetBinding{
		{UserGroupID: groupID, AffinityKey: affinity.Hash, Target: primary},
		{UserGroupID: groupID, AffinityKey: affinity.Hash, Model: model, Target: primary},
	} {
		if err := h.store.UpsertUserGroupTargetBinding(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	var selected storage.TargetRef
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{
		UserGroupID: groupID, Group: "cyber",
	}, func(w http.ResponseWriter, candidate *http.Request) {
		selected, _ = userGroupRouteOverride(candidate.Context())
		writeJSON(w, http.StatusOK, map[string]string{"id": "msg_policy_transfer"})
	})
	if !handled || recorder.Code != http.StatusOK || selected != secondary {
		t.Fatalf("handled=%v status=%d selected=%+v body=%s", handled, recorder.Code, selected, recorder.Body.String())
	}
	rootBinding, rootFound, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !rootFound || rootBinding.Target != primary {
		t.Fatalf("root binding changed: %+v found=%v err=%v", rootBinding, rootFound, err)
	}
	modelBinding, modelFound, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, model)
	if err != nil || !modelFound || modelBinding.Target != secondary {
		t.Fatalf("model binding not transferred: %+v found=%v err=%v", modelBinding, modelFound, err)
	}
}

func TestUserGroupCapacityWaitKeepsStreamAliveAndNewTargetTakesOver(t *testing.T) {
	if userGroupCapacityWaitTimeout != 10*time.Minute {
		t.Fatalf("production capacity wait=%s, want 10m", userGroupCapacityWaitTimeout)
	}
	setUserGroupCapacityTestIntervals(t, 300*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, time.Hour)
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "capacity-takeover"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "capacity-final"}); err != nil {
		t.Fatal(err)
	}
	blocked := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}
	takeover := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "capacity-takeover"}
	final := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "capacity-final"}
	const groupID = "ug_capacity_takeover"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: groupID, Name: "capacity-takeover",
		Targets:                 []storage.TargetRef{blocked},
		BlockClaudeTargetGroups: []string{blocked.ID},
	}); err != nil {
		t.Fatal(err)
	}

	updateDone := make(chan error, 2)
	go func() {
		time.Sleep(30 * time.Millisecond)
		updateDone <- h.store.ReplaceUserGroupDefinition(context.Background(), storage.UserGroup{
			ID: groupID, Name: "capacity-takeover",
			Targets:                 []storage.TargetRef{blocked, takeover},
			BlockClaudeTargetGroups: []string{blocked.ID},
		})
		time.Sleep(30 * time.Millisecond)
		updateDone <- h.store.ReplaceUserGroupDefinition(context.Background(), storage.UserGroup{
			ID: groupID, Name: "capacity-takeover",
			Targets:                 []storage.TargetRef{blocked, takeover, final},
			BlockClaudeTargetGroups: []string{blocked.ID},
		})
	}()

	raw := []byte(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"wait"}]}`)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withSchedulerWait(req.Context(), recorder, true, "anthropic"))
	var selected storage.TargetRef
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{
		UserGroupID: groupID, Group: "cyber",
	}, func(w http.ResponseWriter, candidate *http.Request) {
		selected, _ = userGroupRouteOverride(candidate.Context())
		if selected == takeover {
			if schedulerWaitTerminal(candidate.Context(), "candidate unavailable") {
				t.Error("nested candidate terminated the outer waiting stream")
				return
			}
			writePoolCodeError(w, http.StatusServiceUnavailable, "capability_unavailable", "candidate unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	for range 2 {
		if err := <-updateDone; err != nil {
			t.Fatal(err)
		}
	}
	if !handled || selected != final {
		t.Fatalf("handled=%v selected=%+v, want final takeover target", handled, selected)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: ping") || !strings.Contains(body, "event: message_stop") || strings.Contains(body, "event: error") {
		t.Fatalf("capacity takeover stream=%q", body)
	}
}

func TestUserGroupCapacityWaitTimesOutWithProtocolTerminal(t *testing.T) {
	setUserGroupCapacityTestIntervals(t, 45*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, time.Hour)
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	blocked := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"}
	const groupID = "ug_capacity_timeout"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: groupID, Name: "capacity-timeout",
		Targets:              []storage.TargetRef{blocked},
		BlockGPTTargetGroups: []string{blocked.ID},
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"wait"}`)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(withSchedulerWait(req.Context(), recorder, true, "responses"))
	attempts := 0
	started := time.Now()
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{
		UserGroupID: groupID, Group: "cyber",
	}, func(http.ResponseWriter, *http.Request) {
		attempts++
	})
	elapsed := time.Since(started)
	if !handled || attempts != 0 || elapsed < 35*time.Millisecond || elapsed > time.Second {
		t.Fatalf("handled=%v attempts=%d elapsed=%s", handled, attempts, elapsed)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.in_progress") || !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("capacity timeout stream=%q", body)
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

func TestUserGroupStablePrefixDoesNotCreateDurableTargetBinding(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "fair-prefix-secondary"}); err != nil {
		t.Fatal(err)
	}
	createRouteTestGroup(t, h, "ug_fair_prefix", []storage.TargetRef{
		{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"},
		{Kind: storage.TargetKindAccountPoolGroup, ID: "fair-prefix-secondary"},
	}, nil)
	longPrefix := strings.Repeat("shared-context-", 400)
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"system","content":"` + longPrefix + `"},{"role":"user","content":"new turn"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer stable-prefix-key")
	plan, err := resolveUserGroupRouteCandidates(req.Context(), h.store, downstreamPolicy{UserGroupID: "ug_fair_prefix"}, req, raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AffinityKey == "" || plan.Persist {
		t.Fatalf("stable-prefix plan affinity=%+v persist=%v", plan.Affinity, plan.Persist)
	}
	if _, _, err := resolveUserGroupRoute(req.Context(), h.store, downstreamPolicy{UserGroupID: "ug_fair_prefix"}, req, raw); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.store.GetUserGroupTargetBinding(t.Context(), "ug_fair_prefix", plan.AffinityKey, ""); err != nil || found {
		t.Fatalf("stable prefix durable binding found=%v err=%v", found, err)
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

func TestUserGroupSpeculativeProbeFallsBackWhenResponseMemoryIsSaturated(t *testing.T) {
	dir := t.TempDir()
	budget := bodysource.NewBudget(1, 1<<20)
	if !budget.ReserveMemory(1) {
		t.Fatal("failed to saturate response memory fixture")
	}
	w := newUserGroupAttemptWriter(context.Background(), httptest.NewRecorder(), false, false, bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 64 << 10, TempDir: dir, Budget: budget,
	}, true)
	w.WriteHeader(http.StatusBadRequest)
	for _, chunk := range []string{"NO AVAILABLE ", "ACCOUNT"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !w.RetryableFailure() || !w.PermanentTargetFailure() || userGroupRouteStatusClass(w) != "no_account" {
		t.Fatalf("saturated probe classification retryable=%v permanent=%v class=%q", w.RetryableFailure(), w.PermanentTargetFailure(), userGroupRouteStatusClass(w))
	}
	if w.body != nil || len(w.probeBody) != 0 || !w.probeTruncated {
		t.Fatalf("saturated probe touched general spool: body=%v retained=%d truncated=%v", w.body, len(w.probeBody), w.probeTruncated)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	budget.ReleaseMemory(1)
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("probe budget leaked: %+v", snapshot)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("speculative probe created spool files: entries=%v err=%v", entries, err)
	}
}

func TestUserGroupSpeculativeProbeMemoryAdmissionIsReleased(t *testing.T) {
	budget := bodysource.NewBudget(64<<10, 1<<20)
	w := newUserGroupAttemptWriter(context.Background(), httptest.NewRecorder(), false, false, bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	}, true)
	w.WriteHeader(http.StatusServiceUnavailable)
	payload := []byte(`{"error":{"message":"temporarily unavailable"}}`)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != int64(len(payload)) || snapshot.SpoolUsed != 0 {
		t.Fatalf("probe admission mismatch: %+v", snapshot)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("probe close leaked admission: %+v", snapshot)
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

func TestDispatchUserGroupRoutePersistsTerminalBindingAfterClientCancellation(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	const groupID = "ug_cancelled_terminal_binding"
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{{
		Kind: storage.TargetKindAccountPoolGroup,
		ID:   "cyber",
	}}, nil)

	raw := []byte(`{"model":"gpt-5.6-sol","input":"terminal"}`)
	base := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	base.Header.Set("Thread-Id", "cancelled-terminal-root")
	ctx, cancel := context.WithCancel(base.Context())
	req := base.WithContext(ctx)
	recorder := httptest.NewRecorder()
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{UserGroupID: groupID}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": "resp_cancelled_terminal", "status": "completed"})
		cancel()
	})
	if !handled || recorder.Code != http.StatusOK {
		t.Fatalf("handled=%v status=%d body=%s", handled, recorder.Code, recorder.Body.String())
	}

	affinityRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	affinityRequest.Header.Set("Thread-Id", "cancelled-terminal-root")
	affinity := routing.ExtractAffinityKey(affinityRequest, raw)
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target.ID != "cyber" {
		t.Fatalf("terminal binding=%+v found=%v err=%v", binding, found, err)
	}
}

func TestClaudeMessagesUserGroupFallsThroughCodexTierToExactAntigravityModel(t *testing.T) {
	const (
		groupID          = "ug_messages_exact_antigravity"
		codexGroup       = "route-codex-only"
		antigravityGroup = "route-antigravity-capable"
		antigravityID    = "route-antigravity-account"
		plainKey         = "sk-user-group-exact-antigravity"
	)
	observedModels := make(chan string, 4)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode Antigravity request: %v body=%s", err, body)
		}
		observedModels <- request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"response": {
				"candidates": [{"content":{"role":"model","parts":[{"text":"routed"}]},"finishReason":"STOP"}],
				"usageMetadata": {"promptTokenCount":4,"candidatesTokenCount":2}
			}
		}`)
	})
	for _, name := range []string{codexGroup, antigravityGroup} {
		if err := h.store.CreateGroup(t.Context(), storage.Group{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	codexID := h.importAccount(t, "route-codex-only", "route-codex-upstream", "route-codex-access")
	codex, err := h.store.GetAccount(t.Context(), codexID)
	if err != nil {
		t.Fatal(err)
	}
	codexToken, err := h.store.GetToken(t.Context(), codexID)
	if err != nil {
		t.Fatal(err)
	}
	codex.GroupName = codexGroup
	if err := h.store.UpsertAccount(t.Context(), codex, codexToken); err != nil {
		t.Fatal(err)
	}

	antigravity := storage.Account{ID: antigravityID, Label: antigravityID, GroupName: antigravityGroup, Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccountWithAntigravityCredentials(t.Context(), antigravity, storage.AccountToken{}, storage.AntigravityCredentials{
		ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(2 * time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}
	models := []string{"claude-opus-4-6-thinking", "gemini-3-flash"}
	capabilities := make([]storage.ModelCapability, 0, len(models))
	for _, model := range models {
		capabilities = append(capabilities, storage.ModelCapability{
			AccountID: antigravityID, ModelSlug: model, AvailabilityState: "verified", Source: "antigravity_model_probe",
		})
	}
	if err := h.store.UpsertCapabilities(t.Context(), capabilities); err != nil {
		t.Fatal(err)
	}

	codexTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: codexGroup}
	antigravityTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: antigravityGroup}
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{codexTarget, antigravityTarget}, []storage.ModelRoutingRule{{
		Model: "*", Tiers: [][]storage.TargetRef{{codexTarget}, {antigravityTarget}},
	}})
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(plainKey), KeyType: "downstream", Label: "exact Antigravity route",
		GroupName: codexGroup, UserGroupID: groupID, ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	for index, model := range models {
		body := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
		sessionID := fmt.Sprintf("exact-antigravity-%d", index)
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+plainKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		affinity := routing.ExtractClaudeAffinityKey(req, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Pool-Resolved-Provider") != "antigravity" {
			t.Fatalf("model=%s status=%d provider=%q body=%s", model, resp.StatusCode, resp.Header.Get("X-Pool-Resolved-Provider"), responseBody)
		}
		if observed := <-observedModels; observed != model {
			t.Fatalf("model=%s reached Antigravity as %q", model, observed)
		}
		binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
		if err != nil || !found || binding.Target != antigravityTarget {
			t.Fatalf("model=%s binding=%+v found=%v err=%v, want %+v", model, binding, found, err, antigravityTarget)
		}
	}
	audit, err := h.store.ListAuditLog(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range audit {
		if row.Action == "model_fallback_required" || row.Action == "model_capability_rejected" {
			t.Fatalf("successful zero-config group fallback retained a private target error: %+v", audit)
		}
	}

	before := len(h.requests())
	unknownBody := []byte(`{"model":"gemini-unverified","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	unknownReq, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(unknownBody))
	unknownReq.Header.Set("Authorization", "Bearer "+plainKey)
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownReq.Header.Set("anthropic-version", "2023-06-01")
	unknownReq.Header.Set("X-Claude-Code-Session-Id", "exact-antigravity-unknown")
	unknownResp, err := http.DefaultClient.Do(unknownReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, unknownResp.Body)
	_ = unknownResp.Body.Close()
	if unknownResp.StatusCode == http.StatusOK || len(h.requests()) != before {
		t.Fatalf("unknown model status=%d upstream calls=%d->%d", unknownResp.StatusCode, before, len(h.requests()))
	}
}

func TestClaudeMessagesUserGroupRoutesExactModelToClaudeAccountPool(t *testing.T) {
	const (
		groupID     = "ug_messages_exact_claude"
		codexGroup  = "route-codex-before-claude"
		claudeGroup = "route-native-claude"
		claudeID    = "route-native-claude-account"
		model       = "claude-sonnet-4-6"
		plainKey    = "sk-user-group-exact-native-claude"
	)
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("native Claude request path=%q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "native-claude-token") &&
			!strings.Contains(r.Header.Get("X-Api-Key"), "native-claude-token") {
			t.Fatalf("native Claude credential headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_native_group","type":"message","role":"assistant","model":"`+model+`","content":[{"type":"text","text":"native claude"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	})
	for _, name := range []string{codexGroup, claudeGroup} {
		if err := h.store.CreateGroup(t.Context(), storage.Group{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	codexID := h.importAccount(t, "route-codex-before-claude", "route-codex-before-claude-upstream", "route-codex-before-claude-token")
	codex, err := h.store.GetAccount(t.Context(), codexID)
	if err != nil {
		t.Fatal(err)
	}
	codexToken, err := h.store.GetToken(t.Context(), codexID)
	if err != nil {
		t.Fatal(err)
	}
	codex.GroupName = codexGroup
	if err := h.store.UpsertAccount(t.Context(), codex, codexToken); err != nil {
		t.Fatal(err)
	}

	claude := storage.Account{ID: claudeID, Label: claudeID, GroupName: claudeGroup, Provider: "claude", Status: "active"}
	if err := h.store.UpsertAccount(t.Context(), claude, storage.AccountToken{AccessToken: "native-claude-token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: claudeID, ModelSlug: model, AvailabilityState: "verified", Source: "native_claude_catalog",
	}}); err != nil {
		t.Fatal(err)
	}

	codexTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: codexGroup}
	claudeTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: claudeGroup}
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{codexTarget, claudeTarget}, []storage.ModelRoutingRule{{
		Model: model, Tiers: [][]storage.TargetRef{{codexTarget}, {claudeTarget}},
	}})
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(plainKey), KeyType: "downstream", Label: "exact native Claude route",
		GroupName: codexGroup, UserGroupID: groupID, ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	body := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+plainKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", "native-claude-group-session")
	affinity := routing.ExtractClaudeAffinityKey(req, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Pool-Resolved-Provider") != "claude" ||
		!bytes.Contains(responseBody, []byte("native claude")) || upstreamCalls != 1 {
		t.Fatalf("status=%d provider=%q calls=%d body=%s", resp.StatusCode, resp.Header.Get("X-Pool-Resolved-Provider"), upstreamCalls, responseBody)
	}
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target != claudeTarget {
		t.Fatalf("native Claude binding=%+v found=%v err=%v, want %+v", binding, found, err, claudeTarget)
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

func TestUserGroupPermanentProviderFailureMigratesExistingBinding(t *testing.T) {
	const (
		groupID = "ug_provider_binding_migration"
		model   = "provider-binding-migration-model"
		thread  = "provider-binding-migration-root"
	)
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	primary := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "migration-primary"}
	secondary := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "migration-secondary"}
	for _, provider := range []storage.CustomProvider{
		{ID: primary.ID, Name: "Migration primary", BaseURL: "https://primary.example/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{model}},
		{ID: secondary.ID, Name: "Migration secondary", BaseURL: "https://secondary.example/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true, Models: []string{model}},
	} {
		if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
			t.Fatal(err)
		}
	}
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{primary, secondary}, []storage.ModelRoutingRule{{
		Model: model, Tiers: [][]storage.TargetRef{{primary}, {secondary}},
	}})

	raw := []byte(`{"model":"` + model + `","input":"migrate"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Thread-Id", thread)
	affinity := routing.ExtractAffinityKey(req, raw)
	if err := h.store.UpsertUserGroupTargetBinding(t.Context(), storage.UserGroupTargetBinding{
		UserGroupID: groupID, AffinityKey: affinity.Hash, Target: primary,
	}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	attempts := make([]storage.TargetRef, 0, 2)
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{UserGroupID: groupID}, func(w http.ResponseWriter, candidate *http.Request) {
		target, ok := userGroupRouteOverride(candidate.Context())
		if !ok {
			t.Fatal("candidate request missing route override")
		}
		attempts = append(attempts, target)
		if target == primary {
			writePoolCodeError(w, http.StatusNotFound, "model_not_found", "bound target permanently unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": "resp_migrated", "status": "completed"})
	})
	if !handled || recorder.Code != http.StatusOK || len(attempts) != 2 || attempts[0] != primary || attempts[1] != secondary {
		t.Fatalf("handled=%v status=%d attempts=%+v body=%s", handled, recorder.Code, attempts, recorder.Body.String())
	}
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target != secondary {
		t.Fatalf("migrated binding=%+v found=%v err=%v, want %+v", binding, found, err, secondary)
	}
}

func TestUserGroupExhaustedPlusFallsThroughToAuthorizedPro(t *testing.T) {
	const (
		plusGroup = "route-plus-exhausted"
		proGroup  = "route-pro-healthy"
		groupID   = "ug_plus_to_pro"
		model     = "gpt-5.6-sol"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-ID") != "upstream-pro" {
			t.Fatalf("request reached non-Pro account: %q", r.Header.Get("ChatGPT-Account-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_pro_fallback","status":"completed","output":[]}}`+"\n\n"+
			"data: [DONE]\n\n")
	})
	for _, name := range []string{plusGroup, proGroup} {
		if err := h.store.CreateGroup(t.Context(), storage.Group{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	moveAccount := func(id, group string) {
		account, err := h.store.GetAccount(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		token, err := h.store.GetToken(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		account.GroupName = group
		if err := h.store.UpsertAccount(t.Context(), account, token); err != nil {
			t.Fatal(err)
		}
		setTestCapability(t, h, id, model, 272000)
	}
	plusID := h.importAccount(t, "plus-exhausted", "upstream-plus", "access-plus")
	proID := h.importAccount(t, "pro-healthy", "upstream-pro", "access-pro")
	moveAccount(plusID, plusGroup)
	moveAccount(proID, proGroup)
	now := storage.Now()
	if err := h.store.UpsertAccountRateLimit(t.Context(), storage.AccountRateLimit{
		AccountID: plusID, Provider: "codex", LimiterType: "5h_polled", Source: "test",
		UsedPercent: 100, Status: "rejected", ResetAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	plus := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: plusGroup}
	pro := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: proGroup}
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{plus, pro}, []storage.ModelRoutingRule{{
		Model: model,
		Tiers: [][]storage.TargetRef{{plus}, {pro}},
	}})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/api-keys", `{"label":"plus-to-pro","user_group_id":"`+groupID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create API key = %d: %s", code, raw)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	key, _ := created["key"].(string)
	if key == "" {
		t.Fatalf("created key missing plaintext secret: %s", raw)
	}

	body := `{"model":"` + model + `","input":"use any authorized capacity","stream":true}`
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "plus-to-pro-session")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("authorized Pro fallback queued behind exhausted Plus for %s", elapsed)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("resp_pro_fallback")) {
		t.Fatalf("fallback status=%d body=%s", resp.StatusCode, responseBody)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].AccountID != "upstream-pro" {
		t.Fatalf("upstream requests=%+v, want one Pro request", requests)
	}
}

func TestUserGroupSameTierSelectAcrossSkipsEmptyPool(t *testing.T) {
	const (
		emptyGroup   = "same-tier-empty"
		healthyGroup = "same-tier-healthy"
		groupID      = "ug_same_tier_across"
		model        = "gpt-5.6-sol"
		plainKey     = "sk-same-tier-across"
	)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_same_tier","status":"completed","output":[]}}`+"\n\n"+
			"data: [DONE]\n\n")
	})
	for _, group := range []string{emptyGroup, healthyGroup} {
		if err := h.store.CreateGroup(t.Context(), storage.Group{Name: group}); err != nil {
			t.Fatal(err)
		}
	}
	accountID := h.importAccount(t, "same-tier", "upstream-same-tier", "access-same-tier")
	account, err := h.store.GetAccount(t.Context(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.store.GetToken(t.Context(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	account.GroupName = healthyGroup
	if err := h.store.UpsertAccount(t.Context(), account, token); err != nil {
		t.Fatal(err)
	}
	setTestCapability(t, h, accountID, model, 272000)
	emptyTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: emptyGroup}
	healthyTarget := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: healthyGroup}
	createRouteTestGroup(t, h, groupID, []storage.TargetRef{emptyTarget, healthyTarget}, []storage.ModelRoutingRule{{
		Model: model, Tiers: [][]storage.TargetRef{{emptyTarget, healthyTarget}},
	}})
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(plainKey), KeyType: "downstream", Label: "same-tier across",
		GroupName: emptyGroup, UserGroupID: groupID, ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	body := []byte(`{"model":"` + model + `","input":"route fairly","stream":true}`)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plainKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "same-tier-across-root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("resp_same_tier")) {
		t.Fatalf("same-tier response status=%d body=%s", resp.StatusCode, responseBody)
	}
	if requests := h.requests(); len(requests) != 1 || requests[0].AccountID != "upstream-same-tier" {
		t.Fatalf("same-tier upstream attempts=%+v", requests)
	}
	affinityReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	affinityReq.Header.Set("Thread-Id", "same-tier-across-root")
	affinity := routing.ExtractAffinityKey(affinityReq, body)
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target != healthyTarget {
		t.Fatalf("same-tier binding=%+v found=%v err=%v", binding, found, err)
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
