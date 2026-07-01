package thinking

import (
	"strings"

	"codex-account-pool/internal/config"
)

// ResolveConfig resolves the thinking configuration for a given provider and model.
//
// This function implements the priority hierarchy:
//  1. Model name suffix (e.g., "claude-opus-4-8(high)")
//  2. Model-level configuration (config.ThinkingModels["claude-opus-4-8"])
//  3. Provider-level configuration (config.ThinkingProviders["claude"])
//  4. Global default (config.ThinkingDefaultMode/Level/Budget)
//
// Parameters:
//   - cfg: The global configuration
//   - provider: Provider name (claude, codex)
//   - model: Model name (may include suffix)
//
// Returns:
//   - ThinkingConfig resolved from the priority hierarchy
func ResolveConfig(cfg config.Config, provider, model string) ThinkingConfig {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)

	// 1. Check model name suffix (highest priority)
	suffixResult := ParseSuffix(model)
	if suffixResult.HasSuffix {
		config := parseSuffixToConfig(suffixResult.RawSuffix, provider, model)
		if hasThinkingConfig(config) {
			return config
		}
	}

	// Use base model name for lookups
	baseModel := suffixResult.ModelName

	// 2. Check model-level configuration
	if override, ok := cfg.ThinkingModels[baseModel]; ok {
		config := overrideToConfig(override)
		if hasThinkingConfig(config) {
			return config
		}
	}

	// 3. Check provider-level configuration
	if override, ok := cfg.ThinkingProviders[provider]; ok {
		config := overrideToConfig(override)
		if hasThinkingConfig(config) {
			return config
		}
	}

	// 4. Return global default
	return globalDefaultConfig(cfg)
}

// parseSuffixToConfig converts a raw suffix string to ThinkingConfig.
func parseSuffixToConfig(rawSuffix, provider, model string) ThinkingConfig {
	// 1. Try special values first (none, auto, -1)
	if mode, ok := ParseSpecialSuffix(rawSuffix); ok {
		switch mode {
		case ModeNone:
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		case ModeAuto:
			return ThinkingConfig{Mode: ModeAuto, Budget: -1}
		}
	}

	// 2. Try level parsing (minimal, low, medium, high, xhigh, max)
	if level, ok := ParseLevelSuffix(rawSuffix); ok {
		return ThinkingConfig{Mode: ModeLevel, Level: level}
	}

	// 3. Try numeric parsing
	if budget, ok := ParseNumericSuffix(rawSuffix); ok {
		if budget == 0 {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		return ThinkingConfig{Mode: ModeBudget, Budget: budget}
	}

	// Unknown suffix format - return empty config
	return ThinkingConfig{}
}

// overrideToConfig converts a config.ThinkingOverride to ThinkingConfig.
func overrideToConfig(override config.ThinkingOverride) ThinkingConfig {
	mode := strings.ToLower(strings.TrimSpace(override.Mode))

	switch mode {
	case "none":
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	case "auto":
		return ThinkingConfig{Mode: ModeAuto, Budget: -1}
	case "level":
		level := strings.ToLower(strings.TrimSpace(override.Level))
		if level == "" {
			level = "medium" // Default level
		}
		return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(level)}
	case "budget":
		budget := override.Budget
		if budget == 0 {
			budget = 8192 // Default budget
		}
		return ThinkingConfig{Mode: ModeBudget, Budget: budget}
	default:
		return ThinkingConfig{}
	}
}

// globalDefaultConfig returns the global default thinking configuration.
func globalDefaultConfig(cfg config.Config) ThinkingConfig {
	if !cfg.ThinkingEnabled {
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.ThinkingDefaultMode))

	switch mode {
	case "none":
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	case "auto":
		return ThinkingConfig{Mode: ModeAuto, Budget: -1}
	case "level":
		level := strings.ToLower(strings.TrimSpace(cfg.ThinkingDefaultLevel))
		if level == "" {
			level = "medium" // Fallback default
		}
		return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(level)}
	case "budget":
		budget := cfg.ThinkingDefaultBudget
		if budget == 0 {
			budget = 8192 // Fallback default
		}
		return ThinkingConfig{Mode: ModeBudget, Budget: budget}
	default:
		// No default configured, return disabled
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	}
}
