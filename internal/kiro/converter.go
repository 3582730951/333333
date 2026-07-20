package kiro

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"codex-account-pool/internal/capability"
)

const (
	LossAssistantFirstPadded           = "assistant_first_padded"
	LossAssistantPrefillEmulated       = "assistant_prefill_emulated"
	LossThinkingHistoryTextualized     = "thinking_history_textualized"
	LossUnpairedToolUseTextualized     = "unpaired_tool_use_textualized"
	LossUnsupportedBlockTextualized    = "unsupported_block_textualized"
	LossToolDescriptionTruncated       = "tool_description_truncated"
	LossOutputFormatPromptEmulated     = "output_format_prompt_emulated"
	LossCacheControlNotForwarded       = "cache_control_not_forwarded"
	LossGenerationControlsNotForwarded = "generation_controls_not_forwarded"
	LossThinkingBudgetMappedToAdaptive = "thinking_budget_mapped_to_adaptive"
	LossThinkingForcedAdaptive         = "thinking_forced_adaptive"
	LossThinkingEffortForcedMax        = "thinking_effort_forced_max"
	LossMaxOutputReducedForContext     = "max_output_tokens_reduced_for_context"
	LossContextHistoryTruncated        = "context_history_truncated"
	LossToolSchemaRefBounded           = "tool_schema_ref_bounded"
)

const (
	kiroToolDescriptionRunes = 10_000
	kiroSchemaRefMaxDepth    = 32
	systemAcknowledgement    = "Acknowledged."
	prefillContinuation      = "Continue."
	assistantFirstPadding    = "Continue the conversation."
	kiroContextMinOutput     = int64(4096)
	kiroContextMinHeadroom   = int64(8192)
)

var (
	ErrReasoningUnavailable     = errors.New("kiro reasoning unavailable")
	ErrVerifiedModelUnavailable = errors.New("verified Kiro model unavailable")
	ErrUnsupportedModel         = errors.New("unsupported Kiro model")
	ErrContextTooLong           = errors.New("Kiro input context is too long")
)

type ContextLengthError struct {
	RequestedModel string
	KiroModel      string
	EstimatedInput int64
	EffectiveLimit int64
	ContextMode    string
}

func (e *ContextLengthError) Error() string {
	requested := e.RequestedModel
	if requested == "" {
		requested = e.KiroModel
	}
	return fmt.Sprintf("%v: requested_model=%s kiro_model=%s input_tokens~%d effective_limit=%d; 请运行 /compact 后重试", ErrContextTooLong, requested, e.KiroModel, e.EstimatedInput, e.EffectiveLimit)
}

func (e *ContextLengthError) Unwrap() error { return ErrContextTooLong }

type Conversion struct {
	Body                   []byte
	BodyWithoutCachePoints []byte
	Model                  string
	ToolNameMap            map[string]string
	ToolDescriptionHashes  map[string]string
	EstimatedInputTokens   int64
	// InputTokens remains as a compatibility alias for callers that only need a
	// local estimate (notably the MCP web-search bridge). It must never be reported
	// as upstream metering.
	InputTokens            int64
	WebSearch              *WebSearchRequest
	CompatibilityLosses    []string
	ThinkingEnabled        bool
	ThinkingEffort         string
	MaxOutputTokens        int64
	ContextWindow          int64
	OriginalInputTokens    int64
	HistoryMessagesDropped int
	CachePointCount        int
	CachePointBreakpoints  []KiroCachePointBreakpoint
}

type KiroCachePointBreakpoint struct {
	Section      string `json:"section"`
	ToolIndex    int    `json:"tool_index,omitempty"`
	MessageIndex int    `json:"message_index,omitempty"`
}

// ConversionOptions controls Kiro-only compatibility defaults. Explicit downstream
// fields take precedence over DefaultThinking. ForceMaxQuality is the production
// relay policy: native adaptive thinking, maximum supported effort, and the model's
// maximum output ceiling. The ceiling is reduced only when required to keep the
// converted input plus output reserve inside ContextWindow.
type ConversionOptions struct {
	DefaultThinking bool
	ForceMaxQuality bool
	// EnableCachePoints maps Anthropic cache_control markers into Kiro/Bedrock
	// cachePoint objects. TTL is intentionally not forwarded; Kiro uses its default.
	EnableCachePoints bool
	// ContextWindow is the selected account/model's maximum input+output window.
	// Zero uses the conservative static Kiro catalogue value.
	ContextWindow int64
	// VerifiedModels is the selected account's successfully observed model set.
	// It is required to resolve family/auto aliases; concrete versions never use a
	// different verified version as a substitute.
	VerifiedModels []string
}

