package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

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

// CodexIdentitySnapshot is the canonical internal identity selected by the
// gateway after account and egress selection.  Root SessionID and ThreadID are
// intentionally separate for child agents: a root has the same value for both,
// while every branch receives its own ThreadID under the root SessionID. TurnID is
// one logical submission and is reused by in-attempt transport/auth retries.
//
// Downstream turn state is opaque upstream context, not a correlator to rewrite;
// the gateway validates it through its exact mapping before it reaches this type.
type CodexIdentitySnapshot struct {
	InstallationID string
	// DeviceOSHint is the root-elected OS profile used for every part of the
	// virtual Codex device, including the User-Agent. It may intentionally be
	// empty to select the host-default profile.
	DeviceOSHint       string
	SessionID          string
	ThreadID           string
	TurnID             string
	WindowGeneration   int64
	ParentThreadID     string
	ForkedFromThreadID string
	TurnState          string
}

func (s CodexIdentitySnapshot) WindowID() string {
	return strings.TrimSpace(s.ThreadID) + ":" + fmt.Sprintf("%d", s.WindowGeneration)
}

// codexRequestMetadata is the canonical per-request identity snapshot mirrored
// from codex-rs/core/src/responses_metadata.rs. The same value drives HTTP/WS
// compatibility headers and client_metadata so no transport exposes a different
// session, thread, turn, or window identity.
type codexRequestMetadata struct {
	installationID     string
	sessionID          string
	threadID           string
	turnID             string
	parentTurnID       string
	windowID           string
	parentThreadID     string
	forkedFromThreadID string
	subagent           string
	turnState          string
	turnMetadata       string
	// turnMetadataHeader is the bounded ASCII compatibility projection used on
	// HTTP/WebSocket headers. The full value remains in client_metadata.
	turnMetadataHeader string
	responsesLite      bool
	profile            codexProtocolProfile
	// mappedIdentity marks a CPA-v2 snapshot. In that path hierarchy correlators
	// must come solely from the persisted mapping; falling back to a derived value
	// from a raw downstream fork id would create an untracked relationship.
	mappedIdentity bool
}

// Keep the old internal name as an alias for focused WS tests and helpers while
// making clear that the identity is now shared by HTTP and WebSocket transports.
type codexWebSocketIDs = codexRequestMetadata

func (c *Client) newCodexRequestMetadata(spec Request) codexRequestMetadata {
	responsesLite := false
	if !AccountUsesAPIKey(spec.Token) {
		if lite, _, supported, err := codexRequestUsesResponsesLiteSource(spec); err == nil && supported {
			responsesLite = lite
		} else {
			responsesLite = CodexRequestUsesResponsesLite(requestBody(spec))
		}
	}
	return c.newCodexRequestMetadataWithResponsesLite(spec, responsesLite)
}

func (c *Client) newCodexRequestMetadataWithResponsesLite(spec Request, responsesLite bool) codexRequestMetadata {
	profile := c.codexProtocolProfileForRequest(spec)
	if snapshot := spec.CodexIdentity; snapshot != nil && strings.TrimSpace(snapshot.SessionID) != "" && strings.TrimSpace(snapshot.ThreadID) != "" {
		bodyMetadata := requestCodexBodyClientMetadata(spec)
		incomingTurn := codexIncomingTurnMetadata(spec, bodyMetadata)
		requestKind := codexRequestKind(spec, incomingTurn)
		startedAt := int64(0)
		if requestKind != "prewarm" {
			startedAt = codexMapInt64(incomingTurn, "turn_started_at_unix_ms")
			if startedAt <= 0 {
				startedAt = time.Now().UnixMilli()
			}
		}
		metadata := codexRequestMetadata{
			installationID:     strings.TrimSpace(snapshot.InstallationID),
			sessionID:          strings.TrimSpace(snapshot.SessionID),
			threadID:           strings.TrimSpace(snapshot.ThreadID),
			turnID:             strings.TrimSpace(snapshot.TurnID),
			windowID:           snapshot.WindowID(),
			parentThreadID:     strings.TrimSpace(snapshot.ParentThreadID),
			forkedFromThreadID: strings.TrimSpace(snapshot.ForkedFromThreadID),
			turnState:          strings.TrimSpace(snapshot.TurnState),
			responsesLite:      responsesLite,
			profile:            profile,
			mappedIdentity:     true,
			subagent: firstNonEmpty(
				getHeaderFold(spec.Headers, codexSubagentHeader),
				codexMetadataString(bodyMetadata, codexSubagentHeader),
			),
		}
		if requestKind == "prewarm" {
			metadata.turnID = ""
		}
		metadata.parentTurnID = codexParentTurnID(spec, bodyMetadata, incomingTurn, metadata.installationID, profile)
		metadata.turnMetadata = buildCodexTurnMetadata(metadata, incomingTurn, requestKind, startedAt)
		metadata.turnMetadataHeader = codexTurnMetadataCompatibilityHeader(metadata.turnMetadata, profile.codeModeToolNames)
		return metadata
	}
	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)
	bodyMetadata := requestCodexBodyClientMetadata(spec)
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
		requestCodexBodyString(spec, "thread_id"),
		requestCodexBodyString(spec, "session_id"),
		requestCodexBodyString(spec, "conversation_id"),
		threadIDFromWindowID(rawWindowID),
		requestCodexBodyString(spec, "prompt_cache_key"),
		requestCodexBodyString(spec, "previous_response_id"),
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
		responsesLite: responsesLite,
		profile:       profile,
	}
	metadata.parentTurnID = codexParentTurnID(spec, bodyMetadata, incomingTurn, metadata.installationID, profile)
	metadata.turnMetadata = buildCodexTurnMetadata(metadata, incomingTurn, requestKind, startedAt)
	metadata.turnMetadataHeader = codexTurnMetadataCompatibilityHeader(metadata.turnMetadata, profile.codeModeToolNames)
	return metadata
}

