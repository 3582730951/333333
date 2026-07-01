// Package registry provides model capability information.
package registry

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

// LookupModelInfo looks up model information from the registry.
// Returns nil if the model is not found.
//
// TODO: This is a placeholder. The real implementation should:
//   - Load model data from a JSON/YAML file or database
//   - Support provider-specific lookups
//   - Cache results for performance
func LookupModelInfo(model, provider string) *ModelInfo {
	// Placeholder: return nil for now
	// Real implementation will be added in Phase 2
	return nil
}
