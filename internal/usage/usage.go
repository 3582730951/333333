package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
)

const maxInt64Value = int64(^uint64(0) >> 1)

// DecodeObject keeps counters as json.Number instead of float64. Token
// counters are integers and may exceed float64's 53-bit exact range.
func DecodeObject(raw []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]interface{}
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	// Match json.Unmarshal's refusal of trailing non-whitespace data.
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return object, nil
}

// addNonNegative refuses wrapping when a derived counter exceeds int64.
func addNonNegative(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || value > maxInt64Value-total {
			return 0, false
		}
		total += value
	}
	return total, true
}

func integrityOr(current, next string) string {
	if current != "" {
		return current
	}
	return next
}

type Parsed struct {
	Model                 string
	ServiceTier           string
	PromptTokens          int64
	CompletionTokens      int64
	OutputReasoningTokens int64
	TotalTokens           int64
	CachedTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheMissTokens       int64
	CacheTotalInputTokens int64
	CacheCreation5mTokens int64
	CacheCreation1hTokens int64
	Presence              FieldPresence
	// IntegrityError is set when a provider supplied a malformed numeric usage
	// counter.  The value remains presence-aware but must never be settled or
	// silently rounded into a bill.
	IntegrityError string
	RawUsage       json.RawMessage
}

type FieldPresence struct {
	InputTotal      bool `json:"input_total"`
	CachedRead      bool `json:"cached_read"`
	CacheWrite      bool `json:"cache_write"`
	OutputTotal     bool `json:"output_total"`
	OutputReasoning bool `json:"output_reasoning"`
	TotalReported   bool `json:"total_reported"`
}

func ParseResponse(raw []byte) Parsed {
	root, err := DecodeObject(raw)
	if err != nil {
		return Parsed{}
	}
	model, _ := root["model"].(string)
	serviceTier, _ := root["service_tier"].(string)
	usageValue, ok := root["usage"]
	if !ok {
		if response, ok := root["response"].(map[string]interface{}); ok {
			usageValue = response["usage"]
			if serviceTier == "" {
				serviceTier, _ = response["service_tier"].(string)
			}
		}
	}
	usageMap, ok := usageValue.(map[string]interface{})
	if !ok {
		return Parsed{Model: model}
	}
	rawUsage, _ := json.Marshal(usageMap)
	cached := cachedTokens(usageMap)
	cacheCreation := cacheCreationTokens(usageMap)
	parsed := Parsed{
		Model:                 model,
		ServiceTier:           serviceTier,
		PromptTokens:          promptTokens(usageMap),
		CompletionTokens:      intField(usageMap, "output_tokens", "completion_tokens"),
		OutputReasoningTokens: outputReasoningTokens(usageMap),
		TotalTokens:           intField(usageMap, "total_tokens"),
		CachedTokens:          cached,
		CacheReadTokens:       cached,
		CacheCreationTokens:   cacheCreation,
		CacheCreation5mTokens: nestedIntField(usageMap, "cache_creation", "ephemeral_5m_input_tokens"),
		CacheCreation1hTokens: nestedIntField(usageMap, "cache_creation", "ephemeral_1h_input_tokens"),
		Presence:              usageFieldPresence(usageMap),
		IntegrityError:        invalidUsageIntegrityError(usageMap),
		RawUsage:              rawUsage,
	}
	fillCacheInputBreakdown(&parsed, usageMap)
	// Anthropic usage carries no total_tokens; derive one so downstream billing
	// views have a single comparable figure across providers.  An explicitly
	// reported zero is a real value (for example, a cancelled request), not an
	// invitation to manufacture a total from the other fields.
	if !parsed.Presence.TotalReported {
		var derived int64
		var ok bool
		if isAnthropicCacheUsage(usageMap) {
			derived, ok = addNonNegative(parsed.CacheTotalInputTokens, parsed.CompletionTokens)
		} else {
			derived, ok = addNonNegative(parsed.PromptTokens, parsed.CompletionTokens)
		}
		if ok {
			parsed.TotalTokens = derived
		} else {
			parsed.IntegrityError = integrityOr(parsed.IntegrityError, "derived_total_overflow")
		}
	}
	return parsed
}

var usageNumericKeys = []string{
	"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens",
	"cached_tokens", "input_cached_tokens", "prompt_cached_tokens", "cache_read_input_tokens",
	"prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "cache_creation_input_tokens", "cache_write_tokens",
}

