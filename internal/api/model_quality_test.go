package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/storage"
)

func qualityTestAnswer(body string, wrong bool) string {
	if wrong {
		return "WRONG"
	}
	switch {
	case strings.Contains(body, "Start n=17"):
		return "14"
	case strings.Contains(body, "x=x*i"):
		return "76"
	case strings.Contains(body, "A coin is in"):
		return "B"
	case strings.Contains(body, "n mod 5=2"):
		return "242"
	case strings.Contains(body, "f(1)=2"):
		return "73|ZWYX"
	default:
		return "UNKNOWN"
	}
}

func modelQualityHarnessWithResponder(t *testing.T, accountCount int, responder func(call int, body string) (int, string)) *testHarness {
	t.Helper()
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		status, answer := responder(int(calls.Add(1)), string(raw))
		w.Header().Set("Content-Type", "application/json")
		if status > 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "quality-response", "model": "quality-model",
			"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "content": answer}}},
			"usage":   map[string]interface{}{"prompt_tokens": 40, "completion_tokens": 2, "total_tokens": 42},
		})
	})
	ctx := context.Background()
	if err := h.store.UpsertCustomProvider(ctx, storage.CustomProvider{ID: "quality", Name: "Quality", BaseURL: h.upstream.URL, UpstreamProtocol: storage.CustomProviderProtocolChatCompletions, Enabled: true, Models: []string{"quality-model"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < accountCount; i++ {
		id := "quality-account-" + string(rune('a'+i))
		if err := h.store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "quality", Status: "active"}, storage.AccountToken{OpenAIAPIKey: "sk-quality-" + id}); err != nil {
			t.Fatal(err)
		}
		setTestCapability(t, h, id, "quality-model", 100000)
	}
	h.app.scheduler.InvalidateAccountCache()
	return h
}

func modelQualityHarness(t *testing.T, wrong bool, accountCount int) *testHarness {
	t.Helper()
	return modelQualityHarnessWithResponder(t, accountCount, func(_ int, body string) (int, string) {
		return http.StatusOK, qualityTestAnswer(body, wrong)
	})
}

