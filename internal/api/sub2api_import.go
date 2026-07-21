package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
)

type authDocumentImportResult struct {
	Format       string                   `json:"format"`
	Total        int                      `json:"total"`
	Imported     int                      `json:"imported"`
	Duplicates   int                      `json:"duplicates"`
	Failed       int                      `json:"failed"`
	ProxyCreated int                      `json:"proxy_created,omitempty"`
	ProxyReused  int                      `json:"proxy_reused,omitempty"`
	ProxyFailed  int                      `json:"proxy_failed,omitempty"`
	Items        []authDocumentImportItem `json:"items"`
	Errors       []authDocumentError      `json:"errors,omitempty"`
	Warnings     []authDocumentError      `json:"warnings,omitempty"`
}

type authDocumentImportItem struct {
	Index     int      `json:"index"`
	Name      string   `json:"name,omitempty"`
	AccountID string   `json:"account_id,omitempty"`
	Action    string   `json:"action"`
	EgressID  string   `json:"egress_id,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type authDocumentError struct {
	Kind     string `json:"kind"`
	Index    int    `json:"index,omitempty"`
	Name     string `json:"name,omitempty"`
	ProxyKey string `json:"proxy_key,omitempty"`
	Message  string `json:"message"`
}

func (s *Server) adminImportAuthDocument(w http.ResponseWriter, r *http.Request, req authJSONImportRequest, doc authparse.ImportDocument) {
	result := authDocumentImportResult{
		Format: doc.Format, Total: len(doc.Entries), Items: make([]authDocumentImportItem, 0, len(doc.Entries)),
	}
	proxyEgress := map[string]string{}
	if doc.Format == authparse.ImportFormatSub2API {
		proxyEgress = s.importSub2APIProxies(r.Context(), doc.Proxies, &result)
	}
	defaultEgress := requestedImportEgressID(req.EgressID, req.PrimaryEgressID)
	for _, entry := range doc.Entries {
		item := authDocumentImportItem{Index: entry.Index, Name: entry.Name, Warnings: entry.Warnings}
		if entry.Err != nil {
			s.failAuthDocumentItem(&result, &item, entry.Err)
			continue
		}
		egressID := defaultEgress
		if entry.ProxyKey != "" {
			var found bool
			egressID, found = proxyEgress[entry.ProxyKey]
			if !found {
				err := fmt.Errorf("referenced proxy %s was not imported", safeSub2APIProxyKey(entry.ProxyKey))
				s.failAuthDocumentItem(&result, &item, err)
				continue
			}
		}
		label := entry.Name
		if strings.TrimSpace(req.Label) != "" {
			label = strings.TrimSpace(req.Label)
			if len(doc.Entries) > 1 {
				label = fmt.Sprintf("%s #%d", label, entry.Index)
			}
		}
		parsed := entry.Parsed
		tokenShape := accountTokenFromParsed(parsed, parsed.RefreshToken)
		provider := firstNonEmpty(parsed.Provider, accountprovider.InferProviderFromToken(tokenShape))
		credential := accountprovider.Credential(provider, tokenShape)
		if (provider == "codex" || provider == "claude") &&
			(accountprovider.UsesAPIKey(provider, tokenShape) || accountprovider.LooksLikeAPIKey(provider, credential)) {
			s.failAuthDocumentItem(&result, &item, errors.New("built-in provider API keys require /admin/accounts/import-key with confirm_cost:true"))
			continue
		}
		if existing, err := s.store.GetAccount(r.Context(), parsed.AccountID); err == nil {
			item.AccountID = existing.ID
			item.Name = firstNonEmpty(existing.Label, item.Name)
			item.Action = "duplicate"
			result.Duplicates++
			result.Items = append(result.Items, item)
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.failAuthDocumentItem(&result, &item, err)
			continue
		}
		account, err := s.saveImportedAccount(r.Context(), parsed, label, req.GroupName, "", provider, egressID)
		if err != nil {
			s.failAuthDocumentItem(&result, &item, err)
			continue
		}
		item.AccountID = account.ID
		item.Name = firstNonEmpty(account.Label, item.Name)
		item.Action = "imported"
		if binding, bindErr := s.store.GetEgressBinding(r.Context(), account.ID); bindErr == nil {
			item.EgressID = binding.PrimaryEgressID
		}
		result.Imported++
		result.Items = append(result.Items, item)
		action := "auth_json_batch_import"
		if doc.Format == authparse.ImportFormatSub2API {
			action = "sub2api_data_import"
		}
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
			AccountID: account.ID, AccountLabel: account.Label, Action: action,
			State: "active", Reason: "imported", Detail: "credential_mode=" + firstNonEmpty(parsed.CredentialMode, "token"),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) failAuthDocumentItem(result *authDocumentImportResult, item *authDocumentImportItem, err error) {
	item.Action = "failed"
	item.Message = err.Error()
	result.Failed++
	result.Items = append(result.Items, *item)
	result.Errors = append(result.Errors, authDocumentError{Kind: "account", Index: item.Index, Name: item.Name, Message: item.Message})
}

func accountTokenFromParsed(parsed authparse.ParsedAuth, refreshToken string) storage.AccountToken {
	lastRefresh := parsed.LastRefresh
	if lastRefresh == 0 {
		lastRefresh = storage.Now()
	}
	token := storage.AccountToken{
		CredentialMode: parsed.CredentialMode, AccessToken: parsed.AccessToken, RefreshToken: refreshToken,
		OpenAIAPIKey: parsed.OpenAIAPIKey, IDTokenRaw: parsed.IDTokenRaw,
		AgentRuntimeID: parsed.AgentRuntimeID, AgentPrivateKey: parsed.AgentPrivateKey, AgentTaskID: parsed.AgentTaskID,
		LastRefresh: lastRefresh, ExpiresAt: parsed.ExpiresAt, Scopes: strings.Join(parsed.Scopes, " "), OAuthRateLimitTier: parsed.OAuthRateLimitTier,
	}
	token.AuthMethod = accountprovider.EffectiveAuthMethod(parsed.Provider, token)
	return token
}

func (s *Server) importSub2APIProxies(ctx context.Context, proxies []authparse.Sub2APIProxy, result *authDocumentImportResult) map[string]string {
	resolved := make(map[string]string, len(proxies))
	existing, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		for _, proxy := range proxies {
			key := sub2APIProxyKey(proxy)
			result.ProxyFailed++
			result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: err.Error()})
		}
		return resolved
	}
	byWire := make(map[string]string, len(existing))
	byID := make(map[string]storage.EgressProfile, len(existing))
	for _, profile := range existing {
		byWire[strings.ToLower(profile.Type)+"\x00"+profile.Endpoint] = profile.ID
		byID[profile.ID] = profile
	}
	for _, proxy := range proxies {
		key := sub2APIProxyKey(proxy)
		if proxy.ExpiresAt != nil || strings.TrimSpace(proxy.FallbackMode) != "" || strings.TrimSpace(proxy.BackupProxyName) != "" || proxy.ExpiryWarnDays != 0 {
			result.Warnings = append(result.Warnings, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: "sub2api proxy expiry/fallback metadata was not copied; configure equivalent egress policy explicitly"})
		}
		if key == "" {
			result.ProxyFailed++
			result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, Message: "proxy_key and proxy fields are empty"})
			continue
		}
		profile, profileErr := sub2APIEgressProfile(proxy, key)
		if profileErr != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: profileErr.Error()})
			continue
		}
		wireKey := strings.ToLower(profile.Type) + "\x00" + profile.Endpoint
		if existingID, ok := byWire[wireKey]; ok {
			if existingProfile, found := byID[existingID]; found && strings.TrimSpace(proxy.Status) != "" && existingProfile.Health != profile.Health {
				existingProfile.Health = profile.Health
				if err := s.store.UpsertEgressProfile(ctx, existingProfile); err != nil {
					result.ProxyFailed++
					result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: err.Error()})
					continue
				}
				byID[existingID] = existingProfile
			}
			resolved[key] = existingID
			result.ProxyReused++
			continue
		}
		if collision, ok := byID[profile.ID]; ok && (collision.Type != profile.Type || collision.Endpoint != profile.Endpoint) {
			result.ProxyFailed++
			result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: "generated egress id collides with an unrelated profile"})
			continue
		}
		if err := s.store.UpsertEgressProfile(ctx, profile); err != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, authDocumentError{Kind: "proxy", Name: proxy.Name, ProxyKey: safeSub2APIProxyKey(key), Message: err.Error()})
			continue
		}
		resolved[key] = profile.ID
		byWire[wireKey] = profile.ID
		byID[profile.ID] = profile
		result.ProxyCreated++
	}
	return resolved
}

func sub2APIProxyKey(proxy authparse.Sub2APIProxy) string {
	if key := strings.TrimSpace(proxy.ProxyKey); key != "" {
		return key
	}
	if strings.TrimSpace(proxy.Protocol) == "" && strings.TrimSpace(proxy.Host) == "" && proxy.Port == 0 {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%s|%s", strings.TrimSpace(proxy.Protocol), strings.TrimSpace(proxy.Host), proxy.Port, strings.TrimSpace(proxy.Username), strings.TrimSpace(proxy.Password))
}

func safeSub2APIProxyKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

func sub2APIEgressProfile(proxy authparse.Sub2APIProxy, key string) (storage.EgressProfile, error) {
	host := strings.TrimSpace(proxy.Host)
	if host == "" || proxy.Port < 1 || proxy.Port > 65535 {
		return storage.EgressProfile{}, errors.New("proxy host and a valid port are required")
	}
	protocol := strings.ToLower(strings.TrimSpace(proxy.Protocol))
	typeName, scheme := "", ""
	switch protocol {
	case "http":
		typeName, scheme = "http_proxy", "http"
	case "https":
		typeName, scheme = "https_proxy", "https"
	case "socks5", "socks":
		typeName, scheme = "socks5_proxy", "socks5"
	case "socks5h":
		typeName, scheme = "socks5h_proxy", "socks5h"
	default:
		return storage.EgressProfile{}, fmt.Errorf("unsupported proxy protocol %q", proxy.Protocol)
	}
	endpoint := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(proxy.Port))}
	if proxy.Username != "" || proxy.Password != "" {
		endpoint.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	sum := sha256.Sum256([]byte(key))
	health := "healthy"
	switch strings.ToLower(strings.TrimSpace(proxy.Status)) {
	case "", "active":
	case "disabled", "inactive", "expired":
		health = "disabled"
	default:
		return storage.EgressProfile{}, fmt.Errorf("unsupported proxy status %q", proxy.Status)
	}
	return storage.EgressProfile{
		ID: "sub2api_" + hex.EncodeToString(sum[:])[:16], Name: firstNonEmpty(strings.TrimSpace(proxy.Name), "sub2api proxy"),
		Type: typeName, Endpoint: endpoint.String(), StreamCapable: true, Health: health, MaxConcurrency: 16, ProviderKey: "sub2api_import", DynamicConfigJSON: "{}",
	}, nil
}
