package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"github.com/google/uuid"
)

const (
	UsageSourceUpstream     = "upstream"
	UsageSourceEstimated    = "estimated"
	UsageSourceUnreported   = "unreported"
	LossThinkingTagFallback = "thinking_tag_fallback"
)

var ErrInvalidToolInput = errors.New("kiro invalid tool input")

type MeteredInt struct {
	Value   int64 `json:"value"`
	Present bool  `json:"present"`
}

type MeteredFloat struct {
	Value   float64 `json:"value"`
	Present bool    `json:"present"`
}

type UnknownCacheField struct {
	Name       string `json:"name"`
	NumberType string `json:"number_type"`
	SchemaHash string `json:"schema_hash"`
}

// KiroMetering preserves both values and wire presence. A reported zero is not
// interchangeable with a field that never appeared.
type KiroMetering struct {
	InputTokens         MeteredInt   `json:"input_tokens"`
	OutputTokens        MeteredInt   `json:"output_tokens"`
	CacheReadTokens     MeteredInt   `json:"cache_read_tokens"`
	CacheCreationTokens MeteredInt   `json:"cache_creation_tokens"`
	TotalInputTokens    MeteredInt   `json:"cache_total_input_tokens"`
	TotalTokens         MeteredInt   `json:"total_tokens"`
	Credits             MeteredFloat `json:"credits"`
	// EventCount is retained for compatibility and is the total number of token or
	// credit-bearing event envelopes observed. The two explicit counters below
	// distinguish the modern metadata event from the legacy credits event.
	EventCount          int                 `json:"event_count"`
	MetadataEventCount  int                 `json:"metadata_event_count"`
	MeteringEventCount  int                 `json:"metering_event_count"`
	ExplicitUnsupported bool                `json:"explicitly_unsupported"`
	UnknownCacheFields  []UnknownCacheField `json:"unknown_cache_fields,omitempty"`

	metadataInputPresent         bool
	metadataOutputPresent        bool
	metadataCacheReadPresent     bool
	metadataCacheCreationPresent bool
	metadataTotalPresent         bool
}

func (m KiroMetering) CacheFieldsPresent() bool {
	return m.CacheReadTokens.Present || m.CacheCreationTokens.Present
}

func (m KiroMetering) UsageSource() string {
	if m.InputTokens.Present || m.OutputTokens.Present || m.CacheFieldsPresent() || m.TotalTokens.Present {
		return UsageSourceUpstream
	}
	return UsageSourceUnreported
}

type ToolCall struct{ ID, Name, Input string }
type WebSearchResult struct {
	Title, URL, Snippet string
	PublishedAt         int64
}
type WebSearchData struct {
	Query, ToolUseID string
	Results          []WebSearchResult
}
type ResponseData struct {
	Text, Thinking                                                  string
	Tools                                                           []ToolCall
	WebSearch                                                       *WebSearchData
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64
	StopReason                                                      string
	Model                                                           string
	Metering                                                        KiroMetering
	UsageSource                                                     string
	CompatibilityLosses                                             []string
	SingleflightWaited                                              bool
	ToolDescriptionHashes                                           map[string]string
	CachePointCount                                                 int
	CachePointBreakpoints                                           []KiroCachePointBreakpoint
}

type ResponseDelta struct {
	Kind      string
	Text      string
	ToolID    string
	ToolName  string
	ToolInput string
	NewTool   bool
}

