// Package registry provides model capability information.
package registry

import "strings"

// ModelInfo contains metadata about a model's capabilities.
type ModelInfo struct {
	// ID is the canonical model identifier
	ID string

	// Thinking contains thinking/reasoning support information
	Thinking *ThinkingSupport

	// MaxCompletionTokens is the maximum completion tokens for this model
	MaxCompletionTokens int
}

// ThinkingSupport describes a model's thinking/reasoning capabilities.
type ThinkingSupport struct {
	// Min is the minimum thinking budget (for budget-based models)
	Min int

	// Max is the maximum thinking budget (for budget-based models)
	Max int

	// Levels is the list of supported discrete thinking levels (for level-based models)
	// Examples: ["low", "medium", "high"], ["minimal", "low", "medium", "high", "xhigh", "max"]
	Levels []string

	// ZeroAllowed indicates whether budget=0 is explicitly allowed
	ZeroAllowed bool

	// DynamicAllowed indicates whether auto/dynamic thinking is supported
	DynamicAllowed bool
}

var modelRegistry = map[string]ModelInfo{
	// Current adaptive-thinking Claude models. The level sets and 128K output
	// ceiling mirror Anthropic's current per-model contract. Keeping this table
	// local makes request normalization deterministic when /v1/models only
	// exposes IDs and context limits.
	"claude-fable-5": {
		ID:                  "claude-fable-5",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-mythos-5": {
		ID:                  "claude-mythos-5",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-opus-5": {
		ID:                  "claude-opus-5",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-sonnet-5": {
		ID:                  "claude-sonnet-5",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-opus-4-8": {
		ID:                  "claude-opus-4-8",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-opus-4-7": {
		ID:                  "claude-opus-4-7",
		Thinking:            adaptiveThinking("low", "medium", "high", "xhigh", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-opus-4-6": {
		ID:                  "claude-opus-4-6",
		Thinking:            adaptiveThinking("low", "medium", "high", "max"),
		MaxCompletionTokens: 128000,
	},
	"claude-sonnet-4-6": {
		ID:                  "claude-sonnet-4-6",
		Thinking:            adaptiveThinking("low", "medium", "high", "max"),
		MaxCompletionTokens: 128000,
	},
}

func adaptiveThinking(levels ...string) *ThinkingSupport {
	return &ThinkingSupport{
		Levels:         append([]string(nil), levels...),
		DynamicAllowed: true,
	}
}

// LookupModelInfo looks up immutable model information from the built-in
// registry. It returns a defensive copy so callers cannot mutate global
// capability state.
func LookupModelInfo(model, provider string) *ModelInfo {
	if !strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return nil
	}
	id := canonicalClaudeModel(model)
	info, ok := modelRegistry[id]
	if !ok {
		return nil
	}
	copy := info
	if info.Thinking != nil {
		thinkingCopy := *info.Thinking
		thinkingCopy.Levels = append([]string(nil), info.Thinking.Levels...)
		copy.Thinking = &thinkingCopy
	}
	return &copy
}

func canonicalClaudeModel(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if index := strings.IndexByte(id, '['); index >= 0 {
		id = id[:index]
	}
	id = strings.TrimSuffix(id, "-thinking")
	id = strings.ReplaceAll(id, ".", "-")
	return id
}
