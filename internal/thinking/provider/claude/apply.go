// Package claude implements thinking configuration for Claude models.
//
// Claude models support two thinking control styles:
//   - Manual thinking: thinking.type="enabled" with thinking.budget_tokens (token budget)
//   - Adaptive thinking (Claude 4.6+): thinking.type="adaptive" with output_config.effort (low/medium/high/max)
package claude

import (
	"codex-account-pool/internal/registry"
	"codex-account-pool/internal/thinking"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Applier implements thinking.ProviderApplier for Claude models.
type Applier struct{}

// NewApplier creates a new Claude thinking applier.
func NewApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("claude", NewApplier())
}

// Apply applies thinking configuration to Claude request body.
//
// Expected output format when enabled:
//
//	{
//	  "thinking": {
//	    "type": "enabled",
//	    "budget_tokens": 16384
//	  }
//	}
//
// Expected output format for adaptive:
//
//	{
//	  "thinking": {
//	    "type": "adaptive"
//	  },
//	  "output_config": {
//	    "effort": "high"
//	  }
//	}
//
// Expected output format when disabled:
//
//	{
//	  "thinking": {
//	    "type": "disabled"
//	  }
//	}
func (a *Applier) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	if modelInfo != nil && modelInfo.Thinking == nil {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	supportsAdaptive := modelInfo != nil && modelInfo.Thinking != nil && len(modelInfo.Thinking.Levels) > 0

	switch config.Mode {
	case thinking.ModeNone:
		result, _ := sjson.SetBytes(body, "thinking.type", "disabled")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			result, _ = sjson.DeleteBytes(result, "output_config")
		}
		return result, nil

	case thinking.ModeLevel:
		// Adaptive thinking effort is only valid when the model advertises discrete levels
		if supportsAdaptive && config.Level != "" {
			result, _ := sjson.SetBytes(body, "thinking.type", "adaptive")
			result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
			result, _ = sjson.SetBytes(result, "output_config.effort", string(config.Level))
			return result, nil
		}

		// Fallback for non-adaptive Claude models: convert level to budget_tokens
		if budget, ok := thinking.ConvertLevelToBudget(string(config.Level)); ok {
			config.Mode = thinking.ModeBudget
			config.Budget = budget
			config.Level = ""
		} else {
			return body, nil
		}
		fallthrough

	case thinking.ModeBudget:
		if config.Budget == 0 {
			result, _ := sjson.SetBytes(body, "thinking.type", "disabled")
			result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
			result, _ = sjson.DeleteBytes(result, "output_config.effort")
			if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
				result, _ = sjson.DeleteBytes(result, "output_config")
			}
			return result, nil
		}

		result, _ := sjson.SetBytes(body, "thinking.type", "enabled")
		result, _ = sjson.SetBytes(result, "thinking.budget_tokens", config.Budget)
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			result, _ = sjson.DeleteBytes(result, "output_config")
		}

		// Ensure max_tokens > thinking.budget_tokens (Anthropic API constraint)
		result = a.normalizeClaudeBudget(result, config.Budget, modelInfo)
		return result, nil

	case thinking.ModeAuto:
		// For Claude 4.6+ models, auto maps to adaptive thinking
		if supportsAdaptive {
			result, _ := sjson.SetBytes(body, "thinking.type", "adaptive")
			result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
			result, _ = sjson.DeleteBytes(result, "output_config.effort")
			if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
				result, _ = sjson.DeleteBytes(result, "output_config")
			}
			return result, nil
		}

		// Legacy fallback: enable thinking without specifying budget_tokens
		result, _ := sjson.SetBytes(body, "thinking.type", "enabled")
		result, _ = sjson.DeleteBytes(result, "thinking.budget_tokens")
		result, _ = sjson.DeleteBytes(result, "output_config.effort")
		if oc := gjson.GetBytes(result, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			result, _ = sjson.DeleteBytes(result, "output_config")
		}
		return result, nil

	default:
		return body, nil
	}
}

// normalizeClaudeBudget applies Claude-specific constraints to ensure max_tokens > budget_tokens.
func (a *Applier) normalizeClaudeBudget(body []byte, budgetTokens int, modelInfo *registry.ModelInfo) []byte {
	if budgetTokens <= 0 {
		return body
	}

	effectiveMax, setDefaultMax := a.effectiveMaxTokens(body, modelInfo)
	if setDefaultMax && effectiveMax > 0 {
		body, _ = sjson.SetBytes(body, "max_tokens", effectiveMax)
	}

	adjustedBudget := budgetTokens
	if effectiveMax > 0 && adjustedBudget >= effectiveMax {
		adjustedBudget = effectiveMax - 1
	}

	minBudget := 0
	if modelInfo != nil && modelInfo.Thinking != nil {
		minBudget = modelInfo.Thinking.Min
	}
	if minBudget > 0 && adjustedBudget > 0 && adjustedBudget < minBudget {
		return body
	}

	if adjustedBudget != budgetTokens {
		body, _ = sjson.SetBytes(body, "thinking.budget_tokens", adjustedBudget)
	}

	return body
}

// effectiveMaxTokens returns the max tokens to cap thinking.
func (a *Applier) effectiveMaxTokens(body []byte, modelInfo *registry.ModelInfo) (max int, fromModel bool) {
	if maxTok := gjson.GetBytes(body, "max_tokens"); maxTok.Exists() && maxTok.Int() > 0 {
		return int(maxTok.Int()), false
	}
	if modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
		return modelInfo.MaxCompletionTokens, true
	}
	return 0, false
}
