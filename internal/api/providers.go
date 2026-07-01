package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
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
//	POST /admin/providers  {id,name,base_url,enabled,auto_discover_models,models:[...]}
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
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			BaseURL            string   `json:"base_url"`
			Enabled            *bool    `json:"enabled"`
			AutoDiscoverModels *bool    `json:"auto_discover_models"`
			Models             []string `json:"models"`
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
		if id == "codex" || id == "claude" {
			writeError(w, http.StatusBadRequest, errors.New("'codex' and 'claude' are reserved provider ids"))
			return
		}
		baseURL := strings.TrimSpace(req.BaseURL)
		if err := validateCustomProviderBaseURL(baseURL); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		p := storage.CustomProvider{
			ID:                 id,
			Name:               strings.TrimSpace(req.Name),
			BaseURL:            baseURL,
			Enabled:            true,
			AutoDiscoverModels: true,
			Models:             req.Models,
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		if req.AutoDiscoverModels != nil {
			p.AutoDiscoverModels = *req.AutoDiscoverModels
		}
		if err := s.store.UpsertCustomProvider(r.Context(), p); err != nil {
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

// adminProviderAction handles DELETE /admin/providers/{id}.
func (s *Server) adminProviderAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/providers/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.store.DeleteCustomProvider(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "deleted": true})
}

// adminImportKey imports a custom-provider account from a bare API key.
//
//	POST /admin/accounts/import-key  {provider_id, api_key, label, group_name}
func (s *Server) adminImportKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ProviderID string `json:"provider_id"`
		APIKey     string `json:"api_key"`
		Label      string `json:"label"`
		GroupName  string `json:"group_name"`
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
	account, err := s.saveImportedAccount(r.Context(), parsed, label, req.GroupName, "", providerID)
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
	switch u.Scheme {
	case "http", "https":
		return nil
	default:
		return errors.New("base_url scheme must be http or https")
	}
}