func DecodeWebSearchResponse(raw []byte, request WebSearchRequest, inputTokens int64) (ResponseData, error) {
	var envelope struct {
		Error  any `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := decodeUseNumber(raw, &envelope); err != nil {
		return ResponseData{}, err
	}
	if envelope.Error != nil {
		return ResponseData{}, fmt.Errorf("kiro MCP error: %v", envelope.Error)
	}
	data := &WebSearchData{Query: request.Query, ToolUseID: request.ToolUseID}
	for _, content := range envelope.Result.Content {
		if content.Type != "text" || strings.TrimSpace(content.Text) == "" {
			continue
		}
		var decoded struct {
			Results []struct {
				Title         string `json:"title"`
				URL           string `json:"url"`
				Snippet       string `json:"snippet"`
				PublishedDate int64  `json:"publishedDate"`
			} `json:"results"`
		}
		if decodeUseNumber([]byte(content.Text), &decoded) == nil {
			for _, result := range decoded.Results {
				data.Results = append(data.Results, WebSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Snippet, PublishedAt: result.PublishedDate})
			}
		}
	}
	summary := webSearchSummary(data)
	return ResponseData{
		Text: summary, WebSearch: data,
		InputTokens: max64(1, inputTokens), OutputTokens: max64(1, int64(len(summary)/4)),
		StopReason: "end_turn", UsageSource: UsageSourceEstimated,
	}, nil
}

func webSearchSummary(data *WebSearchData) string {
	if data == nil {
		return "No web search results were returned."
	}
	if len(data.Results) == 0 {
		return "No web search results were returned for: " + data.Query
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n", data.Query)
	for i, result := range data.Results {
		fmt.Fprintf(&b, "\n%d. %s\n%s", i+1, result.Title, result.URL)
		if result.Snippet != "" {
			fmt.Fprintf(&b, "\n%s", result.Snippet)
		}
	}
	return b.String()
}

type ResponseProcessor struct {
	names      map[string]string
	data       ResponseData
	ctx        context.Context
	options    bodysource.CaptureOptions
	text       *responseAccumulator
	thinking   *responseAccumulator
	toolInput  *responseAccumulator
	err        error
	activeTool ToolCall
	toolActive bool
	tagParser  thinkingTagParser
	losses     map[string]struct{}
}

func NewResponseProcessor(names map[string]string) *ResponseProcessor {
	return NewResponseProcessorWithOptions(context.Background(), names, bodysource.CaptureOptions{})
}

func NewResponseProcessorWithOptions(ctx context.Context, names map[string]string, options bodysource.CaptureOptions) *ResponseProcessor {
	if options.MaxBytes <= 0 {
		options.MaxBytes = 1 << 30
		options.MemoryThreshold = 8 << 20
	}
	return &ResponseProcessor{names: names, ctx: ctx, options: options, losses: map[string]struct{}{}}
}

func (p *ResponseProcessor) Data() ResponseData {
	out := p.data
	if p.text != nil {
		if value, err := p.text.String(); err == nil {
			out.Text += value
		} else {
			p.err = err
		}
	}
	if p.thinking != nil {
		if value, err := p.thinking.String(); err == nil {
			out.Thinking += value
		} else {
			p.err = err
		}
	}
	if p.toolActive {
		tool := p.activeTool
		if p.toolInput != nil {
			if value, err := p.toolInput.String(); err == nil {
				tool.Input = value
			} else {
				p.err = err
			}
		}
		out.Tools = append(append([]ToolCall(nil), out.Tools...), tool)
	}
	out.CompatibilityLosses = sortedStringSet(p.losses)
	return out
}

func (p *ResponseProcessor) Model() string          { return p.data.Model }
func (p *ResponseProcessor) Metering() KiroMetering { return p.data.Metering }

func (p *ResponseProcessor) Close() error {
	return errors.Join(p.text.Close(), p.thinking.Close(), p.toolInput.Close())
}

type responseAccumulator struct {
	buffer *bodysource.SpoolBuffer
	err    error
}

func newResponseAccumulator(ctx context.Context, options bodysource.CaptureOptions, prefix string) *responseAccumulator {
	options.TempFileNamePrefix = prefix
	buffer, err := bodysource.NewSpoolBuffer(ctx, options)
	return &responseAccumulator{buffer: buffer, err: err}
}

func (a *responseAccumulator) WriteString(value string) error {
	if a == nil {
		return nil
	}
	if a.err != nil {
		return a.err
	}
	_, a.err = io.WriteString(a.buffer, value)
	return a.err
}

func (a *responseAccumulator) String() (string, error) {
	if a == nil {
		return "", nil
	}
	if a.err != nil {
		return "", a.err
	}
	if view, ok := bodysource.ByteView(a.buffer); ok {
		return string(view), nil
	}
	raw, err := bodysource.ReadAll(a.buffer)
	return string(raw), err
}

func (a *responseAccumulator) Close() error {
	if a == nil || a.buffer == nil {
		return nil
	}
	return a.buffer.Close()
}

func (p *ResponseProcessor) accumulator(target **responseAccumulator, prefix string) *responseAccumulator {
	if *target == nil {
		*target = newResponseAccumulator(p.ctx, p.options, prefix)
	}
	return *target
}

func (p *ResponseProcessor) ProcessFrame(frame Frame) ([]ResponseDelta, error) {
	if p.err != nil {
		return nil, p.err
	}
	messageType := frame.HeaderString(":message-type")
	if messageType == "error" || messageType == "exception" {
		if messageType == "exception" && frame.HeaderString(":exception-type") == "ContentLengthExceededException" {
			return nil, fmt.Errorf("%w: %s", ErrContextTooLong, strings.TrimSpace(string(frame.Payload)))
		}
		return nil, fmt.Errorf("kiro %s %s: %s", messageType, first(frame.HeaderString(":error-code"), frame.HeaderString(":exception-type")), strings.TrimSpace(string(frame.Payload)))
	}
	eventType := frame.HeaderString(":event-type")
	var fields map[string]json.RawMessage
	if len(frame.Payload) > 0 {
		if err := decodeUseNumber(frame.Payload, &fields); err != nil {
			return nil, errors.New("kiro event payload is not JSON")
		}
	}
	switch eventType {
	case "assistantResponseEvent":
		content := rawJSONString(fields["content"])
		thinking := rawJSONString(fields["thinking"])
		if model := first(rawJSONString(fields["modelId"]), rawJSONString(fields["model"])); model != "" {
			if canonical, ok := capability.KiroCanonicalModel(model); ok {
				model = canonical
			}
			p.data.Model = model
		}
		var deltas []ResponseDelta
		if thinking != "" {
			if err := p.accumulator(&p.thinking, "codex-pool-kiro-thinking-*").WriteString(thinking); err != nil {
				p.err = err
				return nil, err
			}
			deltas = append(deltas, ResponseDelta{Kind: "thinking", Text: thinking})
		}
		if content != "" {
			parsed, usedFallback := p.tagParser.Feed(content)
			if usedFallback {
				p.losses[LossThinkingTagFallback] = struct{}{}
			}
			for _, delta := range parsed {
				if err := p.appendTextDelta(delta); err != nil {
					return nil, err
				}
				deltas = append(deltas, delta)
			}
		}
		return deltas, nil
	case "toolUseEvent":
		return p.processToolUseEvent(fields)
	case "metadataEvent":
		p.data.Metering.EventCount++
		p.data.Metering.MetadataEventCount++
		observeMeteringWithSource(fields, &p.data.Metering, true)
		p.syncMeteringFields()
		return nil, nil
	case "meteringEvent":
		p.data.Metering.EventCount++
		p.data.Metering.MeteringEventCount++
		observeKiroCredits(fields, &p.data.Metering)
		observeMeteringWithSource(fields, &p.data.Metering, false)
		p.syncMeteringFields()
		return nil, nil
	case "contextUsageEvent":
		if rawJSONNumberFloat(fields["contextUsagePercentage"]) >= 100 || rawJSONNumberFloat(fields["contextUsage"]) >= 100 {
			p.data.StopReason = "model_context_window_exceeded"
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (p *ResponseProcessor) appendTextDelta(delta ResponseDelta) error {
	var err error
	if delta.Kind == "thinking" {
		err = p.accumulator(&p.thinking, "codex-pool-kiro-thinking-*").WriteString(delta.Text)
	} else if delta.Kind == "text" {
		err = p.accumulator(&p.text, "codex-pool-kiro-text-*").WriteString(delta.Text)
	}
	p.err = err
	return err
}

func (p *ResponseProcessor) syncMeteringFields() {
	p.data.InputTokens = p.data.Metering.InputTokens.Value
	p.data.OutputTokens = p.data.Metering.OutputTokens.Value
	p.data.CacheReadTokens = p.data.Metering.CacheReadTokens.Value
	p.data.CacheCreationTokens = p.data.Metering.CacheCreationTokens.Value
	p.data.UsageSource = p.data.Metering.UsageSource()
}

// processToolUseEvent follows Kiro's stateful tool event protocol. Only the
// opening frame is guaranteed to carry name/toolUseId; input and stop frames
// commonly omit both. Input may be either JSON text fragments or a complete JSON
// object. Buffer until the tool is complete so a replacement object or malformed
// fragment can never be forwarded as an irreversible downstream SSE delta.
func (p *ResponseProcessor) processToolUseEvent(fields map[string]json.RawMessage) ([]ResponseDelta, error) {
	id := rawJSONString(fields["toolUseId"])
	if id == "" {
		id = rawScalarString(fields["toolUseId"])
	}
	name := rawJSONString(fields["name"])
	if original := p.names[name]; original != "" {
		name = original
	}

	var deltas []ResponseDelta
	if id != "" && p.toolActive && id != p.activeTool.ID {
		delta, err := p.finishActiveTool()
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}
	if !p.toolActive && (id != "" || name != "") {
		if id == "" {
			id = newSyntheticToolID()
		}
		p.activeTool = ToolCall{ID: id, Name: name}
		_ = p.toolInput.Close()
		p.toolInput = nil
		p.toolActive = true
	} else if p.toolActive && p.activeTool.Name == "" && name != "" {
		p.activeTool.Name = name
	}

	if raw, present := fields["input"]; present {
		if !p.toolActive {
			return nil, fmt.Errorf("%w: tool input arrived before tool start", ErrInvalidToolInput)
		}
		input, replace, err := decodeToolInput(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: tool_use_id=%s: %v", ErrInvalidToolInput, p.activeTool.ID, err)
		}
		if replace {
			_ = p.toolInput.Close()
			p.toolInput = nil
		}
		if err = p.accumulator(&p.toolInput, "codex-pool-kiro-tool-input-*").WriteString(input); err != nil {
			p.err = err
			return nil, err
		}
	}

	var stop bool
	_ = json.Unmarshal(fields["stop"], &stop)
	if stop && p.toolActive {
		delta, err := p.finishActiveTool()
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}
	return deltas, nil
}

func (p *ResponseProcessor) finishActiveTool() (ResponseDelta, error) {
	tool := p.activeTool
	if p.toolInput != nil {
		input, err := p.toolInput.String()
		if err != nil {
			p.err = err
			return ResponseDelta{}, err
		}
		tool.Input = input
	}
	input, err := normalizeToolInput(tool.Input)
	if err != nil {
		return ResponseDelta{}, fmt.Errorf("%w: tool_use_id=%s: %v", ErrInvalidToolInput, tool.ID, err)
	}
	tool.Input = input
	p.data.Tools = append(p.data.Tools, tool)
	p.activeTool = ToolCall{}
	p.toolActive = false
	_ = p.toolInput.Close()
	p.toolInput = nil
	return ResponseDelta{Kind: "tool", ToolID: tool.ID, ToolName: tool.Name, ToolInput: tool.Input, NewTool: true}, nil
}

func newSyntheticToolID() string {
	return "toolu_kiro_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func decodeToolInput(raw json.RawMessage) (input string, replace bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false, errors.New("empty input field")
	}
	if trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &input); err != nil {
			return "", false, errors.New("input string is not valid JSON")
		}
		return input, false, nil
	}
	// A non-string input is a complete value, not a stream fragment. Kiro uses
	// this shape for native tool calls, so it replaces any partial text collected
	// before it instead of being concatenated with it.
	return string(trimmed), true, nil
}

func normalizeToolInput(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "{}", nil
	}
	// Some endpoint versions wrap the completed JSON object in one additional
	// JSON string. Unwrap it without guessing at or repairing malformed content.
	if strings.HasPrefix(trimmed, `"`) {
		var nested string
		if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
			trimmed = strings.TrimSpace(nested)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
		return "", errors.New("input is not valid JSON")
	}
	if object == nil {
		return "", errors.New("input must be a JSON object")
	}
	return trimmed, nil
}

