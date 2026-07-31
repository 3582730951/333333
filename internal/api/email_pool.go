package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	if pageSize > 200 {
		pageSize = 200
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

	counts, err := s.store.CountEmailAccountsByStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accounts": accounts,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"counts":   counts,
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
	var raw, groupName string
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
		groupName = strings.TrimSpace(req.GroupName)
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, adminJSONBodyLimit+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if int64(len(body)) > adminJSONBodyLimit {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("email pool import exceeds request limit"))
			return
		}
		raw = string(body)
	}
	if len(groupName) > 128 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("group_name exceeds 128 bytes"))
		return
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
		if len(email) > 320 || len(password) > 4096 || len(clientID) > 1024 || len(refreshToken) > 16384 ||
			strings.Count(email, "@") != 1 {
			parseErrors = append(parseErrors, fmt.Sprintf("line %d: one or more fields are invalid or exceed their limit", i+1))
			continue
		}
		accounts = append(accounts, storage.EmailAccount{
			Email:        email,
			Password:     password,
			ClientID:     clientID,
			RefreshToken: refreshToken,
			Status:       "idle",
			GroupName:    groupName,
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

	// Exchange the refresh token using a bounded, request-scoped client. Form
	// encoding is required because OAuth credentials may contain '+', '&', or '='.
	tokenURL := "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	form := url.Values{
		"client_id":     {acct.ClientID},
		"grant_type":   {"refresh_token"},
		"refresh_token": {acct.RefreshToken},
		"scope":         {"https://outlook.office.com/.default"},
	}
	req, err := http.NewRequestWithContext(
		r.Context(), http.MethodPost, tokenURL, strings.NewReader(form.Encode()),
	)
	if err == nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
	}
	var resp *http.Response
	if err == nil {
		resp, err = (&http.Client{Timeout: 15 * time.Second}).Do(req)
	}
	if err != nil {
		acct.ErrorMessage = "token exchange connection failed"
		acct.Status = "error"
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": acct.ErrorMessage,
			"email": acct.Email,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var tokenResp struct {
			AccessToken string `json:"access_token"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&tokenResp)
		if decodeErr != nil || strings.TrimSpace(tokenResp.AccessToken) == "" {
			acct.ErrorMessage = "token exchange returned an incompatible response"
			acct.Status = "error"
			_ = s.store.UpdateEmailAccount(r.Context(), acct)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ok": false, "error": acct.ErrorMessage, "email": acct.Email,
			})
			return
		}
		acct.ErrorMessage = ""
		if acct.Status == "error" {
			acct.Status = "idle"
		}
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":        true,
			"email":     acct.Email,
			"has_token": true,
		})
	} else {
		var oauthErr struct {
			Code string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&oauthErr)
		code := strings.TrimSpace(oauthErr.Code)
		if !oauthErrorCodePattern.MatchString(code) {
			code = "upstream_rejected"
		}
		errMsg := fmt.Sprintf("token exchange rejected: HTTP %d (%s)", resp.StatusCode, code)
		acct.ErrorMessage = errMsg
		acct.Status = "error"
		_ = s.store.UpdateEmailAccount(r.Context(), acct)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": errMsg,
			"email": acct.Email,
		})
	}
}

var oauthErrorCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