type WebSearchRequest struct{ Query, ToolUseID string }

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []json.RawMessage  `json:"tools"`
	Thinking      *anthropicThinking `json:"thinking"`
	OutputConfig  *anthropicOutput   `json:"output_config"`
	Metadata      json.RawMessage    `json:"metadata"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	MaxTokens     json.RawMessage    `json:"max_tokens"`
	Temperature   json.RawMessage    `json:"temperature"`
	TopP          json.RawMessage    `json:"top_p"`
	TopK          json.RawMessage    `json:"top_k"`
	StopSequences json.RawMessage    `json:"stop_sequences"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Cache   json.RawMessage `json:"cache_control"`
}

type anthropicThinking struct {
	Type   string          `json:"type"`
	Budget json.RawMessage `json:"budget_tokens"`
}

type anthropicOutput struct {
	Format json.RawMessage `json:"format"`
	Effort string          `json:"effort"`
}

type contentBlock struct {
	Type      string
	Text      string
	Thinking  string
	ID        json.RawMessage
	Name      string
	Input     json.RawMessage
	ToolUseID json.RawMessage
	IsError   bool
	Source    json.RawMessage
	Content   json.RawMessage
	Raw       json.RawMessage
	HasCache  bool
}

type toolDefinition struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Cache       json.RawMessage `json:"cache_control"`
}

type lossSet map[string]struct{}

func (l lossSet) add(code string) {
	if code != "" {
		l[code] = struct{}{}
	}
}

