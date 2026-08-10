package virtual

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestMaterializeKeepsWithinNativeBudget(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Virtual2MEnabled = true
	planner := NewPlanner(store, cfg)
	large := strings.Repeat("A", 20000)
	body := []byte(`{"model":"gpt","input":[{"role":"user","content":"old ` + large + `"},{"role":"assistant","content":"middle ` + large + `"},{"role":"user","content":"current"}]}`)
	got, err := planner.MaterializeIfNeeded(context.Background(), "route", "acc", 1024, body)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !got.Changed {
		t.Fatalf("expected virtual materialization")
	}
	if got.MaterializedTokens == 0 || got.MaterializedTokens > 1024 {
		t.Fatalf("materialized tokens out of budget: %d", got.MaterializedTokens)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(got.Body, &root)
	// The debug/telemetry summary must NOT be written into the upstream body:
	// /v1/responses rejects unknown top-level fields and any extra field would
	// perturb the cached prompt prefix (Q2). It is returned via MaterializeResult.
	if _, leaked := root["x_virtual_2m"]; leaked {
		t.Fatalf("debug marker leaked into upstream body")
	}
	if root["input"] == nil {
		t.Fatalf("materialized body missing input")
	}
	if EstimateTokensJSON(got.Body) > EstimateTokensJSON(body) {
		t.Fatalf("materialized body grew unexpectedly")
	}
}

func TestMaterializeLeavesResponsesToolItemsVerbatim(t *testing.T) {
	planner := testPlanner(t)
	large := strings.Repeat("A", 20000)
	body := []byte(`{"model":"gpt","input":[` +
		`{"role":"user","content":"old ` + large + `"},` +
		`{"type":"reasoning","id":"rs_1","summary":[]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"guide.pdf\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"page text"},` +
		`{"role":"user","content":"continue"}` +
		`]}`)

	got, err := planner.MaterializeIfNeeded(context.Background(), "route", "acc", 1024, body)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got.Changed {
		t.Fatalf("responses tool/reasoning input must stay verbatim, got changed body: %s", got.Body)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body changed; tool call/output pairing could be invalidated\nwant: %s\n got: %s", body, got.Body)
	}
}

func TestMaterializeLeavesResponsesAttachmentsVerbatim(t *testing.T) {
	planner := testPlanner(t)
	large := strings.Repeat("A", 20000)
	body := []byte(`{"model":"gpt","input":[` +
		`{"role":"user","content":"old ` + large + `"},` +
		`{"role":"user","content":[` +
		`{"type":"input_text","text":"summarize this page"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"},` +
		`{"type":"input_file","filename":"guide.pdf","file_data":"data:application/pdf;base64,BBBB"}` +
		`]}` +
		`]}`)

	got, err := planner.MaterializeIfNeeded(context.Background(), "route", "acc", 1024, body)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got.Changed {
		t.Fatalf("responses attachment input must stay verbatim, got changed body: %s", got.Body)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("attachment body changed\nwant: %s\n got: %s", body, got.Body)
	}
}

// TestEstimateTokensRuneCountEquivalence proves the zero-allocation rune counting
// (utf8.RuneCount / utf8.RuneCountInString) returns EXACTLY the same estimate as the
// previous len([]rune(...)) implementation across ASCII, multi-byte UTF-8, emoji, and
// invalid UTF-8 bytes — so the perf change does not alter any planning/billing gate.
func TestEstimateTokensRuneCountEquivalence(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		"日本語のテキスト",
		"emoji 😀🚀 mix",
		"\xff\xfe invalid bytes \x80",
		strings.Repeat("A", 20000),
		strings.Repeat("漢", 5000),
		`{"model":"claude","messages":[{"role":"user","content":"café ☕"}]}`,
	}
	for _, s := range cases {
		oldText := int64(0)
		if s != "" {
			oldText = int64(len([]rune(s))/4 + 1)
		}
		if got := EstimateTokensText(s); got != oldText {
			t.Fatalf("EstimateTokensText(%q) = %d, old impl = %d", s, got, oldText)
		}
		oldJSON := int64(len([]rune(string([]byte(s))))/4 + 1)
		if got := EstimateTokensJSON([]byte(s)); got != oldJSON {
			t.Fatalf("EstimateTokensJSON(%q) = %d, old impl = %d", s, got, oldJSON)
		}
	}
}

func TestEstimateClaudeTokensJSONIsConservativeForASCIIAndUnicode(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(strings.Repeat("A", 300)),
		[]byte(strings.Repeat("汉", 100)),
		[]byte(`{"messages":[{"content":"emoji 😀🚀 and code {}[]"}]}`),
	} {
		if got, cheap := EstimateClaudeTokensJSON(raw), EstimateTokensJSON(raw); got < cheap {
			t.Fatalf("Claude boundary estimate %d is below scheduler estimate %d for %q", got, cheap, raw)
		}
	}
	if got := EstimateClaudeTokensJSON(nil); got != 0 {
		t.Fatalf("empty Claude estimate = %d, want 0", got)
	}
}

func testPlanner(t *testing.T) *Planner {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Virtual2MEnabled = true
	return NewPlanner(store, cfg)
}
