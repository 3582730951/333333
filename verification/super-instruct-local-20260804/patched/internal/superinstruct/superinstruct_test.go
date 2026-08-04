package superinstruct

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBundledHeadlessResourcesMatchSourceProject(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "other", "Super-Instruct-Codex-5.6")
	bundledRoot := filepath.Join("..", "..", "super-instruct")
	for _, rel := range []string{"bridge.md"} {
		source, err := os.ReadFile(filepath.Join(sourceRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		bundled, err := os.ReadFile(filepath.Join(bundledRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(source, bundled) {
			t.Fatalf("bundled %s differs from source project", rel)
		}
	}
	sourceSkills, err := filepath.Glob(filepath.Join(sourceRoot, "codex-skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceSkills) != 28 {
		t.Fatalf("source skill count=%d, want 28", len(sourceSkills))
	}
	for _, sourcePath := range sourceSkills {
		rel, err := filepath.Rel(sourceRoot, sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		bundled, err := os.ReadFile(filepath.Join(bundledRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(source, bundled) {
			t.Fatalf("bundled skill differs from source: %s", rel)
		}
	}
	if got := DefaultTamperEngine().RuleCount(); got != len(DefaultTamperPatterns()) || got != 26 {
		t.Fatalf("compiled M3 rule count=%d patterns=%d, want source tree's 26", got, len(DefaultTamperPatterns()))
	}
}

func TestLibraryListAndCompileSelectedSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", `---
name: alpha-name
description: Alpha skill description
---
# Alpha
Alpha directive.`)
	writeSkill(t, dir, "beta", `---
name: beta-name
description: Beta skill description
---
# Beta
Beta directive.`)
	if err := os.MkdirAll(filepath.Join(dir, "beta", "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beta", "scripts", "helper.py"), []byte("print('beta helper')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	lib := New(dir)
	skills, err := lib.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].ID != "alpha" || skills[0].Name != "alpha-name" || skills[0].FileCount != 1 {
		t.Fatalf("unexpected skills: %+v", skills)
	}

	compiled, selected, err := lib.Compile(context.Background(), []string{"beta", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != "beta" || !selected[0].Enabled {
		t.Fatalf("unexpected selected: %+v", selected)
	}
	if !strings.Contains(compiled, "Super-Instruct Codex 5.6") ||
		!strings.Contains(compiled, "Beta directive") ||
		!strings.Contains(compiled, "scripts/helper.py") ||
		!strings.Contains(compiled, "print('beta helper')") ||
		strings.Contains(compiled, "Alpha directive") {
		t.Fatalf("compiled selected bundle mismatch:\n%s", compiled)
	}

	compiled, selected, err = lib.Compile(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || !strings.Contains(compiled, "Alpha directive") || !strings.Contains(compiled, "Beta directive") {
		t.Fatalf("compiled all bundle mismatch selected=%+v:\n%s", selected, compiled)
	}
	if strings.Contains(compiled, "scripts/helper.py") || strings.Contains(compiled, "print('beta helper')") {
		t.Fatalf("implicit all-skills bundle must contain SKILL.md only:\n%s", compiled)
	}
}

func TestNormalizeSkillIDsRejectsPathTraversal(t *testing.T) {
	if got, err := NormalizeSkillIDs([]string{" alpha ", "alpha", "beta_2"}); err != nil || len(got) != 2 || got[0] != "alpha" || got[1] != "beta_2" {
		t.Fatalf("NormalizeSkillIDs = %#v, %v", got, err)
	}
	for _, value := range []string{"../x", "x/y", "x.y", "x y"} {
		if _, err := NormalizeSkillIDs([]string{value}); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestExtractUserSourceMatchesInputPrecedenceAndFiltering(t *testing.T) {
	raw := []byte(`{
  "input":[
    {"role":"system","content":"ignore system"},
    {"role":"user","content":[{"type":"input_text","text":"reverse SAMPLE"},{"type":"input_text","text":"<environment_context>ignore</environment_context>"}]}
  ],
  "messages":[{"role":"user","content":"ignore messages when input is an array"}]
}`)
	if got := ExtractUserSource(raw); got != "reverse SAMPLE" {
		t.Fatalf("source extractor=%q", got)
	}
	if got := Categorize(ExtractUserSource(raw)); got != CategoryReverse {
		t.Fatalf("source category=%q", got)
	}
}

func TestResponseProcessorTamperMemoryMonitor(t *testing.T) {
	memory := NewMemoryKernel(filepath.Join(t.TempDir(), "memory.json"))
	monitor := NewMonitorPanel()
	updates, cancelUpdates := monitor.Subscribe(2)
	defer cancelUpdates()
	processor := NewProcessor(memory, monitor)
	meta := RequestMeta{
		UserMessage: "please bypass license activation",
		Category:    Categorize("please bypass license activation"),
		Path:        "/v1/responses",
		Timestamp:   time.Unix(100, 0).UTC(),
	}
	refusal := []byte(`{"output_text":"I can't assist with that request to bypass license activation."}`)
	result := processor.Process(meta, 200, refusal, 25*time.Millisecond, ProcessOptions{
		ResponseRewriteEnabled: true,
		MemoryEnabled:          true,
		MonitorEnabled:         true,
	})
	if !result.Tampered || !strings.Contains(string(result.Body), "Rei Protocol") {
		t.Fatalf("tamper result mismatch: tampered=%v body=%s", result.Tampered, result.Body)
	}
	if got := memory.SuccessCount(); got != 0 {
		t.Fatalf("tampered response should not be learned, got %d", got)
	}
	snap := monitor.Snapshot(memory.SuccessCount())
	if snap.Stats.Total != 1 || snap.Stats.Tamper != 1 || len(snap.History) != 1 || !snap.History[0].Tampered {
		t.Fatalf("monitor snapshot mismatch: %+v", snap)
	}
	select {
	case update := <-updates:
		if !update.Interaction.Tampered || update.Stats.Total != 1 || update.Stats.Tamper != 1 {
			t.Fatalf("headless M6 event mismatch: %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("headless M6 event was not published")
	}

	success := []byte(`{"output_text":"This is a sufficiently long successful answer that should be learned by the memory kernel and retained for later observation."}`)
	result = processor.Process(RequestMeta{
		UserMessage: "reverse this binary safely",
		Category:    Categorize("reverse this binary safely"),
		Path:        "/v1/responses",
		Timestamp:   time.Unix(101, 0).UTC(),
	}, 200, success, 10*time.Millisecond, ProcessOptions{
		MemoryEnabled:  true,
		MonitorEnabled: true,
	})
	if result.Tampered || !strings.Contains(string(result.Body), "sufficiently long successful answer") {
		t.Fatalf("success result mismatch: tampered=%v body=%s", result.Tampered, result.Body)
	}
	mem := memory.Snapshot()
	if mem.Stats.Total != 1 || mem.Stats.Reverse != 1 || len(mem.Successes) != 1 || mem.Patterns["reverse"] != 1 {
		t.Fatalf("memory snapshot mismatch: %+v", mem)
	}
	snap = monitor.Snapshot(memory.SuccessCount())
	if snap.Stats.Total != 2 || snap.Stats.Tamper != 1 || snap.Stats.MemoryCount != 1 || len(snap.History) != 2 {
		t.Fatalf("monitor snapshot after success mismatch: %+v", snap)
	}
}

func writeSkill(t *testing.T, root, id, content string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