func (l lossSet) sorted() []string {
	out := make([]string, 0, len(l))
	for code := range l {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

func ConvertAnthropicRequest(raw []byte, affinity string) (Conversion, error) {
	return ConvertAnthropicRequestWithOptions(raw, affinity, ConversionOptions{})
}

func ConvertAnthropicRequestWithOptions(raw []byte, affinity string, options ConversionOptions) (Conversion, error) {
	var req anthropicRequest
	if err := decodeUseNumber(raw, &req); err != nil {
		return Conversion{}, err
	}
	canonical, ok := capability.ResolveKiroModel(req.Model, options.VerifiedModels)
	if !ok {
		if capability.KiroModelAlias(req.Model) {
			return Conversion{}, fmt.Errorf("%w for alias %q", ErrVerifiedModelUnavailable, req.Model)
		}
		return Conversion{}, fmt.Errorf("%w %q", ErrUnsupportedModel, req.Model)
	}
	if len(req.Messages) == 0 {
		return Conversion{}, errors.New("messages required")
	}

	losses := lossSet{}
	cachePlan, cacheMarkersPresent, err := planKiroCachePoints(req, options.EnableCachePoints)
	if err != nil {
		return Conversion{}, err
	}
	if cacheMarkersPresent && !options.EnableCachePoints {
		losses.add(LossCacheControlNotForwarded)
	}
	toolNames := map[string]string{}
	descriptionHashes := map[string]string{}
	tools, err := convertTools(req.Tools, toolNames, descriptionHashes, losses)
	if err != nil {
		return Conversion{}, err
	}
	webSearch := pureWebSearchRequest(req.Tools, req.Messages, affinity)
	if webSearch != nil {
		cachePlan = kiroCachePointPlan{tools: map[int]bool{}, messages: map[int]bool{}}
	}
	uses, paired, err := collectToolPairs(req.Messages)
	if err != nil {
		return Conversion{}, err
	}

	history := make([]any, 0, len(req.Messages)+4)
	historySpans := make([]kiroHistorySpan, 0, len(req.Messages))
	systemPrompt, systemPresent, err := systemContent(req.System, losses)
	if err != nil {
		return Conversion{}, err
	}
	if req.OutputConfig != nil && len(bytes.TrimSpace(req.OutputConfig.Format)) > 0 && !bytes.Equal(bytes.TrimSpace(req.OutputConfig.Format), []byte("null")) {
		format, err := canonicalJSON(req.OutputConfig.Format)
		if err != nil {
			return Conversion{}, fmt.Errorf("output_config.format: %w", err)
		}
		if systemPresent {
			systemPrompt += "\n"
		}
		systemPrompt += "Return output that strictly satisfies this JSON format: " + string(format)
		systemPresent = true
		losses.add(LossOutputFormatPromptEmulated)
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{{"metadata", req.Metadata}, {"tool_choice", req.ToolChoice}} {
		if len(bytes.TrimSpace(field.raw)) == 0 || bytes.Equal(bytes.TrimSpace(field.raw), []byte("null")) {
			continue
		}
		value, err := canonicalValue(field.raw, nil)
		if err != nil {
			return Conversion{}, fmt.Errorf("%s: %w", field.name, err)
		}
		encoded, _ := json.Marshal(map[string]any{"name": field.name, "value": value})
		if systemPresent {
			systemPrompt += "\n"
		}
		systemPrompt += "<unsupported_request_field>" + string(encoded) + "</unsupported_request_field>"
		systemPresent = true
		losses.add(LossUnsupportedBlockTextualized)
	}
	controls := []json.RawMessage{req.Temperature, req.TopP, req.TopK, req.StopSequences}
	if !options.ForceMaxQuality {
		controls = append(controls, req.MaxTokens)
	}
	for _, control := range controls {
		if len(bytes.TrimSpace(control)) > 0 && !bytes.Equal(bytes.TrimSpace(control), []byte("null")) {
			losses.add(LossGenerationControlsNotForwarded)
			break
		}
	}
	systemAcknowledgementHistoryIndex := -1
	if systemPresent {
		history = append(history,
			map[string]any{"userInputMessage": map[string]any{"content": systemPrompt, "origin": "AI_EDITOR", "userInputMessageContext": map[string]any{}}},
			map[string]any{"assistantResponseMessage": map[string]any{"content": systemAcknowledgement}},
		)
		systemAcknowledgementHistoryIndex = len(history) - 1
	}

	lastIsUser := strings.EqualFold(strings.TrimSpace(req.Messages[len(req.Messages)-1].Role), "user")
	historyEnd := len(req.Messages) - 1
	if !lastIsUser {
		historyEnd = len(req.Messages)
		losses.add(LossAssistantPrefillEmulated)
	}
	messageHistoryIndexes := map[int]int{}
	for i := 0; i < historyEnd; i++ {
		spanStart := len(history)
		converted, role, err := convertHistory(req.Messages[i], canonical, toolNames, uses, paired, losses)
		if err != nil {
			return Conversion{}, err
		}
		if role == "assistant" && (len(history) == 0 || historyItemRole(history[len(history)-1]) == "assistant") {
			history = append(history, map[string]any{
				"userInputMessage": map[string]any{"content": assistantFirstPadding, "modelId": canonical, "origin": "AI_EDITOR", "userInputMessageContext": map[string]any{}},
			})
			losses.add(LossAssistantFirstPadded)
		}
		messageHistoryIndexes[i] = len(history)
		history = append(history, converted)
		startsTurn, err := anthropicMessageStartsTurn(req.Messages[i])
		if err != nil {
			return Conversion{}, fmt.Errorf("message %d: %w", i, err)
		}
		historySpans = append(historySpans, kiroHistorySpan{
			messageIndex: i,
			start:        spanStart,
			end:          len(history),
			startsTurn:   startsTurn,
		})
	}

	var current map[string]any
	if lastIsUser {
		current, err = convertUserMessage(req.Messages[len(req.Messages)-1], canonical, uses, losses)
		if err != nil {
			return Conversion{}, err
		}
	} else {
		current = map[string]any{"content": prefillContinuation, "modelId": canonical, "origin": "AI_EDITOR", "userInputMessageContext": map[string]any{}}
	}
	ctx, _ := current["userInputMessageContext"].(map[string]any)
	if ctx == nil {
		ctx = map[string]any{}
		current["userInputMessageContext"] = ctx
	}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}

	state := map[string]any{
		"conversationId":      stableUUID("conversation", affinity),
		"agentContinuationId": stableUUID("continuation", affinity),
		"agentTaskType":       "vibe",
		"chatTriggerType":     "MANUAL",
		"currentMessage":      map[string]any{"userInputMessage": current},
	}
	if len(history) > 0 {
		state["history"] = history
	}
	out := map[string]any{"conversationState": state}

	thinkingEnabled := options.DefaultThinking
	explicitlyDisabled := false
	if req.Thinking != nil {
		switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
		case "disabled", "none":
			thinkingEnabled = false
			explicitlyDisabled = true
		case "enabled", "adaptive", "auto":
			thinkingEnabled = true
		}
	}
	if options.ForceMaxQuality {
		if explicitlyDisabled {
			losses.add(LossThinkingForcedAdaptive)
		}
		thinkingEnabled = true
	}
	if thinkingEnabled && !capability.KiroSupportsAdaptiveThinking(canonical) {
		return Conversion{}, fmt.Errorf("%w: model %s does not support adaptive thinking", ErrReasoningUnavailable, canonical)
	}
	thinkingEffort := ""
	if req.OutputConfig != nil {
		thinkingEffort = normalizeKiroThinkingEffort(req.OutputConfig.Effort)
	}
	if options.ForceMaxQuality {
		if thinkingEffort != "" && thinkingEffort != "max" {
			losses.add(LossThinkingEffortForcedMax)
		}
		thinkingEffort = "max"
	}
	additionalFields := map[string]any{}
	if thinkingEnabled {
		if req.Thinking != nil && len(bytes.TrimSpace(req.Thinking.Budget)) > 0 && !bytes.Equal(bytes.TrimSpace(req.Thinking.Budget), []byte("null")) {
			losses.add(LossThinkingBudgetMappedToAdaptive)
		}
		additionalFields["thinking"] = map[string]any{"type": "adaptive"}
		if thinkingEffort != "" {
			additionalFields["output_config"] = map[string]any{"effort": thinkingEffort}
		}
	}
	maxOutputTokens := int64(0)
	if options.ForceMaxQuality {
		maxOutputTokens = kiroMaxOutputTokens(canonical)
		additionalFields["max_tokens"] = maxOutputTokens
	}
	if len(additionalFields) > 0 {
		out["additionalModelRequestFields"] = additionalFields
	}
	bodyWithoutCachePoints, err := json.Marshal(out)
	if err != nil {
		return Conversion{}, err
	}
	contextWindow := options.ContextWindow
	if contextWindow <= 0 {
		contextWindow = capability.KiroContextWindow(canonical)
	}
	originalInputTokens := estimateKiroTokens(bodyWithoutCachePoints)
	estimatedInputTokens := originalInputTokens
	historyMessagesDropped := 0
	if maxOutputTokens > 0 && webSearch == nil {
		if estimatedInputTokens >= contextWindow {
			return Conversion{}, &ContextLengthError{KiroModel: canonical, EstimatedInput: estimatedInputTokens, EffectiveLimit: contextWindow}
		}
		availableOutput := contextWindow - estimatedInputTokens
		if maxOutputTokens > availableOutput {
			maxOutputTokens = availableOutput
			additionalFields["max_tokens"] = maxOutputTokens
			losses.add(LossMaxOutputReducedForContext)
			bodyWithoutCachePoints, err = json.Marshal(out)
			if err != nil {
				return Conversion{}, err
			}
			estimatedInputTokens = estimateKiroTokens(bodyWithoutCachePoints)
		}
	}
	if options.EnableCachePoints && cachePlan.count > 0 {
		if len(cachePlan.tools) > 0 {
			ctx["tools"] = insertKiroToolCachePoints(tools, cachePlan.tools)
		}
		if cachePlan.system && systemAcknowledgementHistoryIndex >= 0 {
			setKiroHistoryCachePoint(history[systemAcknowledgementHistoryIndex])
		}
		for messageIndex := range cachePlan.messages {
			if historyIndex, ok := messageHistoryIndexes[messageIndex]; ok {
				setKiroHistoryCachePoint(history[historyIndex])
			}
		}
		if lastIsUser && cachePlan.messages[len(req.Messages)-1] {
			current["cachePoint"] = defaultKiroCachePoint()
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return Conversion{}, err
	}
	if cachePlan.count == 0 {
		bodyWithoutCachePoints = nil
	}
	if options.EnableCachePoints && !cachePlan.dropped {
		delete(losses, LossCacheControlNotForwarded)
	}
	return Conversion{
		Body:                   body,
		BodyWithoutCachePoints: bodyWithoutCachePoints,
		Model:                  canonical,
		ToolNameMap:            toolNames,
		ToolDescriptionHashes:  descriptionHashes,
		EstimatedInputTokens:   estimatedInputTokens,
		InputTokens:            estimatedInputTokens,
		WebSearch:              webSearch,
		CompatibilityLosses:    losses.sorted(),
		ThinkingEnabled:        thinkingEnabled,
		ThinkingEffort:         thinkingEffort,
		MaxOutputTokens:        maxOutputTokens,
		ContextWindow:          contextWindow,
		OriginalInputTokens:    originalInputTokens,
		HistoryMessagesDropped: historyMessagesDropped,
		CachePointCount:        cachePlan.count,
		CachePointBreakpoints:  append([]KiroCachePointBreakpoint(nil), cachePlan.breakpoints...),
	}, nil
}

type kiroHistorySpan struct {
	messageIndex int
	start        int
	end          int
	startsTurn   bool
}

type kiroHistoryGroup struct {
	start int
	end   int
}

func anthropicMessageStartsTurn(message anthropicMessage) (bool, error) {
	if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
		return false, nil
	}
	blocks, _, err := parseContentBlocks(message.Content)
	if err != nil {
		return false, err
	}
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return false, nil
		}
	}
	return true, nil
}

