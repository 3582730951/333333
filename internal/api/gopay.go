package api

import (
	"errors"
	"fmt"
	"net/http"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/gopay"
	"codex-account-pool/internal/storage"
)

// adminGopay is the GoPay auto-subscribe control surface (admin-gated, default
// OFF). GET returns status + redacted settings; PATCH/POST saves settings and/or
// flips the enable flag (which starts/stops the managed Python services).
func (s *Server) adminGopay(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if s.gopay == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("gopay manager not initialized"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.gopay.Status(r.Context()))
	case http.MethodPatch, http.MethodPost:
		var req struct {
			Enabled  *bool           `json:"enabled"`
			Settings *gopay.Settings `json:"settings"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Settings != nil {
			if _, err := s.gopay.SaveSettings(r.Context(), *req.Settings); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		var warning string
		if req.Enabled != nil {
			if err := s.gopay.SetEnabled(r.Context(), *req.Enabled); err != nil {
				// The flag is persisted regardless; surface the start failure (e.g.
				// missing python3 / deps) as a warning rather than failing the call.
				warning = err.Error()
			}
		}
		st := s.gopay.Status(r.Context())
		if warning != "" {
			st["warning"] = warning
		}
		writeJSON(w, http.StatusOK, st)
	default:
		methodNotAllowed(w)
	}
}

// adminGopaySubscribe runs the auto-subscribe flow for a pooled account. The
// account's STORED session token is used unless an explicit one is provided.
func (s *Server) adminGopaySubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.gopay == nil || !s.gopay.Enabled(r.Context()) {
		writeError(w, http.StatusForbidden, errors.New("gopay disabled"))
		return
	}
	var req struct {
		AccountID    string `json:"account_id"`
		SessionToken string `json:"session_token"`
		PhoneNumber  string `json:"phone_number"`
		Pin          string `json:"pin"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token := req.SessionToken
	label := req.AccountID
	if token == "" && req.AccountID != "" {
		t, err := s.store.GetToken(r.Context(), req.AccountID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if accountprovider.UsesAPIKey("codex", t) {
			writePoolCodeError(w, http.StatusBadRequest, "subscription_operation_unavailable", "API-key accounts do not support subscription operations")
			return
		}
		token = accountprovider.Credential("codex", t)
		if acc, e := s.store.GetAccount(r.Context(), req.AccountID); e == nil {
			label = firstNonEmpty(acc.Label, acc.Email, acc.ID)
		}
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("no session token (account has no stored access token)"))
		return
	}
	result, err := s.gopay.Subscribe(r.Context(), token, req.PhoneNumber, req.Pin)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	ok, _ := result["ok"].(bool)
	state := "gopay_failed"
	if ok {
		state = "gopay_subscribed"
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID:    req.AccountID,
		AccountLabel: label,
		Action:       "gopay_subscribe",
		State:        state,
		Reason:       fmt.Sprintf("%v", result["error"]),
		Detail:       storage.MustJSON(result),
	})
	writeJSON(w, http.StatusOK, result)
}

// adminGopayOTP forwards a manual OTP to the orchestrator (manual OTP mode).
func (s *Server) adminGopayOTP(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.gopay == nil || !s.gopay.Enabled(r.Context()) {
		writeError(w, http.StatusForbidden, errors.New("gopay disabled"))
		return
	}
	var req struct {
		OTP   string `json:"otp"`
		Phone string `json:"phone"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.gopay.PushOTP(r.Context(), req.OTP, req.Phone)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
