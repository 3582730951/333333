package reliability

import (
	"fmt"
	"strings"
)

// Finding is one output-guard violation: a place where the model's prose claims an
// action or result that the request's ground-truth Facts do not support, or a
// required element that is missing for the task/risk.
type Finding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Finding codes.
const (
	FindingFabricatedTest    = "fabricated_test_result"
	FindingFabricatedBuild   = "fabricated_build_result"
	FindingFabricatedCommand = "fabricated_command_execution"
	FindingFabricatedRead    = "fabricated_file_read"
	FindingCompletedNoVerify = "completed_without_verification"
	FindingMissingRiskAssump = "missing_assumptions_or_risks"
	FindingMissingSections   = "missing_required_code_sections"
)

// Guard inspects a model response's text against the ground-truth Facts and the
// request Classification, returning the violations found. The first four checks are
// high precision (a claim phrase AND absent evidence both required) and always run.
// The last three are conservative completeness checks that only run in strict mode,
// because they can false-positive on terse-but-correct answers.
func Guard(responseText string, f Facts, c Classification, strict bool) []Finding {
	t := strings.ToLower(responseText)
	if strings.TrimSpace(t) == "" {
		return nil
	}
	var out []Finding

	if containsAny(t, testClaimPhrases) && !f.HasTestEvidence {
		out = append(out, Finding{FindingFabricatedTest,
			"response claims tests passed/ran, but no test runner appears in this turn's tool results"})
	}
	if containsAny(t, buildClaimPhrases) && !f.HasBuildEvidence {
		out = append(out, Finding{FindingFabricatedBuild,
			"response claims a successful build/compile, but no build command appears in this turn's tool results"})
	}
	if containsAny(t, commandClaimPhrases) && f.ToolCalls == 0 && len(f.Commands) == 0 {
		out = append(out, Finding{FindingFabricatedCommand,
			"response claims a command was executed, but this turn carries no tool calls or command results"})
	}
	if containsAny(t, readClaimPhrases) && len(f.FilesSeen) == 0 && f.ToolCalls == 0 {
		out = append(out, Finding{FindingFabricatedRead,
			"response claims a file was read/inspected, but this turn carries no file-access tool calls"})
	}

	if strict {
		if containsAny(t, completedPhrases) && c.Task.IsCode() && !f.HasTestEvidence && !f.HasBuildEvidence && !mentionsVerification(t) {
			out = append(out, Finding{FindingCompletedNoVerify,
				"response is presented as completed for a code task without any verification evidence or a verification section"})
		}
		if (c.Risk == RiskHigh || c.Risk == RiskCritical) && !mentionsAssumptionsOrRisks(t) {
			out = append(out, Finding{FindingMissingRiskAssump,
				"high/critical-risk task response does not state assumptions or risks"})
		}
		if c.Task.IsCode() && len(responseText) > 400 && !mentionsCodeSections(t) {
			out = append(out, Finding{FindingMissingSections,
				"code-task response is missing the required Changed files / Verification / Risks sections"})
		}
	}
	return out
}

// HasFabrication reports whether any finding is a hard fabrication (vs a softer
// completeness gap). The api layer uses this to decide between a strong downgrade and
// a mild annotation.
func HasFabrication(findings []Finding) bool {
	for _, f := range findings {
		switch f.Code {
		case FindingFabricatedTest, FindingFabricatedBuild, FindingFabricatedCommand, FindingFabricatedRead:
			return true
		}
	}
	return false
}

// RepairInstruction renders the spec's repair prompt for a re-ask, listing the
// concrete failures. The api layer appends this as one extra developer/user turn for
// a single repair attempt (non-streaming paths only).
func RepairInstruction(findings []Finding) string {
	return "Your previous response failed gateway validation.\n\nFailure:\n" +
		failureList(findings) +
		"\n\nYou must repair the response without inventing new facts.\n\n" +
		"Rules:\n" +
		"- Do not claim any command was run unless present in tool_results.\n" +
		"- Do not claim any file was inspected unless present in retrieved_context.\n" +
		"- If verification is missing, mark status as partial or not verified.\n" +
		"- Preserve useful content.\n" +
		"- Return only the required final response format."
}

// DowngradeNotice renders the banner the gateway prepends to a response it could not
// (or chose not to) repair, converting a fabricated "completed" into an explicit
// not-verified state. It introduces no new facts — it only removes false certainty.
func DowngradeNotice(findings []Finding) string {
	var b strings.Builder
	b.WriteString("⚠️ Gateway reliability notice — status downgraded to: NOT VERIFIED / PARTIAL.\n")
	b.WriteString("The following claims could not be confirmed from this turn's tool results and must be treated as unverified:\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  - [%s] %s\n", f.Code, f.Detail)
	}
	b.WriteString("Re-run the relevant commands and rely only on their actual output before treating any result as done.\n")
	b.WriteString("———\n")
	return b.String()
}

func failureList(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Code, f.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	testClaimPhrases    = []string{"tests pass", "tests passed", "all tests pass", "test suite pass", "tests are passing", "passing tests", "all green", "tests succeed", "✓ test", "测试通过", "测试都通过", "全部测试通过", "测试全部通过"}
	buildClaimPhrases   = []string{"build succeeded", "build passes", "builds successfully", "compiles cleanly", "compiled successfully", "compiles without", "构建成功", "编译通过", "编译成功"}
	commandClaimPhrases = []string{"i ran ", "i've run", "i have run", "i executed", "i've executed", "ran the command", "executed the command", "after running", "when i ran", "运行了", "执行了", "我跑了", "已运行", "已执行"}
	readClaimPhrases    = []string{"i read the", "i've read", "i have read", "i inspected the", "i looked at the file", "i reviewed the file", "after reading the", "reading the file", "i opened the file", "读取了文件", "查看了文件", "我看了文件", "阅读了文件"}
	completedPhrases    = []string{"completed", "is complete", "task is done", "all done", "finished", "ready to merge", "ready for review", "fully implemented", "已完成", "完成了", "搞定", "已实现"}
)

func mentionsVerification(t string) bool {
	return containsAny(t, []string{"verification", "verified", "i ran", "test output", "command output", "验证", "verify"})
}

func mentionsAssumptionsOrRisks(t string) bool {
	return containsAny(t, []string{"assumption", "assume", "risk", "假设", "风险", "not verified", "unverified"})
}

func mentionsCodeSections(t string) bool {
	// Require at least two of the expected code-task section labels to be present.
	hits := 0
	for _, s := range []string{"changed file", "summary", "verification", "risk", "evidence", "assumption", "unverified", "变更", "验证", "风险", "证据", "假设"} {
		if strings.Contains(t, s) {
			hits++
		}
	}
	return hits >= 2
}