func trimKiroHistoryToBudget(out, state map[string]any, history []any, spans []kiroHistorySpan, systemHistoryLen int, targetInput int64, protectLastGroup bool) ([]any, int, int, []byte, int64, error) {
	groups := make([]kiroHistoryGroup, 0, len(spans))
	for _, span := range spans {
		if len(groups) == 0 || span.startsTurn {
			groups = append(groups, kiroHistoryGroup{start: span.start, end: span.end})
			continue
		}
		groups[len(groups)-1].end = span.end
	}
	removable := len(groups)
	if protectLastGroup && removable > 0 {
		removable--
	}
	body, err := json.Marshal(out)
	if err != nil {
		return history, systemHistoryLen, 0, nil, 0, err
	}
	estimate := estimateKiroTokens(body)
	cutoff := systemHistoryLen
	dropped := 0
	trimmed := history
	for i := 0; i < removable && estimate > targetInput; i++ {
		cutoff = groups[i].end
		dropped = 0
		for _, span := range spans {
			if span.end <= cutoff {
				dropped++
			}
		}
		trimmed = make([]any, 0, systemHistoryLen+len(history)-cutoff)
		trimmed = append(trimmed, history[:systemHistoryLen]...)
		trimmed = append(trimmed, history[cutoff:]...)
		if len(trimmed) == 0 {
			delete(state, "history")
		} else {
			state["history"] = trimmed
		}
		body, err = json.Marshal(out)
		if err != nil {
			return history, systemHistoryLen, 0, nil, 0, err
		}
		estimate = estimateKiroTokens(body)
	}
	return trimmed, cutoff, dropped, body, estimate, nil
}

