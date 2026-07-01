package upstream

import (
	"codex-account-pool/internal/registry"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/thinking"

	log "github.com/sirupsen/logrus"
)

// applyThinkingConfig applies thinking configuration to the request body.
//
// This function is called before forwarding to upstream, implementing the
// thinking configuration injection pipeline:
//  1. Resolve configuration (suffix > model > provider > global)
//  2. Look up model capabilities from registry
//  3. Apply thinking configuration using the provider-specific applier
//
// On error, returns the original body (graceful degradation).
func (c *Client) applyThinkingConfig(body []byte, provider, model string, account storage.Account) []byte {
	if !c.cfg.ThinkingEnabled {
		return body
	}

	// 1. Resolve configuration from all sources
	config := thinking.ResolveConfig(c.cfg, provider, model)

	// 2. Look up model information
	modelInfo := registry.LookupModelInfo(model, provider)

	// 3. Apply thinking configuration
	result, err := thinking.ApplyThinking(body, config, modelInfo, provider)
	if err != nil {
		log.WithFields(log.Fields{
			"provider": provider,
			"model":    model,
			"account":  account.ID,
			"error":    err.Error(),
		}).Warn("thinking: failed to apply configuration, using original body")
		return body
	}

	log.WithFields(log.Fields{
		"provider": provider,
		"model":    model,
		"account":  account.ID,
		"mode":     config.Mode,
	}).Debug("thinking: configuration applied successfully")

	return result
}
