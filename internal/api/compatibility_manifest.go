package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/compatmanifest"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

func compatibilityManifestBootConfig(cfg config.Config) compatmanifest.Config {
	return compatmanifest.Config{
		Enabled: cfg.CompatibilityManifestEnabled, Source: cfg.CompatibilityManifestSource,
		URL: cfg.CompatibilityManifestURL, PublicKey: cfg.CompatibilityManifestPublicKey,
		RefreshEvery: time.Duration(cfg.CompatibilityManifestRefreshHours) * time.Hour,
		MaxStale:     time.Duration(cfg.CompatibilityManifestMaxStaleDays) * 24 * time.Hour,
	}
}

func (s *Server) compatibilityManifestConfig(ctx context.Context) compatmanifest.Config {
	cfg := compatibilityManifestBootConfig(s.cfg)
	cfg.Enabled = s.flagEnabled(ctx, "compatibility_manifest_enabled", cfg.Enabled)
	cfg.Source = s.settingString(ctx, "compatibility_manifest_source", cfg.Source)
	cfg.URL = s.settingString(ctx, "compatibility_manifest_url", cfg.URL)
	cfg.PublicKey = s.settingString(ctx, "compatibility_manifest_public_key", cfg.PublicKey)
	refreshHours := s.settingInt(ctx, "compatibility_manifest_refresh_hours", s.cfg.CompatibilityManifestRefreshHours)
	if refreshHours < 1 || refreshHours > 168 {
		refreshHours = config.DefaultCompatibilityManifestRefreshHours
	}
	staleDays := s.settingInt(ctx, "compatibility_manifest_max_stale_days", s.cfg.CompatibilityManifestMaxStaleDays)
	if staleDays < 1 || staleDays > 365 {
		staleDays = config.DefaultCompatibilityManifestMaxStaleDays
	}
	cfg.RefreshEvery = time.Duration(refreshHours) * time.Hour
	cfg.MaxStale = time.Duration(staleDays) * 24 * time.Hour
	return cfg
}

func (s *Server) activeCompatibilityManifest(ctx context.Context) (compatmanifest.Payload, bool) {
	if s == nil || s.compatibilityManifest == nil || !s.compatibilityManifestConfig(ctx).Enabled {
		return compatmanifest.Payload{}, false
	}
	return s.compatibilityManifest.Active()
}

func (s *Server) applyCompatibilityManifest(payload compatmanifest.Payload) {
	models := make([]capability.RemoteCodexModel, 0, len(payload.Models))
	for _, model := range payload.Models {
		models = append(models, capability.RemoteCodexModel{
			Slug: model.Slug, ContextWindow: model.ContextWindow, MaxContextWindow: model.MaxContextWindow,
			AutoCompactTokenLimit: model.AutoCompactTokenLimit, MinimumClientVersion: model.MinimumClientVersion,
			RequiresCurrentClient: model.RequiresCurrentClient, PreferWebSocket: model.PreferWebSocket,
			ResponsesLite: model.ResponsesLite, ReasoningLevels: append([]string(nil), model.ReasoningLevels...),
		})
	}
	capability.SetRemoteCodexModels(models)
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
}

func applyCompatibilityManifestConfig(cfg config.Config, payload compatmanifest.Payload) config.Config {
	// A config/DB override always wins. ClientVersion is an older boot-only alias;
	// a non-default value is also treated as an explicit operator pin.
	if version := strings.TrimSpace(payload.Codex.Version); version != "" &&
		strings.TrimSpace(cfg.CodexCLIVersionOverride) == "" &&
		(strings.TrimSpace(cfg.ClientVersion) == "" || cfg.ClientVersion == config.DefaultClientVersion) &&
		compatmanifest.CompareDottedVersions(version, firstNonEmpty(cfg.ClientVersion, config.DefaultClientVersion)) >= 0 {
		cfg.ClientVersion = version
		cfg.CodexCLIVersionOverride = version
	}
	// Claude's CLI, embedded Node, and Stainless SDK form one fingerprint tuple.
	// Apply it only when the signed source supplied every axis and the operator did
	// not pin any axis. The official npm endpoint does not prove the full tuple, so
	// its intentionally partial payload cannot create a synthetic combination.
	manualClaude := strings.TrimSpace(cfg.ClaudeCLIVersionOverride) != "" ||
		strings.TrimSpace(cfg.ClaudeNodeVersion) != "" || strings.TrimSpace(cfg.ClaudeStainlessVersion) != ""
	completeClaude := strings.TrimSpace(payload.Claude.CLIVersion) != "" &&
		strings.TrimSpace(payload.Claude.NodeVersion) != "" && strings.TrimSpace(payload.Claude.StainlessVersion) != ""
	if !manualClaude && completeClaude {
		cfg.ClaudeCLIVersionOverride = strings.TrimSpace(payload.Claude.CLIVersion)
		cfg.ClaudeNodeVersion = strings.TrimSpace(payload.Claude.NodeVersion)
		cfg.ClaudeStainlessVersion = strings.TrimSpace(payload.Claude.StainlessVersion)
	}
	return cfg
}

