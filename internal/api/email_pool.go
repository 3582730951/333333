package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// ── Email Pool Management ──────────────────────────────────────────

func (s *Server) adminEmailPool(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.adminEmailPoolList(w, r)
	case http.MethodDelete:
		s.adminEmailPoolDelete(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminEmailPoolList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))

	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}

	accounts, total, err := s.store.ListEmailAccounts(r.Context(), page, pageSize, search, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Zero out sensitive fields in list responses.
	for i := range accounts {
		accounts[i].Password = ""
		accounts[i].RefreshToken = ""
	}

	counts, _ := s.store.CountEmailAccountsByStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accounts":  accounts,
		"total":     total,
		"page":      page,
		"pageSize":  pageSize,
		"counts":    counts,
	})
}

func (s *Server) adminEmailPoolDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("ids is required"))
		return
	}
	var errs []string
	for _, id := range req.IDs {
		if err := s.store.DeleteEmailAccount(r.Context(), id); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusPartialContent, map[string]interface{}{
			"deleted": len(req.IDs) - len(errs),
			"errors":  errs,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": len(req.IDs)})
}

// adminEmailPoolImport handles POST /admin/email-pool/import
// Body is a plain text of one account per line: email----password----client_id----refresh_token
func (s *Server) adminEmailPoolImport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	// Support both JSON body {text: "..."} and raw text/plain body.
	var raw string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var req struct {
			Text      string `json:"text"`
			GroupName string `json:"group_name"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		raw = req.Text
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, adminJSONBodyLimit))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		raw = string(body)
	}

	lines := strings.Split(raw, "\n")
	var accounts []storage.EmailAccount
	var parseErrors []string
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "----", 4)
		if len(parts) < 4 {
			parseErrors = append(parseErrors, fmt.Sprintf("line %d: expected email----password----client_id----refresh_token, got %d fields", i+1, len(parts)))
			continue
		}
		email := strings.TrimSpace(parts[0])
		password := strings.TrimSpace(parts[1])
		clientID := strings.TrimSpace(parts[2])
		refreshToken := strings.TrimSpace(parts[3])
		if email == "" || refreshToken == "" {
			parseErrors = append(parseErrors, fmt.Sprintf("line %d: email and refresh_token are required", i+1))
			continue
		}
		accounts = append(accounts, storage.EmailAccount{
			Email:        email,
			Password:     password,
			ClientID:     clientID,
			RefreshToken: refreshToken,
			Status:       "idle",
		})
	}

	if len(accounts) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"imported": 0,
			"errors":   parseErrors,
		})
		return
	}

	imported, err := s.store.BulkInsertEmailAccounts(r.Context(), accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	result := map[string]interface{}{
		"imported": imported,
		"total":    len(accounts),
	}
	if len(parseErrors) > 0 {
		result["parse_errors"] = parseErrors
	}
	writeJSON(w, http.StatusOK, result)
}

// adminEmailPoolAction handles /admin/email-pool/{id}/... paths.
func (s *Server) adminEmailPoolAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	// Path: /admin/email-pool/{id}/{action}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/admin/email-pool/"), "/", 3)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("email pool action not found"))
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "test" && r.Method == http.MethodPost:
		s.adminEmailPoolTest(w, r, id)
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown email pool action: %s", action))
	}
}

// adminEmailPoolTest tests IMAP connectivity for an email account.
func (s *Server) adminEmailPoolTest(w http.ResponseWriter, r *http.Request, id string) {
	acct, found, err := s.store.GetEmailAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("email account not found"))
		return
	}

	// Try to exchange the refresh token for an IMAP access token.
	// This is a basic connectivity check — the full IMAP test can be added later.
	tokenURL := "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	form := fmt.Sprintf("client_id=%s&grant_type=refresh_token&refresh_token=%s&scope=%s",
		acct.ClientID, acct.RefreshToken, "https://outlook.office.com/.default")
	resp, err := http.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		acct.ErrorMessage = fmt.Sprintf("token exchange failed: %v", err)
		acct.Status = "error"
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":       false,
			"error":    err.Error(),
			"email":    acct.Email,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var tokenResp struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tokenResp)
		acct.ErrorMessage = ""
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":          true,
			"email":       acct.Email,
			"has_token":   tokenResp.AccessToken != "",
		})
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		errMsg := fmt.Sprintf("token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
		acct.ErrorMessage = errMsg
		acct.Status = "error"
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":       false,
			"error":    errMsg,
			"email":    acct.Email,
		})
	}
}