func invalidUsageIntegrityError(m map[string]interface{}) string {
	if len(m) == 0 {
		return ""
	}
	for _, key := range usageNumericKeys {
		if value, present := m[key]; present {
			if _, valid := nonNegativeInteger(value); !valid {
				return "invalid_numeric_" + key
			}
		}
	}
	for _, parent := range []string{"input_tokens_details", "prompt_tokens_details", "output_tokens_details", "completion_tokens_details", "cache_creation"} {
		detail, ok := m[parent].(map[string]interface{})
		if !ok {
			if _, present := m[parent]; present {
				return "invalid_numeric_" + parent
			}
			continue
		}
		for _, key := range []string{"cached_tokens", "cache_write_tokens", "reasoning_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
			if value, present := detail[key]; present {
				if _, valid := nonNegativeInteger(value); !valid {
					return "invalid_numeric_" + parent + "." + key
				}
			}
		}
	}
	return ""
}

func fillCacheInputBreakdown(parsed *Parsed, usageMap map[string]interface{}) {
	if parsed == nil {
		return
	}
	if isAnthropicCacheUsage(usageMap) {
		parsed.CacheMissTokens = parsed.PromptTokens
		if total, ok := addNonNegative(parsed.PromptTokens, parsed.CacheReadTokens, parsed.CacheCreationTokens); ok {
			parsed.CacheTotalInputTokens = total
		} else {
			parsed.CacheTotalInputTokens = 0
			parsed.IntegrityError = integrityOr(parsed.IntegrityError, "cache_input_overflow")
		}
		return
	}
	parsed.CacheMissTokens = parsed.PromptTokens - parsed.CacheReadTokens
	if parsed.CacheMissTokens < 0 {
		parsed.CacheMissTokens = 0
	}
	parsed.CacheTotalInputTokens = parsed.PromptTokens
}

func isAnthropicCacheUsage(m map[string]interface{}) bool {
	if _, ok := m["cache_read_input_tokens"]; ok {
		return true
	}
	if _, ok := m["cache_creation_input_tokens"]; ok {
		return true
	}
	if _, ok := m["cache_creation"].(map[string]interface{}); ok {
		return true
	}
	return false
}

func intField(m map[string]interface{}, keys ...string) int64 {
	value, _ := firstIntegerField(m, keys...)
	return value
}

// firstIntegerField preserves an explicitly reported zero and does not fall
// through to a less-preferred alias. If a present value is malformed, it
// returns zero with present=true; invalidUsageIntegrityError separately marks
// the payload as integrity_error so the value is never billed as authoritative.
func firstIntegerField(m map[string]interface{}, keys ...string) (int64, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if value, valid := nonNegativeInteger(v); valid {
				return value, true
			}
			return 0, true
		}
	}
	return 0, false
}

// nonNegativeInteger deliberately refuses fractional, negative, NaN/Inf and
// overflowing values.  Usage counters are integers; truncating a malformed
// upstream value would create a plausible but false bill.  Presence is tracked
// separately, so callers can surface the row as integrity_error/unsettled.
func nonNegativeInteger(value interface{}) (int64, bool) {
	// JSON numbers decode to float64 in this parser. 2^63 is representable as a
	// float but cannot fit in int64; reject it before conversion to avoid wrap.
	const maxInt64FloatExclusive = 9223372036854775808.0
	var result int64
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < 0 || typed != math.Trunc(typed) || typed >= maxInt64FloatExclusive {
			return 0, false
		}
		result = int64(typed)
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f != math.Trunc(f) || f >= maxInt64FloatExclusive {
			return 0, false
		}
		result = int64(typed)
	case int:
		if typed < 0 {
			return 0, false
		}
		result = int64(typed)
	case int8:
		if typed < 0 {
			return 0, false
		}
		result = int64(typed)
	case int16:
		if typed < 0 {
			return 0, false
		}
		result = int64(typed)
	case int32:
		if typed < 0 {
			return 0, false
		}
		result = int64(typed)
	case int64:
		if typed < 0 {
			return 0, false
		}
		result = typed
	case uint:
		if uint64(typed) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		result = int64(typed)
	case uint8:
		result = int64(typed)
	case uint16:
		result = int64(typed)
	case uint32:
		result = int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, false
		}
		result = int64(typed)
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		result = parsed
	default:
		return 0, false
	}
	return result, true
}

func nestedIntField(m map[string]interface{}, parent, child string) int64 {
	if detail, ok := m[parent].(map[string]interface{}); ok {
		return intField(detail, child)
	}
	return 0
}

