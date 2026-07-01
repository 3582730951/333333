package api

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"codex-account-pool/internal/reliability"
	"codex-account-pool/internal/routing"
)

// reliability.go wires the internal/reliability "Model Reliability Layer" into the
// live Codex/Responses request path. Everything here is gated by reliabilityEnabled
// (the "gateway_reliability" flag), which is OFF by default — when off, none of this
// runs and the relay behaves exactly as before. The heavy lifting (classification,
// envelope, guard, working_state) lives in the dependency-free internal/reliability
// package; this file only adapts it to the Server's settings overlay and request
// shapes, and holds the in-memory working_state store (same pattern as oauth/login).

const (
	// relStateTTL bounds how long an idle conversation's working_state survives; it is
	// re-derivable from the next request's tool history, so expiry only loses the
	// accumulation of turns that no longer echo their history.
	relStateTTL = 2 * time.Hour
	// relStateMax bounds the number of tracked conversations (LRU eviction beyond it).
	relStateMax = 5000
)

func (s *Server) reliabilityEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "gateway_reliability", s.cfg.GatewayReliabilityEnabled)
}

func (s *Server) reliabilityEffortFloorEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "gateway_reliability_effort_floor", s.cfg.GatewayReliabilityEffortFloor)
}

func (s *Server) reliabilityGuardMode(ctx context.Context) string {
	return strings.ToLower(s.settingString(ctx, "gateway_reliability_guard_mode", s.cfg.GatewayReliabilityGuardMode))
}

func (s *Server) reliabilityModel(ctx context.Context) string {
	return s.settingString(ctx, "gateway_reliability_model", s.cfg.GatewayReliabilityModel)
}

func (s *Server) reliabilityRepairEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "gateway_reliability_repair", s.cfg.GatewayReliabilityRepair)
}

// reliabilityEnvelopeRole selects the role of the appended per-turn envelope item.
// "developer" (default) is semantically correct and accepted across this codebase's
// request handling; an operator can switch to "user"/"system" via the settings table
// if a specific upstream rejects developer-role input items.
func (s *Server) reliabilityEnvelopeRole(ctx context.Context) string {
	switch r := strings.ToLower(s.settingString(ctx, "gateway_reliability_envelope_role", "developer")); r {
	case "user", "system", "developer":
		return r
	default:
		return "developer"
	}
}

// reliabilityTurn carries the per-request reliability context (classification +
// ground-truth facts) forward to the non-streaming response guard, so the guard does
// not have to re-parse the request.
type reliabilityTurn struct {
	Active      bool
	Class       reliability.Classification
	Facts       reliability.Facts
	Policy      reliability.Policy
	EffortFloor string
}

// applyReliabilityRequest is the request-side entry point. It expects a
// Responses-shaped body (post chat→responses conversion) and:
//   - extracts ground-truth Facts and classifies task + risk from the user goal,
//   - accumulates per-conversation working_state (keyed by the contamination-safe
//     ledger key, so sibling sub-agents are never merged),
//   - prepends the static developer preamble to instructions (cache-stable) and
//     appends the dynamic <gateway_request> envelope as a trailing high-salience item.
//
// It returns the transformed body and the turn context for the response guard.
func (s *Server) applyReliabilityRequest(ctx context.Context, body []byte, affinity routing.AffinityKey) ([]byte, reliabilityTurn) {
	facts := reliability.ExtractFacts(body)
	goal := facts.LatestUserText
	if strings.TrimSpace(goal) == "" {
		goal = facts.FirstUserText
	}
	class := reliability.Classify(goal)
	policy := reliability.PolicyFor(class.Risk)

	var state reliability.WorkingState
	if key := ledgerKey(affinity); key != "" {
		state = s.relState.Update(key, func(ws *reliability.WorkingState) { ws.MergeFacts(facts, class) })
	} else {
		// No true conversation correlator: build a one-shot state from this turn only,
		// never persisted (avoids merging unrelated requests that share a coarse key).
		state.MergeFacts(facts, class)
	}

	envelope := reliability.BuildEnvelope(reliability.EnvelopeInput{Class: class, Policy: policy, Goal: goal, State: state})
	body = injectReliabilityResponses(body, reliability.Preamble(), envelope, s.reliabilityEnvelopeRole(ctx))
	return body, reliabilityTurn{Active: true, Class: class, Facts: facts, Policy: policy, EffortFloor: policy.EffortFloor}
}