func adjustKiroMessageIndexes(indexes map[int]int, systemHistoryLen, cutoff int) {
	removed := cutoff - systemHistoryLen
	for messageIndex, historyIndex := range indexes {
		if historyIndex >= systemHistoryLen && historyIndex < cutoff {
			delete(indexes, messageIndex)
			continue
		}
		if historyIndex >= cutoff {
			indexes[messageIndex] = historyIndex - removed
		}
	}
}

func dropKiroCachePlanMessages(plan *kiroCachePointPlan, spans []kiroHistorySpan, cutoff int) {
	if plan == nil || cutoff <= 0 {
		return
	}
	dropped := map[int]bool{}
	for _, span := range spans {
		if span.end <= cutoff {
			dropped[span.messageIndex] = true
			if plan.messages[span.messageIndex] {
				delete(plan.messages, span.messageIndex)
				plan.count--
			}
		}
	}
	if len(dropped) == 0 {
		return
	}
	breakpoints := plan.breakpoints[:0]
	for _, breakpoint := range plan.breakpoints {
		if breakpoint.Section == "messages" && dropped[breakpoint.MessageIndex] {
			continue
		}
		breakpoints = append(breakpoints, breakpoint)
	}
	plan.breakpoints = breakpoints
}

func estimateKiroTokens(body []byte) int64 {
	return max64(1, int64(utf8.RuneCount(body)/4+1))
}

type kiroCachePointPlan struct {
	tools       map[int]bool
	system      bool
	messages    map[int]bool
	count       int
	dropped     bool
	breakpoints []KiroCachePointBreakpoint
}

func planKiroCachePoints(req anthropicRequest, enabled bool) (kiroCachePointPlan, bool, error) {
	plan := kiroCachePointPlan{tools: map[int]bool{}, messages: map[int]bool{}}
	toolMarkers := make([]bool, len(req.Tools))
	markersPresent := false
	for i, raw := range req.Tools {
		var tool toolDefinition
		if err := decodeUseNumber(raw, &tool); err != nil {
			return plan, false, fmt.Errorf("tool: %w", err)
		}
		toolMarkers[i] = jsonFieldPresent(tool.Cache)
		markersPresent = markersPresent || toolMarkers[i]
	}
	systemMarker, err := rawContentHasCacheControl(req.System)
	if err != nil {
		return plan, false, fmt.Errorf("system: %w", err)
	}
	markersPresent = markersPresent || systemMarker
	messageMarkers := make([]bool, len(req.Messages))
	for i, message := range req.Messages {
		messageMarkers[i] = jsonFieldPresent(message.Cache)
		contentMarker, err := rawContentHasCacheControl(message.Content)
		if err != nil {
			return plan, false, fmt.Errorf("message %d: %w", i, err)
		}
		messageMarkers[i] = messageMarkers[i] || contentMarker
		markersPresent = markersPresent || messageMarkers[i]
	}
	if !enabled {
		return plan, markersPresent, nil
	}
	add := func(breakpoint KiroCachePointBreakpoint, selectPoint func()) {
		if plan.count >= 4 {
			plan.dropped = true
			return
		}
		selectPoint()
		plan.count++
		plan.breakpoints = append(plan.breakpoints, breakpoint)
	}
	for i, marked := range toolMarkers {
		if !marked {
			continue
		}
		index := i
		add(KiroCachePointBreakpoint{Section: "tools", ToolIndex: i}, func() { plan.tools[index] = true })
	}
	if systemMarker {
		add(KiroCachePointBreakpoint{Section: "system"}, func() { plan.system = true })
	}
	for i, marked := range messageMarkers {
		if !marked {
			continue
		}
		index := i
		add(KiroCachePointBreakpoint{Section: "messages", MessageIndex: i}, func() { plan.messages[index] = true })
	}
	return plan, markersPresent, nil
}

func jsonFieldPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func rawContentHasCacheControl(raw json.RawMessage) (bool, error) {
	blocks, _, err := parseContentBlocks(raw)
	if err != nil {
		return false, err
	}
	for _, block := range blocks {
		if block.HasCache {
			return true, nil
		}
	}
	return false, nil
}

func defaultKiroCachePoint() map[string]any {
	return map[string]any{"type": "default"}
}

