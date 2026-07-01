// Package reliability implements the gateway's "Model Reliability Layer": the
// server-side logic that compensates for thin / zero-config downstream clients by
// injecting high-priority developer rules, classifying each request's task type and
// risk, choosing a reasoning-effort floor, maintaining per-conversation working
// state, and guarding model output against fabricated tool/test/command results.
//
// It is intentionally self-contained and dependency-free (stdlib only): every
// exported function is pure or operates on an in-memory store, so the whole package
// is unit-testable without network, DB, or the api package. The api package wires
// these primitives into the live request path behind a single runtime flag
// (gateway_reliability), which is OFF by default — when off, nothing here runs and
// the relay behaves exactly as before.
package reliability

import "strings"

// Preamble returns the static, cache-stable developer/system rules block injected
// at the highest-priority channel available (instructions for Responses, system for
// Chat). It never changes between turns, so prepending it keeps the upstream
// prompt-cache prefix warm. The dynamic, per-turn context (task/risk envelope +
// working state) is delivered separately as a trailing input item so it does not
// bust that prefix — see BuildEnvelope.
func Preamble() string { return preamble }

const preamble = `Formatting re-enabled

You are a senior software engineering and reasoning agent operating behind a company API gateway. The downstream client may be a thin CLI and may not provide complete configuration. You must compensate by being conservative, evidence-based, and verification-oriented.

Core rules:

1. Do not invent facts, file contents, command results, APIs, package versions, test results, or external information.
2. Evidence comes only from user-provided content, retrieved files, tool outputs, or explicit API-provided context.
3. Previous assistant guesses are not evidence unless confirmed by current context or tools.
4. If evidence is missing, say "not verified" instead of guessing.
5. If multiple interpretations exist, state the assumption used.
6. Never convert uncertainty into certainty for fluency.
7. Do not claim tests, builds, lint, type checks, migrations, scripts, or tools passed unless actual tool output proves it.
8. For code tasks, inspect relevant files before proposing edits when file access exists.
9. For debugging tasks, identify evidence before patching.
10. For complex tasks, plan before acting.
11. Keep changes minimal and scoped.
12. Preserve existing behavior unless explicitly asked to change it.
13. Do not present partial work as complete.

For code-related tasks, final responses must include:
- Summary
- Changed files, if any
- Evidence
- Verification commands and results
- Assumptions
- Risks
- Unverified items

Use these labels when relevant:
- Verified: supported by tool output or provided context
- Inferred: logically derived but not directly observed
- Assumed: chosen because the task lacked enough information
- Not verified: cannot be confirmed from available context`

// TaskType is the auto-detected category of a downstream request.
type TaskType string

const (
	TaskCoding      TaskType = "coding"
	TaskDebugging   TaskType = "debugging"
	TaskCodeReview  TaskType = "code_review"
	TaskRefactor    TaskType = "refactor"
	TaskTestGen     TaskType = "test_generation"
	TaskExplanation TaskType = "explanation"
	TaskPlanning    TaskType = "planning"
	TaskQA          TaskType = "qa"
	TaskUnknown     TaskType = "unknown"
)

// IsCode reports whether the task category is one whose final answer must carry the
// code-task evidence/verification fields (Summary / Changed files / Verification / …).
func (t TaskType) IsCode() bool {
	switch t {
	case TaskCoding, TaskDebugging, TaskCodeReview, TaskRefactor, TaskTestGen:
		return true
	}
	return false
}

// RiskLevel is the auto-detected blast radius of a request. It drives the reasoning
// effort floor and whether a plan / review is required.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// rank gives a total order so risk levels can be compared / floored.
func (r RiskLevel) rank() int {
	switch r {
	case RiskCritical:
		return 3
	case RiskHigh:
		return 2
	case RiskMedium:
		return 1
	default:
		return 0
	}
}

// AtLeast returns the higher of two risk levels (used to floor a classification so a
// downstream can never silently lower it).
func (r RiskLevel) AtLeast(other RiskLevel) RiskLevel {
	if other.rank() > r.rank() {
		return other
	}
	return r
}

// normalize lowercases for case-insensitive keyword matching while preserving the
// original for excerpting.
func normalize(s string) string { return strings.ToLower(s) }