func intFieldPresent(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func nestedFieldPresent(m map[string]interface{}, parent, child string) bool {
	detail, ok := m[parent].(map[string]interface{})
	if !ok {
		return false
	}
	_, ok = detail[child]
	return ok
}

func outputReasoningTokens(m map[string]interface{}) int64 {
	for _, parent := range []string{"output_tokens_details", "completion_tokens_details"} {
		if detail, ok := m[parent].(map[string]interface{}); ok {
			if value, present := firstIntegerField(detail, "reasoning_tokens"); present {
				return value
			}
		}
	}
	return 0
}

func usageFieldPresence(m map[string]interface{}) FieldPresence {
	presence := FieldPresence{
		InputTotal:    intFieldPresent(m, "input_tokens", "prompt_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens"),
		CachedRead:    intFieldPresent(m, "cached_tokens", "input_cached_tokens", "prompt_cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"),
		CacheWrite:    intFieldPresent(m, "cache_creation_input_tokens", "cache_write_tokens"),
		OutputTotal:   intFieldPresent(m, "output_tokens", "completion_tokens"),
		TotalReported: intFieldPresent(m, "total_tokens"),
	}
	for _, parent := range []string{"input_tokens_details", "prompt_tokens_details"} {
		presence.CachedRead = presence.CachedRead || nestedFieldPresent(m, parent, "cached_tokens")
		presence.CacheWrite = presence.CacheWrite || nestedFieldPresent(m, parent, "cache_write_tokens")
	}
	// Anthropic exposes cache creation as a nested object in some API versions;
	// a zero-valued child is still authoritative evidence that the field exists.
	presence.CacheWrite = presence.CacheWrite || nestedFieldPresent(m, "cache_creation", "ephemeral_5m_input_tokens") ||
		nestedFieldPresent(m, "cache_creation", "ephemeral_1h_input_tokens") ||
		nestedFieldPresent(m, "cache_creation", "cache_write_tokens")
	for _, parent := range []string{"output_tokens_details", "completion_tokens_details"} {
		presence.OutputReasoning = presence.OutputReasoning || nestedFieldPresent(m, parent, "reasoning_tokens")
	}
	return presence
}

func PresenceFromRaw(raw json.RawMessage) FieldPresence {
	root, err := DecodeObject(raw)
	if len(raw) == 0 || err != nil {
		return FieldPresence{}
	}
	if nested, ok := root["usage"].(map[string]interface{}); ok {
		root = nested
	} else if response, ok := root["response"].(map[string]interface{}); ok {
		if nested, ok := response["usage"].(map[string]interface{}); ok {
			root = nested
		}
	}
	return usageFieldPresence(root)
}

// IntegrityFromRaw exposes the same strict numeric validation used by the
// parser to storage/reconciliation paths that receive a raw usage payload plus
// legacy scalar columns.  Empty means no malformed counter was observed.
func IntegrityFromRaw(raw json.RawMessage) string {
	root, err := DecodeObject(raw)
	if len(raw) == 0 || err != nil {
		return ""
	}
	if nested, ok := root["usage"].(map[string]interface{}); ok {
		root = nested
	} else if response, ok := root["response"].(map[string]interface{}); ok {
		if nested, ok := response["usage"].(map[string]interface{}); ok {
			root = nested
		}
	}
	return invalidUsageIntegrityError(root)
}

func promptTokens(m map[string]interface{}) int64 {
	if v, present := firstIntegerField(m, "input_tokens", "prompt_tokens"); present {
		return v
	}
	// DeepSeek reports prompt cache split counters. If prompt_tokens is omitted,
	// the input token count is the stable hit prefix plus the uncached miss prefix.
	hit, hitPresent := firstIntegerField(m, "prompt_cache_hit_tokens")
	miss, missPresent := firstIntegerField(m, "prompt_cache_miss_tokens")
	if hitPresent || missPresent {
		if total, ok := addNonNegative(hit, miss); ok {
			return total
		}
	}
	return 0
}

func cachedTokens(m map[string]interface{}) int64 {
	if v, present := firstIntegerField(m, "cached_tokens", "input_cached_tokens", "prompt_cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"); present {
		return v
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if detail, ok := m[key].(map[string]interface{}); ok {
			if v, present := firstIntegerField(detail, "cached_tokens"); present {
				return v
			}
		}
	}
	return 0
}

// cacheCreationTokens normalizes provider cache-write counters without changing
// the provider's raw usage payload. Anthropic exposes a top-level counter while
// OpenAI Responses and Chat Completions expose cache_write_tokens in the same
// details object that carries cached_tokens.
func cacheCreationTokens(m map[string]interface{}) int64 {
	if v, present := firstIntegerField(m, "cache_creation_input_tokens", "cache_write_tokens"); present {
		return v
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if detail, ok := m[key].(map[string]interface{}); ok {
			if v, present := firstIntegerField(detail, "cache_write_tokens"); present {
				return v
			}
		}
	}
	return 0
}

// maxScannerLine caps the partial-line buffer so a malformed upstream stream that
// never emits a newline cannot grow memory without bound. A single SSE `data:`
// frame is well under this; anything larger is not a usage frame we care about.
const maxScannerLine = 1 << 20 // 1 MiB

// StreamScanner extracts token usage from a streamed SSE response as the bytes flow
// through, without buffering the whole stream. It implements io.Writer so a caller
// can tee the upstream body into it (io.TeeReader) while the stream is relayed to
// the client unchanged. The bulk of real Codex/Claude traffic is `stream:true`, so
// without this the usage tables — and the entire admin overview built on them —
// stay empty. Provider-aware:
//
//   - "codex" (/v1/responses): the terminal `response.completed` frame carries
//     `response.usage` (input_tokens, output_tokens, total_tokens, and cached under
//     input_tokens_details.cached_tokens). Last usage-bearing frame wins.
//   - "claude" (/v1/messages): `message_start` carries `message.usage` with the
//     input + cache-read tokens (output_tokens is a placeholder there), and each
//     `message_delta` carries `usage.output_tokens` with the running output count.
//     The two are merged: input/cache from message_start, output from the last
//     message_delta.
//
// Call Parsed() once the stream has been fully read.
type StreamScanner struct {
	provider string
	buf      []byte // partial trailing line not yet terminated by '\n'

	model                 string
	serviceTier           string
	promptTokens          int64
	completionTokens      int64
	outputReasoningTokens int64
	integrityError        string
	totalTokens           int64
	cachedTokens          int64
	cacheReadTokens       int64
	cacheCreationTokens   int64
	cacheMissTokens       int64
	cacheTotalInputTokens int64
	cacheCreation5mTokens int64
	cacheCreation1hTokens int64
	lastUsage             json.RawMessage
	presence              FieldPresence
	got                   bool
}

// NewStreamScanner returns a usage scanner for the given relay provider ("codex",
// "claude", or "openai_chat" for a custom OpenAI-compatible provider). Any other value
// is treated as "codex" (the OpenAI Responses-style shape).
func NewStreamScanner(provider string) *StreamScanner {
	return &StreamScanner{provider: provider}
}

// Write feeds a chunk of the SSE byte stream. It is line-oriented: complete lines
// (terminated by '\n') are parsed immediately; an unterminated trailing fragment is
// retained for the next Write. It never errors and always reports len(p) consumed so
// it is safe as the destination of an io.TeeReader.
func (s *StreamScanner) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	data := p
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			s.buf = append(s.buf, data...)
			if len(s.buf) > maxScannerLine {
				// Overlong frame with no newline: drop it rather than grow unbounded.
				s.buf = s.buf[:0]
			}
			return len(p), nil
		}
		line := data[:idx]
		if len(s.buf) > 0 {
			s.buf = append(s.buf, line...)
			line = s.buf
		}
		s.observeLine(line)
		s.buf = s.buf[:0]
		data = data[idx+1:]
	}
}