func (p *ResponseProcessor) Finish() ([]ResponseDelta, error) {
	var deltas []ResponseDelta
	parsed, usedFallback := p.tagParser.Finish()
	if usedFallback {
		p.losses[LossThinkingTagFallback] = struct{}{}
	}
	for _, delta := range parsed {
		if err := p.appendTextDelta(delta); err != nil {
			return deltas, err
		}
		deltas = append(deltas, delta)
	}
	if p.toolActive {
		delta, err := p.finishActiveTool()
		if err != nil {
			return deltas, err
		}
		deltas = append(deltas, delta)
	}
	for i := range p.data.Tools {
		input, err := normalizeToolInput(p.data.Tools[i].Input)
		if err != nil {
			return deltas, fmt.Errorf("%w: tool_use_id=%s: %v", ErrInvalidToolInput, p.data.Tools[i].ID, err)
		}
		p.data.Tools[i].Input = input
	}
	if len(p.data.Tools) > 0 {
		p.data.StopReason = "tool_use"
	} else if p.data.StopReason == "" {
		p.data.StopReason = "end_turn"
	}
	if p.data.UsageSource == "" {
		p.data.UsageSource = p.data.Metering.UsageSource()
	}
	return deltas, nil
}

func DecodeResponse(r io.Reader, names map[string]string) (ResponseData, error) {
	return DecodeResponseWithOptions(context.Background(), r, names, bodysource.CaptureOptions{})
}

