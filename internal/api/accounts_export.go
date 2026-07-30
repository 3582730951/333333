package api

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"strings"
)

// adminAccountsExport exports the pooled accounts in several formats (the aBaiAutoplus /
// GuJumpgate export need): ?format=json (default, full), csv, at (access-token list), or
// auth (auth.json-style array for re-import). Registered as an exact route so it wins over
// the /admin/accounts/{id}/{action} subtree.
func (s *Server) adminAccountsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	if format == "backup" || format == "archive" || format == "portable" {
		s.writeAccountsBackupDownload(w, r)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	tokens, err := s.store.ListTokensByAccountIDs(r.Context(), accountIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	type rec struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		Label        string `json:"label"`
		Group        string `json:"group_name"`
		Provider     string `json:"provider"`
		Status       string `json:"status"`
		AccessToken  string `json:"access_token,omitempty"`
		RefreshToken string `json:"refresh_token,omitempty"`
		OpenAIAPIKey string `json:"openai_api_key,omitempty"`
	}
	recs := make([]rec, 0, len(accounts))
	for _, a := range accounts {
		tok := tokens[a.ID]
		recs = append(recs, rec{
			ID: a.ID, Email: a.Email, Label: a.Label, Group: a.GroupName,
			Provider: a.Provider, Status: a.Status,
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, OpenAIAPIKey: tok.OpenAIAPIKey,
		})
	}

	switch format {
	case "csv":
		var buf bytes.Buffer
		cw := csv.NewWriter(&buf)
		_ = cw.Write([]string{"id", "email", "label", "group", "provider", "status", "access_token", "refresh_token", "openai_api_key"})
		for _, r := range recs {
			_ = cw.Write([]string{r.ID, r.Email, r.Label, r.Group, r.Provider, r.Status, r.AccessToken, r.RefreshToken, r.OpenAIAPIKey})
		}
		cw.Flush()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=accounts.csv")
		_, _ = w.Write(buf.Bytes())
	case "at", "tokens":
		// Plain access-token list (one per line) — the GuJumpgate "纯 AT" style.
		var b strings.Builder
		for _, r := range recs {
			if r.AccessToken != "" {
				b.WriteString(r.AccessToken)
				b.WriteByte('\n')
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=access_tokens.txt")
		_, _ = w.Write([]byte(b.String()))
	case "auth":
		// auth.json-style array (re-importable via /admin/accounts/import-auth-json per item).
		items := make([]map[string]interface{}, 0, len(recs))
		for _, r := range recs {
			items = append(items, map[string]interface{}{
				"email":          r.Email,
				"access_token":   r.AccessToken,
				"refresh_token":  r.RefreshToken,
				"openai_api_key": r.OpenAIAPIKey,
			})
		}
		w.Header().Set("Content-Disposition", "attachment; filename=auth.json")
		writeJSON(w, http.StatusOK, items)
	default: // json
		w.Header().Set("Content-Disposition", "attachment; filename=accounts.json")
		writeJSON(w, http.StatusOK, recs)
	}
}
