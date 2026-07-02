package reliability

import (
	"strings"
	"testing"
)

func TestClassifyTask(t *testing.T) {
	cases := []struct {
		text string
		want TaskType
	}{
		{"implement a login endpoint", TaskCoding},
		{"please add a new function to parse config", TaskCoding},
		{"write a unit test for the parser", TaskTestGen},
		{"add tests for the auth module", TaskTestGen},
		{"can you review this code for me", TaskCodeReview},
		{"there's a bug, it crashes on startup", TaskDebugging},
		{"this fails with a traceback", TaskDebugging},
		{"refactor the storage layer", TaskRefactor},
		{"design the architecture for a queue", TaskPlanning},
		{"explain how goroutines work", TaskExplanation},
		{"what is a closure?", TaskExplanation},
		{"is it sunny today?", TaskQA},
		{"", TaskUnknown},
		{"asdf qwer zxcv", TaskUnknown},
	}
	for _, c := range cases {
		got := Classify(c.text).Task
		if got != c.want {
			t.Errorf("Classify(%q).Task = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestClassifyRisk(t *testing.T) {
	cases := []struct {
		text string
		want RiskLevel
	}{
		{"add authentication to the login flow", RiskCritical},
		{"change the payment processing logic", RiskCritical},
		{"run a database migration", RiskCritical},
		{"rotate the encryption key", RiskCritical},
		{"refactor across multiple files", RiskHigh},
		{"fix a race condition in the scheduler", RiskHigh},
		{"improve performance of the deploy pipeline", RiskHigh},
		{"implement a small helper", RiskMedium},
		{"fix the typo handling", RiskMedium},
		{"format this file", RiskLow},
		{"explain what this does", RiskLow},
		{"write documentation", RiskLow},
	}
	for _, c := range cases {
		got := Classify(c.text).Risk
		if got != c.want {
			t.Errorf("Classify(%q).Risk = %q, want %q (signals=%v)", c.text, got, c.want, Classify(c.text).RiskSignals)
		}
	}
}

func TestClassifyRiskHighestWins(t *testing.T) {
	// "refactor" alone is high, but with "auth" present it must escalate to critical.
	got := Classify("refactor the auth layer").Risk
	if got != RiskCritical {
		t.Fatalf("risk = %q, want critical", got)
	}
}

func TestClassifyCodeTaskFloorsToMedium(t *testing.T) {
	// A code task with no explicit risk keyword should floor to medium, not low.
	got := Classify("write a function that adds two numbers").Risk
	if got != RiskMedium {
		t.Fatalf("risk = %q, want medium (code task default)", got)
	}
}

func TestWordBoundaryNoFalsePositive(t *testing.T) {
	// "author" must not trigger the "auth" critical keyword.
	c := Classify("explain who the author of this file is")
	if c.Risk == RiskCritical {
		t.Fatalf("'author' falsely matched 'auth' → critical: signals=%v", c.RiskSignals)
	}
}

func TestPolicyFor(t *testing.T) {
	cases := []struct {
		risk      RiskLevel
		effort    string
		plan, rev bool
	}{
		{RiskCritical, "xhigh", true, true},
		{RiskHigh, "high", true, false},
		{RiskMedium, "medium", true, false},
		{RiskLow, "low", false, false},
	}
	for _, c := range cases {
		p := PolicyFor(c.risk)
		if p.EffortFloor != c.effort || p.RequirePlan != c.plan || p.RequireReview != c.rev {
			t.Errorf("PolicyFor(%q) = %+v, want effort=%s plan=%v review=%v", c.risk, p, c.effort, c.plan, c.rev)
		}
	}
}

func TestMaxEffort(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"low", "high", "high"},
		{"high", "low", "high"},
		{"xhigh", "medium", "xhigh"},
		{"", "medium", "medium"},
		{"high", "", "high"},
		{"", "", ""},
		{"bogus", "low", "low"},
		{"medium", "medium", "medium"},
	}
	for _, c := range cases {
		if got := MaxEffort(c.a, c.b); got != c.want {
			t.Errorf("MaxEffort(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestEffortFloorNeverDowngrades(t *testing.T) {
	// A high-risk task's floor must not lower an operator-forced xhigh.
	floor := PolicyFor(RiskHigh).EffortFloor // "high"
	if got := MaxEffort("xhigh", floor); got != "xhigh" {
		t.Fatalf("forced xhigh was downgraded by high floor: got %q", got)
	}
	// And it must RAISE a too-low forced effort up to the floor for critical.
	floor = PolicyFor(RiskCritical).EffortFloor // "xhigh"
	if got := MaxEffort("low", floor); got != "xhigh" {
		t.Fatalf("critical floor failed to raise low effort: got %q", got)
	}
}

func TestPreambleStable(t *testing.T) {
	if Preamble() != Preamble() {
		t.Fatal("Preamble must be stable across calls (cache safety)")
	}
	if !strings.Contains(Preamble(), "Do not invent facts") {
		t.Fatal("Preamble missing core rule")
	}
}

func TestBuildEnvelopeContainsCoreFields(t *testing.T) {
	env := BuildEnvelope(EnvelopeInput{
		Class:  Classification{Task: TaskCoding, Risk: RiskHigh},
		Policy: PolicyFor(RiskHigh),
		Goal:   "implement the cache layer",
		State:  WorkingState{Objective: "build cache", VerificationStatus: "unknown"},
	})
	for _, want := range []string{
		"<gateway_request>", "<task_type>coding</task_type>", "<risk_level>high</risk_level>",
		"implement the cache layer", "<working_state>", "ordered plan", "not verified",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("envelope missing %q\n---\n%s", want, env)
		}
	}
}

func TestBuildEnvelopeReviewConstraintOnlyWhenRequired(t *testing.T) {
	low := BuildEnvelope(EnvelopeInput{Class: Classification{Task: TaskExplanation, Risk: RiskLow}, Policy: PolicyFor(RiskLow)})
	if strings.Contains(low, "self-review") {
		t.Fatal("low-risk envelope should not require self-review")
	}
	crit := BuildEnvelope(EnvelopeInput{Class: Classification{Task: TaskCoding, Risk: RiskCritical}, Policy: PolicyFor(RiskCritical)})
	if !strings.Contains(crit, "self-review") {
		t.Fatal("critical envelope must require self-review")
	}
}