func DecodeResponseWithOptions(ctx context.Context, r io.Reader, names map[string]string, options bodysource.CaptureOptions) (ResponseData, error) {
	decoder := NewDecoder()
	processor := NewResponseProcessorWithOptions(ctx, names, options)
	defer processor.Close()
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			frames, err := decoder.Feed(buf[:n])
			if err != nil {
				return processor.Data(), err
			}
			for _, frame := range frames {
				if _, err := processor.ProcessFrame(frame); err != nil {
					return processor.Data(), err
				}
			}
		}
		if readErr == io.EOF {
			if err := decoder.Finish(); err != nil {
				return processor.Data(), err
			}
			break
		}
		if readErr != nil {
			return processor.Data(), readErr
		}
	}
	if _, err := processor.Finish(); err != nil {
		return processor.Data(), err
	}
	return processor.Data(), nil
}

// applyFrame is retained for package-level compatibility tests and small callers.
// Stateful decoding should use ResponseProcessor so fragmented thinking tags and
// tool inputs are validated at stream completion.
func applyFrame(out *ResponseData, frame Frame, names map[string]string, tools map[string]int) error {
	processor := NewResponseProcessor(names)
	defer processor.Close()
	processor.data = *out
	// applyFrame predates the stateful processor and has no production callers.
	// Preserve state through a private sentinel for package-local compatibility;
	// streaming and non-streaming production decoders keep one processor instead.
	const activeToolIndexKey = "\x00kiro-active-tool"
	if index, ok := tools[activeToolIndexKey]; ok && index >= 0 && index < len(processor.data.Tools) {
		processor.activeTool = processor.data.Tools[index]
		processor.data.Tools = append(processor.data.Tools[:index], processor.data.Tools[index+1:]...)
		processor.toolActive = true
	}
	_, err := processor.ProcessFrame(frame)
	result := processor.Data()
	if tools != nil {
		for key := range tools {
			delete(tools, key)
		}
		for index, tool := range result.Tools {
			tools[tool.ID] = index
		}
		if processor.toolActive {
			tools[activeToolIndexKey] = len(result.Tools) - 1
		}
	}
	*out = result
	return err
}

