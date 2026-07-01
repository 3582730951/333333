package reliability

import (
	"encoding/json"
	"strings"
)

// Facts are the ground-truth signals derived from a Responses-shaped request body's
// input[] array — the real tool calls and tool results the conversation actually
// contains. They are extracted deterministically from the REQUEST (not the model's
// prose), which is why they are trustworthy: the output guard compares the model's
// claims ("I ran the tests", "I read the file") against these, and the working-state
// builder accumulates them across turns.
type Facts struct {
	ToolCalls        int      `json:"tool_calls"`
	ToolResults      int      `json:"tool_results"`
	Tools            []string `json:"tools,omitempty"`    // distinct function/tool names invoked
	Commands         []string `json:"commands,omitempty"` // shell command strings seen in tool args
	FilesSeen        []string `json:"files_seen,omitempty"`
	HasTestEvidence  bool     `json:"has_test_evidence"`  // a test command/run is present
	HasBuildEvidence bool     `json:"has_build_evidence"` // a build command/run is present
	HasLintEvidence  bool     `json:"has_lint_evidence"`  // a lint/typecheck command/run is present
	FirstUserText    string   `json:"-"`                  // conversation objective (first user turn)
	LatestUserText   string   `json:"-"`                  // current goal (last user turn)
}

// ExtractFacts parses a Responses-shaped body ({input, instructions}) and returns
// the ground-truth Facts. A bare-string input is treated as a single user turn. A
// parse failure yields zero Facts (the caller degrades gracefully — the guard simply
// has no evidence to check against, which only ever makes it more conservative).
func ExtractFacts(body []byte) Facts {
	var f Facts
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return f
	}
	switch in := root["input"].(type) {
	case string:
		f.FirstUserText, f.LatestUserText = in, in
	case []interface{}:
		for _, item := range in {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch itemType, _ := m["type"].(string); itemType {
			case "function_call":
				f.ToolCalls++
				name := strings.TrimSpace(stringOf(m["name"]))
				if name != "" {
					f.Tools = appendUnique(f.Tools, name)
				}
				f.absorbToolCall(name, stringOf(m["arguments"]))
			case "function_call_output":
				f.ToolResults++
				f.absorbEvidenceText(toolOutputText(m["output"]))
			case "message", "":
				if role, _ := m["role"].(string); role == "user" {
					txt := contentText(m["content"])
					if txt != "" {
						if f.FirstUserText == "" {
							f.FirstUserText = txt
						}
						f.LatestUserText = txt
					}
				}
			}
		}
	}
	return f
}

// absorbToolCall pulls command strings and file paths out of one function_call's
// JSON arguments and updates the evidence flags. It is tolerant of the several arg
// shapes Codex / third-party clients use (command as string or array, cmd, path,
// apply_patch bodies). The raw argument text is ALWAYS scanned for runner evidence
// and patch-file markers so extraction is robust even when the args are not parseable
// JSON (or carry an unusual encoding).
func (f *Facts) absorbToolCall(name, args string) {
	args = strings.TrimSpace(args)
	// Raw-text pass: evidence + patch file markers, regardless of JSON validity.
	f.absorbEvidenceText(name + " " + args)
	for _, p := range patchFiles(args) {
		f.FilesSeen = appendUnique(f.FilesSeen, p)
	}
	if args == "" || args[0] != '{' {
		return
	}
	var a map[string]interface{}
	if json.Unmarshal([]byte(args), &a) != nil {
		return
	}
	// command: string or array of args.
	if cmd := commandString(a["command"]); cmd != "" {
		f.Commands = appendUnique(f.Commands, cmd)
		f.absorbEvidenceText(cmd)
	}
	if cmd := commandString(a["cmd"]); cmd != "" {
		f.Commands = appendUnique(f.Commands, cmd)
		f.absorbEvidenceText(cmd)
	}
	// file paths under several common keys.
	for _, k := range []string{"path", "file_path", "filename", "file", "target_file"} {
		if p := strings.TrimSpace(stringOf(a[k])); p != "" {
			f.FilesSeen = appendUnique(f.FilesSeen, p)
		}
	}
	// apply_patch / diff bodies name files inline.
	for _, k := range []string{"patch", "input", "diff", "content"} {
		if body := stringOf(a[k]); body != "" {
			for _, p := range patchFiles(body) {
				f.FilesSeen = appendUnique(f.FilesSeen, p)
			}
			f.absorbEvidenceText(body)
		}
	}
}

// absorbEvidenceText sets the test/build/lint evidence flags when a command or tool
// result mentions a recognized runner. Matching a test RUNNER means a test was
// actually executed (pass or fail) — which is the evidence the guard requires before
// the model may claim "tests passed".
func (f *Facts) absorbEvidenceText(s string) {
	if s == "" {
		return
	}
	l := strings.ToLower(s)
	if !f.HasTestEvidence && containsAny(l, testRunners) {
		f.HasTestEvidence = true
	}
	if !f.HasBuildEvidence && containsAny(l, buildRunners) {
		f.HasBuildEvidence = true
	}
	if !f.HasLintEvidence && containsAny(l, lintRunners) {
		f.HasLintEvidence = true
	}
}

var (
	testRunners  = []string{"go test", "pytest", "py.test", "npm test", "npm run test", "yarn test", "pnpm test", "jest", "vitest", "mocha", "cargo test", "rspec", "phpunit", "gradle test", "mvn test", "dotnet test", "ctest", "unittest", "rake test", "bun test"}
	buildRunners = []string{"go build", "go install", "make", "npm run build", "yarn build", "pnpm build", "cargo build", "tsc", "webpack", "vite build", "gradle build", "mvn package", "dotnet build", "cmake", "bazel build", "gcc ", "g++ ", "clang "}
	lintRunners  = []string{"golangci-lint", "go vet", "eslint", "ruff", "flake8", "pylint", "mypy", "tsc --noemit", "tsc --noEmit", "clippy", "rubocop", "prettier --check", "black --check", "staticcheck", "gofmt -l"}
)

// commandString flattens a tool-arg command that may be a plain string or an argv
// array (["bash","-lc","go test ./..."]) into a single string.
func commandString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := stringOf(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}

// patchFiles extracts file paths from an apply_patch / unified-diff body.
func patchFiles(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "+++ ", "--- "} {
			if strings.HasPrefix(line, marker) {
				p := strings.TrimSpace(strings.TrimPrefix(line, marker))
				p = strings.TrimPrefix(p, "a/")
				p = strings.TrimPrefix(p, "b/")
				if p != "" && p != "/dev/null" {
					out = appendUnique(out, p)
				}
			}
		}
	}
	return out
}

// toolOutputText flattens a function_call_output `output` (string / array / object)
// to text for evidence scanning.
func toolOutputText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		return contentText(t)
	case map[string]interface{}:
		if s, ok := t["text"].(string); ok {
			return s
		}
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	}
	return ""
}

// contentText flattens a message content (string, or array of {text|input_text|...}
// parts) into plain text.
func contentText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var b strings.Builder
		for _, p := range t {
			m, ok := p.(map[string]interface{})
			if !ok {
				if s, ok := p.(string); ok {
					b.WriteString(s)
				}
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func stringOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func appendUnique(list []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return list
	}
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}
