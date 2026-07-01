// Package thinking provides unified thinking configuration processing.
package thinking

import (
	"strings"

	"codex-account-pool/internal/registry"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// providerAppliers maps provider names to their ProviderApplier implementations.
var providerAppliers = map[string]ProviderApplier{}

// RegisterProvider registers a provider applier by name.
func RegisterProvider(name string, applier ProviderApplier) {
	providerAppliers[name] = applier
}

// GetProviderApplier returns the ProviderApplier for the given provider name.
// Returns nil if the provider is not registered.
func GetProviderApplier(provider string) ProviderApplier {
	return providerAppliers[provider]
}

// ApplyThinking applies thinking configuration to a request body.
//
// This is the unified entry point for all providers. It follows the processing
// order: route check → model capability query → config extraction → validation → application.
//
// Suffix Priority: When the model name includes a thinking suffix (e.g., "gemini-2.5-pro(8192)"),
// the suffix configuration takes priority over any thinking parameters in the request body.
//
// Parameters:
//   - body: Original request body JSON
//   - config: Thinking configuration to apply
//   - modelInfo: Model information from registry
//   - provider: Provider identifier (claude, codex)
//
// Returns:
//   - Modified request body JSON with thinking configuration applied
//   - Error if validation fails. On error, the original body is returned.
//
// Passthrough behavior (returns original body without error):
//   - Unknown provider (not in providerAppliers map)
//   - modelInfo.Thinking is nil (model doesn't support thinking)
func ApplyThinking(body []byte, config ThinkingConfig, modelInfo *registry.ModelInfo, provider string) ([]byte, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))

	// 1. Route check: Get provider applier
	applier := GetProviderApplier(provider)
	if applier == nil {
		log.WithFields(log.Fields{
			"provider": provider,
		}).Debug("thinking: unknown provider, passthrough |")
		return body, nil
	}

	// 2. Model capability check
	if modelInfo != nil && modelInfo.Thinking == nil {
		if hasThinkingConfig(config) {
			log.WithFields(log.Fields{
				"model":    modelInfo.ID,
				"provider": provider,
			}).Debug("thinking: model does not support thinking, stripping config |")
			return StripThinkingConfig(body, provider), nil
		}
		log.WithFields(log.Fields{
			"provider": provider,
			"model":    modelInfo.ID,
		}).Debug("thinking: model does not support thinking, passthrough |")
		return body, nil
	}

	if !hasThinkingConfig(config) {
		log.WithFields(log.Fields{
			"provider": provider,
		}).Debug("thinking: no config found, passthrough |")
		return body, nil
	}

	// 3. Validate and normalize configuration
	validated, err := ValidateConfig(config, modelInfo, provider, provider, false)
	if err != nil {
		log.WithFields(log.Fields{
			"provider": provider,
			"error":    err.Error(),
		}).Warn("thinking: validation failed |")
		return body, err
	}

	// Defensive check
	if validated == nil {
		log.WithFields(log.Fields{
			"provider": provider,
		}).Warn("thinking: ValidateConfig returned nil config without error, passthrough |")
		return body, nil
	}

	log.WithFields(log.Fields{
		"provider": provider,
		"mode":     validated.Mode,
		"budget":   validated.Budget,
		"level":    validated.Level,
	}).Debug("thinking: processed config to apply |")

	// 4. Apply configuration using provider-specific applier
	return applier.Apply(body, *validated, modelInfo)
}

// StripThinkingConfig removes thinking configuration fields from request body.
//
// This function is used when a model doesn't support thinking but the request
// contains thinking configuration. The configuration is silently removed to
// prevent upstream API errors.
//
// Parameters:
//   - body: Original request body JSON
//   - provider: Provider name (determines which fields to strip)
//
// Returns:
//   - Modified request body JSON with thinking configuration removed
func StripThinkingConfig(body []byte, provider string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	var paths []string
	switch provider {
	case "claude":
		paths = []string{"thinking", "output_config.effort"}
	case "codex":
		paths = []string{"reasoning.effort"}
	default:
		return body
	}

	result := body
	for _, path := range paths {
		result, _ = sjson.DeleteBytes(result, path)
	}

	// Avoid leaving an empty output_config object for Claude
	if provider == "claude" {
		if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			result, _ = sjson.DeleteBytes(result, "output_config")
		}
	}
	return result
}

func hasThinkingConfig(config ThinkingConfig) bool {
	return config.Mode != ModeBudget || config.Budget != 0 || config.Level != ""
}

// extractClaudeConfig extracts thinking configuration from Claude format request body.
//
// Claude API format:
//   - thinking.type: "enabled", "disabled", "adaptive"
//   - thinking.budget_tokens: integer (-1=auto, 0=disabled, >0=budget)
//   - output_config.effort: "low", "medium", "high", "max" (adaptive mode)
func extractClaudeConfig(body []byte) ThinkingConfig {
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if thinkingType == "disabled" {
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	}
	if thinkingType == "adaptive" || thinkingType == "auto" {
		if effort := gjson.GetBytes(body, "output_config.effort"); effort.Exists() && effort.Type == gjson.String {
			value := strings.ToLower(strings.TrimSpace(effort.String()))
			if value == "" {
				return ThinkingConfig{}
			}
			switch value {
			case "none":
				return ThinkingConfig{Mode: ModeNone, Budget: 0}
			case "auto":
				return ThinkingConfig{Mode: ModeAuto, Budget: -1}
			default:
				return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(value)}
			}
		}
		return ThinkingConfig{}
	}

	// Check budget_tokens
	if budget := gjson.GetBytes(body, "thinking.budget_tokens"); budget.Exists() {
		value := int(budget.Int())
		switch value {
		case 0:
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		case -1:
			return ThinkingConfig{Mode: ModeAuto, Budget: -1}
		default:
			return ThinkingConfig{Mode: ModeBudget, Budget: value}
		}
	}

	// If type="enabled" but no budget_tokens, treat as auto
	if thinkingType == "enabled" {
		return ThinkingConfig{Mode: ModeAuto, Budget: -1}
	}

	return ThinkingConfig{}
}

// extractCodexConfig extracts thinking configuration from Codex format request body.
//
// Codex API format (OpenAI Responses API):
//   - reasoning.effort: "none", "low", "medium", "high"
func extractCodexConfig(body []byte) ThinkingConfig {
	if effort := gjson.GetBytes(body, "reasoning.effort"); effort.Exists() {
		value := effort.String()
		if value == "none" {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(value)}
	}

	return ThinkingConfig{}
}
