package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/identity"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesLiteHeader       = "x-openai-internal-codex-responses-lite"
	codexWSResponsesLiteMetadata   = "ws_request_header_x_openai_internal_codex_responses_lite"
	codexWSRequestStartMetadata    = "x-codex-ws-stream-request-start-ms"
	codexInstallationIDMetadataKey = "x-codex-installation-id"
	codexSubagentHeader            = "x-openai-subagent"
	codexBetaFeaturesHeader        = "remote_compaction_v2"
)

// codexRequestMetadata is the canonical per-request identity snapshot mirrored
// from codex-rs/core/src/responses_metadata.rs. The same value drives HTTP/WS
// compatibility headers and client_metadata so no transport exposes a different
// session, thread, turn, or window identity.
type codexRequestMetadata struct {
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
	parentThreadID string
	subagent       string
	turnState      string
	turnMetadata   string
	responsesLite  bool
}

// Keep the old internal name as an alias for focused WS tests and helpers while
// making clear that the identity is now shared by HTTP and WebSocket transports.
type codexWebSocketIDs = codexRequestMetadata

func (c *Client) newCodexRequestMetadata(spec Request) codexRequestMetadata {
	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)
	bodyMetadata := codexBodyClientMetadata(spec.Body)
	incomingTurn := codexIncomingTurnMetadata(spec, bodyMetadata)

	rawWindowID := firstNonEmpty(
		getHeaderFold(spec.Headers, "x-codex-window-id"),
		codexMetadataString(bodyMetadata, "x-codex-window-id"),
		codexMapString(incomingTurn, "window_id"),
	)
	threadSeed := firstNonEmpty(
		getHeaderFold(spec.Headers, "thread-id"),
		getHeaderFold(spec.Headers, "x-client-request-id"),
		codexMetadataString(bodyMetadata, "thread_id"),
		codexMapString(incomingTurn, "thread_id"),
		codexBodyString(spec.Body, "thread_id"),
		codexBodyString(spec.Body, "session_id"),
		codexBodyString(spec.Body, "conversation_id"),
		threadIDFromWindowID(rawWindowID),
		codexBodyString(spec.Body, "prompt_cache_key"),
		codexBodyString(spec.Body, "previous_response_id"),
		codexRunCorrelator(spec.Headers),
		id.SessionID,
	)
	threadID := identity.DerivedUUIDv7(id.MachineID+"\x00thread", threadSeed)
	// The current Codex protocol converts ThreadId directly into SessionId, so the
	// two UUIDv7 values are intentionally identical (as is x-client-request-id).
	sessionID := threadID
	windowID := threadID + ":" + codexWindowOrdinal(rawWindowID)

	requestKind := codexRequestKind(spec, incomingTurn)
	startedAt := int64(0)
	turnID := ""
	if requestKind != "prewarm" {
		startedAt = codexMapInt64(incomingTurn, "turn_started_at_unix_ms")
		if startedAt <= 0 {
			startedAt = time.Now().UnixMilli()
		}
	}
	rawTurnID := firstNonEmpty(
		codexMetadataString(bodyMetadata, "turn_id"),
		codexMapString(incomingTurn, "turn_id"),
	)
	if requestKind != "prewarm" {
		if rawTurnID != "" {
			turnID = identity.DerivedUUIDv7(id.MachineID+"\x00turn", rawTurnID)
		} else {
			turnSeed := fmt.Sprintf("%s:%d", threadID, startedAt)
			turnID = identity.DerivedUUIDv7At(id.MachineID+"\x00turn", turnSeed, startedAt)
		}
	}

	rawParent := firstNonEmpty(
		getHeaderFold(spec.Headers, "x-codex-parent-thread-id"),
		codexMetadataString(bodyMetadata, "x-codex-parent-thread-id"),
		codexMapString(incomingTurn, "parent_thread_id"),
	)
	parentThreadID := ""
	if rawParent != "" {
		parentThreadID = identity.DerivedUUIDv7(id.MachineID+"\x00parent", rawParent)
	}

	metadata := codexRequestMetadata{
		installationID: id.MachineID,
		sessionID:      sessionID,
		threadID:       threadID,
		turnID:         turnID,
		windowID:       windowID,
		parentThreadID: parentThreadID,
		subagent: firstNonEmpty(
			getHeaderFold(spec.Headers, codexSubagentHeader),
			codexMetadataString(bodyMetadata, codexSubagentHeader),
		),
		turnState: firstNonEmpty(
			getHeaderFold(spec.Headers, "x-codex-turn-state"),
			codexMetadataString(bodyMetadata, "x-codex-turn-state"),
		),
		responsesLite: capability.CodexUsesResponsesLite(codexBodyString(spec.Body, "model")),
	}
	metadata.turnMetadata = buildCodexTurnMetadata(metadata, incomingTurn, requestKind, startedAt)
	return metadata
}