// observeLine parses a single (CR-trimmed) SSE line, acting only on `data:` frames.
func (s *StreamScanner) observeLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	// Fast path: every usage extraction below requires the literal "usage" key
	// (claude message_start/message_delta, codex response.usage, openai_chat usage),
	// and model is only ever reported alongside usage. The bulk of a stream is
	// token-delta frames (output_text.delta / content_block_delta) with no usage
	// field; skipping the full map unmarshal for those is byte-for-byte
	// behavior-preserving and avoids a parse+alloc on the per-token hot path.
	if !bytes.Contains(payload, []byte("usage")) {
		return
	}
	ev, err := DecodeObject(payload)
	if err != nil {
		return
	}
	if s.provider == "claude" {
		s.observeClaude(ev)
		return
	}
	if s.provider == "openai_chat" {
		s.observeOpenAIChat(ev)
		return
	}
	s.observeCodex(ev)
}

// observeOpenAIChat reads usage from an OpenAI Chat Completions chunk stream (a custom
// provider such as DeepSeek). When the client requests stream_options.include_usage the
// terminal chunk carries a top-level `usage` object (prompt_tokens / completion_tokens /
// total_tokens); the last usage-bearing chunk wins.
func (s *StreamScanner) observeOpenAIChat(ev map[string]interface{}) {
	if m, _ := ev["model"].(string); m != "" {
		s.model = m
	}
	if tier, _ := ev["service_tier"].(string); tier != "" {
		s.serviceTier = tier
	}
	um, ok := ev["usage"].(map[string]interface{})
	if !ok {
		return
	}
	s.promptTokens = promptTokens(um)
	s.completionTokens = intField(um, "completion_tokens", "output_tokens")
	s.outputReasoningTokens = outputReasoningTokens(um)
	s.totalTokens = intField(um, "total_tokens")
	s.cachedTokens = cachedTokens(um)
	s.cacheReadTokens = s.cachedTokens
	s.cacheCreationTokens = cacheCreationTokens(um)
	s.cacheCreation5mTokens = nestedIntField(um, "cache_creation", "ephemeral_5m_input_tokens")
	s.cacheCreation1hTokens = nestedIntField(um, "cache_creation", "ephemeral_1h_input_tokens")
	s.fillCacheInputBreakdown(um)
	s.presence = usageFieldPresence(um)
	s.integrityError = invalidUsageIntegrityError(um)
	s.lastUsage, _ = json.Marshal(um)
	s.got = true
}

