// Package clientidentity classifies the downstream client that originated a
// request.  The package is intentionally small and deterministic: callers may
// use the result for attribution and metering, but it is never an
// authentication or authorization decision.
package clientidentity

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Client identifies a wire client family. Keep this set closed so values can
// safely be used as database dimensions and dashboard filters.
type Client string

const (
	ClientClaudeCode Client = "claude_code"
	ClientCodexCLI   Client = "codex_cli"
	ClientOpenAISDK  Client = "openai_sdk"
	ClientOther      Client = "other"
	ClientUnknown    Client = "unknown"
)

// Verbose aliases match the DTO field name and make call sites self-documenting.
const (
	ClientFamilyClaudeCode = ClientClaudeCode
	ClientFamilyCodexCLI   = ClientCodexCLI
	ClientFamilyOpenAISDK  = ClientOpenAISDK
	ClientFamilyOther      = ClientOther
	ClientFamilyUnknown    = ClientUnknown
)

// AgentClass identifies request lineage within a client process.
type AgentClass string

const (
	AgentRoot     AgentClass = "root"
	AgentSubagent AgentClass = "subagent"
	AgentUnknown  AgentClass = "unknown"
)

const (
	AgentClassRoot     = AgentRoot
	AgentClassSubagent = AgentSubagent
	AgentClassUnknown  = AgentUnknown
)

// Confidence is the evidence quality, not a probability.  High is reserved
// for protocol-native markers; medium for a reliable but indirect marker;
// low for a heuristic (usually User-Agent) or an explicit conflict.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Evidence records one normalized observation. Value is intentionally omitted
// from JSON output by default: raw identifiers can be sensitive and are not
// needed by consumers to explain a classification.
type Evidence struct {
	Source string     `json:"source"`
	Kind   string     `json:"kind"`
	Grade  Confidence `json:"grade"`
	Value  string     `json:"-"`
}

// RequestClientIdentity is the stable DTO shared by HTTP, WebSocket and body
// adapters. ClientType is retained as a compatibility alias for Client.
type RequestClientIdentity struct {
	ClientFamily           Client     `json:"client_family"`
	Client                 Client     `json:"client"`
	ClientType             Client     `json:"client_type"`
	AgentClass             AgentClass `json:"agent_class"`
	Confidence             Confidence `json:"confidence"`
	EvidenceBits           uint64     `json:"evidence_bits"`
	ConflictReason         string     `json:"conflict_reason,omitempty"`
	ClassifierVersion      string     `json:"classifier_version"`
	InboundProtocol        string     `json:"inbound_protocol,omitempty"`
	RequestedModelFamily   string     `json:"requested_model_family,omitempty"`
	ResolvedProviderFamily string     `json:"resolved_provider_family,omitempty"`
	Evidence               []Evidence `json:"evidence,omitempty"`
	Conflict               bool       `json:"conflict,omitempty"`
	ConflictKeys           []string   `json:"conflict_keys,omitempty"`
}

// Normalize fills compatibility aliases and clamps all externally supplied
// values to the closed enums. It is safe to call on a zero-value DTO.
func (i RequestClientIdentity) Normalize() RequestClientIdentity {
	if i.ClientFamily == "" {
		i.ClientFamily = i.Client
	}
	if i.ClientFamily == "" {
		i.ClientFamily = ClientUnknown
	}
	i.ClientFamily = NormalizeClient(string(i.ClientFamily))
	i.Client, i.ClientType = i.ClientFamily, i.ClientFamily
	i.AgentClass = NormalizeAgent(string(i.AgentClass))
	i.Confidence = NormalizeConfidence(string(i.Confidence))
	if i.ClassifierVersion == "" {
		i.ClassifierVersion = ClassifierVersion
	}
	return i
}

func NormalizeClient(v string) Client {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(ClientClaudeCode), "claude", "claude-code", "anthropic_messages":
		return ClientClaudeCode
	case string(ClientCodexCLI), "codex", "codex-cli", "codex_cli_rs":
		return ClientCodexCLI
	case string(ClientOpenAISDK), "openai", "openai-sdk", "sdk":
		return ClientOpenAISDK
	case string(ClientOther):
		return ClientOther
	default:
		return ClientUnknown
	}
}

