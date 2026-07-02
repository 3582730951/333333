package reliability

import (
	"strings"
	"testing"
)

func TestGuardFlagsFabricatedTests(t *testing.T) {
	// Model claims tests pass but the request carries no test runner.
	findings := Guard("All tests pass and the feature is complete.", Facts{}, Classification{Task: TaskCoding, Risk: RiskMedium}, false)
	if !hasCode(findings, FindingFabricatedTest) {
		t.Fatalf("expected fabricated_test_result, got %+v", findings)
	}
	if !HasFabrication(findings) {
		t.Fatal("HasFabrication should be true")
	}
}

func TestGuardAcceptsTestsWithEvidence(t *testing.T) {
	// Same claim, but this turn actually ran the tests → no finding.
	f := Facts{HasTestEvidence: true, ToolCalls: 1, Commands: []string{"go test ./..."}}
	findings := Guard("All tests pass.", f, Classification{Task: TaskCoding, Risk: RiskMedium}, false)
	if hasCode(findings, FindingFabricatedTest) {
		t.Fatalf("should not flag tests when evidence present: %+v", findings)
	}
}

func TestGuardFlagsFabricatedCommandAndRead(t *testing.T) {
	findings := Guard("I ran the migration and I read the config file.", Facts{}, Classification{Task: TaskCoding, Risk: RiskMedium}, false)
	if !hasCode(findings, FindingFabricatedCommand) {
		t.Errorf("expected fabricated_command_execution: %+v", findings)
	}
	if !hasCode(findings, FindingFabricatedRead) {
		t.Errorf("expected fabricated_file_read: %+v", findings)
	}
}

func TestGuardReadAcceptedWithFiles(t *testing.T) {
	f := Facts{FilesSeen: []string{"config.go"}, ToolCalls: 1}
	findings := Guard("I read the config file and it sets the port.", f, Classification{Task: TaskCoding}, false)
	if hasCode(findings, FindingFabricatedRead) {
		t.Fatalf("should not flag file read when a file was actually accessed: %+v", findings)
	}
}

func TestGuardStrictCompletedWithoutVerification(t *testing.T) {
	findings := Guard("The task is complete and the change is fully implemented.", Facts{}, Classification{Task: TaskCoding, Risk: RiskMedium}, true)
	if !hasCode(findings, FindingCompletedNoVerify) {
		t.Fatalf("expected completed_without_verification in strict mode: %+v", findings)
	}
	// Lenient mode must NOT raise the completeness finding.
	lenient := Guard("The task is complete and the change is fully implemented.", Facts{}, Classification{Task: TaskCoding, Risk: RiskMedium}, false)
	if hasCode(lenient, FindingCompletedNoVerify) {
		t.Fatalf("lenient mode should not raise completeness finding: %+v", lenient)
	}
}

func TestGuardStrictMissingRisksOnCritical(t *testing.T) {
	findings := Guard("Here is the patch.", Facts{}, Classification{Task: TaskCoding, Risk: RiskCritical}, true)
	if !hasCode(findings, FindingMissingRiskAssump) {
		t.Fatalf("expected missing_assumptions_or_risks for critical task: %+v", findings)
	}
	// With assumptions/risks mentioned, no finding.
	ok := Guard("Here is the patch. Assumptions: none. Risks: could affect auth.", Facts{}, Classification{Task: TaskCoding, Risk: RiskCritical}, true)
	if hasCode(ok, FindingMissingRiskAssump) {
		t.Fatalf("should not flag when assumptions/risks present: %+v", ok)
	}
}

func TestGuardCleanResponseNoFindings(t *testing.T) {
	f := Facts{HasTestEvidence: true, FilesSeen: []string{"a.go"}, ToolCalls: 2, Commands: []string{"go test ./..."}}
	resp := "Summary: fixed it. Changed files: a.go. Verification: go test ./... passed. Assumptions: none. Risks: low. Unverified items: none."
	findings := Guard(resp, f, Classification{Task: TaskCoding, Risk: RiskHigh}, true)
	if len(findings) != 0 {
		t.Fatalf("clean, evidenced response should have no findings: %+v", findings)
	}
}

func TestGuardEmptyTextNoFindings(t *testing.T) {
	if f := Guard("   ", Facts{}, Classification{Task: TaskCoding, Risk: RiskCritical}, true); len(f) != 0 {
		t.Fatalf("empty text should yield no findings: %+v", f)
	}
}

func TestRepairInstructionListsFailures(t *testing.T) {
	instr := RepairInstruction([]Finding{{FindingFabricatedTest, "no test evidence"}})
	for _, want := range []string{"failed gateway validation", "fabricated_test_result", "without inventing new facts", "partial or not verified"} {
		if !strings.Contains(instr, want) {
			t.Errorf("repair instruction missing %q", want)
		}
	}
}

func TestDowngradeNoticeRemovesCertainty(t *testing.T) {
	notice := DowngradeNotice([]Finding{{FindingFabricatedTest, "no test evidence"}})
	if !strings.Contains(notice, "NOT VERIFIED") {
		t.Fatal("downgrade notice must mark status not verified")
	}
	if !strings.Contains(notice, "fabricated_test_result") {
		t.Fatal("downgrade notice must list the finding code")
	}
}

func hasCode(findings []Finding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
