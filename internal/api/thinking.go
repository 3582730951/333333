package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registry"
	"codex-account-pool/internal/thinking"

	log "github.com/sirupsen/logrus"
)

// handleThinkingConfig routes thinking config requests by method.
func (s *Server) handleThinkingConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetThinkingConfig(w, r)
	case http.MethodPost:
		s.handleSaveThinkingConfig(w, r)
	default:
		methodNotAllowed(w)
	}
}

// handleGetThinkingConfig returns the current thinking configuration.
// GET /admin/thinking
func (s *Server) handleGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	response := map[string]interface{}{
		"enabled":        s.cfg.ThinkingEnabled,
		"default_mode":   s.cfg.ThinkingDefaultMode,
		"default_level":  s.cfg.ThinkingDefaultLevel,
		"default_budget": s.cfg.ThinkingDefaultBudget,
		"providers":      s.cfg.ThinkingProviders,
		"models":         s.cfg.ThinkingModels,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleSaveThinkingConfig saves the thinking configuration.
// POST /admin/thinking
func (s *Server) handleSaveThinkingConfig(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	var req struct {
		Enabled       bool                               `json:"enabled"`
		DefaultMode   string                             `json:"default_mode"`
		DefaultLevel  string                             `json:"default_level"`
		DefaultBudget int                                `json:"default_budget"`
		Providers     map[string]config.ThinkingOverride `json:"providers"`
		Models        map[string]config.ThinkingOverride `json:"models"`
	}

	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	// Update configuration (in-memory hot reload)
	s.cfg.ThinkingEnabled = req.Enabled
	s.cfg.ThinkingDefaultMode = req.DefaultMode
	s.cfg.ThinkingDefaultLevel = req.DefaultLevel
	s.cfg.ThinkingDefaultBudget = req.DefaultBudget
	s.cfg.ThinkingProviders = req.Providers
	s.cfg.ThinkingModels = req.Models

	log.WithFields(log.Fields{
		"enabled": req.Enabled,
		"mode":    req.DefaultMode,
	}).Info("thinking: configuration updated")

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePreviewThinking previews thinking configuration application.
// POST /admin/thinking/preview
func (s *Server) handlePreviewThinking(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	var req struct {
		Provider string          `json:"provider"`
		Model    string          `json:"model"`
		Body     json.RawMessage `json:"body"`
	}

	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	if req.Provider == "" || req.Model == "" {
		writeError(w, http.StatusBadRequest, errors.New("provider and model are required"))
		return
	}

	// Resolve configuration
	thinkingConfig := thinking.ResolveConfig(s.cfg, req.Provider, req.Model)

	// Determine configuration source
	source := determineConfigSource(s.cfg, req.Provider, req.Model)

	// Look up model information
	modelInfo := registry.LookupModelInfo(req.Model, req.Provider)

	// Apply thinking configuration
	appliedBody := req.Body
	if len(req.Body) == 0 {
		appliedBody = []byte("{}")
	}

	result, err := thinking.ApplyThinking(appliedBody, thinkingConfig, modelInfo, req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to apply: %w", err))
		return
	}

	response := map[string]interface{}{
		"source":          source,
		"resolved_config": thinkingConfig,
		"applied_body":    json.RawMessage(result),
	}

	writeJSON(w, http.StatusOK, response)
}

// checkAdminAuth checks admin authorization.
func (s *Server) checkAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.Header.Get("X-Admin-Token")
	}

	if strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimPrefix(token, "Bearer ")
	}

	if s.cfg.AdminToken != "" && token != s.cfg.AdminToken {
		writeError(w, http.StatusUnauthorized, errors.New("unauthorized"))
		return false
	}

	return true
}

// determineConfigSource determines where the configuration came from.
func determineConfigSource(cfg config.Config, provider, model string) string {
	suffixResult := thinking.ParseSuffix(model)
	if suffixResult.HasSuffix {
		return "model name suffix"
	}

	if _, ok := cfg.ThinkingModels[suffixResult.ModelName]; ok {
		return "model override"
	}

	if _, ok := cfg.ThinkingProviders[provider]; ok {
		return "provider override"
	}

	return "global default"
}
