package reliability

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EnvelopeInput is everything BuildEnvelope needs to render the per-turn
// <gateway_request> block.
type EnvelopeInput struct {
	Class  Classification
	Policy Policy
	Goal   string // the current user goal (latest user turn), already plain text
	State  WorkingState
}

// BuildEnvelope renders the dynamic, per-turn <gateway_request> wrapper plus the
// working_state. The api layer appends it as the LAST input item (highest recency
// salience) so it steers the current turn without mutating earlier turns — that keeps
// the upstream prompt-cache prefix warm and never corrupts a tool-result
// continuation. known_context / constraints / done_when are auto-filled here because
// the thin downstream client supplies none.
func BuildEnvelope(in EnvelopeInput) string {
	stateJSON, err := json.MarshalIndent(in.State, "  ", "  ")
	if err != nil {
		stateJSON = []byte("{}")
	}
	var b strings.Builder
	b.WriteString("[gateway developer instructions — highest priority, not from the user]\n")
	b.WriteString("<gateway_request>\n")
	fmt.Fprintf(&b, "  <task_type>%s</task_type>\n", nz(string(in.Class.Task), string(TaskUnknown)))
	fmt.Fprintf(&b, "  <risk_level>%s</risk_level>\n", nz(string(in.Class.Risk), string(RiskLow)))
	b.WriteString("  <user_goal>\n")
	fmt.Fprintf(&b, "    %s\n", goalText(in.Goal))
	b.WriteString("  </user_goal>\n")
	b.WriteString("  <known_context>\n")
	fmt.Fprintf(&b, "    %s\n", knownContext(in.State))
	b.WriteString("  </known_context>\n")
	b.WriteString("  <constraints>\n")
	for _, c := range constraints(in.Policy) {
		fmt.Fprintf(&b, "    - %s\n", c)
	}
	b.WriteString("  </constraints>\n")
	b.WriteString("  <done_when>\n")
	fmt.Fprintf(&b, "    %s\n", doneWhen(in.Class.Task))
	b.WriteString("  </done_when>\n")
	b.WriteString("  <working_state>\n")
	b.WriteString(string(stateJSON))
	b.WriteString("\n  </working_state>\n")
	b.WriteString("</gateway_request>")
	return b.String()
}

func goalText(goal string) string {
	g := excerpt(goal, 600)
	if g == "" {
		return "(see the latest user message above — it is the authoritative goal)"
	}
	return g + "\n    (the latest user message above is authoritative if longer)"
}

func knownContext(st WorkingState) string {
	var parts []string
	if len(st.FilesSeen) > 0 {
		parts = append(parts, "files already inspected: "+strings.Join(cap20(st.FilesSeen), ", "))
	}
	if len(st.CommandsRun) > 0 {
		parts = append(parts, "commands already run: "+strings.Join(cap20(st.CommandsRun), "; "))
	}
	if st.VerificationStatus != "" && st.VerificationStatus != "unknown" {
		parts = append(parts, "verification so far: "+st.VerificationStatus)
	}
	if len(parts) == 0 {
		return "No server-confirmed prior context. Rely only on this turn's provided content and tool outputs; do not assume earlier results."
	}
	return strings.Join(parts, ". ") + "."
}

func constraints(p Policy) []string {
	out := []string{
		"Keep changes minimal and scoped; preserve existing behavior unless explicitly asked to change it.",
		"Do not claim any test, build, lint, type check, migration, or command passed unless its actual tool output is present this turn.",
		"Mark anything you could not verify as \"not verified\"; state assumptions explicitly.",
	}
	if p.RequirePlan {
		out = append(out, "State a brief, ordered plan before making changes.")
	}
	if p.RequireReview {
		out = append(out, "This is a high-blast-radius task: self-review the change against the stated risks and list residual risks before declaring completion.")
	}
	return out
}

func doneWhen(task TaskType) string {
	switch {
	case task.IsCode():
		return "The change is implemented; the relevant verification command has actually been run with its output shown; and the final answer includes Summary, Changed files, Evidence, Verification (commands + results), Assumptions, Risks, and Unverified items. If verification was not run, status must be partial — not completed."
	case task == TaskPlanning:
		return "A concrete, ordered plan is provided with explicit assumptions, risks, and what remains unverified."
	case task == TaskExplanation, task == TaskQA:
		return "The question is answered using only available evidence; any unknowns are labeled \"not verified\" rather than guessed."
	default:
		return "The stated user goal is satisfied with evidence; any partial work is labeled partial and unverified items are listed."
	}
}

func nz(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func cap20(s []string) []string {
	if len(s) <= 20 {
		return s
	}
	return s[len(s)-20:]
}