func insertKiroToolCachePoints(tools []any, selected map[int]bool) []any {
	out := make([]any, 0, len(tools)+len(selected))
	for i, tool := range tools {
		out = append(out, tool)
		if selected[i] {
			out = append(out, map[string]any{"cachePoint": defaultKiroCachePoint()})
		}
	}
	return out
}

func setKiroHistoryCachePoint(item any) {
	wrapper, _ := item.(map[string]any)
	if wrapper == nil {
		return
	}
	for _, key := range []string{"userInputMessage", "assistantResponseMessage"} {
		if message, ok := wrapper[key].(map[string]any); ok {
			message["cachePoint"] = defaultKiroCachePoint()
			return
		}
	}
}

func normalizeKiroThinkingEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

func kiroMaxOutputTokens(model string) int64 {
	if canonical, ok := capability.KiroCanonicalModel(model); ok && canonical == "claude-opus-4.8" {
		return 128000
	}
	return 64000
}

func historyItemRole(v any) string {
	m, _ := v.(map[string]any)
	if m["assistantResponseMessage"] != nil {
		return "assistant"
	}
	if m["userInputMessage"] != nil {
		return "user"
	}
	return ""
}

func pureWebSearchRequest(toolsRaw []json.RawMessage, messages []anthropicMessage, affinity string) *WebSearchRequest {
	if len(toolsRaw) != 1 {
		return nil
	}
	var tool toolDefinition
	if decodeUseNumber(toolsRaw[0], &tool) != nil {
		return nil
	}
	name := tool.Name
	if name == "" && strings.HasPrefix(strings.ToLower(tool.Type), "web_search") {
		name = "web_search"
	}
	if !strings.EqualFold(name, "web_search") {
		return nil
	}
	var query string
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		query = strings.TrimSpace(contentText(messages[i].Content))
		break
	}
	query = strings.TrimSpace(strings.TrimPrefix(query, "Perform a web search for the query: "))
	if query == "" {
		return nil
	}
	id := strings.ReplaceAll(stableUUID("web_search", affinity+"\x00"+query), "-", "")
	return &WebSearchRequest{Query: query, ToolUseID: "srvtoolu_" + id}
}

func stableUUID(kind, key string) string {
	s := sha256.Sum256([]byte(kind + "\x00" + key))
	b := s[:16]
	// Version 4 nibble: a real Kiro client emits v4 conversation/continuation IDs.
	// Kept deterministic (derived, not random) so the Kiro prompt-cache key stays
	// stable across a conversation — only the version shape is corrected, v5→v4.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func convertTools(rawTools []json.RawMessage, names, descriptionHashes map[string]string, losses lossSet) ([]any, error) {
	out := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		var tool toolDefinition
		if err := decodeUseNumber(raw, &tool); err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		nameValue := tool.Name
		if nameValue == "" && strings.HasPrefix(strings.ToLower(tool.Type), "web_search") {
			nameValue = "web_search"
			tool.Description = "Search the web through Kiro MCP"
			tool.InputSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
		}
		if len(bytes.TrimSpace(tool.Cache)) > 0 && !bytes.Equal(bytes.TrimSpace(tool.Cache), []byte("null")) {
			losses.add(LossCacheControlNotForwarded)
		}
		name := shortToolName(nameValue, names)
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(bytes.TrimSpace(tool.InputSchema)) > 0 && !bytes.Equal(bytes.TrimSpace(tool.InputSchema), []byte("null")) {
			if err := decodeUseNumber(tool.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("tool %q input_schema: %w", nameValue, err)
			}
		}
		schema = expandRefs(schema, schema, 0, map[string]bool{}, losses)
		description := tool.Description
		if utf8.RuneCountInString(description) > kiroToolDescriptionRunes {
			sum := sha256.Sum256([]byte(description))
			descriptionHashes[nameValue] = hex.EncodeToString(sum[:])
			description = string([]rune(description)[:kiroToolDescriptionRunes])
			losses.add(LossToolDescriptionTruncated)
		}
		out = append(out, map[string]any{"toolSpecification": map[string]any{
			"name": name, "description": description, "inputSchema": map[string]any{"json": schema},
		}})
	}
	return out, nil
}

func shortToolName(name string, names map[string]string) string {
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	prefix := []rune(name)
	if len(prefix) > 54 {
		prefix = prefix[:54]
	}
	short := string(prefix) + "_" + hex.EncodeToString(sum[:4])
	names[short] = name
	return short
}

func expandRefs(v, root any, depth int, visiting map[string]bool, losses lossSet) any {
	if depth >= kiroSchemaRefMaxDepth {
		losses.add(LossToolSchemaRefBounded)
		return map[string]any{}
	}
	switch value := v.(type) {
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = expandRefs(value[i], root, depth+1, visiting, losses)
		}
		return out
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			if visiting[ref] {
				losses.add(LossToolSchemaRefBounded)
				return map[string]any{}
			}
			resolved, ok := resolveLocalRef(root, ref)
			if !ok {
				losses.add(LossToolSchemaRefBounded)
				return map[string]any{"$ref": ref}
			}
			next := cloneStringBoolMap(visiting)
			next[ref] = true
			return expandRefs(resolved, root, depth+1, next, losses)
		}
		out := make(map[string]any, len(value))
		for key, child := range value {
			if key == "$defs" || key == "definitions" {
				continue
			}
			out[key] = expandRefs(child, root, depth+1, visiting, losses)
		}
		return out
	default:
		return v
	}
}