func applyCodexClientMetadata(raw []byte, metadata codexRequestMetadata, websocket bool) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	out := raw
	set := func(path string, value interface{}) bool {
		var err error
		out, err = sjson.SetBytes(out, path, value)
		return err == nil
	}
	del := func(path string) bool {
		var err error
		out, err = sjson.DeleteBytes(out, path)
		return err == nil
	}
	for _, key := range []string{
		codexInstallationIDMetadataKey,
		"session_id",
		"thread_id",
		"turn_id",
		"x-codex-window-id",
		codexSubagentHeader,
		"x-codex-parent-thread-id",
		"x-codex-turn-metadata",
		"x-codex-turn-state",
		codexWSResponsesLiteMetadata,
		codexWSRequestStartMetadata,
	} {
		if !del("client_metadata." + key) {
			return raw
		}
	}
	for _, field := range []struct{ path, value string }{
		{codexInstallationIDMetadataKey, metadata.installationID},
		{"session_id", metadata.sessionID},
		{"thread_id", metadata.threadID},
		{"x-codex-window-id", metadata.windowID},
	} {
		if !set("client_metadata."+field.path, field.value) {
			return raw
		}
	}
	for _, field := range []struct{ path, value string }{
		{"turn_id", metadata.turnID},
		{codexSubagentHeader, metadata.subagent},
		{"x-codex-parent-thread-id", metadata.parentThreadID},
		{"x-codex-turn-metadata", metadata.turnMetadata},
	} {
		if field.value != "" && !set("client_metadata."+field.path, field.value) {
			return raw
		}
	}
	if websocket {
		if metadata.turnState != "" && !set("client_metadata.x-codex-turn-state", metadata.turnState) {
			return raw
		}
		if metadata.responsesLite && !set("client_metadata."+codexWSResponsesLiteMetadata, "true") {
			return raw
		}
	}
	return out
}

func stampCodexWebSocketRequestStart(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	out, err := sjson.SetBytes(raw, "client_metadata."+codexWSRequestStartMetadata, fmt.Sprintf("%d", time.Now().UnixMilli()))
	if err != nil {
		return raw
	}
	return out
}

// stripCodexTopLevelTransportCorrelators removes downstream routing/session hints
// that are not members of the Responses API request schema. Their values have
// already been projected into canonical headers and client_metadata by the time
// this runs. Keeping them at the top level makes the live backend reject an
// otherwise valid request (for example, "Unsupported parameter: thread_id").
// Targeted deletes leave all actual prompt/context bytes untouched.
func stripCodexTopLevelTransportCorrelators(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	out := raw
	for _, key := range []string{"thread_id", "session_id", "conversation_id"} {
		if _, present := fields[key]; !present {
			continue
		}
		updated, err := sjson.DeleteBytes(out, key)
		if err != nil {
			return raw
		}
		out = updated
	}
	return out
}

func buildCodexTurnMetadata(metadata codexRequestMetadata, incoming map[string]interface{}, requestKind string, startedAt int64) string {
	turn := make(map[string]interface{}, len(incoming)+10)
	for key, value := range incoming {
		turn[key] = value
	}
	for _, key := range []string{
		"installation_id",
		codexInstallationIDMetadataKey,
		"session_id",
		"thread_id",
		"turn_id",
		"window_id",
		"x-codex-window-id",
		"x-codex-turn-metadata",
		"x-codex-parent-thread-id",
		codexSubagentHeader,
		"request_kind",
		"turn_started_at_unix_ms",
		"forked_from_thread_id",
		"parent_thread_id",
	} {
		delete(turn, key)
	}
	turn["installation_id"] = metadata.installationID
	turn["session_id"] = metadata.sessionID
	turn["thread_id"] = metadata.threadID
	if metadata.turnID != "" {
		turn["turn_id"] = metadata.turnID
	}
	turn["window_id"] = metadata.windowID
	turn["request_kind"] = requestKind
	if requestKind != "prewarm" && startedAt > 0 {
		turn["turn_started_at_unix_ms"] = startedAt
	}
	if _, ok := turn["thread_source"]; !ok {
		turn["thread_source"] = "user"
	}
	if metadata.parentThreadID != "" {
		turn["parent_thread_id"] = metadata.parentThreadID
	}
	if forked := codexMapString(incoming, "forked_from_thread_id"); forked != "" {
		turn["forked_from_thread_id"] = identity.DerivedUUIDv7(metadata.installationID+"\x00forked", forked)
	}
	raw, err := json.Marshal(turn)
	if err != nil {
		return ""
	}
	return string(raw)
}

func codexRequestKind(spec Request, incoming map[string]interface{}) string {
	if strings.Contains(strings.ToLower(spec.DownstreamPath), "compact") {
		return "compaction"
	}
	if generate, ok := codexBodyBool(spec.Body, "generate"); ok && !generate {
		return "prewarm"
	}
	switch kind := strings.ToLower(codexMapString(incoming, "request_kind")); kind {
	case "turn", "prewarm", "compaction", "memory":
		return kind
	default:
		return "turn"
	}
}

func codexBodyClientMetadata(body []byte) map[string]interface{} {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	metadata, _ := payload["client_metadata"].(map[string]interface{})
	return metadata
}

func codexIncomingTurnMetadata(spec Request, bodyMetadata map[string]interface{}) map[string]interface{} {
	for _, raw := range []string{
		codexMetadataString(bodyMetadata, "x-codex-turn-metadata"),
		getHeaderFold(spec.Headers, "x-codex-turn-metadata"),
	} {
		var parsed map[string]interface{}
		if strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), &parsed) == nil {
			return parsed
		}
	}
	return map[string]interface{}{}
}

func codexMetadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func codexMapString(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func codexMapInt64(values map[string]interface{}, key string) int64 {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case float64:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	}
	return 0
}

func codexBodyBool(body []byte, key string) (bool, bool) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, false
	}
	value, ok := payload[key].(bool)
	return value, ok
}

func codexWindowOrdinal(windowID string) string {
	windowID = strings.TrimSpace(windowID)
	if idx := strings.LastIndex(windowID, ":"); idx >= 0 && idx+1 < len(windowID) {
		ordinal := windowID[idx+1:]
		for _, r := range ordinal {
			if r < '0' || r > '9' {
				return "0"
			}
		}
		return ordinal
	}
	return "0"
}
