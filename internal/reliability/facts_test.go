package reliability

import "testing"

func TestExtractFactsFromToolHistory(t *testing.T) {
	body := []byte(`{
		"instructions":"x",
		"input":[
			{"role":"user","content":"fix the bug and run tests"},
			{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":[\"bash\",\"-lc\",\"go test ./...\"]}"},
			{"type":"function_call_output","call_id":"c1","output":"ok  example/pkg  0.2s"},
			{"type":"function_call","name":"read_file","call_id":"c2","arguments":"{\"path\":\"internal/api/server.go\"}"}
		]
	}`)
	f := ExtractFacts(body)
	if f.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", f.ToolCalls)
	}
	if f.ToolResults != 1 {
		t.Errorf("ToolResults = %d, want 1", f.ToolResults)
	}
	if !f.HasTestEvidence {
		t.Error("HasTestEvidence = false, want true (go test was run)")
	}
	if !contains(f.Commands, "bash -lc go test ./...") {
		t.Errorf("Commands missing the joined argv: %v", f.Commands)
	}
	if !contains(f.FilesSeen, "internal/api/server.go") {
		t.Errorf("FilesSeen missing read path: %v", f.FilesSeen)
	}
	if f.FirstUserText != "fix the bug and run tests" || f.LatestUserText != "fix the bug and run tests" {
		t.Errorf("user text not captured: first=%q latest=%q", f.FirstUserText, f.LatestUserText)
	}
}

func TestExtractFactsNoTools(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"just explain something"}]}`)
	f := ExtractFacts(body)
	if f.ToolCalls != 0 || f.HasTestEvidence || f.HasBuildEvidence {
		t.Errorf("expected no evidence, got %+v", f)
	}
}

func TestExtractFactsBareStringInput(t *testing.T) {
	f := ExtractFacts([]byte(`{"input":"hello world"}`))
	if f.FirstUserText != "hello world" || f.LatestUserText != "hello world" {
		t.Errorf("bare string input not captured: %+v", f)
	}
}

func TestExtractFactsApplyPatchFiles(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\n*** Update File: pkg/a.go\n+x\n*** End Patch\"}"}
	]}`)
	f := ExtractFacts(body)
	if !contains(f.FilesSeen, "pkg/a.go") {
		t.Errorf("apply_patch file not extracted: %v", f.FilesSeen)
	}
}

func TestExtractFactsBuildAndLint(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call","name":"shell","arguments":"{\"command\":\"go build ./... && golangci-lint run\"}"}
	]}`)
	f := ExtractFacts(body)
	if !f.HasBuildEvidence {
		t.Error("HasBuildEvidence = false, want true")
	}
	if !f.HasLintEvidence {
		t.Error("HasLintEvidence = false, want true")
	}
}

func TestExtractFactsMalformedDegrades(t *testing.T) {
	f := ExtractFacts([]byte(`not json`))
	if f.ToolCalls != 0 || f.HasTestEvidence {
		t.Errorf("malformed body should yield zero facts, got %+v", f)
	}
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
