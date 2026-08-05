package api

import (
	"context"
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

const thinkingConfigSettingKey = "thinking_config_v1"

type thinkingSettings struct {
	Enabled       bool                               `json:"enabled"`
	DefaultMode   string                             `json:"default_mode"`
	DefaultLevel  string                             `json:"default_level"`
	DefaultBudget int                                `json:"default_budget"`
	Providers     map[string]config.ThinkingOverride `json:"providers"`
	Models        map[string]config.ThinkingOverride `json:"models"`
}

func validateThinkingSettings(settings thinkingSettings) error {
	if !validThinkingMode(settings.DefaultMode) {
		return fmt.Errorf("default_mode %q is invalid; valid modes: level, budget, auto, none", settings.DefaultMode)
	}
	if !validThinkingLevel(settings.DefaultLevel) {
		return fmt.Errorf("default_level %q is invalid; valid levels: minimal, low, medium, high, xhigh, max, ultra", settings.DefaultLevel)
	}
	if settings.DefaultBudget < 0 {
		return errors.New("default_budget must be non-negative")
	}
	if err := validateThinkingOverrides("providers", settings.Providers); err != nil {
		return err
	}
	return validateThinkingOverrides("models", settings.Models)
}

func validateThinkingOverrides(scope string, overrides map[string]config.ThinkingOverride) error {
	for name, override := range overrides {
		field := fmt.Sprintf("%s[%q]", scope, name)
		if !validThinkingMode(override.Mode) {
			return fmt.Errorf("%s.mode %q is invalid; valid modes: level, budget, auto, none", field, override.Mode)
		}
		if !validThinkingLevel(override.Level) {
			return fmt.Errorf("%s.level %q is invalid; valid levels: minimal, low, medium, high, xhigh, max, ultra", field, override.Level)
		}
		if override.Budget < 0 {
			return fmt.Errorf("%s.budget must be non-negative", field)
		}
	}
	return nil
}

func validThinkingMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "level", "budget", "auto", "none":
		return true
	default:
		return false
	}
}

func validThinkingLevel(level string) bool {
	level = strings.TrimSpace(level)
	if level == "" {
		// The resolver intentionally treats an omitted level as the medium default.
		return true
	}
	_, ok := thinking.ParseLevelSuffix(level)
	return ok
}

func thinkingSettingsFromConfig(cfg config.Config) thinkingSettings {
	providers := cfg.ThinkingProviders
	if providers == nil {
		providers = map[string]config.ThinkingOverride{}
	}
	models := cfg.ThinkingModels
	if models == nil {
		models = map[string]config.ThinkingOverride{}
	}
	return thinkingSettings{
		Enabled:       cfg.ThinkingEnabled,
		DefaultMode:   cfg.ThinkingDefaultMode,
		DefaultLevel:  cfg.ThinkingDefaultLevel,
		DefaultBudget: cfg.ThinkingDefaultBudget,
		Providers:     providers,
		Models:        models,
	}
}

func (settings thinkingSettings) apply(cfg config.Config) config.Config {
	if settings.Providers == nil {
		settings.Providers = map[string]config.ThinkingOverride{}
	}
	if settings.Models == nil {
		settings.Models = map[string]config.ThinkingOverride{}
	}
	cfg.ThinkingEnabled = settings.Enabled
	cfg.ThinkingDefaultMode = settings.DefaultMode
	cfg.ThinkingDefaultLevel = settings.DefaultLevel
	cfg.ThinkingDefaultBudget = settings.DefaultBudget
	cfg.ThinkingProviders = settings.Providers
	cfg.ThinkingModels = settings.Models
	return cfg
}

func (s *Server) resolvedThinkingConfig(ctx context.Context, base config.Config) (config.Config, error) {
	raw, ok, err := s.store.GetSetting(ctx, thinkingConfigSettingKey)
	if err != nil {
		return base, err
	}
	if !ok {
		return base, nil
	}
	var settings thinkingSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return base, fmt.Errorf("decode persisted thinking configuration: %w", err)
	}
	return settings.apply(base), nil
}

func (s *Server) effectiveThinkingConfig(ctx context.Context, base config.Config) config.Config {
	cfg, err := s.resolvedThinkingConfig(ctx, base)
	if err != nil {
		log.WithError(err).Error("thinking: persisted configuration unavailable; using boot configuration")
		return base
	}
	return cfg
}

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
	cfg, err := s.resolvedThinkingConfig(r.Context(), s.cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, thinkingSettingsFromConfig(cfg))
}

// handleSaveThinkingConfig saves the thinking configuration.
// POST /admin/thinking
func (s *Server) handleSaveThinkingConfig(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	var req thinkingSettings

	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	if err := validateThinkingSettings(req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if req.Providers == nil {
		req.Providers = map[string]config.ThinkingOverride{}
	}
	if req.Models == nil {
		req.Models = map[string]config.ThinkingOverride{}
	}
	raw, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.persistAndPublishEffectiveUpstreamConfig(r.Context(), func(ctx context.Context) error {
		return s.store.SetSetting(ctx, thinkingConfigSettingKey, string(raw))
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

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

	cfg, err := s.resolvedThinkingConfig(r.Context(), s.cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Resolve configuration
	thinkingConfig := thinking.ResolveConfig(cfg, req.Provider, req.Model)

	// Determine configuration source
	source := determineConfigSource(cfg, req.Provider, req.Model)

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
	return s.adminAllowed(w, r)
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