type thinkingTagParser struct {
	pending    string
	inThinking bool
	sawTag     bool
}

func (p *thinkingTagParser) Feed(text string) ([]ResponseDelta, bool) {
	p.pending += text
	var out []ResponseDelta
	for {
		tag := "<thinking>"
		kind := "text"
		if p.inThinking {
			tag = "</thinking>"
			kind = "thinking"
		}
		if index := strings.Index(p.pending, tag); index >= 0 {
			if index > 0 {
				out = append(out, ResponseDelta{Kind: kind, Text: p.pending[:index]})
			}
			p.pending = p.pending[index+len(tag):]
			p.inThinking = !p.inThinking
			p.sawTag = true
			if p.inThinking && strings.HasPrefix(p.pending, "\n") {
				p.pending = p.pending[1:]
			}
			continue
		}
		keep := longestTagPrefixSuffix(p.pending, tag)
		emitLen := len(p.pending) - keep
		if emitLen > 0 {
			out = append(out, ResponseDelta{Kind: kind, Text: p.pending[:emitLen]})
			p.pending = p.pending[emitLen:]
		}
		return out, p.sawTag
	}
}

func (p *thinkingTagParser) Finish() ([]ResponseDelta, bool) {
	if p.pending == "" {
		return nil, p.sawTag
	}
	kind := "text"
	if p.inThinking {
		kind = "thinking"
	}
	out := []ResponseDelta{{Kind: kind, Text: p.pending}}
	p.pending = ""
	return out, p.sawTag
}