func (s *Server) effectiveCodexClientVersion(ctx context.Context) string {
	cfg := s.effectiveUpstreamConfig(ctx)
	if version := strings.TrimSpace(cfg.CodexCLIVersionOverride); version != "" {
		return version
	}
	if version := strings.TrimSpace(cfg.ClientVersion); version != "" {
		return version
	}
	return config.DefaultClientVersion
}

func (s *Server) startCompatibilityManifest(ctx context.Context) {
	if s == nil || s.compatibilityManifest == nil {
		return
	}
	supervisor.Go(ctx, "compatibility-manifest", s.runCompatibilityManifest)
}

func (s *Server) runCompatibilityManifest(ctx context.Context) {
	backoff := time.Minute
	loadedSource := ""
	disabledApplied := false
	for {
		if ctx.Err() != nil {
			return
		}
		cfg := s.compatibilityManifestConfig(ctx)
		s.compatibilityManifest.SetEnabled(cfg.Enabled, cfg.Source)
		if !cfg.Enabled {
			loadedSource = ""
			if !disabledApplied {
				disabledApplied = true
				s.compatibilityManifest.ClearActive("disabled")
				s.applyCompatibilityManifest(compatmanifest.Payload{})
				s.publishEffectiveUpstreamConfig(context.WithoutCancel(ctx))
			}
			if !s.waitCompatibilityManifest(ctx, time.Minute) {
				return
			}
			continue
		}
		disabledApplied = false
		_, active := s.compatibilityManifest.Active()
		if cfg.Source != loadedSource || !active {
			loadedSource = cfg.Source
			if payload, loaded, err := s.compatibilityManifest.Load(cfg); err != nil {
				log.Printf("compatibility manifest LKG load: %v", err)
			} else if loaded {
				s.applyCompatibilityManifest(payload)
				s.publishEffectiveUpstreamConfig(context.WithoutCancel(ctx))
			}
		}
		if s.diskGuardPausesBackground() {
			if !s.waitCompatibilityManifest(ctx, 5*time.Minute) {
				return
			}
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		payload, changed, err := s.compatibilityManifest.Refresh(refreshCtx, cfg, s.canaryCompatibilityManifest)
		cancel()
		delay := jitterCompatibilityInterval(cfg.RefreshEvery, s.cfg.NodeID)
		if err != nil {
			log.Printf("compatibility manifest refresh: %v", err)
			delay = backoff
			backoff *= 2
			if backoff > time.Hour {
				backoff = time.Hour
			}
		} else {
			backoff = time.Minute
			if changed {
				s.applyCompatibilityManifest(payload)
				s.publishEffectiveUpstreamConfig(context.WithoutCancel(ctx))
			}
		}
		if !s.waitCompatibilityManifest(ctx, delay) {
			return
		}
	}
}

func (s *Server) waitCompatibilityManifest(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = time.Minute
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.compatibilityWake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Server) wakeCompatibilityManifest() {
	if s == nil || s.compatibilityWake == nil {
		return
	}
	select {
	case s.compatibilityWake <- struct{}{}:
	default:
	}
}

func (s *Server) compatibilityManifestSettingsChanged(body map[string]interface{}) (changed, trustChanged bool) {
	for key := range body {
		switch key {
		case "compatibility_manifest_enabled", "compatibility_manifest_refresh_hours", "compatibility_manifest_max_stale_days":
			changed = true
		case "compatibility_manifest_source", "compatibility_manifest_url", "compatibility_manifest_public_key":
			changed, trustChanged = true, true
		}
	}
	return changed, trustChanged
}

func (s *Server) handleCompatibilityManifestSettings(ctx context.Context, body map[string]interface{}) {
	changed, trustChanged := s.compatibilityManifestSettingsChanged(body)
	if !changed || s.compatibilityManifest == nil {
		return
	}
	cfg := s.compatibilityManifestConfig(ctx)
	if trustChanged || !cfg.Enabled {
		state := "waiting"
		if !cfg.Enabled {
			state = "disabled"
		}
		s.compatibilityManifest.ClearActive(state)
		s.applyCompatibilityManifest(compatmanifest.Payload{})
		s.publishEffectiveUpstreamConfig(context.WithoutCancel(ctx))
	}
	s.wakeCompatibilityManifest()
}

func jitterCompatibilityInterval(base time.Duration, nodeID string) time.Duration {
	if base <= 0 {
		base = time.Duration(config.DefaultCompatibilityManifestRefreshHours) * time.Hour
	}
	// Stable ±10% jitter prevents a fleet from polling both official registries at
	// once while keeping tests and restarts deterministic for a node.
	var hash uint32 = 2166136261
	for _, value := range []byte(nodeID + "\x00compatibility-manifest") {
		hash ^= uint32(value)
		hash *= 16777619
	}
	permille := int64(900 + hash%201)
	return time.Duration(int64(base) * permille / 1000)
}

// canaryCompatibilityManifest runs only the free ChatGPT /models operation, on
// at most three active OAuth accounts. No inference request or quota-bearing Kiro
// operation is ever issued. With no eligible account the validated signed/release
// candidate is allowed and the status explicitly records that canary was skipped.
func (s *Server) canaryCompatibilityManifest(ctx context.Context, candidate compatmanifest.Payload) error {
	if s.compatibilityManifest == nil {
		return nil
	}
	current, _ := s.compatibilityManifest.Active()
	candidateVersion := strings.TrimSpace(candidate.Codex.Version)
	if candidateVersion != "" && compatmanifest.CompareDottedVersions(candidateVersion, config.DefaultClientVersion) < 0 {
		s.compatibilityManifest.SetCanary("rejected_version_downgrade")
		return fmt.Errorf("Codex compatibility version %s is older than bundled %s", candidateVersion, config.DefaultClientVersion)
	}
	if currentVersion := strings.TrimSpace(current.Codex.Version); candidateVersion != "" && currentVersion != "" &&
		compatmanifest.CompareDottedVersions(candidateVersion, currentVersion) < 0 {
		s.compatibilityManifest.SetCanary("rejected_version_downgrade")
		return fmt.Errorf("Codex compatibility version %s is older than active %s", candidateVersion, currentVersion)
	}
	if candidateVersion == "" || candidateVersion == strings.TrimSpace(current.Codex.Version) {
		s.compatibilityManifest.SetCanary("not_required")
		return nil
	}
	if s.store == nil || s.upstream == nil {
		s.compatibilityManifest.SetCanary("skipped_no_runtime")
		return nil
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	eligible := 0
	var failures []string
	for _, account := range accounts {
		if account.Status != "active" || eligible >= 3 {
			continue
		}
		token, tokenErr := s.store.GetToken(ctx, account.ID)
		if tokenErr != nil || s.accountProvider(account, token) != "codex" ||
			accountprovider.UsesAPIKey("codex", token) || accountprovider.IsAgentIdentity(token) {
			continue
		}
		binding, bindingErr := s.store.GetEgressBinding(ctx, account.ID)
		if bindingErr != nil {
			continue
		}
		binding, bindingErr = s.store.EffectiveEgressBinding(ctx, binding)
		if bindingErr != nil {
			continue
		}
		egress, egressErr := s.store.ResolvePrimaryEgressBinding(ctx, binding)
		if egressErr != nil {
			continue
		}
		egress, egressErr = s.store.ApplySidecarEgressBinding(ctx, binding, egress)
		if egressErr != nil {
			continue
		}
		eligible++
		canaryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		resp, probeErr := s.upstream.Do(canaryCtx, upstream.Request{
			Method: http.MethodGet, DownstreamPath: capability.ProbePath(candidateVersion), Headers: http.Header{},
			Account: account, Token: token, Egress: egress, CookieJarKey: binding.CookieJarKey,
			CodexClientVersion: candidateVersion,
		})
		if probeErr == nil && resp != nil {
			raw, readErr := upstream.DrainAndClose(resp.Body)
			if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				caps, parseErr := capability.Parse(account.ID, raw, capability.ETagFromHeader(resp.Header))
				if parseErr == nil && len(caps) > 0 {
					cancel()
					s.compatibilityManifest.SetCanary("passed_models_probe")
					return nil
				}
				probeErr = firstError(parseErr, errors.New("models canary returned an empty catalog"))
			} else if readErr != nil {
				probeErr = readErr
			} else {
				probeErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		cancel()
		failures = append(failures, fmt.Sprintf("attempt_%d:%v", eligible, probeErr))
	}
	if eligible == 0 {
		s.compatibilityManifest.SetCanary("skipped_no_eligible_account")
		return nil
	}
	s.compatibilityManifest.SetCanary("failed")
	return fmt.Errorf("non-billable /models canary failed on %d account(s): %s", eligible, strings.Join(failures, "; "))
}

func (s *Server) compatibilityManifestStatus() interface{} {
	if s == nil || s.compatibilityManifest == nil {
		return map[string]interface{}{"enabled": false, "state": "unavailable"}
	}
	return s.compatibilityManifest.Status()
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}
