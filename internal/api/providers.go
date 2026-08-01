package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
)

// providers.go is the admin surface for custom OpenAI-compatible providers (DeepSeek,
// Kimi, OpenRouter, a local vLLM, …): list/create/update/delete the registry and
// import an account from a bare API key. The model list is sent/received as a JSON
// array (`models:[...]`) — the admin UI edits it via input boxes, never raw JSON.

// adminProviders lists or upserts custom providers.
//
//	GET  /admin/providers
//	POST /admin/providers  {id,name,base_url,upstream_protocol,enabled,auto_discover_models,models:[...]}
func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		ps, err := s.store.ListCustomProviders(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if ps == nil {
			ps = []storage.CustomProvider{}
		}
		writeJSON(w, http.StatusOK, ps)
	case http.MethodPost, http.MethodPatch:
		var req struct {
			ID                 string                         `json:"id"`
			Name               string                         `json:"name"`
			BaseURL            string                         `json:"base_url"`
			UpstreamProtocol   *string                        `json:"upstream_protocol"`
			TransportProfile   *string                        `json:"transport_profile"`
			Routes             *[]storage.CustomProviderRoute `json:"routes"`
			EgressIDs          *[]string                      `json:"egress_ids"`
			Enabled            *bool                          `json:"enabled"`
			AutoDiscoverModels *bool                          `json:"auto_discover_models"`
			Models             []string                       `json:"models"`
			ModelMappings      map[string]string              `json:"model_mappings"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		id := slugify(req.ID)
		if id == "" {
			id = slugify(req.Name)
		}
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("provider id or name required"))
			return
		}
		if id == "codex" || id == "claude" || id == "kiro" || id == "antigravity" {
			writeError(w, http.StatusBadRequest, errors.New("'codex', 'claude', 'kiro', and 'antigravity' are reserved provider ids"))
			return
		}
		existing, exists, err := s.store.GetCustomProvider(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		p := storage.CustomProvider{
			ID:                 id,
			Name:               strings.TrimSpace(req.Name),
			BaseURL:            strings.TrimSpace(req.BaseURL),
			Enabled:            true,
			AutoDiscoverModels: true,
			Models:             req.Models,
			ModelMappings:      req.ModelMappings,
			TransportProfile:   inferredProviderTransportProfile(id, req.Name),
		}
		if exists {
			p = existing
			if strings.TrimSpace(req.Name) != "" {
				p.Name = strings.TrimSpace(req.Name)
			}
			if strings.TrimSpace(req.BaseURL) != "" {
				p.BaseURL = strings.TrimSpace(req.BaseURL)
			}
			if req.Models != nil {
				p.Models = req.Models
			}
			if req.ModelMappings != nil {
				p.ModelMappings = req.ModelMappings
			}
			if req.Routes != nil {
				p.Routes = *req.Routes
			}
		} else if req.Routes != nil {
			p.Routes = *req.Routes
		}
		baseURL := p.BaseURL
		if err := validateCustomProviderBaseURL(baseURL); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.TransportProfile != nil {
			profile, ok := storage.NormalizeCustomProviderTransportProfile(*req.TransportProfile)
			if !ok {
				writeError(w, http.StatusBadRequest, errors.New("transport_profile must be generic, codex_cli, or claude_code"))
				return
			}
			p.TransportProfile = profile
		}
		if req.UpstreamProtocol != nil {
			proto, ok := storage.NormalizeCustomProviderProtocol(*req.UpstreamProtocol)
			if !ok {
				writeError(w, http.StatusBadRequest, errors.New("upstream_protocol must be chat_completions, responses, or anthropic_messages"))
				return
			}
			p.UpstreamProtocol = proto
		} else if !exists {
			p.UpstreamProtocol = defaultProtocolForTransportProfile(p.TransportProfile)
		}
		if req.EgressIDs != nil {
			if err := s.validateOrderedEgressIDs(r, *req.EgressIDs); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			p.EgressIDs = *req.EgressIDs
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		if req.AutoDiscoverModels != nil {
			p.AutoDiscoverModels = *req.AutoDiscoverModels
		}
		for index, route := range p.Routes {
			baseURL := strings.TrimSpace(route.BaseURL)
			if baseURL == "" {
				baseURL = p.BaseURL
			}
			if err := validateCustomProviderBaseURL(baseURL); err != nil {
				writeError(w, http.StatusBadRequest, errors.New("route "+strconv.Itoa(index+1)+": "+err.Error()))
				return
			}
		}
		if err := s.store.UpsertCustomProvider(r.Context(), p); err != nil {
			if errors.Is(err, storage.ErrInvalidProviderModelMapping) || errors.Is(err, storage.ErrInvalidProviderRoute) {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out, ok, err := s.store.GetCustomProvider(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("provider was not persisted"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		methodNotAllowed(w)
	}
}

func inferredProviderTransportProfile(id, name string) string {
	identity := strings.ToLower(strings.TrimSpace(id + " " + name))
	switch {
	case strings.Contains(identity, "claude-code"), strings.Contains(identity, "claude_code"):
		return storage.CustomProviderTransportClaudeCode
	case strings.Contains(identity, "codex"):
		return storage.CustomProviderTransportCodexCLI
	default:
		return storage.CustomProviderTransportGeneric
	}
}

func defaultProtocolForTransportProfile(profile string) string {
	switch profile {
	case storage.CustomProviderTransportCodexCLI:
		return storage.CustomProviderProtocolResponses
	case storage.CustomProviderTransportClaudeCode:
		return storage.CustomProviderProtocolAnthropicMessages
	default:
		return storage.CustomProviderProtocolChatCompletions
	}
}

func (s *Server) validateOrderedEgressIDs(r *http.Request, ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, err := s.store.GetEgressProfile(r.Context(), id); err != nil {
			return errors.New("egress " + id + " not found")
		}
	}
	return nil
}

// adminProviderAction handles DELETE /admin/providers/{id}.
func (s *Server) adminProviderAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	relative := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/providers/"), "/")
	parts := strings.Split(relative, "/")
	id := strings.TrimSpace(parts[0])
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "test" {
		s.adminCustomProviderModelTest(w, r, id)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.store.DeleteCustomProvider(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrTargetInUse) {
			writePoolCodeError(w, http.StatusConflict, "target_in_use", err.Error())
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "deleted": true})
}

// adminImportKey imports a custom-provider account from a bare API key.
//
//	POST /admin/accounts/import-key  {provider_id, api_key, label, group_name, egress_id?}
func (s *Server) adminImportKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ProviderID      string `json:"provider_id"`
		APIKey          string `json:"api_key"`
		Label           string `json:"label"`
		GroupName       string `json:"group_name"`
		EgressID        string `json:"egress_id"`
		PrimaryEgressID string `json:"primary_egress_id"`
		ConfirmCost     bool   `json:"confirm_cost"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	providerID := slugify(req.ProviderID)
	apiKey := strings.TrimSpace(req.APIKey)
	if providerID == "" || apiKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("provider_id and api_key are required"))
		return
	}
	if providerID == "codex" || providerID == "claude" {
		s.adminImportProviderAPIKey(w, r, providerAPIKeyImportRequest{
			ProviderID: providerID, APIKey: apiKey, Label: req.Label, GroupName: req.GroupName,
			EgressID: requestedImportEgressID(req.EgressID, req.PrimaryEgressID), ConfirmCost: req.ConfirmCost,
		})
		return
	}
	if _, ok, err := s.store.GetCustomProvider(r.Context(), providerID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown provider "+providerID))
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = providerID
	}
	parsed := authparse.ParsedAuth{AccountID: customAccountID(providerID, apiKey), OpenAIAPIKey: apiKey}
	account, err := s.saveImportedAccount(r.Context(), parsed, label, req.GroupName, "", providerID, requestedImportEgressID(req.EgressID, req.PrimaryEgressID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// customAccountID derives a stable account id from a provider id + API key, so
// re-importing the same key updates the existing account rather than duplicating it.
func customAccountID(providerID, apiKey string) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + apiKey))
	return "acc_" + hex.EncodeToString(sum[:])[:16]
}

// slugify normalizes a provider id/name to a safe lowercase slug (letters, digits,
// '-', '_'); spaces and dots become '-'. Other characters are dropped.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func validateCustomProviderBaseURL(raw string) error {
	if raw == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("base_url must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must not contain credentials, query parameters, or a fragment")
	}
	switch u.Scheme {
	case "http", "https":
		return nil
	default:
		return errors.New("base_url scheme must be http or https")
	}
}