func TestModelQualityChecksOncePerGroupModelNotPerAccount(t *testing.T) {
	h := modelQualityHarness(t, false, 3)
	statuses, err := h.app.runModelQualityChecks(context.Background(), modelQualityRunRequest{Group: "cyber", Model: "quality-model", Provider: "quality"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].State != "healthy" || statuses[0].TotalChecks != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	if got := len(h.requests()); got != 1 {
		t.Fatalf("three accounts in one group must still produce one primary request, got %d", got)
	}
	if statuses[0].TotalTokens != 42 {
		t.Fatalf("quality token accounting = %d, want 42", statuses[0].TotalTokens)
	}
}

func TestModelQualityRequiresRepeatedConfirmedAnomaly(t *testing.T) {
	h := modelQualityHarness(t, true, 2)
	filter := modelQualityRunRequest{Group: "cyber", Model: "quality-model", Provider: "quality"}
	first, err := h.app.runModelQualityChecks(context.Background(), filter, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].State != "suspect" || first[0].ConsecutiveAnomalies != 1 {
		t.Fatalf("first anomaly must be suspect: %+v", first)
	}
	second, err := h.app.runModelQualityChecks(context.Background(), filter, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].State != "degraded" || second[0].ConsecutiveAnomalies != 2 {
		t.Fatalf("second confirmed anomaly must be degraded: %+v", second)
	}
	if got := len(h.requests()); got != 4 {
		t.Fatalf("each anomaly should use primary+one combined confirmation, got %d calls", got)
	}
	runs, err := h.store.ListModelQualityRuns(context.Background(), "cyber", "quality-model", 10)
	if err != nil || len(runs) != 4 {
		t.Fatalf("runs = %+v err=%v", runs, err)
	}
	confirmations := 0
	for _, run := range runs {
		if run.Phase == "confirmation" {
			confirmations++
		}
	}
	if confirmations != 2 {
		t.Fatalf("confirmation rows = %d, want 2", confirmations)
	}
}

func TestModelQualityConfirmationErrorNeverMarksIntelligenceDegraded(t *testing.T) {
	h := modelQualityHarnessWithResponder(t, 2, func(call int, body string) (int, string) {
		if call == 1 {
			return http.StatusOK, qualityTestAnswer(body, true)
		}
		return http.StatusBadGateway, ""
	})
	statuses, err := h.app.runModelQualityChecks(context.Background(), modelQualityRunRequest{Group: "cyber", Model: "quality-model", Provider: "quality"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	status := statuses[0]
	if status.State != "unknown" || status.ConsecutiveAnomalies != 0 || status.ConsecutiveErrors != 1 || status.LastOutcome != "inconclusive" {
		t.Fatalf("inconclusive network failure must not become an intelligence anomaly: %+v", status)
	}
}

func TestModelQualityIndependentConfirmationClearsFalseAlarm(t *testing.T) {
	h := modelQualityHarnessWithResponder(t, 2, func(call int, body string) (int, string) {
		return http.StatusOK, qualityTestAnswer(body, call == 1)
	})
	statuses, err := h.app.runModelQualityChecks(context.Background(), modelQualityRunRequest{Group: "cyber", Model: "quality-model", Provider: "quality"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	status := statuses[0]
	if status.State != "healthy" || status.LastOutcome != "false_alarm" || status.ConsecutiveAnomalies != 0 {
		t.Fatalf("successful independent confirmation should clear the primary false alarm: %+v", status)
	}
	if status.LastProbeID != modelQualityConfirmationProbe.ID || status.LastExpected != modelQualityConfirmationProbe.Expected || status.LastActual != modelQualityConfirmationProbe.Expected {
		t.Fatalf("status must report the confirmation answer coherently: %+v", status)
	}
}

func TestModelQualityDueGatePreventsMoreThanOneHourlyPrimary(t *testing.T) {
	h := modelQualityHarness(t, false, 3)
	filter := modelQualityRunRequest{Group: "cyber", Model: "quality-model", Provider: "quality"}
	if _, err := h.app.runModelQualityChecks(context.Background(), filter, true); err != nil {
		t.Fatal(err)
	}
	statuses, err := h.app.runModelQualityChecks(context.Background(), filter, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 || len(h.requests()) != 1 {
		t.Fatalf("not-yet-due group/model must not consume another request: statuses=%+v requests=%d", statuses, len(h.requests()))
	}
}

func TestModelQualityUsesCheapPrimaryAndStrongerConfirmation(t *testing.T) {
	h := modelQualityHarness(t, false, 1)
	ctx := context.Background()
	if got := h.app.modelQualityReasoningEffortForPhase(ctx, "primary"); got != "low" {
		t.Fatalf("default primary reasoning effort = %q, want low", got)
	}
	if got := h.app.modelQualityReasoningEffortForPhase(ctx, "confirmation"); got != "medium" {
		t.Fatalf("default confirmation reasoning effort = %q, want medium", got)
	}
}

func TestModelQualityAnswerAndModelNormalization(t *testing.T) {
	if !qualityAnswerMatches("Answer: 73 | ZWYX.", "73|ZWYX") {
		t.Fatal("compact answer normalization rejected a correct response")
	}
	if qualityAnswerMatches("72|ZWYX", "73|ZWYX") {
		t.Fatal("incorrect answer accepted")
	}
	if !qualityModelMatches("gpt-5.6-sol", "gpt-5.6-sol-2026-07-10") || qualityModelMatches("gpt-5.6-sol", "gpt-5.4") {
		t.Fatal("returned-model downgrade matching is incorrect")
	}
}

func TestModelQualityStatusListHidesRetiredCombos(t *testing.T) {
	statuses := []storage.ModelQualityStatus{
		{GroupName: "cyber", ModelSlug: "retired-model", Provider: "quality", State: "degraded"},
		{GroupName: "cyber", ModelSlug: "quality-model", Provider: "quality", State: "healthy"},
	}
	combos := []modelQualityCombo{
		{Group: "cyber", Model: "quality-model", Provider: "quality"},
		{Group: "cyber", Model: "new-model", Provider: "quality"},
	}
	got := mergeUnknownModelQualityStatuses(statuses, combos, "", "")
	if len(got) != 2 || got[0].ModelSlug != "new-model" || got[0].State != "unknown" || got[1].ModelSlug != "quality-model" || got[1].State != "healthy" {
		t.Fatalf("active quality status reconciliation = %+v", got)
	}
}
