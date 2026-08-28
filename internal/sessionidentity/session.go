// Package sessionidentity keeps logical CLI sessions separate from the virtual
// device that carries them. Device convergence is allowed to make several
// accounts or processes present one installation, but it must never turn that
// installation into one shared conversation.
package sessionidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/identity"
)

// PoolSessionHeader is an internal, already-pseudonymized logical-session hint
// emitted by the local gateway when the downstream protocol did not provide a
// usable session header. It is consumed as input to the account-bound projection
// and is never copied to an upstream provider.
const PoolSessionHeader = "X-Pool-Session-ID"

// Signal is the strongest stable logical-conversation signal found on a request.
// DeviceID is populated only for metadata.user_id's JSON-string representation.
type Signal struct {
	Source   string
	Value    string
	DeviceID string
}

// Present reports whether a logical-session signal was found.
func (s Signal) Present() bool { return strings.TrimSpace(s.Value) != "" }

// SessionScoped distinguishes a conversation/run signal from the weak durable
// client-installation fallback. Gateways should forward only session-scoped
// projections as PoolSessionHeader.
func (s Signal) SessionScoped() bool {
	return s.Present() && s.Source != "x-client-id" && s.Source != "x-pool-client-id"
}

// Strongest resolves session identity in descending order of specificity. This
// follows the useful part of sub2api's approach: keep the device fixed, but derive
// the outgoing session from the incoming logical session. Account-wide masked
// sessions are deliberately not used because they merge concurrent CLI contexts.
func Strongest(headers http.Header, body []byte) Signal {
	if headers != nil {
		for _, candidate := range []struct {
			source string
			name   string
		}{
			{"x-claude-code-session-id", "X-Claude-Code-Session-Id"},
			{"x-session-id", "X-Session-ID"},
			{"x-pool-session-id", PoolSessionHeader},
			{"session-id", "Session-Id"},
			{"conversation-id", "Conversation-Id"},
			{"thread-id", "Thread-Id"},
		} {
			if value := strings.TrimSpace(headers.Get(candidate.name)); value != "" {
				return Signal{Source: candidate.source, Value: value}
			}
		}
	}

	if root := decodeRoot(body); root != nil {
		if deviceID, sessionID := embeddedUserID(root); sessionID != "" {
			return Signal{Source: "metadata.user_id", Value: sessionID, DeviceID: deviceID}
		}
		if field, value := bodySessionField(root); value != "" {
			return Signal{Source: field, Value: value}
		}
		if anchor := conversationAnchor(root); anchor != "" {
			return Signal{Source: "conversation-anchor", Value: anchor}
		}
	}

	if headers != nil {
		for _, candidate := range []struct {
			source string
			name   string
		}{
			{"x-client-id", "X-Client-ID"},
			{"x-pool-client-id", "X-Pool-Client-ID"},
		} {
			if value := strings.TrimSpace(headers.Get(candidate.name)); value != "" {
				return Signal{Source: candidate.source, Value: value}
			}
		}
	}
	return Signal{}
}

// Project deterministically replaces a downstream logical session using an
// account/downstream-key scoped seed. The raw value is never returned.
func Project(seed, fallback string, headers http.Header, body []byte) string {
	return ProjectSignal(seed, fallback, Strongest(headers, body))
}

// ProjectSignal is Project for a signal already extracted by the caller.
func ProjectSignal(seed, fallback string, signal Signal) string {
	seed = strings.TrimSpace(seed)
	if seed == "" || !signal.Present() {
		return fallback
	}
	value := strings.TrimSpace(signal.Value)
	switch signal.Source {
	case "conversation-anchor":
		value = "claude-session-anchor\x00" + value
	case "x-client-id", "x-pool-client-id":
		value = "claude-client\x00" + value
	}
	return identity.DerivedUUID(seed, value)
}

// ResolveProjected is used at the final upstream boundary. A metadata session
// whose device already equals expectedDevice was produced by the cloak pass and
// must be reused verbatim so metadata.user_id and X-Claude-Code-Session-Id match.
// Every other signal is account-bound with ProjectSignal.
func ResolveProjected(seed, fallback string, headers http.Header, body []byte, expectedDevice string) string {
	signal := Strongest(headers, body)
	if signal.Source == "metadata.user_id" && signal.Present() &&
		strings.TrimSpace(signal.DeviceID) == strings.TrimSpace(expectedDevice) {
		return strings.TrimSpace(signal.Value)
	}
	return ProjectSignal(seed, fallback, signal)
}

// EmbeddedUserID parses Claude Code's current metadata.user_id JSON-string
// representation and returns its device and logical session fields.
func EmbeddedUserID(body []byte) (deviceID, sessionID string) {
	root := decodeRoot(body)
	if root == nil {
		return "", ""
	}
	return embeddedUserID(root)
}

func embeddedUserID(root map[string]json.RawMessage) (deviceID, sessionID string) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(root["metadata"], &metadata) != nil || metadata == nil {
		return "", ""
	}
	var encoded string
	if json.Unmarshal(metadata["user_id"], &encoded) != nil || strings.TrimSpace(encoded) == "" {
		return "", ""
	}
	var fields struct {
		DeviceID  string `json:"device_id"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(encoded), &fields) != nil {
		return "", ""
	}
	return strings.TrimSpace(fields.DeviceID), strings.TrimSpace(fields.SessionID)
}

func bodySessionField(root map[string]json.RawMessage) (field, value string) {
	for _, name := range []string{"session_id", "conversation_id", "thread_id", "prompt_cache_key"} {
		raw, ok := root[name]
		if !ok {
			continue
		}
		var candidate string
		if json.Unmarshal(raw, &candidate) == nil {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				return name, candidate
			}
		}
	}
	return "", ""
}

func decodeRoot(body []byte) map[string]json.RawMessage {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil
	}
	return root
}

// conversationAnchor mirrors routing.ConversationAnchor while decoding only the
// selected first item rather than materializing an entire long conversation.
func conversationAnchor(root map[string]json.RawMessage) string {
	raw := root["messages"]
	if len(raw) == 0 || string(raw) == "null" {
		raw = root["input"]
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}

	var anchor interface{}
	switch raw[0] {
	case '"':
		var text string
		if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
			return ""
		}
		anchor = text
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
			return ""
		}
		selected := items[0]
		for _, item := range items {
			var envelope struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(item, &envelope) == nil && envelope.Role == "user" {
				selected = item
				break
			}
		}
		if json.Unmarshal(selected, &anchor) != nil {
			return ""
		}
	default:
		return ""
	}
	canonical, err := json.Marshal(anchor)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16]
}