// injectReliabilityResponses prepends the preamble to instructions and appends the
// envelope as a trailing input item. Fail-open: any parse error returns body intact.
func injectReliabilityResponses(body []byte, preamble, envelope, role string) []byte {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	if instr, ok := root["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		if !strings.HasPrefix(instr, preamble) {
			root["instructions"] = preamble + "\n\n" + instr
		}
	} else {
		root["instructions"] = preamble
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return appendDeveloperTurn(out, role, envelope)
}

// appendDeveloperTurn appends a {role, content} message as the LAST input item of a
// Responses body (promoting a bare-string input to an array first). Fail-open.
func appendDeveloperTurn(body []byte, role, text string) []byte {
	if strings.TrimSpace(text) == "" {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	item := map[string]interface{}{"role": role, "content": text}
	switch in := root["input"].(type) {
	case []interface{}:
		root["input"] = append(in, item)
	case string:
		root["input"] = []interface{}{
			map[string]interface{}{"role": "user", "content": in},
			item,
		}
	default:
		root["input"] = []interface{}{item}
	}
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return body
}

// reliabilityGuardChatBody runs the output guard for a non-streaming chat response.
// It reads the assistant text from the upstream Responses body (falling back to the
// converted chat body), and if the model fabricated a tool/test/command result or
// (in strict mode) skipped required verification, prepends a downgrade notice to the
// chat body the downstream receives. Returns the (possibly downgraded) chat body.
func (s *Server) reliabilityGuardChatBody(ctx context.Context, upstreamResponses, chatBody []byte, turn reliabilityTurn) []byte {
	mode := s.reliabilityGuardMode(ctx)
	if !turn.Active || mode == "off" {
		return chatBody
	}
	text := reliability.ExtractResponseText(upstreamResponses)
	if strings.TrimSpace(text) == "" {
		text = reliability.ExtractChatText(chatBody)
	}
	findings := reliability.Guard(text, turn.Facts, turn.Class, mode == "strict")
	if len(findings) == 0 {
		return chatBody
	}
	log.Printf("[RELIABILITY] chat guard downgraded response: %d finding(s) task=%s risk=%s fabrication=%v",
		len(findings), turn.Class.Task, turn.Class.Risk, reliability.HasFabrication(findings))
	return reliability.PrependChatNotice(chatBody, reliability.DowngradeNotice(findings))
}

// reliabilityGuardResponsesBody runs the output guard for a non-streaming Responses
// response, prepending a downgrade notice to the assistant text on a finding.
func (s *Server) reliabilityGuardResponsesBody(ctx context.Context, body []byte, turn reliabilityTurn) []byte {
	mode := s.reliabilityGuardMode(ctx)
	if !turn.Active || mode == "off" {
		return body
	}
	findings := reliability.Guard(reliability.ExtractResponseText(body), turn.Facts, turn.Class, mode == "strict")
	if len(findings) == 0 {
		return body
	}
	log.Printf("[RELIABILITY] responses guard downgraded response: %d finding(s) task=%s risk=%s fabrication=%v",
		len(findings), turn.Class.Task, turn.Class.Risk, reliability.HasFabrication(findings))
	return reliability.PrependResponsesNotice(body, reliability.DowngradeNotice(findings))
}

// reliabilityFindings runs the guard and returns the findings (used by the optional
// repair path to decide whether a re-ask is warranted).
func (s *Server) reliabilityFindings(ctx context.Context, responsesBody []byte, turn reliabilityTurn) []reliability.Finding {
	mode := s.reliabilityGuardMode(ctx)
	if !turn.Active || mode == "off" {
		return nil
	}
	return reliability.Guard(reliability.ExtractResponseText(responsesBody), turn.Facts, turn.Class, mode == "strict")
}
