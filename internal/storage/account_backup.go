package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const accountBackupQueryBatchSize = 500

func accountBackupIDBatches(accountIDs []string) [][]string {
	ids := make([]string, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	batches := make([][]string, 0, (len(ids)+accountBackupQueryBatchSize-1)/accountBackupQueryBatchSize)
	for start := 0; start < len(ids); start += accountBackupQueryBatchSize {
		end := start + accountBackupQueryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
}

func forEachAccountBackupIDBatch(accountIDs []string, fn func([]string) error) error {
	for _, batch := range accountBackupIDBatches(accountIDs) {
		if err := fn(batch); err != nil {
			return err
		}
	}
	return nil
}

// AccountGroupMembership is the durable many-group membership attached to an
// account. Group definitions are intentionally not part of an account backup;
// group_name values remain usable even when the destination has not configured a
// matching policy yet.
type AccountGroupMembership struct {
	AccountID string `json:"account_id"`
	GroupName string `json:"group_name"`
	IsPrimary bool   `json:"is_primary"`
	CreatedAt int64  `json:"created_at"`
}

// AccountModelCatalogStatus preserves whether the last model catalog was an
// authoritative upstream result rather than a static compatibility floor.
type AccountModelCatalogStatus struct {
	AccountID     string `json:"account_id"`
	Authoritative bool   `json:"authoritative"`
	LastProbeAt   int64  `json:"last_probe_at"`
}

// AccountBackup contains the complete durable account configuration,
// credentials, and directly referenced provider/egress definitions required to
// move an account between pool installations. Runtime telemetry, request
// affinity, audit history, and queued jobs are intentionally excluded: they
// describe the source process rather than the imported account.
type AccountBackup struct {
	Account                Account
	Token                  AccountToken
	CustomProvider         *CustomProvider
	EgressProfiles         []EgressProfile
	KiroCredentials        *KiroCredentials
	AntigravityCredentials *AntigravityCredentials
	SessionCookie          *string
	InjectedCookies        []InjectedCookie
	EgressBinding          *AccountEgressBinding
	Capabilities           []ModelCapability
	ModelCatalogStatus     *AccountModelCatalogStatus
	CodexReauthConfig      *AccountCodexReauthConfig
	GroupMemberships       []AccountGroupMembership
}

func accountBackupCustomProviderID(provider string) string {
	provider = strings.TrimSpace(provider)
	switch strings.ToLower(provider) {
	case "", "codex", "claude", "kiro", "antigravity":
		return ""
	default:
		return provider
	}
}

func normalizeAccountBackupCustomProvider(provider CustomProvider) (CustomProvider, error) {
	provider.ID = strings.TrimSpace(provider.ID)
	if provider.ID == "" {
		return CustomProvider{}, errors.New("custom provider definition has no id")
	}
	provider.Name = strings.TrimSpace(provider.Name)
	if provider.Name == "" {
		provider.Name = provider.ID
	}
	provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	if provider.BaseURL == "" {
		return CustomProvider{}, fmt.Errorf("custom provider %q has no base_url", provider.ID)
	}
	if strings.TrimSpace(provider.UpstreamProtocol) == "" {
		provider.UpstreamProtocol = CustomProviderProtocolChatCompletions
	} else if normalized, ok := NormalizeCustomProviderProtocol(provider.UpstreamProtocol); ok {
		provider.UpstreamProtocol = normalized
	} else {
		return CustomProvider{}, fmt.Errorf("custom provider %q has invalid upstream_protocol %q", provider.ID, provider.UpstreamProtocol)
	}
	if strings.TrimSpace(provider.TransportProfile) == "" {
		provider.TransportProfile = CustomProviderTransportGeneric
	} else if normalized, ok := NormalizeCustomProviderTransportProfile(provider.TransportProfile); ok {
		provider.TransportProfile = normalized
	} else {
		return CustomProvider{}, fmt.Errorf("custom provider %q has invalid transport_profile %q", provider.ID, provider.TransportProfile)
	}
	provider.EgressIDs = normalizeOrderedIDs(provider.EgressIDs)
	provider.Models = decodeProviderModelsFromSlice(provider.Models)
	sort.Strings(provider.Models)
	mappings, err := canonicalProviderModelMappings(provider.ModelMappings, true)
	if err != nil {
		return CustomProvider{}, fmt.Errorf("custom provider %q: %w", provider.ID, err)
	}
	provider.ModelMappings = mappings
	routes, err := canonicalCustomProviderRoutes(provider.Routes, provider, true)
	if err != nil {
		return CustomProvider{}, fmt.Errorf("custom provider %q: %w", provider.ID, err)
	}
	provider.Routes = routes
	return provider, nil
}

func normalizeAccountBackupEgressProfile(profile EgressProfile) (EgressProfile, error) {
	profile.ID = strings.TrimSpace(profile.ID)
	if profile.ID == "" {
		return EgressProfile{}, errors.New("egress profile definition has no id")
	}
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		profile.Name = profile.ID
	}
	profile.Type = strings.ToLower(strings.TrimSpace(profile.Type))
	if profile.Type == "" {
		profile.Type = "direct"
	}
	profile.Endpoint = strings.TrimSpace(profile.Endpoint)
	profile.ChainProxy = strings.TrimSpace(profile.ChainProxy)
	profile.Region = strings.TrimSpace(profile.Region)
	// Runtime health/CF observations are deliberately not portable definitions.
	// Resetting them also makes a freshly restored outlet immediately eligible
	// instead of carrying a stale source-process cooldown into the destination.
	profile.ExitIP = ""
	profile.Health = "healthy"
	profile.LatencyMillis = 0
	profile.CFScore = 0
	profile.LastCFRay = ""
	profile.CooldownUntil = 0
	profile.ProxyAuthMode = strings.TrimSpace(profile.ProxyAuthMode)
	profile.ProxyAPIKey = strings.TrimSpace(profile.ProxyAPIKey)
	profile.IPMode = strings.TrimSpace(profile.IPMode)
	profile.ProviderKey = strings.TrimSpace(profile.ProviderKey)
	profile.TransportSidecarID = ""
	profile.TransportSidecarMaxConcurrency = 0
	profile.TransportBaseType = ""
	profile.TransportBaseURL = ""
	profile.TransportBaseChain = ""
	dynamic := strings.TrimSpace(profile.DynamicConfigJSON)
	if dynamic == "" || dynamic == "null" {
		profile.DynamicConfigJSON = "{}"
		return profile, nil
	}
	if !json.Valid([]byte(dynamic)) {
		return EgressProfile{}, fmt.Errorf("egress profile %q dynamic_config_json must be valid JSON", profile.ID)
	}
	var decoded interface{}
	decoder := json.NewDecoder(strings.NewReader(dynamic))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return EgressProfile{}, fmt.Errorf("egress profile %q dynamic_config_json: %w", profile.ID, err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return EgressProfile{}, fmt.Errorf("egress profile %q dynamic_config_json: %w", profile.ID, err)
	}
	profile.DynamicConfigJSON = string(canonical)
	return profile, nil
}

func accountBackupDefinitionFingerprint(value interface{}) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func accountBackupCustomProviderFingerprint(provider CustomProvider) (string, error) {
	provider.CreatedAt = 0
	provider.UpdatedAt = 0
	return accountBackupDefinitionFingerprint(provider)
}

func accountBackupEgressProfileFingerprint(profile EgressProfile) (string, error) {
	profile.CreatedAt = 0
	profile.UpdatedAt = 0
	return accountBackupDefinitionFingerprint(profile)
}

func accountBackupReferencedEgressIDs(backup AccountBackup) []string {
	var ids []string
	if binding := backup.EgressBinding; binding != nil {
		ids = append(ids, binding.PrimaryEgressID)
		ids = append(ids, binding.StandbyIDs()...)
		ids = append(ids, binding.SidecarEgressID)
	}
	for _, cookie := range backup.InjectedCookies {
		ids = append(ids, cookie.EgressID)
	}
	if provider := backup.CustomProvider; provider != nil {
		ids = append(ids, provider.EgressIDs...)
	}
	return normalizeOrderedIDs(ids)
}

func normalizedAccountBackupDefinitions(backups []AccountBackup) ([]CustomProvider, []EgressProfile, error) {
	providersByID := make(map[string]CustomProvider)
	providerFingerprints := make(map[string]string)
	egressesByID := make(map[string]EgressProfile)
	egressFingerprints := make(map[string]string)

	for index := range backups {
		backup := &backups[index]
		backup.Account.Provider = strings.TrimSpace(backup.Account.Provider)
		portableDefinitions := backup.CustomProvider != nil || backup.EgressProfiles != nil

		if backup.CustomProvider != nil {
			provider, err := normalizeAccountBackupCustomProvider(*backup.CustomProvider)
			if err != nil {
				return nil, nil, fmt.Errorf("account %q: %w", backup.Account.ID, err)
			}
			expectedProviderID := accountBackupCustomProviderID(backup.Account.Provider)
			if expectedProviderID == "" || provider.ID != expectedProviderID {
				return nil, nil, fmt.Errorf("account %q custom provider definition %q does not match account provider %q", backup.Account.ID, provider.ID, backup.Account.Provider)
			}
			fingerprint, err := accountBackupCustomProviderFingerprint(provider)
			if err != nil {
				return nil, nil, err
			}
			if previous, duplicate := providerFingerprints[provider.ID]; duplicate && previous != fingerprint {
				return nil, nil, fmt.Errorf("conflicting custom provider definitions for id %q", provider.ID)
			}
			if previous, duplicate := providersByID[provider.ID]; duplicate {
				if provider.CreatedAt > 0 && (previous.CreatedAt == 0 || provider.CreatedAt < previous.CreatedAt) {
					previous.CreatedAt = provider.CreatedAt
				}
				if provider.UpdatedAt > previous.UpdatedAt {
					previous.UpdatedAt = provider.UpdatedAt
				}
				providersByID[provider.ID] = previous
			} else {
				providerFingerprints[provider.ID] = fingerprint
				providersByID[provider.ID] = provider
			}
			copy := provider
			backup.CustomProvider = &copy
		} else if portableDefinitions && accountBackupCustomProviderID(backup.Account.Provider) != "" {
			return nil, nil, fmt.Errorf("account %q portable backup is missing custom provider definition %q", backup.Account.ID, backup.Account.Provider)
		}

		documentEgresses := make(map[string]struct{}, len(backup.EgressProfiles))
		documentEgressFingerprints := make(map[string]string, len(backup.EgressProfiles))
		// Preserve nil for legacy account-only backups. A non-nil (even empty)
		// slice is the v1 marker that portable dependency definitions are
		// present, so converting nil to [] here would make a second validation
		// pass reject historical custom-provider rows as incomplete.
		var normalizedDocumentEgresses []EgressProfile
		if backup.EgressProfiles != nil {
			normalizedDocumentEgresses = make([]EgressProfile, 0, len(backup.EgressProfiles))
		}
		for egressIndex := range backup.EgressProfiles {
			profile, err := normalizeAccountBackupEgressProfile(backup.EgressProfiles[egressIndex])
			if err != nil {
				return nil, nil, fmt.Errorf("account %q: %w", backup.Account.ID, err)
			}
			fingerprint, err := accountBackupEgressProfileFingerprint(profile)
			if err != nil {
				return nil, nil, err
			}
			if previous, duplicate := documentEgressFingerprints[profile.ID]; duplicate {
				if previous != fingerprint {
					return nil, nil, fmt.Errorf("account %q contains conflicting egress profile definitions for id %q", backup.Account.ID, profile.ID)
				}
				continue
			}
			documentEgresses[profile.ID] = struct{}{}
			documentEgressFingerprints[profile.ID] = fingerprint
			normalizedDocumentEgresses = append(normalizedDocumentEgresses, profile)
			if previous, duplicate := egressFingerprints[profile.ID]; duplicate && previous != fingerprint {
				return nil, nil, fmt.Errorf("conflicting egress profile definitions for id %q", profile.ID)
			}
			if previous, duplicate := egressesByID[profile.ID]; duplicate {
				if profile.CreatedAt > 0 && (previous.CreatedAt == 0 || profile.CreatedAt < previous.CreatedAt) {
					previous.CreatedAt = profile.CreatedAt
				}
				if profile.UpdatedAt > previous.UpdatedAt {
					previous.UpdatedAt = profile.UpdatedAt
				}
				egressesByID[profile.ID] = previous
			} else {
				egressFingerprints[profile.ID] = fingerprint
				egressesByID[profile.ID] = profile
			}
		}
		backup.EgressProfiles = normalizedDocumentEgresses
		sort.Slice(backup.EgressProfiles, func(left, right int) bool {
			return backup.EgressProfiles[left].ID < backup.EgressProfiles[right].ID
		})
		if portableDefinitions {
			for _, egressID := range accountBackupReferencedEgressIDs(*backup) {
				if _, found := documentEgresses[egressID]; !found {
					return nil, nil, fmt.Errorf("account %q portable backup is missing egress profile definition %q", backup.Account.ID, egressID)
				}
			}
		}
	}

	providerIDs := make([]string, 0, len(providersByID))
	for id := range providersByID {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)
	providers := make([]CustomProvider, 0, len(providerIDs))
	for _, id := range providerIDs {
		providers = append(providers, providersByID[id])
	}
	egressIDs := make([]string, 0, len(egressesByID))
	for id := range egressesByID {
		egressIDs = append(egressIDs, id)
	}
	sort.Strings(egressIDs)
	egresses := make([]EgressProfile, 0, len(egressIDs))
	for _, id := range egressIDs {
		egresses = append(egresses, egressesByID[id])
	}
	return providers, egresses, nil
}

// ValidateAndNormalizeAccountBackupDefinitions validates the portable global
// dependencies embedded in account backups. Definitions are optional so older
// account-only documents remain importable. Once a document contains portable
// definitions, it must be self-contained, and repeated IDs across a batch must
// describe the same canonical provider/egress configuration.
func ValidateAndNormalizeAccountBackupDefinitions(backups []AccountBackup) error {
	_, _, err := normalizedAccountBackupDefinitions(backups)
	return err
}

func (s *Store) accountBackupCustomProvidersByIDs(ctx context.Context, providerIDs []string) (map[string]CustomProvider, error) {
	out := make(map[string]CustomProvider, len(providerIDs))
	err := forEachAccountBackupIDBatch(providerIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT `+customProviderCols+` FROM custom_providers WHERE id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			provider, err := scanCustomProvider(rows.Scan)
			if err != nil {
				_ = rows.Close()
				return err
			}
			provider, err = normalizeAccountBackupCustomProvider(provider)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out[provider.ID] = provider
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) accountBackupEgressProfilesByIDs(ctx context.Context, egressIDs []string) (map[string]EgressProfile, error) {
	out := make(map[string]EgressProfile, len(egressIDs))
	err := forEachAccountBackupIDBatch(egressIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT id, name, type, endpoint, chain_proxy, region, exit_ip, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at, proxy_auth_mode, proxy_api_key, ip_mode, provider_key, dynamic_config_json FROM egress_profiles WHERE id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			profile, err := scanEgress(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			profile, err = normalizeAccountBackupEgressProfile(profile)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out[profile.ID] = profile
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

// ExportAccountBackups batch-loads complete account backup records without an
// N+1 credential query. The supplied account order is retained.
func (s *Store) ExportAccountBackups(ctx context.Context, accounts []Account) ([]AccountBackup, error) {
	if len(accounts) == 0 {
		return []AccountBackup{}, nil
	}
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if id := strings.TrimSpace(account.ID); id != "" {
			accountIDs = append(accountIDs, id)
		}
	}
	tokens := make(map[string]AccountToken, len(accountIDs))
	if err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		items, err := s.ListTokensByAccountIDs(ctx, batch)
		if err != nil {
			return err
		}
		for id, item := range items {
			tokens[id] = item
		}
		return nil
	}); err != nil {
		return nil, err
	}
	capabilities := make(map[string][]ModelCapability, len(accountIDs))
	if err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		items, err := s.ListCapabilitiesByAccountIDs(ctx, batch)
		if err != nil {
			return err
		}
		for id, item := range items {
			capabilities[id] = append(capabilities[id], item...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	bindings := make(map[string]AccountEgressBinding, len(accountIDs))
	if err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		items, err := s.ListEgressBindingsByAccountIDs(ctx, batch)
		if err != nil {
			return err
		}
		for id, item := range items {
			bindings[id] = item
		}
		return nil
	}); err != nil {
		return nil, err
	}
	kiroCredentials, err := s.kiroCredentialsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	antigravityCredentials, err := s.antigravityCredentialsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	sessionCookies, err := s.sessionCookiesByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	injectedCookies, err := s.injectedCookiesByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	reauthConfigs, err := s.codexReauthConfigsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	catalogStatuses, err := s.modelCatalogStatusesByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	memberships, err := s.groupMembershipsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	customProviderIDs := make([]string, 0)
	for _, account := range accounts {
		if providerID := accountBackupCustomProviderID(account.Provider); providerID != "" {
			customProviderIDs = append(customProviderIDs, providerID)
		}
	}
	customProviders, err := s.accountBackupCustomProvidersByIDs(ctx, customProviderIDs)
	if err != nil {
		return nil, err
	}
	for _, providerID := range accountBackupIDBatches(customProviderIDs) {
		for _, id := range providerID {
			if _, found := customProviders[id]; !found {
				return nil, fmt.Errorf("custom provider definition %q referenced by an exported account was not found", id)
			}
		}
	}
	accountEgressIDs := make(map[string][]string, len(accounts))
	allEgressIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		dependencies := AccountBackup{
			InjectedCookies: injectedCookies[account.ID],
		}
		if binding, found := bindings[account.ID]; found {
			copy := binding
			dependencies.EgressBinding = &copy
		}
		if providerID := accountBackupCustomProviderID(account.Provider); providerID != "" {
			provider := customProviders[providerID]
			dependencies.CustomProvider = &provider
		}
		ids := accountBackupReferencedEgressIDs(dependencies)
		accountEgressIDs[account.ID] = ids
		allEgressIDs = append(allEgressIDs, ids...)
	}
	egressProfiles, err := s.accountBackupEgressProfilesByIDs(ctx, allEgressIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range accountBackupIDBatches(allEgressIDs) {
		for _, egressID := range id {
			if _, found := egressProfiles[egressID]; !found {
				return nil, fmt.Errorf("egress profile definition %q referenced by an exported account was not found", egressID)
			}
		}
	}

	out := make([]AccountBackup, 0, len(accounts))
	for _, account := range accounts {
		token, ok := tokens[account.ID]
		if !ok {
			return nil, fmt.Errorf("account %q has no authentication row", account.ID)
		}
		backup := AccountBackup{
			Account:          account,
			Token:            token,
			Capabilities:     capabilities[account.ID],
			InjectedCookies:  injectedCookies[account.ID],
			GroupMemberships: memberships[account.ID],
		}
		if providerID := accountBackupCustomProviderID(account.Provider); providerID != "" {
			item := customProviders[providerID]
			backup.CustomProvider = &item
		}
		for _, egressID := range accountEgressIDs[account.ID] {
			backup.EgressProfiles = append(backup.EgressProfiles, egressProfiles[egressID])
		}
		sort.Slice(backup.EgressProfiles, func(left, right int) bool {
			return backup.EgressProfiles[left].ID < backup.EgressProfiles[right].ID
		})
		if item, found := kiroCredentials[account.ID]; found {
			copy := item
			backup.KiroCredentials = &copy
		}
		if item, found := antigravityCredentials[account.ID]; found {
			copy := item
			backup.AntigravityCredentials = &copy
		}
		if item, found := sessionCookies[account.ID]; found {
			copy := item
			backup.SessionCookie = &copy
		}
		if item, found := bindings[account.ID]; found {
			copy := item
			backup.EgressBinding = &copy
		}
		if item, found := reauthConfigs[account.ID]; found {
			copy := item
			backup.CodexReauthConfig = &copy
		}
		if item, found := catalogStatuses[account.ID]; found {
			copy := item
			backup.ModelCatalogStatus = &copy
		}
		out = append(out, backup)
	}
	return out, nil
}

func (s *Store) kiroCredentialsByAccountIDs(ctx context.Context, accountIDs []string) (map[string]KiroCredentials, error) {
	out := make(map[string]KiroCredentials, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, auth_method, client_id, client_secret, profile_arn, auth_region, api_region, machine_id, kiro_api_key, endpoint, credential_hash, created_at, updated_at FROM account_kiro_credentials WHERE account_id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item KiroCredentials
			if err := rows.Scan(&item.AccountID, &item.AuthMethod, &item.ClientID, &item.ClientSecret, &item.ProfileARN, &item.AuthRegion, &item.APIRegion, &item.MachineID, &item.KiroAPIKey, &item.Endpoint, &item.CredentialHash, &item.CreatedAt, &item.UpdatedAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.ClientSecret = s.openToken(item.ClientSecret)
			item.KiroAPIKey = s.openToken(item.KiroAPIKey)
			out[item.AccountID] = item
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) antigravityCredentialsByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AntigravityCredentials, error) {
	out := make(map[string]AntigravityCredentials, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, email, project_id, access_token, refresh_token, expires_at, base_url, user_agent, created_at, updated_at FROM account_antigravity_credentials WHERE account_id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item AntigravityCredentials
			if err := rows.Scan(&item.AccountID, &item.Email, &item.ProjectID, &item.AccessToken, &item.RefreshToken, &item.ExpiresAt, &item.BaseURL, &item.UserAgent, &item.CreatedAt, &item.UpdatedAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.AccessToken = s.openToken(item.AccessToken)
			item.RefreshToken = s.openToken(item.RefreshToken)
			out[item.AccountID] = item
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) sessionCookiesByAccountIDs(ctx context.Context, accountIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, cookie FROM account_session_cookies WHERE account_id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var accountID, cookie string
			if err := rows.Scan(&accountID, &cookie); err != nil {
				_ = rows.Close()
				return err
			}
			out[accountID] = s.openToken(cookie)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) injectedCookiesByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]InjectedCookie, error) {
	out := make(map[string][]InjectedCookie, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, egress_id, upstream_host, cookie_header, user_agent, exit_ip, updated_at FROM account_injected_cookies WHERE account_id IN (`+sqlPlaceholders(len(batch))+`) ORDER BY account_id, egress_id, upstream_host`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item InjectedCookie
			if err := rows.Scan(&item.AccountID, &item.EgressID, &item.UpstreamHost, &item.CookieHeader, &item.UserAgent, &item.ExitIP, &item.UpdatedAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.CookieHeader = s.openToken(item.CookieHeader)
			out[item.AccountID] = append(out[item.AccountID], item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) codexReauthConfigsByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountCodexReauthConfig, error) {
	out := make(map[string]AccountCodexReauthConfig, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at FROM account_codex_reauth_config WHERE account_id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item AccountCodexReauthConfig
			var encryptedPassword, encryptedOTPURL string
			var autoEnabled int
			if err := rows.Scan(&item.AccountID, &item.LoginEmail, &encryptedPassword, &encryptedOTPURL, &item.TargetWorkspaceID, &autoEnabled, &item.LastStatus, &item.LastError, &item.CreatedAt, &item.UpdatedAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.Password = s.openToken(encryptedPassword)
			item.OTPURL = s.openToken(encryptedOTPURL)
			item.PasswordConfigured = item.Password != ""
			item.OTPURLConfigured = item.OTPURL != ""
			item.AutoEnabled = autoEnabled != 0
			out[item.AccountID] = item
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) modelCatalogStatusesByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountModelCatalogStatus, error) {
	out := make(map[string]AccountModelCatalogStatus, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, authoritative, last_probe_at FROM account_model_catalog_status WHERE account_id IN (`+sqlPlaceholders(len(batch))+`)`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item AccountModelCatalogStatus
			var authoritative int
			if err := rows.Scan(&item.AccountID, &authoritative, &item.LastProbeAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.Authoritative = authoritative != 0
			out[item.AccountID] = item
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func (s *Store) groupMembershipsByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]AccountGroupMembership, error) {
	out := make(map[string][]AccountGroupMembership, len(accountIDs))
	err := forEachAccountBackupIDBatch(accountIDs, func(batch []string) error {
		rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, group_name, is_primary, created_at FROM account_group_memberships WHERE account_id IN (`+sqlPlaceholders(len(batch))+`) ORDER BY account_id, is_primary DESC, group_name`, stringArgs(batch)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item AccountGroupMembership
			var primary int
			if err := rows.Scan(&item.AccountID, &item.GroupName, &primary, &item.CreatedAt); err != nil {
				_ = rows.Close()
				return err
			}
			item.IsPrimary = primary != 0
			out[item.AccountID] = append(out[item.AccountID], item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	return out, err
}

func restoreAccountBackupEgressProfileTx(ctx context.Context, tx *sql.Tx, profile EgressProfile, now int64) error {
	profile.CreatedAt = backupTimestamp(profile.CreatedAt, now)
	profile.UpdatedAt = backupTimestamp(profile.UpdatedAt, now)
	_, err := tx.ExecContext(ctx, `
INSERT INTO egress_profiles(id, name, type, endpoint, chain_proxy, region, exit_ip, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at, proxy_auth_mode, proxy_api_key, ip_mode, provider_key, dynamic_config_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name=excluded.name,
 type=excluded.type,
 endpoint=excluded.endpoint,
 chain_proxy=excluded.chain_proxy,
 region=excluded.region,
 exit_ip=excluded.exit_ip,
 stream_capable=excluded.stream_capable,
 health=excluded.health,
 latency_millis=excluded.latency_millis,
 cf_score=excluded.cf_score,
 last_cf_ray=excluded.last_cf_ray,
 cooldown_until=excluded.cooldown_until,
 max_concurrency=excluded.max_concurrency,
 updated_at=excluded.updated_at,
 proxy_auth_mode=excluded.proxy_auth_mode,
 proxy_api_key=excluded.proxy_api_key,
 ip_mode=excluded.ip_mode,
 provider_key=excluded.provider_key,
 dynamic_config_json=excluded.dynamic_config_json`,
		profile.ID, profile.Name, profile.Type, profile.Endpoint, profile.ChainProxy,
		profile.Region, profile.ExitIP, boolInt(profile.StreamCapable), profile.Health,
		profile.LatencyMillis, profile.CFScore, profile.LastCFRay, profile.CooldownUntil,
		profile.MaxConcurrency, profile.CreatedAt, profile.UpdatedAt, profile.ProxyAuthMode,
		profile.ProxyAPIKey, profile.IPMode, profile.ProviderKey, profile.DynamicConfigJSON,
	)
	return err
}

func restoreAccountBackupCustomProviderTx(ctx context.Context, tx *sql.Tx, provider CustomProvider, now int64) error {
	provider.CreatedAt = backupTimestamp(provider.CreatedAt, now)
	provider.UpdatedAt = backupTimestamp(provider.UpdatedAt, now)
	modelsJSON, err := json.Marshal(provider.Models)
	if err != nil {
		return err
	}
	mappingsJSON, err := json.Marshal(provider.ModelMappings)
	if err != nil {
		return err
	}
	routes, err := canonicalCustomProviderRoutes(provider.Routes, provider, true)
	if err != nil {
		return err
	}
	routesJSON, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO custom_providers(id, name, base_url, upstream_protocol, transport_profile, routes_json, egress_ids, enabled, auto_discover_models, models_json, model_mappings_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name=excluded.name,
 base_url=excluded.base_url,
 upstream_protocol=excluded.upstream_protocol,
 transport_profile=excluded.transport_profile,
 routes_json=excluded.routes_json,
 egress_ids=excluded.egress_ids,
 enabled=excluded.enabled,
 auto_discover_models=excluded.auto_discover_models,
 models_json=excluded.models_json,
 model_mappings_json=excluded.model_mappings_json,
 updated_at=excluded.updated_at`,
		provider.ID, provider.Name, provider.BaseURL, provider.UpstreamProtocol,
		provider.TransportProfile, string(routesJSON), encodeOrderedIDs(provider.EgressIDs), boolInt(provider.Enabled),
		boolInt(provider.AutoDiscoverModels), string(modelsJSON), string(mappingsJSON),
		provider.CreatedAt, provider.UpdatedAt,
	)
	return err
}

// RestoreAccountBackups atomically and completely replaces the durable
// configuration of every included account. Accounts not present in backups are
// untouched. A validation or database failure leaves every account unchanged.
func (s *Store) RestoreAccountBackups(ctx context.Context, backups []AccountBackup) error {
	if len(backups) == 0 {
		return errors.New("account backup contains no accounts")
	}
	seen := make(map[string]struct{}, len(backups))
	for i := range backups {
		id := strings.TrimSpace(backups[i].Account.ID)
		if id == "" {
			return fmt.Errorf("account backup %d has no id", i+1)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("account backup contains duplicate id %q", id)
		}
		seen[id] = struct{}{}
		backups[i].Account.ID = id
		normalizeAccountBackupChildren(&backups[i])
	}
	providers, egresses, err := normalizedAccountBackupDefinitions(backups)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	for _, profile := range egresses {
		if err := restoreAccountBackupEgressProfileTx(ctx, tx, profile, now); err != nil {
			return fmt.Errorf("restore egress profile %q: %w", profile.ID, err)
		}
	}
	for _, provider := range providers {
		if err := restoreAccountBackupCustomProviderTx(ctx, tx, provider, now); err != nil {
			return fmt.Errorf("restore custom provider %q: %w", provider.ID, err)
		}
	}
	for i := range backups {
		if err := s.restoreAccountBackupTx(ctx, tx, backups[i]); err != nil {
			return fmt.Errorf("restore account %q: %w", backups[i].Account.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, backup := range backups {
		s.tokenCache.Delete(backup.Account.ID)
		s.kiroCache.Delete(backup.Account.ID)
	}
	s.rateLimitGen.Add(1)
	s.affinityGen.Add(1)
	return nil
}

func normalizeAccountBackupChildren(backup *AccountBackup) {
	accountID := backup.Account.ID
	backup.Token.AccountID = accountID
	if backup.KiroCredentials != nil {
		backup.KiroCredentials.AccountID = accountID
	}
	if backup.AntigravityCredentials != nil {
		backup.AntigravityCredentials.AccountID = accountID
	}
	if backup.EgressBinding != nil {
		backup.EgressBinding.AccountID = accountID
	}
	if backup.ModelCatalogStatus != nil {
		backup.ModelCatalogStatus.AccountID = accountID
	}
	if backup.CodexReauthConfig != nil {
		backup.CodexReauthConfig.AccountID = accountID
	}
	for i := range backup.InjectedCookies {
		backup.InjectedCookies[i].AccountID = accountID
	}
	for i := range backup.Capabilities {
		backup.Capabilities[i].AccountID = accountID
	}
	for i := range backup.GroupMemberships {
		backup.GroupMemberships[i].AccountID = accountID
	}
}

func backupTimestamp(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func restoreAccountBackupDefaultEgressIDTx(ctx context.Context, tx *sql.Tx, account Account) (string, error) {
	candidates := make([]string, 0, 4)
	groupName := strings.TrimSpace(account.GroupName)
	if groupName != "" {
		var defaultEgressID, egressJSON string
		err := tx.QueryRowContext(ctx, `SELECT default_egress_id, egress_ids FROM groups WHERE name = ?`, groupName).Scan(&defaultEgressID, &egressJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if err == nil {
			candidates = append(candidates, defaultEgressID)
			candidates = append(candidates, decodeStringList(egressJSON)...)
		}
	}
	candidates = append(candidates, DefaultDirectEgressID)
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM egress_profiles WHERE id = ?`, candidate).Scan(&exists)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return DefaultDirectEgressID, nil
}

func (s *Store) restoreAccountBackupTx(ctx context.Context, tx *sql.Tx, backup AccountBackup) error {
	now := Now()
	account := backup.Account
	account.CreatedAt = backupTimestamp(account.CreatedAt, now)
	account.UpdatedAt = backupTimestamp(account.UpdatedAt, now)
	if strings.TrimSpace(account.Status) == "" {
		account.Status = "active"
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO accounts(id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, quarantine_until, quarantine_reason, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 label=excluded.label, group_name=excluded.group_name,
 upstream_account_id=excluded.upstream_account_id, chatgpt_user_id=excluded.chatgpt_user_id,
 email=excluded.email, plan_type=excluded.plan_type, provider=excluded.provider,
 status=excluded.status, is_fedramp=excluded.is_fedramp,
 ignore_rate_limit_controls=excluded.ignore_rate_limit_controls,
 quarantine_until=excluded.quarantine_until, quarantine_reason=excluded.quarantine_reason,
 created_at=excluded.created_at, updated_at=excluded.updated_at`,
		account.ID, account.Label, account.GroupName, account.UpstreamAccountID, account.ChatGPTUserID,
		account.Email, account.PlanType, account.Provider, account.Status, boolInt(account.IsFedramp),
		boolInt(account.IgnoreRateLimitControls), account.QuarantineUntil, account.QuarantineReason,
		account.CreatedAt, account.UpdatedAt,
	); err != nil {
		return err
	}

	for _, table := range []string{
		"account_auth_tokens",
		"account_kiro_credentials",
		"account_antigravity_credentials",
		"account_session_cookies",
		"account_injected_cookies",
		"account_egress_bindings",
		"account_model_capabilities",
		"account_model_catalog_status",
		"account_codex_reauth_config",
		"account_group_memberships",
		"account_lifecycle_status",
		"account_codex_reauth_jobs",
		"account_rate_limits",
		"kiro_runtime_capabilities",
		"kiro_model_catalog",
		"kiro_probe_state",
		"affinity_aliases",
		"affinity_bindings",
		"codex_reset_credit_consumptions",
		"antigravity_cache_entries",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE account_id = ?`, account.ID); err != nil {
			return err
		}
	}

	token := backup.Token
	token.CreatedAt = backupTimestamp(token.CreatedAt, now)
	token.UpdatedAt = backupTimestamp(token.UpdatedAt, now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO account_auth_tokens(account_id, auth_method, credential_mode, access_token, refresh_token, openai_api_key, id_token_raw, agent_runtime_id, agent_private_key, agent_task_id, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account.ID, token.AuthMethod, token.CredentialMode, s.sealToken(token.AccessToken),
		s.sealToken(token.RefreshToken), s.sealToken(token.OpenAIAPIKey), s.sealToken(token.IDTokenRaw),
		s.sealToken(token.AgentRuntimeID), s.sealToken(token.AgentPrivateKey), s.sealToken(token.AgentTaskID),
		token.LastRefresh, token.ExpiresAt, token.Scopes, token.OAuthRateLimitTier, token.CreatedAt, token.UpdatedAt,
	); err != nil {
		return err
	}

	if item := backup.KiroCredentials; item != nil {
		item.CreatedAt = backupTimestamp(item.CreatedAt, now)
		item.UpdatedAt = backupTimestamp(item.UpdatedAt, now)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_kiro_credentials(account_id, auth_method, client_id, client_secret, profile_arn, auth_region, api_region, machine_id, kiro_api_key, endpoint, credential_hash, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account.ID, item.AuthMethod, item.ClientID, s.sealToken(item.ClientSecret), item.ProfileARN,
			item.AuthRegion, item.APIRegion, item.MachineID, s.sealToken(item.KiroAPIKey),
			item.Endpoint, item.CredentialHash, item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return err
		}
	}
	if item := backup.AntigravityCredentials; item != nil {
		item.CreatedAt = backupTimestamp(item.CreatedAt, now)
		item.UpdatedAt = backupTimestamp(item.UpdatedAt, now)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_antigravity_credentials(account_id, email, project_id, access_token, refresh_token, expires_at, base_url, user_agent, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account.ID, item.Email, item.ProjectID, s.sealToken(item.AccessToken),
			s.sealToken(item.RefreshToken), item.ExpiresAt, item.BaseURL, item.UserAgent,
			item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return err
		}
	}
	if backup.SessionCookie != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_session_cookies(account_id, cookie, updated_at) VALUES(?, ?, ?)`, account.ID, s.sealToken(*backup.SessionCookie), now); err != nil {
			return err
		}
	}
	for _, item := range backup.InjectedCookies {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_injected_cookies(account_id, egress_id, upstream_host, cookie_header, user_agent, exit_ip, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`,
			account.ID, item.EgressID, item.UpstreamHost, s.sealToken(item.CookieHeader),
			item.UserAgent, item.ExitIP, backupTimestamp(item.UpdatedAt, now),
		); err != nil {
			return err
		}
	}
	item := backup.EgressBinding
	if item == nil {
		defaultEgressID, err := restoreAccountBackupDefaultEgressIDTx(ctx, tx, account)
		if err != nil {
			return err
		}
		item = &AccountEgressBinding{
			AccountID:        account.ID,
			PrimaryEgressID:  defaultEgressID,
			StandbyEgressIDs: "",
			SidecarEgressID:  "",
			CookieJarKey:     account.ID + ":" + defaultEgressID,
			CooldownUntil:    0,
			CreatedAt:        account.CreatedAt,
			UpdatedAt:        now,
		}
	}
	item.CreatedAt = backupTimestamp(item.CreatedAt, now)
	item.UpdatedAt = backupTimestamp(item.UpdatedAt, now)
	if strings.TrimSpace(item.PrimaryEgressID) == "" {
		item.PrimaryEgressID = DefaultDirectEgressID
	}
	if item.CookieJarKey == "" {
		item.CookieJarKey = account.ID + ":" + item.PrimaryEgressID
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at)
	VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account.ID, item.PrimaryEgressID, item.StandbyEgressIDs, item.SidecarEgressID,
		item.CookieJarKey, item.CooldownUntil, boolInt(item.RecheckPending),
		item.CreatedAt, item.UpdatedAt,
	); err != nil {
		return err
	}
	for _, item := range backup.Capabilities {
		if strings.TrimSpace(item.ModelSlug) == "" {
			return errors.New("model capability has no model_slug")
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_model_capabilities(account_id, model_slug, availability_state, context_1m_state, context_1m_source, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account.ID, item.ModelSlug, item.AvailabilityState, item.Context1MState, item.Context1MSource,
			item.NativeContextWindow, item.NativeMaxContextWindow, item.EffectiveContextWindowPercent,
			item.AutoCompactTokenLimit, item.Visibility, item.ETag, item.RawModelJSONHash,
			item.RawModelJSON, item.Source, backupTimestamp(item.LastProbeAt, now),
		); err != nil {
			return err
		}
	}
	if item := backup.ModelCatalogStatus; item != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_model_catalog_status(account_id, authoritative, last_probe_at) VALUES(?, ?, ?)`,
			account.ID, boolInt(item.Authoritative), item.LastProbeAt); err != nil {
			return err
		}
	}
	if item := backup.CodexReauthConfig; item != nil {
		item.CreatedAt = backupTimestamp(item.CreatedAt, now)
		item.UpdatedAt = backupTimestamp(item.UpdatedAt, now)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_codex_reauth_config(account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account.ID, item.LoginEmail, s.sealToken(item.Password), s.sealToken(item.OTPURL),
			item.TargetWorkspaceID, boolInt(item.AutoEnabled), item.LastStatus, item.LastError,
			item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return err
		}
	}
	memberships := backup.GroupMemberships
	if len(memberships) == 0 && strings.TrimSpace(account.GroupName) != "" {
		memberships = []AccountGroupMembership{{
			AccountID: account.ID, GroupName: account.GroupName, IsPrimary: true, CreatedAt: account.CreatedAt,
		}}
	}
	seenGroups := make(map[string]struct{}, len(memberships))
	for _, item := range memberships {
		groupName := strings.TrimSpace(item.GroupName)
		if groupName == "" {
			continue
		}
		if _, duplicate := seenGroups[groupName]; duplicate {
			return fmt.Errorf("duplicate group membership %q", groupName)
		}
		seenGroups[groupName] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at) VALUES(?, ?, ?, ?)`,
			account.ID, groupName, boolInt(item.IsPrimary), backupTimestamp(item.CreatedAt, account.CreatedAt)); err != nil {
			return err
		}
	}
	return nil
}