func resolveLocalRef(root any, ref string) (any, bool) {
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func cloneStringBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func convertHistory(message anthropicMessage, model string, names map[string]string, uses, paired map[string]bool, losses lossSet) (any, string, error) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role == "assistant" {
		text, _, _, blocks, err := contentParts(message.Content, uses, losses)
		if err != nil {
			return nil, role, err
		}
		var toolUses []any
		for _, block := range blocks {
			if block.Type != "tool_use" {
				continue
			}
			id := rawScalarString(block.ID)
			if !paired[id] {
				text = appendText(text, textualizedBlock("unpaired_tool_use", block.Raw))
				losses.add(LossUnpairedToolUseTextualized)
				continue
			}
			input, err := canonicalValue(block.Input, map[string]any{})
			if err != nil {
				return nil, role, fmt.Errorf("tool_use %s input: %w", id, err)
			}
			toolUses = append(toolUses, map[string]any{"toolUseId": id, "name": shortToolName(block.Name, names), "input": input})
		}
		response := map[string]any{"content": text}
		if len(toolUses) > 0 {
			response["toolUses"] = toolUses
		}
		return map[string]any{"assistantResponseMessage": response}, role, nil
	}
	user, err := convertUserMessage(message, model, uses, losses)
	if err != nil {
		return nil, role, err
	}
	return map[string]any{"userInputMessage": user}, "user", nil
}

func convertUserMessage(message anthropicMessage, model string, uses map[string]bool, losses lossSet) (map[string]any, error) {
	text, images, results, _, err := contentParts(message.Content, uses, losses)
	if err != nil {
		return nil, err
	}
	ctx := map[string]any{}
	if len(results) > 0 {
		ctx["toolResults"] = results
	}
	user := map[string]any{"content": text, "modelId": model, "origin": "AI_EDITOR", "userInputMessageContext": ctx}
	if len(images) > 0 {
		user["images"] = images
	}
	return user, nil
}

func collectToolPairs(messages []anthropicMessage) (map[string]bool, map[string]bool, error) {
	uses, results := map[string]bool{}, map[string]bool{}
	for _, message := range messages {
		blocks, _, err := parseContentBlocks(message.Content)
		if err != nil {
			return nil, nil, err
		}
		for _, block := range blocks {
			switch block.Type {
			case "tool_use":
				uses[rawScalarString(block.ID)] = true
			case "tool_result":
				results[rawScalarString(block.ToolUseID)] = true
			}
		}
	}
	paired := map[string]bool{}
	for id := range uses {
		if id != "" && results[id] {
			paired[id] = true
		}
	}
	return uses, paired, nil
}

func contentParts(raw json.RawMessage, uses map[string]bool, losses lossSet) (string, []any, []any, []contentBlock, error) {
	blocks, plain, err := parseContentBlocks(raw)
	if err != nil {
		return "", nil, nil, nil, err
	}
	text := plain
	var images, results []any
	for _, block := range blocks {
		if block.HasCache {
			losses.add(LossCacheControlNotForwarded)
		}
		switch block.Type {
		case "text":
			text = appendText(text, block.Text)
		case "thinking", "redacted_thinking":
			thinking := block.Thinking
			if thinking == "" {
				thinking = block.Text
			}
			text = appendText(text, "<thinking>\n"+thinking+"\n</thinking>")
			losses.add(LossThinkingHistoryTextualized)
		case "image":
			image, ok, err := convertImage(block.Source)
			if err != nil {
				return "", nil, nil, nil, err
			}
			if ok {
				images = append(images, image)
			} else {
				text = appendText(text, textualizedBlock("unsupported_block", block.Raw))
				losses.add(LossUnsupportedBlockTextualized)
			}
		case "tool_result":
			id := rawScalarString(block.ToolUseID)
			if uses != nil && !uses[id] {
				text = appendText(text, textualizedBlock("unsupported_block", block.Raw))
				losses.add(LossUnsupportedBlockTextualized)
				continue
			}
			status := "success"
			if block.IsError {
				status = "error"
			}
			resultText, err := toolResultContent(block.Content, losses)
			if err != nil {
				return "", nil, nil, nil, err
			}
			results = append(results, map[string]any{
				"toolUseId": id,
				"content":   []any{map[string]any{"text": resultText}},
				"status":    status,
				"isError":   block.IsError,
			})
		case "tool_use":
			// Assistant tool uses are emitted natively by convertHistory. A tool-use
			// block in user content has no Kiro representation and is preserved here.
			if uses == nil {
				text = appendText(text, textualizedBlock("unpaired_tool_use", block.Raw))
				losses.add(LossUnpairedToolUseTextualized)
			}
		default:
			text = appendText(text, textualizedBlock("unsupported_block", block.Raw))
			losses.add(LossUnsupportedBlockTextualized)
		}
	}
	return text, images, results, blocks, nil
}