func (s *StreamScanner) observeCodex(ev map[string]interface{}) {
	resp, ok := ev["response"].(map[string]interface{})
	if !ok {
		return
	}
	if m, _ := resp["model"].(string); m != "" {
		s.model = m
	}
	if tier, _ := resp["service_tier"].(string); tier != "" {
		s.serviceTier = tier
	}
	um, ok := resp["usage"].(map[string]interface{})
	if !ok {
		return
	}
	s.promptTokens = promptTokens(um)
	s.completionTokens = intField(um, "output_tokens", "completion_tokens")
	s.outputReasoningTokens = outputReasoningTokens(um)
	s.totalTokens = intField(um, "total_tokens")
	s.cachedTokens = cachedTokens(um)
	s.cacheReadTokens = s.cachedTokens
	s.cacheCreationTokens = cacheCreationTokens(um)
	s.cacheCreation5mTokens = nestedIntField(um, "cache_creation", "ephemeral_5m_input_tokens")
	s.cacheCreation1hTokens = nestedIntField(um, "cache_creation", "ephemeral_1h_input_tokens")
	s.fillCacheInputBreakdown(um)
	s.presence = usageFieldPresence(um)
	s.integrityError = invalidUsageIntegrityError(um)
	s.lastUsage, _ = json.Marshal(um)
	s.got = true
}

func (s *StreamScanner) observeClaude(ev map[string]interface{}) {
	switch ev["type"] {
	case "message_start":
		m, ok := ev["message"].(map[string]interface{})
		if !ok {
			return
		}
		if name, _ := m["model"].(string); name != "" {
			s.model = name
		}
		um, ok := m["usage"].(map[string]interface{})
		if !ok {
			return
		}
		s.promptTokens = intField(um, "input_tokens")
		s.cacheReadTokens = intField(um, "cache_read_input_tokens")
		s.cachedTokens = s.cacheReadTokens
		s.cacheCreationTokens = intField(um, "cache_creation_input_tokens")
		s.cacheCreation5mTokens = nestedIntField(um, "cache_creation", "ephemeral_5m_input_tokens")
		s.cacheCreation1hTokens = nestedIntField(um, "cache_creation", "ephemeral_1h_input_tokens")
		s.fillCacheInputBreakdown(um)
		observed := usageFieldPresence(um)
		s.presence.InputTotal = s.presence.InputTotal || observed.InputTotal
		s.presence.CachedRead = s.presence.CachedRead || observed.CachedRead
		s.presence.CacheWrite = s.presence.CacheWrite || observed.CacheWrite
		if err := invalidUsageIntegrityError(um); err != "" {
			s.integrityError = err
		}
		s.lastUsage, _ = json.Marshal(um)
		s.got = true
	case "message_delta":
		um, ok := ev["usage"].(map[string]interface{})
		if !ok {
			return
		}
		if output, present := firstIntegerField(um, "output_tokens"); present {
			s.completionTokens = output
			s.got = true
		}
		if err := invalidUsageIntegrityError(um); err != "" {
			s.integrityError = err
		}
		if intFieldPresent(um, "output_tokens") {
			s.presence.OutputTotal = true
		}
		if read, present := firstIntegerField(um, "cache_read_input_tokens"); present {
			s.cacheReadTokens = read
			s.cachedTokens = read
			s.got = true
		}
		if intFieldPresent(um, "cache_read_input_tokens") {
			s.presence.CachedRead = true
		}
		if created, present := firstIntegerField(um, "cache_creation_input_tokens"); present {
			s.cacheCreationTokens = created
			s.got = true
		}
		if intFieldPresent(um, "cache_creation_input_tokens") {
			s.presence.CacheWrite = true
		}
		if detail, ok := um["cache_creation"].(map[string]interface{}); ok {
			if created5m, present := firstIntegerField(detail, "ephemeral_5m_input_tokens"); present {
				s.cacheCreation5mTokens = created5m
				s.got = true
			}
		}
		if detail, ok := um["cache_creation"].(map[string]interface{}); ok {
			if created1h, present := firstIntegerField(detail, "ephemeral_1h_input_tokens"); present {
				s.cacheCreation1hTokens = created1h
				s.got = true
			}
		}
		s.fillCacheInputBreakdown(um)
	}
}