func longestTagPrefixSuffix(text, tag string) int {
	max := len(tag) - 1
	if len(text) < max {
		max = len(text)
	}
	for size := max; size > 0; size-- {
		if strings.HasSuffix(text, tag[:size]) {
			return size
		}
	}
	return 0
}

var meteringAliases = map[string]string{
	"uncachedInputTokens": "input", "uncached_input_tokens": "input",
	"inputTokens": "input", "inputTokenCount": "input", "input_tokens": "input",
	"outputTokens": "output", "outputTokenCount": "output", "output_tokens": "output",
	"cacheReadInputTokens": "cache_read", "cacheReadTokens": "cache_read", "cache_read_input_tokens": "cache_read", "cache_read_tokens": "cache_read",
	"cacheCreationInputTokens": "cache_creation", "cacheWriteInputTokens": "cache_creation", "cacheWriteTokens": "cache_creation", "cache_creation_input_tokens": "cache_creation", "cache_write_input_tokens": "cache_creation", "cache_write_tokens": "cache_creation",
	"totalTokens": "total", "totalTokenCount": "total", "total_tokens": "total",
}

func observeMetering(fields map[string]json.RawMessage, metering *KiroMetering) {
	observeMeteringWithSource(fields, metering, false)
}

func observeMeteringWithSource(fields map[string]json.RawMessage, metering *KiroMetering, metadata bool) {
	if metering == nil {
		return
	}
	unknown := map[string]UnknownCacheField{}
	observeMeteringObject(fields, "", metering, unknown, metadata)
	refreshKiroInputTotal(metering)
	if len(unknown) > 0 {
		byKey := map[string]UnknownCacheField{}
		for _, field := range metering.UnknownCacheFields {
			byKey[field.Name+"\x00"+field.NumberType] = field
		}
		for key, field := range unknown {
			byKey[key] = field
		}
		metering.UnknownCacheFields = metering.UnknownCacheFields[:0]
		for _, field := range byKey {
			metering.UnknownCacheFields = append(metering.UnknownCacheFields, field)
		}
		sort.Slice(metering.UnknownCacheFields, func(i, j int) bool {
			if metering.UnknownCacheFields[i].Name == metering.UnknownCacheFields[j].Name {
				return metering.UnknownCacheFields[i].NumberType < metering.UnknownCacheFields[j].NumberType
			}
			return metering.UnknownCacheFields[i].Name < metering.UnknownCacheFields[j].Name
		})
	}
}

