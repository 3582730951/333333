package turbo_gpt_register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const executorOutputLimit = 2 << 20

// Executor runs one durable registration phase.
type Executor interface {
	Execute(context.Context, string, ExecutorInput) (ExecutorResult, error)
}

// NodeExecutor communicates with the browser worker over stdin/stdout JSON.
// Secrets remain in stdin instead of command-line arguments.
type NodeExecutor struct {
	NodePath   string
	ScriptPath string
}

type cappedBuffer struct {
	buf bytes.Buffer
	n   int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := executorOutputLimit - b.n
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
		b.n += len(p)
	}
	return original, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

func (e NodeExecutor) Execute(ctx context.Context, phase string, input ExecutorInput) (ExecutorResult, error) {
	if strings.TrimSpace(e.ScriptPath) == "" {
		return ExecutorResult{}, errors.New("turbo register node script path is empty")
	}
	node := strings.TrimSpace(e.NodePath)
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			return ExecutorResult{}, fmt.Errorf("find node: %w", err)
		}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return ExecutorResult{}, fmt.Errorf("encode executor input: %w", err)
	}
	cmd := exec.CommandContext(ctx, node, e.ScriptPath, phase)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message != "" {
			return ExecutorResult{}, fmt.Errorf("node executor %s: %w: %s", phase, err, message)
		}
		return ExecutorResult{}, fmt.Errorf("node executor %s: %w", phase, err)
	}
	line := lastNonEmptyLine(stdout.String())
	if line == "" {
		return ExecutorResult{}, errors.New("node executor returned empty stdout")
	}
	var result ExecutorResult
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		return ExecutorResult{}, fmt.Errorf("decode node executor result: %w", err)
	}
	if !result.Success {
		if result.Error == "" {
			result.Error = "registration phase failed"
		}
		return result, errors.New(result.Error)
	}
	return result, nil
}

func lastNonEmptyLine(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