func NormalizeAgent(v string) AgentClass {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "root", "main", "false", "0", "no":
		return AgentRoot
	case "subagent", "sub-agent", "true", "1", "yes":
		return AgentSubagent
	default:
		return AgentUnknown
	}
}

func NormalizeConfidence(v string) Confidence {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "high":
		return ConfidenceHigh
	case "medium", "med":
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// FromRequest extracts only protocol-native, non-secret markers. Explicit
// headers outrank heuristics; conflicting markers are retained as a diagnostic
// bit and lower confidence rather than silently choosing a winner.
func FromRequest(r *http.Request) RequestClientIdentity {
	if r == nil {
		return Unknown()
	}
	var ev []Evidence
	add := func(client Client, source, kind, value string, grade Confidence) {
		if strings.TrimSpace(value) == "" {
			return
		}
		ev = append(ev, Evidence{Source: source, Kind: kind, Value: strings.TrimSpace(value), Grade: grade})
		if kind == "client" && client != ClientUnknown {
			ev[len(ev)-1].Kind = string(client)
		}
	}
	ua := r.Header.Get("User-Agent")
	if strings.Contains(strings.ToLower(ua), "codex_cli_rs/") {
		add(ClientCodexCLI, "user_agent", "client", ua, ConfidenceLow)
	}
	if strings.Contains(strings.ToLower(ua), "claude-code") {
		add(ClientClaudeCode, "user_agent", "client", ua, ConfidenceLow)
	}
	if h := r.Header.Get("X-Anthropic-Billing-Header"); h != "" || r.Header.Get("anthropic-version") != "" {
		add(ClientClaudeCode, "anthropic_header", "client", "present", ConfidenceHigh)
	}
	if r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("OpenAI-Organization") != "" {
		add(ClientOpenAISDK, "openai_header", "client", "present", ConfidenceMedium)
	}
	if originator := r.Header.Get("Originator"); strings.Contains(strings.ToLower(originator), "codex") {
		add(ClientCodexCLI, "originator_header", "client", "present", ConfidenceHigh)
	}
	if v := r.Header.Get("X-OpenAI-Subagent"); v != "" {
		add(ClientCodexCLI, "x-openai-subagent", "client", "present", ConfidenceHigh)
		add(ClientCodexCLI, "x-openai-subagent", "agent", v, ConfidenceHigh)
	}
	if v := r.Header.Get("X-Claude-Code-Is-Subagent"); v != "" {
		add(ClientClaudeCode, "x-claude-code-is-subagent", "agent", v, ConfidenceHigh)
	}
	return resolve(ev)
}

// Resolve is the transport-neutral entry point. It intentionally accepts
// headers and an optional bounded JSON body so HTTP and WebSocket callers use
// exactly the same precedence/conflict rules.
func Resolve(headers http.Header, body []byte) RequestClientIdentity {
	return FromHeadersAndBody(headers, body, "")
}

func FromHeaders(headers http.Header) RequestClientIdentity {
	return FromHeadersAndBody(headers, nil, "")
}

func FromBody(body []byte) RequestClientIdentity { return FromHeadersAndBody(nil, body, "") }

// Classify is a convenience alias retained for callers that do not need to
// distinguish the request-oriented naming used by Resolve.
func Classify(headers http.Header, body []byte) RequestClientIdentity {
	return Resolve(headers, body)
}

// FromHeadersAndBody is the allocation-bounded adapter used by transports
// which have already consumed an http.Request (notably WebSocket upgrades).
// The body probe only examines a handful of scalar fields and never retains the
// payload. Invalid/large JSON simply contributes no body evidence.
func FromHeadersAndBody(headers http.Header, body []byte, protocol string) RequestClientIdentity {
	r := &http.Request{Header: headers}
	out := FromRequest(r)
	out.InboundProtocol = strings.ToLower(strings.TrimSpace(protocol))
	if len(body) == 0 || len(body) > 64<<10 {
		return out
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return out
	}
	var model, originator, clientFamily, clientType, agent string
	_ = json.Unmarshal(root["model"], &model)
	_ = json.Unmarshal(root["originator"], &originator)
	_ = json.Unmarshal(root["client_family"], &clientFamily)
	_ = json.Unmarshal(root["client_type"], &clientType)
	_ = json.Unmarshal(root["agent_class"], &agent)
	// Re-run resolution with body evidence so incompatible strong signals are
	// represented as an explicit conflict instead of one silently overriding the
	// other.
	if strings.Contains(strings.ToLower(originator), "codex") {
		bodyEvidence := Evidence{Source: "body_originator", Kind: string(ClientCodexCLI), Value: "present", Grade: ConfidenceHigh}
		out.Evidence = append(out.Evidence, bodyEvidence)
		out = resolve(out.Evidence)
		out.InboundProtocol = strings.ToLower(strings.TrimSpace(protocol))
	}
	// These are attribution markers, never authentication input.  They are
	// useful for transports (notably WebSocket turns) that have already scanned
	// the body into bounded top-level metadata before classification.
	for _, value := range []string{clientFamily, clientType} {
		if client := NormalizeClient(value); client != ClientUnknown {
			out.Evidence = append(out.Evidence, Evidence{Source: "body_client_family", Kind: string(client), Value: "present", Grade: ConfidenceMedium})
			out = resolve(out.Evidence)
			out.InboundProtocol = strings.ToLower(strings.TrimSpace(protocol))
			break
		}
	}
	if agent != "" {
		out.AgentClass = NormalizeAgent(agent)
		out.EvidenceBits |= EvidenceBodyMarker
	}
	if model != "" {
		out.RequestedModelFamily = modelFamily(model)
	}
	return out.Normalize()
}

func modelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "gpt"
	case strings.HasPrefix(m, "claude"):
		return "claude"
	default:
		return "unknown"
	}
}

