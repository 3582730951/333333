// Package codex implements thinking configuration for Codex (OpenAI Responses API) models.
//
// Codex models use the reasoning.effort format with discrete levels (low/medium/high).
package codex

import (
	"codex-account-pool/internal/registry"
	"codex-account-pool/internal/thinking"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Applier implements thinking.ProviderApplier for Codex models.
type Applier struct{}

// NewApplier creates a new Codex thinking applier.
func NewApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("codex", NewApplier())
}

// Apply applies thinking configuration to Codex request body.
//
// Expected output format:
//
//	{
//	  "reasoning": {
//	    "effort": "high"
//	  }
//	}
func (a *Applier) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	if modelInfo != nil && modelInfo.Thinking == nil {
		return body, nil
	}

	// Only handle ModeLevel and ModeNone; other modes pass through unchanged.
	if config.Mode != thinking.ModeLevel && config.Mode != thinking.ModeNone {
		return body, nil
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	if config.Mode == thinking.ModeLevel {
		result, _ := sjson.SetBytes(body, "reasoning.effort", string(config.Level))
		return result, nil
	}

	// ModeNone
	effort := ""
	if modelInfo != nil && modelInfo.Thinking != nil {
		support := modelInfo.Thinking
		if config.Budget == 0 {
			if support.ZeroAllowed || thinking.HasLevel(support.Levels, string(thinking.LevelNone)) {
				effort = string(thinking.LevelNone)
			}
		}
		if effort == "" && config.Level != "" {
			effort = string(config.Level)
		}
		if effort == "" && len(support.Levels) > 0 {
			effort = support.Levels[0]
		}
	}
	if effort == "" {
		return body, nil
	}

	result, _ := sjson.SetBytes(body, "reasoning.effort", effort)
	return result, nil
}