// CodexRequestUsesResponsesLite recognizes both stable Lite request shapes. The
// prewarm/first frame starts with an additional_tools developer item; subsequent
// frames on the same Responses WebSocket carry the explicit Lite client_metadata
// marker but may start with an ordinary developer message because the tool envelope
// is already part of the warm connection state.
//
// A classic/older client can send hosted tools such as web_search or image_generation,
// but it does not emit the private WebSocket Lite marker. Once that explicit marker is
// present it is authoritative even if gateway processing temporarily added top-level
// tools; normalizeCodexResponsesLiteEnvelope will move those tools into the incremental
// input prefix before dispatch.
func CodexRequestUsesResponsesLite(raw []byte) bool {
	if !capability.CodexUsesResponsesLite(codexBodyString(raw, "model")) {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	if codexResponsesLiteContinuationMarker(fields) {
		return true
	}
	input, ok := codexResponsesInputItems(fields["input"])
	if !ok || len(input) == 0 {
		return false
	}
	var first struct {
		Type  string          `json:"type"`
		Role  string          `json:"role"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(input[0], &first); err != nil || first.Type != "additional_tools" || first.Role != "developer" {
		return false
	}
	var tools []json.RawMessage
	return json.Unmarshal(first.Tools, &tools) == nil
}

func codexResponsesLiteContinuationMarker(fields map[string]json.RawMessage) bool {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(fields["client_metadata"], &metadata); err != nil {
		return false
	}
	var marker string
	return json.Unmarshal(metadata[codexWSResponsesLiteMetadata], &marker) == nil && strings.EqualFold(strings.TrimSpace(marker), "true")
}

func applyCodexClientMetadata(raw []byte, metadata codexRequestMetadata, websocket bool) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	return applyCodexClientMetadataWithFields(raw, fields, metadata, websocket)
}

// applyCodexClientMetadataWithFields is the core taking the top-level object parsed
// once at the dispatch choke point. The map only gates a non-object body (nil ->
// unchanged, matching the wrapper's malformed-JSON passthrough); the client_metadata
// members themselves are rewritten in place with targeted sjson edits.
func applyCodexClientMetadataWithFields(raw []byte, fields map[string]json.RawMessage, metadata codexRequestMetadata, websocket bool) []byte {
	if fields == nil {
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
		"installation_id",
		codexInstallationIDMetadataKey,
		"session_id",
		"thread_id",
		"turn_id",
		"parent_turn_id",
		"window_id",
		"x-codex-window-id",
		codexSubagentHeader,
		"parent_thread_id",
		"x-codex-parent-thread-id",
		"forked_from_thread_id",
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
		{"parent_turn_id", metadata.parentTurnID},
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
	return stripCodexTopLevelTransportCorrelatorsWithFields(raw, fields)
}

// stripCodexTopLevelTransportCorrelatorsWithFields is the core taking the top-level
// object parsed once at the dispatch choke point. Presence is read from the shared
// map (these keys are not added by any earlier normalization step), and only present
// keys are deleted from the current bytes, leaving all prompt/context bytes untouched.
func stripCodexTopLevelTransportCorrelatorsWithFields(raw []byte, fields map[string]json.RawMessage) []byte {
	if fields == nil {
		return raw
	}
	out := raw
	for _, key := range []string{
		"thread_id",
		"session_id",
		"conversation_id",
		"window_id",
		"parent_thread_id",
		"parent_turn_id",
		"forked_from_thread_id",
		"turn_metadata",
		"turn_state",
	} {
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
		"parent_turn_id",
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
	if metadata.profile.parentTurnID && metadata.parentTurnID != "" {
		turn["parent_turn_id"] = metadata.parentTurnID
	}
	if !metadata.profile.codeModeToolNames {
		delete(turn, "code_mode_tool_names")
	}
	if metadata.forkedFromThreadID != "" {
		turn["forked_from_thread_id"] = metadata.forkedFromThreadID
	} else if !metadata.mappedIdentity {
		if forked := codexMapString(incoming, "forked_from_thread_id"); forked != "" {
			turn["forked_from_thread_id"] = identity.DerivedUUIDv7(metadata.installationID+"\x00forked", forked)
		}
	}
	raw, err := json.Marshal(turn)
	if err != nil {
		return ""
	}
	return string(raw)
}

// codexTurnMetadataCompatibilityHeader mirrors Codex 0.146.0's split metadata
// transport. The complete turn snapshot belongs in client_metadata, while the
// direct header omits the potentially unbounded Code Mode tool-name map. Header
// JSON is ASCII escaped exactly so workspace labels cannot make net/http reject
// an otherwise valid request.
func codexTurnMetadataCompatibilityHeader(full string, omitCodeModeToolNames bool) string {
	if strings.TrimSpace(full) == "" {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(full), &fields); err != nil {
		return ""
	}
	if omitCodeModeToolNames {
		delete(fields, "code_mode_tool_names")
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return asciiJSON(raw)
}

func codexParentTurnID(spec Request, bodyMetadata, incomingTurn map[string]interface{}, installationID string, profile codexProtocolProfile) string {
	if !profile.parentTurnID {
		return ""
	}
	raw := firstNonEmpty(
		codexMetadataString(bodyMetadata, "parent_turn_id"),
		codexMapString(incomingTurn, "parent_turn_id"),
		requestCodexBodyString(spec, "parent_turn_id"),
	)
	if raw == "" {
		return ""
	}
	return identity.DerivedUUIDv7(strings.TrimSpace(installationID)+"\x00parent-turn", raw)
}

func normalizeCodexPromptCacheKeyForProfileWithFields(raw []byte, fields map[string]json.RawMessage, metadata codexRequestMetadata) []byte {
	if fields == nil {
		return raw
	}
	encoded, present := fields["prompt_cache_key"]
	if !present {
		return raw
	}
	var current string
	if json.Unmarshal(encoded, &current) != nil || !looksLikeCodexGeneratedUUID(current) {
		return raw
	}
	replacement := metadata.threadID
	if metadata.profile.promptCacheKeyBySession {
		replacement = metadata.sessionID
	}
	if strings.TrimSpace(replacement) == "" || replacement == current {
		return raw
	}
	out, err := sjson.SetBytes(raw, "prompt_cache_key", replacement)
	if err != nil {
		return raw
	}
	return out
}

func looksLikeCodexGeneratedUUID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func asciiJSON(raw []byte) string {
	var out strings.Builder
	out.Grow(len(raw))
	for _, r := range string(raw) {
		switch {
		case r <= 0x7f:
			out.WriteRune(r)
		case r <= 0xffff:
			_, _ = fmt.Fprintf(&out, `\u%04x`, r)
		default:
			high, low := utf16.EncodeRune(r)
			_, _ = fmt.Fprintf(&out, `\u%04x\u%04x`, high, low)
		}
	}
	return out.String()
}

func codexRequestKind(spec Request, incoming map[string]interface{}) string {
	if strings.Contains(strings.ToLower(spec.DownstreamPath), "compact") {
		return "compaction"
	}
	if generate, ok := requestCodexBodyBool(spec, "generate"); ok && !generate {
		return "prewarm"
	}
	switch kind := strings.ToLower(codexMapString(incoming, "request_kind")); kind {
	case "turn", "prewarm", "compaction", "memory":
		return kind
	default:
		return "turn"
	}
}

func requestCodexBodyClientMetadata(spec Request) map[string]interface{} {
	if spec.BodyMeta != nil {
		span, present := spec.BodyMeta.Fields["client_metadata"]
		if !present {
			return nil
		}
		if raw, ok, err := requestSpan(spec.Body, span, maxCodexPatchFieldBytes); err == nil && ok {
			var metadata map[string]interface{}
			if json.Unmarshal(raw, &metadata) == nil {
				return metadata
			}
		}
	}
	return codexBodyClientMetadata(requestBody(spec))
}

func requestCodexBodyString(spec Request, key string) string {
	if spec.BodyMeta != nil {
		switch key {
		case "model":
			return spec.BodyMeta.Model
		case "prompt_cache_key":
			return spec.BodyMeta.PromptCacheKey
		case "previous_response_id":
			return spec.BodyMeta.PreviousResponseID
		case "conversation_id":
			return spec.BodyMeta.ConversationID
		case "session_id":
			return spec.BodyMeta.SessionID
		case "thread_id":
			return spec.BodyMeta.ThreadID
		}
		if raw, present := spec.BodyMeta.Scalars[key]; present {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	return codexBodyString(requestBody(spec), key)
}

func requestCodexBodyBool(spec Request, key string) (bool, bool) {
	if spec.BodyMeta != nil {
		switch spec.BodyMeta.Kinds[key] {
		case 't':
			return true, true
		case 'f':
			return false, true
		default:
			return false, false
		}
	}
	return codexBodyBool(requestBody(spec), key)
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