func observeMeteringObject(fields map[string]json.RawMessage, prefix string, metering *KiroMetering, unknown map[string]UnknownCacheField, metadata bool) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := fields[key]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if target := meteringAliases[key]; target != "" {
			if value, present := rawJSONInt(raw); present {
				switch target {
				case "input":
					observeKiroToken(&metering.InputTokens, &metering.metadataInputPresent, value, metadata)
				case "output":
					observeKiroToken(&metering.OutputTokens, &metering.metadataOutputPresent, value, metadata)
				case "cache_read":
					observeKiroToken(&metering.CacheReadTokens, &metering.metadataCacheReadPresent, value, metadata)
				case "cache_creation":
					observeKiroToken(&metering.CacheCreationTokens, &metering.metadataCacheCreationPresent, value, metadata)
				case "total":
					observeKiroToken(&metering.TotalTokens, &metering.metadataTotalPresent, value, metadata)
				}
			}
			continue
		}
		lower := strings.ToLower(key)
		if (lower == "cachesupported" || lower == "promptcachesupported" || lower == "cache_supported") && bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			metering.ExplicitUnsupported = true
		}
		if (lower == "cachestatus" || lower == "cache_status") && strings.EqualFold(rawJSONString(raw), "unsupported") {
			metering.ExplicitUnsupported = true
		}
		var nested map[string]json.RawMessage
		if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' && decodeUseNumber(raw, &nested) == nil {
			observeMeteringObject(nested, path, metering, unknown, metadata)
			continue
		}
		if strings.Contains(lower, "cache") {
			if numberType := rawNumberType(raw); numberType != "" {
				sum := sha256.Sum256([]byte(path + "\x00" + numberType))
				field := UnknownCacheField{Name: path, NumberType: numberType, SchemaHash: hex.EncodeToString(sum[:])}
				unknown[path+"\x00"+numberType] = field
			}
		}
	}
}

func observeKiroToken(field *MeteredInt, metadataPresent *bool, value int64, metadata bool) {
	if field == nil || metadataPresent == nil {
		return
	}
	if metadata {
		if !*metadataPresent || value > field.Value {
			field.Value = value
		}
		field.Present = true
		*metadataPresent = true
		return
	}
	// Modern metadataEvent tokenUsage is authoritative regardless of whether it
	// arrives before or after the legacy meteringEvent.
	if *metadataPresent {
		return
	}
	observeMeteredInt(field, value)
}

func refreshKiroInputTotal(metering *KiroMetering) {
	if metering == nil {
		return
	}
	if !metering.InputTokens.Present && !metering.CacheReadTokens.Present && !metering.CacheCreationTokens.Present {
		return
	}
	metering.TotalInputTokens = MeteredInt{
		Value:   metering.InputTokens.Value + metering.CacheReadTokens.Value + metering.CacheCreationTokens.Value,
		Present: true,
	}
}

func observeKiroCredits(fields map[string]json.RawMessage, metering *KiroMetering) {
	if metering == nil {
		return
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := fields[key]
		if key == "usage" || key == "credits" {
			if value, present := rawJSONFloat(raw); present {
				if !metering.Credits.Present || value > metering.Credits.Value {
					metering.Credits.Value = value
				}
				metering.Credits.Present = true
			}
		}
		trimmed := bytes.TrimSpace(raw)
		var nested map[string]json.RawMessage
		if len(trimmed) > 0 && trimmed[0] == '{' && decodeUseNumber(raw, &nested) == nil {
			observeKiroCredits(nested, metering)
		}
	}
}

func observeMeteredInt(field *MeteredInt, value int64) {
	if !field.Present || value > field.Value {
		field.Value = value
	}
	field.Present = true
}

func rawJSONInt(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		value, err := strconv.ParseInt(text, 10, 64)
		return value, err == nil && value >= 0
	}
	number := json.Number(trimmed)
	value, err := number.Int64()
	if err == nil {
		return value, value >= 0
	}
	floatValue, err := number.Float64()
	if err != nil {
		return 0, false
	}
	if math.IsNaN(floatValue) || math.IsInf(floatValue, 0) || floatValue < 0 || math.Trunc(floatValue) != floatValue || floatValue >= 9223372036854775808.0 {
		return 0, false
	}
	return int64(floatValue), true
}

func rawJSONFloat(raw json.RawMessage) (float64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return 0, false
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		trimmed = text
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}

func rawJSONNumberFloat(raw json.RawMessage) float64 {
	trimmed := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	value, _ := strconv.ParseFloat(trimmed, 64)
	return value
}

