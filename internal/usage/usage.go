package usage

import (
	"bytes"
	"encoding/json"
)

type Parsed struct {
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	RawUsage         json.RawMessage
}

func ParseResponse(raw []byte) Parsed {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return Parsed{}
	}
	model, _ := root["model"].(string)
	usageValue, ok := root["usage"]
	if !ok {
		if response, ok := root["response"].(map[string]interface{}); ok {
			usageValue = response["usage"]
		}
	}
	usageMap, ok := usageValue.(map[string]interface{})
	if !ok {
		return Parsed{Model: model}
	}
	rawUsage, _ := json.Marshal(usageMap)
	parsed := Parsed{
		Model:            model,
		PromptTokens:     promptTokens(usageMap),
		CompletionTokens: intField(usageMap, "output_tokens", "completion_tokens"),
		TotalTokens:      intField(usageMap, "total_tokens"),
		CachedTokens:     cachedTokens(usageMap),
		RawUsage:         rawUsage,
	}
	// Anthropic usage carries no total_tokens; derive one so downstream billing
	// views have a single comparable figure across providers.
	if parsed.TotalTokens == 0 {
		parsed.TotalTokens = parsed.PromptTokens + parsed.CompletionTokens
	}
	return parsed
}

func intField(m map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case int:
				return int64(t)
			}
		}
	}
	return 0
}

func promptTokens(m map[string]interface{}) int64 {
	if v := intField(m, "input_tokens", "prompt_tokens"); v > 0 {
		return v
	}
	// DeepSeek reports prompt cache split counters. If prompt_tokens is omitted,
	// the input token count is the stable hit prefix plus the uncached miss prefix.
	return intField(m, "prompt_cache_hit_tokens") + intField(m, "prompt_cache_miss_tokens")
}

func cachedTokens(m map[string]interface{}) int64 {
	if v := intField(m, "cached_tokens", "input_cached_tokens", "prompt_cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"); v > 0 {
		return v
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if detail, ok := m[key].(map[string]interface{}); ok {
			if v := intField(detail, "cached_tokens"); v > 0 {
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

	model            string
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	cachedTokens     int64
	lastUsage        json.RawMessage
	got              bool
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
	var ev map[string]interface{}
	if json.Unmarshal(payload, &ev) != nil {
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
	um, ok := ev["usage"].(map[string]interface{})
	if !ok {
		return
	}
	s.promptTokens = promptTokens(um)
	s.completionTokens = intField(um, "completion_tokens", "output_tokens")
	s.totalTokens = intField(um, "total_tokens")
	s.cachedTokens = cachedTokens(um)
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
	um, ok := resp["usage"].(map[string]interface{})
	if !ok {
		return
	}
	s.promptTokens = promptTokens(um)
	s.completionTokens = intField(um, "output_tokens", "completion_tokens")
	s.totalTokens = intField(um, "total_tokens")
	s.cachedTokens = cachedTokens(um)
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
		s.cachedTokens = cachedTokens(um)
		s.lastUsage, _ = json.Marshal(um)
		s.got = true
	case "message_delta":
		um, ok := ev["usage"].(map[string]interface{})
		if !ok {
			return
		}
		if out := intField(um, "output_tokens"); out > 0 {
			s.completionTokens = out
			s.got = true
		}
	}
}

// Parsed returns the accumulated usage and whether any usage frame was seen. The
// total is derived (prompt + completion) when the provider omits an explicit total
// (Anthropic does), mirroring ParseResponse so streamed and buffered usage agree.
func (s *StreamScanner) Parsed() (Parsed, bool) {
	if s == nil || !s.got {
		return Parsed{}, false
	}
	total := s.totalTokens
	if total == 0 {
		total = s.promptTokens + s.completionTokens
	}
	if total == 0 {
		// A frame was seen but carried no countable tokens — nothing worth recording.
		return Parsed{}, false
	}
	return Parsed{
		Model:            s.model,
		PromptTokens:     s.promptTokens,
		CompletionTokens: s.completionTokens,
		TotalTokens:      total,
		CachedTokens:     s.cachedTokens,
		RawUsage:         s.lastUsage,
	}, true
}
