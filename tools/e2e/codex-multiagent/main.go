// Command codex-multiagent runs one disposable official Codex CLI container
// through pool_server and verifies the stable V1 multi-agent protocol end to end.
//
// It intentionally uses only the bearer already present in the mounted, non-secret
// E2E config. The command never reads auth.json, accepts no credential flag, and
// keeps the Codex JSONL stream in memory.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConfigPath = "tools/e2e/codex_pool_config.toml"
	defaultImage      = "pool-fingerprint-capture:20260710"
	defaultModel      = "gpt-5.6-luna"
	defaultEffort     = "low"

	childAMarker = "CHILD_A_UNIQUE_CRT"
	childBMarker = "CHILD_B_UNIQUE_ORDER"
	childAAnswer = "73"
	childBAnswer = "ZWYX"
	finalAnswer  = "MULTI_AGENT_OK|73|ZWYX"
)

type options struct {
	configPath string
	image      string
	model      string
	effort     string
	timeout    time.Duration
	dockerBin  string
}

type rootUsage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
}

type childResult struct {
	Label    string `json:"label"`
	ThreadID string `json:"thread_id"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Exact    bool   `json:"exact"`
}

type parseResult struct {
	Children []childResult
	Final    string
	Usage    rootUsage
}

type report struct {
	Passed            bool          `json:"passed"`
	Image             string        `json:"image"`
	Model             string        `json:"model"`
	Effort            string        `json:"effort"`
	MultiAgentVersion string        `json:"multi_agent_version"`
	ForkContext       bool          `json:"fork_context"`
	ChildCount        int           `json:"child_count"`
	DistinctChildren  int           `json:"distinct_children"`
	Children          []childResult `json:"children"`
	Final             string        `json:"final"`
	ExpectedFinal     string        `json:"expected_final"`
	RootUsage         rootUsage     `json:"root_usage"`
	RootCacheRate     float64       `json:"root_cache_rate"`
	DurationMS        int64         `json:"duration_ms"`
}

type eventEnvelope struct {
	Type    string     `json:"type"`
	Message string     `json:"message"`
	Error   eventError `json:"error"`
	Item    eventItem  `json:"item"`
	Usage   rootUsage  `json:"usage"`
}

type eventError struct {
	Message string `json:"message"`
}

type eventItem struct {
	Type              string                `json:"type"`
	Tool              string                `json:"tool"`
	Status            string                `json:"status"`
	Prompt            string                `json:"prompt"`
	Text              string                `json:"text"`
	ReceiverThreadIDs []string              `json:"receiver_thread_ids"`
	AgentsStates      map[string]agentState `json:"agents_states"`
}

type agentState struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type spawnRecord struct {
	Label      string
	ThreadID   string
	Expected   string
	EventIndex int
}

func main() {
	var opt options
	flag.StringVar(&opt.configPath, "config", defaultConfigPath, "read-only base Codex config (must contain only an E2E placeholder bearer)")
	flag.StringVar(&opt.image, "image", defaultImage, "Docker image containing the official Codex CLI")
	flag.StringVar(&opt.model, "model", defaultModel, "Codex model override")
	flag.StringVar(&opt.effort, "effort", defaultEffort, "reasoning effort inherited by both children")
	flag.DurationVar(&opt.timeout, "timeout", 180*time.Second, "container/Codex timeout")
	flag.StringVar(&opt.dockerBin, "docker", "docker", "Docker executable")
	flag.Parse()

	if err := normalizeOptions(&opt); err != nil {
		fatal(err)
	}
	started := time.Now()
	parsed, err := runOfficialCodex(context.Background(), opt)
	if err != nil {
		fatal(err)
	}

	ids := make(map[string]struct{}, len(parsed.Children))
	for _, child := range parsed.Children {
		ids[child.ThreadID] = struct{}{}
	}
	out := report{
		Passed:            true,
		Image:             opt.image,
		Model:             opt.model,
		Effort:            opt.effort,
		MultiAgentVersion: "v1",
		ForkContext:       false,
		ChildCount:        len(parsed.Children),
		DistinctChildren:  len(ids),
		Children:          parsed.Children,
		Final:             parsed.Final,
		ExpectedFinal:     finalAnswer,
		RootUsage:         parsed.Usage,
		DurationMS:        time.Since(started).Milliseconds(),
	}
	if parsed.Usage.Input > 0 {
		out.RootCacheRate = float64(parsed.Usage.Cached) / float64(parsed.Usage.Input)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(raw))
}

func normalizeOptions(opt *options) error {
	if opt == nil {
		return errors.New("nil options")
	}
	opt.configPath = strings.TrimSpace(opt.configPath)
	opt.image = strings.TrimSpace(opt.image)
	opt.model = strings.TrimSpace(opt.model)
	opt.effort = strings.TrimSpace(opt.effort)
	opt.dockerBin = strings.TrimSpace(opt.dockerBin)
	if opt.configPath == "" || opt.image == "" || opt.model == "" || opt.effort == "" || opt.dockerBin == "" {
		return errors.New("config, image, model, effort, and docker must be non-empty")
	}
	if opt.timeout < time.Second {
		return errors.New("timeout must be at least 1s")
	}
	if !safeConfigValue.MatchString(opt.model) || !safeConfigValue.MatchString(opt.effort) {
		return errors.New("model and effort may contain only letters, digits, dot, underscore, and dash")
	}
	abs, err := filepath.Abs(opt.configPath)
	if err != nil {
		return fmt.Errorf("resolve config: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config is not a regular file: %s", abs)
	}
	opt.configPath = abs
	if _, err := exec.LookPath(opt.dockerBin); err != nil {
		return fmt.Errorf("find Docker executable: %w", err)
	}
	return nil
}

func runOfficialCodex(parent context.Context, opt options) (parseResult, error) {
	containerName := fmt.Sprintf("codex-multiagent-%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	seconds := int((opt.timeout + time.Second - 1) / time.Second)
	args := []string{
		"run", "--rm", "--name", containerName, "--network", "host",
		"-e", "CODEX_HOME=/root/.codex",
		"-v", opt.configPath + ":/root/.codex/config.toml:ro",
		"-w", "/tmp", "--entrypoint", "timeout", opt.image,
		strconv.Itoa(seconds) + "s",
		"codex", "--enable", "multi_agent", "--disable", "multi_agent_v2",
		"exec", "--ephemeral", "--skip-git-repo-check", "--json",
		"-m", opt.model,
		"-c", "model_reasoning_effort=" + strconv.Quote(opt.effort),
		multiAgentPrompt(),
	}

	ctx, cancel := context.WithTimeout(parent, opt.timeout+20*time.Second)
	defer cancel()
	defer removeContainer(opt.dockerBin, containerName)

	cmd := exec.CommandContext(ctx, opt.dockerBin, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		diagnostic := bounded(redactSensitive(stderr.String()+"\n"+stdout.String()), 2000)
		if ctx.Err() != nil {
			return parseResult{}, fmt.Errorf("official Codex Docker timed out: %w: %s", ctx.Err(), diagnostic)
		}
		return parseResult{}, fmt.Errorf("official Codex Docker failed: %w: %s", err, diagnostic)
	}

	parsed, err := parseAndValidateJSONL(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		stderrDiagnostic := bounded(redactSensitive(stderr.String()), 800)
		if stderrDiagnostic != "" {
			return parseResult{}, fmt.Errorf("Codex JSONL assertion failed: %w; stderr: %s", err, stderrDiagnostic)
		}
		return parseResult{}, fmt.Errorf("Codex JSONL assertion failed: %w", err)
	}
	return parsed, nil
}

func multiAgentPrompt() string {
	return `This is an explicitly authorized, deterministic multi-agent transport test. You MUST use the stable V1 multi_agent_v1 tools and MUST NOT solve either child task yourself. Call spawn_agent exactly twice and copy each task below verbatim into its message. Set fork_context=false for both, and omit agent_type, model, reasoning_effort, and service_tier so both children inherit the parent model and effort. Perform both spawn calls before the first wait_agent call so the children run concurrently.

CHILD_A_UNIQUE_CRT: Find the least nonnegative integer x satisfying x mod 8 = 1, x mod 5 = 3, and x mod 7 = 3. Return only the integer, with no prose.

CHILD_B_UNIQUE_ORDER: Arrange W,X,Y,Z exactly once subject to Z immediately before W, X last, and Y before X. Return only the four letters, with no prose.

After both spawn calls have returned, call wait_agent as many times as needed until both children are completed. Then return exactly MULTI_AGENT_OK|<child A raw answer>|<child B raw answer> and nothing else. Do not use shell, Python, web, or any non-collaboration tool.`
}

func parseAndValidateJSONL(r io.Reader) (parseResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	spawns := make([]spawnRecord, 0, 2)
	completed := make(map[string]string, 2)
	firstWaitIndex := -1
	waitCalls := 0
	eventIndex := 0
	final := ""
	usage := rootUsage{}
	usageSeen := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		eventIndex++
		var event eventEnvelope
		if err := json.Unmarshal(line, &event); err != nil {
			return parseResult{}, fmt.Errorf("line %d is not valid JSON: %w", eventIndex, err)
		}
		if strings.TrimSpace(event.Type) == "" {
			return parseResult{}, fmt.Errorf("line %d has no event type", eventIndex)
		}

		switch event.Type {
		case "error":
			message := firstNonEmpty(event.Message, event.Error.Message, "unspecified stream error")
			return parseResult{}, fmt.Errorf("stream error: %s", message)
		case "turn.failed":
			return parseResult{}, fmt.Errorf("turn failed: %s", firstNonEmpty(event.Error.Message, "unspecified turn failure"))
		case "turn.completed":
			usage = event.Usage
			usageSeen = true
		case "item.started", "item.completed":
			item := event.Item
			if strings.EqualFold(item.Status, "failed") {
				return parseResult{}, fmt.Errorf("failed item: type=%s tool=%s", item.Type, item.Tool)
			}
			if item.Type == "agent_message" && event.Type == "item.completed" {
				final = item.Text
			}
			if item.Type != "collab_tool_call" {
				continue
			}
			if item.Tool == "wait" {
				if firstWaitIndex < 0 {
					firstWaitIndex = eventIndex
				}
				if event.Type == "item.completed" {
					waitCalls++
				}
			}
			if event.Type == "item.completed" && item.Tool == "spawn_agent" {
				if item.Status != "completed" {
					return parseResult{}, fmt.Errorf("spawn_agent ended with status %q", item.Status)
				}
				if len(item.ReceiverThreadIDs) != 1 || strings.TrimSpace(item.ReceiverThreadIDs[0]) == "" {
					return parseResult{}, fmt.Errorf("spawn_agent must return exactly one child id: %v", item.ReceiverThreadIDs)
				}
				label, expected, err := expectedChildForPrompt(item.Prompt)
				if err != nil {
					return parseResult{}, err
				}
				for _, existing := range spawns {
					if existing.ThreadID == item.ReceiverThreadIDs[0] {
						return parseResult{}, fmt.Errorf("spawned child ids are not distinct: %s", existing.ThreadID)
					}
					if existing.Label == label {
						return parseResult{}, fmt.Errorf("spawned duplicate child task %s", label)
					}
				}
				spawns = append(spawns, spawnRecord{Label: label, ThreadID: item.ReceiverThreadIDs[0], Expected: expected, EventIndex: eventIndex})
			}
			for threadID, state := range item.AgentsStates {
				switch state.Status {
				case "completed":
					if old, exists := completed[threadID]; exists && old != state.Message {
						return parseResult{}, fmt.Errorf("child %s reported conflicting results", threadID)
					}
					completed[threadID] = state.Message
				case "errored", "not_found", "shutdown", "interrupted":
					return parseResult{}, fmt.Errorf("child %s ended in status %s", threadID, state.Status)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return parseResult{}, fmt.Errorf("read JSONL: %w", err)
	}
	if len(spawns) != 2 {
		return parseResult{}, fmt.Errorf("completed spawn count = %d, want exactly 2", len(spawns))
	}
	if spawns[0].ThreadID == spawns[1].ThreadID {
		return parseResult{}, fmt.Errorf("spawned child ids are not distinct: %s", spawns[0].ThreadID)
	}
	if spawns[0].Label == spawns[1].Label {
		return parseResult{}, fmt.Errorf("spawned duplicate child task %s", spawns[0].Label)
	}
	if firstWaitIndex < 0 || waitCalls == 0 {
		return parseResult{}, errors.New("no completed wait_agent call observed")
	}
	if firstWaitIndex < spawns[1].EventIndex {
		return parseResult{}, errors.New("wait_agent started before both children were spawned")
	}
	if len(completed) != 2 {
		return parseResult{}, fmt.Errorf("completed child result count = %d, want exactly 2", len(completed))
	}

	children := make([]childResult, 0, 2)
	spawnedIDs := make(map[string]struct{}, 2)
	for _, spawn := range spawns {
		spawnedIDs[spawn.ThreadID] = struct{}{}
		actual, ok := completed[spawn.ThreadID]
		if !ok {
			return parseResult{}, fmt.Errorf("child %s (%s) has no completed result", spawn.ThreadID, spawn.Label)
		}
		if actual != spawn.Expected {
			return parseResult{}, fmt.Errorf("child %s result = %q, want %q", spawn.Label, actual, spawn.Expected)
		}
		children = append(children, childResult{Label: spawn.Label, ThreadID: spawn.ThreadID, Expected: spawn.Expected, Actual: actual, Exact: true})
	}
	for threadID := range completed {
		if _, ok := spawnedIDs[threadID]; !ok {
			return parseResult{}, fmt.Errorf("result from unknown child %s", threadID)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Label < children[j].Label })
	if final != finalAnswer {
		return parseResult{}, fmt.Errorf("final answer = %q, want %q", final, finalAnswer)
	}
	if !usageSeen {
		return parseResult{}, errors.New("root turn.completed usage was not emitted")
	}
	return parseResult{Children: children, Final: final, Usage: usage}, nil
}

func expectedChildForPrompt(prompt string) (string, string, error) {
	hasA := strings.Contains(prompt, childAMarker)
	hasB := strings.Contains(prompt, childBMarker)
	switch {
	case hasA && !hasB:
		return "A", childAAnswer, nil
	case hasB && !hasA:
		return "B", childBAnswer, nil
	case hasA && hasB:
		return "", "", errors.New("one spawn_agent prompt contains both child task markers")
	default:
		return "", "", errors.New("spawn_agent prompt is missing its unique child task marker")
	}
}

func removeContainer(dockerBin, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBin, "rm", "-f", name)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

var (
	safeConfigValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	redactionRules  = []struct {
		pattern     *regexp.Regexp
		replacement string
	}{
		{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"']+`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)(bearer\s+)[^\s"',}]+`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)((?:access_token|refresh_token|id_token|api_key|openai_api_key|experimental_bearer_token)"?\s*[:=]\s*"?)[^"\s,}]+`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)\b(?:sk|at)-[A-Za-z0-9._-]{6,}`), `[REDACTED_TOKEN]`},
		{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_.-]*`), `[REDACTED_JWT]`},
		{regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`), `[REDACTED_EMAIL]`},
	}
)

func redactSensitive(value string) string {
	for _, rule := range redactionRules {
		value = rule.pattern.ReplaceAllString(value, rule.replacement)
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func bounded(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "codex-multiagent:", bounded(redactSensitive(err.Error()), 2400))
	os.Exit(1)
}