func (s *StreamScanner) fillCacheInputBreakdown(um map[string]interface{}) {
	if s == nil {
		return
	}
	if isAnthropicCacheUsage(um) || s.provider == "claude" {
		s.cacheMissTokens = s.promptTokens
		if total, ok := addNonNegative(s.promptTokens, s.cacheReadTokens, s.cacheCreationTokens); ok {
			s.cacheTotalInputTokens = total
		} else {
			s.cacheTotalInputTokens = 0
			s.integrityError = integrityOr(s.integrityError, "cache_input_overflow")
		}
		return
	}
	s.cacheMissTokens = s.promptTokens - s.cacheReadTokens
	if s.cacheMissTokens < 0 {
		s.cacheMissTokens = 0
	}
	s.cacheTotalInputTokens = s.promptTokens
}

// Parsed returns the accumulated usage and whether any usage frame was seen. The
// total is derived (prompt + completion) when the provider omits an explicit total
// (Anthropic does), mirroring ParseResponse so streamed and buffered usage agree.
func (s *StreamScanner) Parsed() (Parsed, bool) {
	if s != nil && len(s.buf) > 0 {
		s.observeLine(s.buf)
		s.buf = nil
	}
	if s == nil || !s.got {
		return Parsed{}, false
	}
	total := s.totalTokens
	if !s.presence.TotalReported {
		if s.provider == "claude" && s.cacheTotalInputTokens > 0 {
			if derived, ok := addNonNegative(s.cacheTotalInputTokens, s.completionTokens); ok {
				total = derived
			} else {
				total = 0
				s.integrityError = integrityOr(s.integrityError, "derived_total_overflow")
			}
		} else {
			if derived, ok := addNonNegative(s.promptTokens, s.completionTokens); ok {
				total = derived
			} else {
				total = 0
				s.integrityError = integrityOr(s.integrityError, "derived_total_overflow")
			}
		}
	}
	rawUsage := s.lastUsage
	if s.provider == "claude" {
		um := map[string]int64{
			"input_tokens":  s.promptTokens,
			"output_tokens": s.completionTokens,
		}
		if s.presence.CachedRead {
			um["cache_read_input_tokens"] = s.cacheReadTokens
		}
		if s.presence.CacheWrite {
			um["cache_creation_input_tokens"] = s.cacheCreationTokens
		}
		rawUsage, _ = json.Marshal(um)
	}
	return Parsed{
		Model:                 s.model,
		ServiceTier:           s.serviceTier,
		PromptTokens:          s.promptTokens,
		CompletionTokens:      s.completionTokens,
		OutputReasoningTokens: s.outputReasoningTokens,
		TotalTokens:           total,
		CachedTokens:          s.cachedTokens,
		CacheReadTokens:       s.cacheReadTokens,
		CacheCreationTokens:   s.cacheCreationTokens,
		CacheMissTokens:       s.cacheMissTokens,
		CacheTotalInputTokens: s.cacheTotalInputTokens,
		CacheCreation5mTokens: s.cacheCreation5mTokens,
		CacheCreation1hTokens: s.cacheCreation1hTokens,
		Presence:              s.presence,
		IntegrityError:        s.integrityError,
		RawUsage:              rawUsage,
	}, true
}