func parseContentBlocks(raw json.RawMessage) ([]contentBlock, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, "", err
		}
		return nil, text, nil
	}
	var rawBlocks []json.RawMessage
	if err := decodeUseNumber(trimmed, &rawBlocks); err != nil {
		return nil, "", err
	}
	out := make([]contentBlock, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		block, err := parseContentBlock(rawBlock)
		if err != nil {
			return nil, "", err
		}
		out = append(out, block)
	}
	return out, "", nil
}

func parseContentBlock(raw json.RawMessage) (contentBlock, error) {
	var fields map[string]json.RawMessage
	if err := decodeUseNumber(raw, &fields); err != nil {
		return contentBlock{}, err
	}
	block := contentBlock{Raw: canonicalRawOrOriginal(raw)}
	_ = json.Unmarshal(fields["type"], &block.Type)
	_ = json.Unmarshal(fields["text"], &block.Text)
	_ = json.Unmarshal(fields["thinking"], &block.Thinking)
	_ = json.Unmarshal(fields["name"], &block.Name)
	_ = json.Unmarshal(fields["is_error"], &block.IsError)
	block.ID = canonicalRawOrOriginal(fields["id"])
	block.Input = canonicalRawOrOriginal(fields["input"])
	block.ToolUseID = canonicalRawOrOriginal(fields["tool_use_id"])
	block.Source = canonicalRawOrOriginal(fields["source"])
	block.Content = canonicalRawOrOriginal(fields["content"])
	cache := bytes.TrimSpace(fields["cache_control"])
	block.HasCache = len(cache) > 0 && !bytes.Equal(cache, []byte("null"))
	return block, nil
}

func convertImage(raw json.RawMessage) (map[string]any, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := decodeUseNumber(raw, &source); err != nil {
		return nil, false, err
	}
	if source.Type != "base64" || !strings.HasPrefix(source.MediaType, "image/") || source.Data == "" {
		return nil, false, nil
	}
	format := strings.TrimPrefix(source.MediaType, "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	return map[string]any{"format": format, "source": map[string]any{"bytes": source.Data}}, true, nil
}

func toolResultContent(raw json.RawMessage, losses lossSet) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	blocks, plain, err := parseContentBlocks(trimmed)
	if err != nil {
		// Tool results may legally be arbitrary JSON rather than Anthropic blocks.
		canonical, canonicalErr := canonicalJSON(trimmed)
		if canonicalErr != nil {
			return "", err
		}
		losses.add(LossUnsupportedBlockTextualized)
		return textualizedBlock("unsupported_block", canonical), nil
	}
	text := plain
	for _, block := range blocks {
		if block.HasCache {
			losses.add(LossCacheControlNotForwarded)
		}
		if block.Type == "text" {
			text = appendText(text, block.Text)
			continue
		}
		text = appendText(text, textualizedBlock("unsupported_block", block.Raw))
		losses.add(LossUnsupportedBlockTextualized)
	}
	return text, nil
}

func systemContent(raw json.RawMessage, losses lossSet) (string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", false, err
		}
		return text, true, nil
	}
	blocks, _, err := parseContentBlocks(trimmed)
	if err != nil {
		return "", false, err
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.HasCache {
			losses.add(LossCacheControlNotForwarded)
		}
		if block.Type == "text" {
			parts = append(parts, block.Text)
			continue
		}
		parts = append(parts, textualizedBlock("unsupported_block", block.Raw))
		losses.add(LossUnsupportedBlockTextualized)
	}
	return strings.Join(parts, "\n"), true, nil
}

func textualizedBlock(kind string, raw json.RawMessage) string {
	canonical := canonicalRawOrOriginal(raw)
	return "<" + kind + ">" + string(canonical) + "</" + kind + ">"
}

func appendText(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + "\n" + next
}

func contentText(raw json.RawMessage) string {
	blocks, plain, err := parseContentBlocks(raw)
	if err != nil {
		return ""
	}
	text := plain
	for _, block := range blocks {
		if block.Type == "text" {
			text = appendText(text, block.Text)
		}
	}
	return text
}

func rawScalarString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) == nil {
			return value
		}
	}
	return string(trimmed)
}

func canonicalValue(raw json.RawMessage, fallback any) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fallback, nil
	}
	var value any
	if err := decodeUseNumber(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := decodeUseNumber(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func canonicalRawOrOriginal(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return canonical
}

func decodeUseNumber(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain one value")
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