func rawNumberType(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if _, err := strconv.ParseInt(text, 10, 64); err == nil {
				return "string_integer"
			}
			if _, err := strconv.ParseFloat(text, 64); err == nil {
				return "string_number"
			}
		}
		return ""
	}
	if _, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return "integer"
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return "number"
	}
	return ""
}

func rawJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func sortedStringSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func AnthropicJSON(data ResponseData, model, id string) []byte {
	blocks := contentBlocks(data)
	body := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": data.StopReason, "stop_sequence": nil, "usage": usageMap(data)}
	raw, _ := json.Marshal(body)
	return raw
}

func AnthropicSSE(data ResponseData, model, id string) []byte {
	var b strings.Builder
	emit := func(event string, value any) {
		raw, _ := json.Marshal(value)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, raw)
	}
	startUsage := map[string]any{"input_tokens": data.InputTokens, "output_tokens": 0}
	addCacheUsageFields(startUsage, data.Metering)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage}})
	index := 0
	if data.Thinking != "" {
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": data.Thinking}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	if data.WebSearch != nil {
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "server_tool_use", "id": data.WebSearch.ToolUseID, "name": "web_search", "input": map[string]any{"query": data.WebSearch.Query}}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "web_search_tool_result", "content": webSearchResultBlocks(data.WebSearch)}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	if data.Text != "" {
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": data.Text}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	for _, tool := range data.Tools {
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": tool.Input}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	deltaUsage := map[string]any{"output_tokens": data.OutputTokens}
	addCacheUsageFields(deltaUsage, data.Metering)
	if data.WebSearch != nil {
		deltaUsage["server_tool_use"] = map[string]any{"web_search_requests": 1}
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": data.StopReason, "stop_sequence": nil}, "usage": deltaUsage})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return []byte(b.String())
}

func contentBlocks(data ResponseData) []any {
	var out []any
	if data.Thinking != "" {
		out = append(out, map[string]any{"type": "thinking", "thinking": data.Thinking, "signature": ""})
	}
	if data.WebSearch != nil {
		out = append(out, map[string]any{"type": "server_tool_use", "id": data.WebSearch.ToolUseID, "name": "web_search", "input": map[string]any{"query": data.WebSearch.Query}})
		out = append(out, map[string]any{"type": "web_search_tool_result", "content": webSearchResultBlocks(data.WebSearch)})
	}
	if data.Text != "" {
		out = append(out, map[string]any{"type": "text", "text": data.Text})
	}
	for _, tool := range data.Tools {
		var input any
		if err := decodeUseNumber([]byte(tool.Input), &input); err != nil {
			// DecodeResponse/ResponseProcessor already rejects this. Keep a null
			// sentinel here rather than manufacturing an empty object if a caller
			// bypasses the decoder.
			input = nil
		}
		out = append(out, map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": input})
	}
	return out
}

func webSearchResultBlocks(data *WebSearchData) []any {
	out := make([]any, 0, len(data.Results))
	for _, result := range data.Results {
		var pageAge any
		if result.PublishedAt > 0 {
			pageAge = time.UnixMilli(result.PublishedAt).UTC().Format("January 2, 2006")
		}
		out = append(out, map[string]any{"type": "web_search_result", "title": result.Title, "url": result.URL, "encrypted_content": result.Snippet, "page_age": pageAge})
	}
	return out
}

func usageMap(data ResponseData) map[string]any {
	usage := map[string]any{"input_tokens": data.InputTokens, "output_tokens": data.OutputTokens}
	addCacheUsageFields(usage, data.Metering)
	if data.WebSearch != nil {
		usage["server_tool_use"] = map[string]any{"web_search_requests": 1}
	}
	return usage
}

func addCacheUsageFields(usage map[string]any, metering KiroMetering) {
	if metering.CacheReadTokens.Present {
		usage["cache_read_input_tokens"] = metering.CacheReadTokens.Value
	}
	if metering.CacheCreationTokens.Present {
		usage["cache_creation_input_tokens"] = metering.CacheCreationTokens.Value
	}
}