func Unknown() RequestClientIdentity {
	return RequestClientIdentity{ClientFamily: ClientUnknown, Client: ClientUnknown, ClientType: ClientUnknown, AgentClass: AgentUnknown, Confidence: ConfidenceLow, ClassifierVersion: ClassifierVersion}
}

const ClassifierVersion = "client-identity-v1"

// Evidence bit assignments are stable storage/API dimensions. New evidence
// must use a new bit; changing an existing bit would make historical rows
// impossible to interpret.
const (
	EvidenceUserAgent uint64 = 1 << iota
	EvidenceAnthropicHeader
	EvidenceOpenAIHeader
	EvidenceSubagentMarker
	EvidenceBodyMarker
)

func resolve(ev []Evidence) RequestClientIdentity {
	if len(ev) == 0 {
		return Unknown()
	}
	out := Unknown()
	out.Evidence = append([]Evidence(nil), ev...)
	priority := map[Confidence]int{ConfidenceLow: 1, ConfidenceMedium: 2, ConfidenceHigh: 3}
	best := 0
	for _, e := range ev {
		if e.Kind == "agent" {
			continue
		}
		c := NormalizeClient(e.Kind)
		p := priority[e.Grade]
		previous := out.Client
		if p > best {
			out.ClientFamily, out.Client, out.ClientType, out.Confidence, best = c, c, c, e.Grade, p
		}
		if previous != ClientUnknown && c != ClientUnknown && c != previous {
			out.Conflict = true
			out.ConflictKeys = append(out.ConflictKeys, "client")
		}
	}
	for _, e := range ev {
		if e.Source == "x-openai-subagent" || e.Source == "x-claude-code-is-subagent" {
			out.AgentClass = NormalizeAgent(e.Value)
		}
	}
	if out.Conflict {
		out.ClientFamily, out.Client, out.ClientType = ClientUnknown, ClientUnknown, ClientUnknown
		out.Confidence = ConfidenceLow
		out.ConflictReason = "incompatible_client_signals"
	}
	for _, e := range ev {
		switch e.Source {
		case "user_agent":
			out.EvidenceBits |= EvidenceUserAgent
		case "anthropic_header":
			out.EvidenceBits |= EvidenceAnthropicHeader
		case "openai_header":
			out.EvidenceBits |= EvidenceOpenAIHeader
		case "originator_header":
			out.EvidenceBits |= EvidenceBodyMarker
		case "x-openai-subagent", "x-claude-code-is-subagent":
			out.EvidenceBits |= EvidenceSubagentMarker
		case "body_originator", "body_agent_class", "body_client_family":
			out.EvidenceBits |= EvidenceBodyMarker
		}
	}
	return out.Normalize()
}
