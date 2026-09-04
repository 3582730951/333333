package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
)

const (
	strictExportSub2API  = "sub2api-v1"
	strictExportCodex    = "codex-auth"
	strictExportCLIProxy = "cliproxyapi"
	strictExportMaxIDs   = 500
	strictExportNonceTTL = 5 * time.Minute
)

type strictAccountExportRequest struct {
	AccountIDs         []string `json:"account_ids"`
	Format             string   `json:"format"`
	IncludeProxies     bool     `json:"include_proxies"`
	IncompatiblePolicy string   `json:"incompatible_policy"`
	ConfirmationNonce  string   `json:"confirmation_nonce"`
}

type strictAccountExportItem struct {
	AccountID       string   `json:"account_id"`
	AccountCode     string   `json:"account_code"`
	Compatible      bool     `json:"compatible"`
	Errors          []string `json:"errors"`
	Warnings        []string `json:"warnings"`
	PlannedFilename string   `json:"planned_filename"`
	SecretTypes     []string `json:"secret_types"`
}

type strictExportCandidate struct {
	ID     string
	Found  bool
	Record accountExportRecord
	Item   strictAccountExportItem
}

type sub2APIProxyExportPlan struct {
	AccountProxyKeys map[string]*string
	Proxies          []sub2APIDataProxy
	Warnings         map[string][]string
	SecretTypes      map[string][]string
}

type sub2APIDataPayload struct {
	Type       string               `json:"type"`
	Version    int                  `json:"version"`
	ExportedAt string               `json:"exported_at"`
	Proxies    []sub2APIDataProxy   `json:"proxies"`
	Accounts   []sub2APIDataAccount `json:"accounts"`
}

type sub2APIDataProxy struct {
	ProxyKey string `json:"proxy_key"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Status   string `json:"status"`
}

type sub2APIDataAccount struct {
	Name               string                 `json:"name"`
	Platform           string                 `json:"platform"`
	Type               string                 `json:"type"`
	Credentials        map[string]interface{} `json:"credentials"`
	Extra              map[string]interface{} `json:"extra,omitempty"`
	ProxyKey           *string                `json:"proxy_key,omitempty"`
	Concurrency        int                    `json:"concurrency"`
	Priority           int                    `json:"priority"`
	ExpiresAt          *int64                 `json:"expires_at,omitempty"`
	AutoPauseOnExpired *bool                  `json:"auto_pause_on_expired,omitempty"`
}

type strictExportNoncePayload struct {
	Version            int      `json:"v"`
	ExpiresAt          int64    `json:"exp"`
	Actor              string   `json:"actor"`
	AccountIDs         []string `json:"account_ids"`
	Format             string   `json:"format"`
	IncludeProxies     bool     `json:"include_proxies"`
	IncompatiblePolicy string   `json:"incompatible_policy"`
}

func normalizeStrictAccountExportRequest(req strictAccountExportRequest) (strictAccountExportRequest, error) {
	switch strings.TrimSpace(req.Format) {
	case strictExportSub2API, strictExportCodex, strictExportCLIProxy:
		req.Format = strings.TrimSpace(req.Format)
	default:
		return req, errors.New("format must be sub2api-v1, codex-auth, or cliproxyapi")
	}
	if req.IncompatiblePolicy == "" {
		req.IncompatiblePolicy = "fail_all"
	}
	if req.IncompatiblePolicy != "fail_all" && req.IncompatiblePolicy != "skip_with_report" {
		return req, errors.New("incompatible_policy must be fail_all or skip_with_report")
	}
	seen := make(map[string]struct{}, len(req.AccountIDs))
	ids := make([]string, 0, len(req.AccountIDs))
	for _, raw := range req.AccountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(ids) >= strictExportMaxIDs {
			return req, fmt.Errorf("account_ids supports at most %d unique values", strictExportMaxIDs)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return req, errors.New("account_ids must be a non-empty array")
	}
	req.AccountIDs = ids
	return req, nil
}

func strictExportActor(s *Server, r *http.Request) string {
	if user, ok := s.currentUser(r); ok {
		return "user:" + user.ID
	}
	return "admin_token"
}

func (s *Server) issueStrictExportNonce(r *http.Request, req strictAccountExportRequest, expiresAt int64) (string, error) {
	if len(s.identitySecretCached) < 16 {
		return "", errors.New("export confirmation signer unavailable")
	}
	payload := strictExportNoncePayload{Version: 1, ExpiresAt: expiresAt, Actor: strictExportActor(s, r),
		AccountIDs: req.AccountIDs, Format: req.Format, IncludeProxies: req.IncludeProxies, IncompatiblePolicy: req.IncompatiblePolicy}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.identitySecretCached)
	_, _ = mac.Write([]byte("account-export-confirmation-v1\x00"))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) validateStrictExportNonce(r *http.Request, req strictAccountExportRequest) error {
	parts := strings.Split(req.ConfirmationNonce, ".")
	if len(parts) != 2 || len(s.identitySecretCached) < 16 {
		return errors.New("valid confirmation_nonce is required")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid confirmation_nonce")
	}
	mac := hmac.New(sha256.New, s.identitySecretCached)
	_, _ = mac.Write([]byte("account-export-confirmation-v1\x00"))
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return errors.New("invalid confirmation_nonce")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(raw) > 32<<10 {
		return errors.New("invalid confirmation_nonce")
	}
	var payload strictExportNoncePayload
	if json.Unmarshal(raw, &payload) != nil || payload.Version != 1 || payload.ExpiresAt < time.Now().Unix() {
		return errors.New("confirmation_nonce is invalid or expired")
	}
	want := strictExportNoncePayload{Version: 1, ExpiresAt: payload.ExpiresAt, Actor: strictExportActor(s, r),
		AccountIDs: req.AccountIDs, Format: req.Format, IncludeProxies: req.IncludeProxies, IncompatiblePolicy: req.IncompatiblePolicy}
	payloadRaw, _ := json.Marshal(payload)
	wantRaw, _ := json.Marshal(want)
	if !hmac.Equal(payloadRaw, wantRaw) {
		return errors.New("confirmation_nonce does not match this export")
	}
	return nil
}

func accountExportCode(id string) string {
	digest := sha256.Sum256([]byte("account-export-v1\x00" + strings.TrimSpace(id)))
	return "ACC-" + strings.ToUpper(hex.EncodeToString(digest[:6]))
}

func syntheticCodexIDToken(raw string) bool {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return false
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header map[string]interface{}
	if json.Unmarshal(headerRaw, &header) != nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(header["alg"])), "none") {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	return err == nil && string(signature) == "external-chatgpt-session"
}

func (s *Server) strictExportCandidates(ctx context.Context, req strictAccountExportRequest) ([]strictExportCandidate, error) {
	accounts, err := s.store.ListAccountsByIDs(ctx, req.AccountIDs)
	if err != nil {
		return nil, err
	}
	tokens, err := s.store.ListTokensByAccountIDs(ctx, req.AccountIDs)
	if err != nil {
		return nil, err
	}
	candidates := make([]strictExportCandidate, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		candidate := strictExportCandidate{ID: id, Item: strictAccountExportItem{
			AccountID: id, AccountCode: accountExportCode(id), Errors: []string{}, Warnings: []string{}, SecretTypes: []string{},
		}}
		account, found := accounts[id]
		if !found {
			candidate.Item.Errors = append(candidate.Item.Errors, "account not found")
			candidates = append(candidates, candidate)
			continue
		}
		candidate.Found = true
		candidate.Record.Account = account
		token, hasToken := tokens[id]
		if !hasToken {
			candidate.Item.Errors = append(candidate.Item.Errors, "credential not found")
			candidates = append(candidates, candidate)
			continue
		}
		candidate.Record.Token = token
		provider := accountprovider.EffectiveProvider(account.Provider, token, true)
		if provider != "codex" {
			candidate.Item.Errors = append(candidate.Item.Errors, fmt.Sprintf("provider %q is not supported by this exact adapter", provider))
			candidates = append(candidates, candidate)
			continue
		}
		switch req.Format {
		case strictExportCodex:
			_, err = officialCodexAuthDocument(candidate.Record)
			candidate.Item.PlannedFilename = "codex-" + strings.ToLower(candidate.Item.AccountCode) + "-auth.json"
		case strictExportCLIProxy:
			_, err = cliProxyDocument(candidate.Record)
			candidate.Item.PlannedFilename = "codex-" + strings.ToLower(candidate.Item.AccountCode) + ".json"
		case strictExportSub2API:
			_, candidate.Item.Warnings, err = sub2APIAccountDocument(candidate.Record, nil)
			candidate.Item.PlannedFilename = "sub2api-data-v1.json"
		}
		if err != nil {
			candidate.Item.Errors = append(candidate.Item.Errors, err.Error())
		}
		if accountprovider.UsesAPIKey("codex", token) {
			candidate.Item.SecretTypes = append(candidate.Item.SecretTypes, "api_key")
		} else {
			candidate.Item.SecretTypes = append(candidate.Item.SecretTypes, "access_token")
			if token.RefreshToken != "" {
				candidate.Item.SecretTypes = append(candidate.Item.SecretTypes, "refresh_token")
			}
			if token.IDTokenRaw != "" && !syntheticCodexIDToken(token.IDTokenRaw) {
				candidate.Item.SecretTypes = append(candidate.Item.SecretTypes, "id_token")
			}
		}
		candidate.Item.Compatible = len(candidate.Item.Errors) == 0
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func sub2APIAccountDocument(record accountExportRecord, proxyKey *string) (sub2APIDataAccount, []string, error) {
	warnings := []string{}
	name := strings.TrimSpace(firstNonEmpty(record.Account.Label, record.Account.Email, accountExportCode(record.Account.ID)))
	doc := sub2APIDataAccount{Name: name, Platform: "openai", Concurrency: 1, Priority: 50, ProxyKey: proxyKey,
		Credentials: map[string]interface{}{}, Extra: map[string]interface{}{"micliproxy": map[string]interface{}{
			"source_account_code": accountExportCode(record.Account.ID), "group_name": record.Account.GroupName,
			"status": record.Account.Status, "plan_type": record.Account.PlanType, "routing_weight": record.Account.RoutingWeight,
		}}}
	if accountprovider.UsesAPIKey("codex", record.Token) {
		key := accountprovider.Credential("codex", record.Token)
		if strings.TrimSpace(key) == "" {
			return doc, warnings, errors.New("Sub2API API-key export requires api_key")
		}
		// Sub2API's DataAccount validator uses the exact wire enum `apikey`
		// (without an underscore).  `api_key` is our internal auth method and
		// would make an otherwise valid export fail validation on the target.
		doc.Type = "apikey"
		doc.Credentials["api_key"] = key
		return doc, warnings, nil
	}
	if strings.TrimSpace(record.Token.AccessToken) == "" {
		return doc, warnings, errors.New("Sub2API OAuth export requires access_token")
	}
	doc.Type = "oauth"
	doc.Credentials["access_token"] = record.Token.AccessToken
	if record.Token.RefreshToken != "" {
		doc.Credentials["refresh_token"] = record.Token.RefreshToken
	} else {
		warnings = append(warnings, "refresh_token is absent; imported account cannot refresh through OAuth")
	}
	if record.Token.IDTokenRaw != "" {
		if syntheticCodexIDToken(record.Token.IDTokenRaw) {
			warnings = append(warnings, "local synthetic id_token was omitted")
		} else {
			doc.Credentials["id_token"] = record.Token.IDTokenRaw
		}
	}
	if record.Account.UpstreamAccountID != "" {
		doc.Credentials["chatgpt_account_id"] = record.Account.UpstreamAccountID
	} else {
		warnings = append(warnings, "chatgpt_account_id is absent and must be derivable from the access token")
	}
	if record.Account.Email != "" {
		doc.Credentials["email"] = record.Account.Email
	}
	if record.Account.PlanType != "" {
		doc.Credentials["plan_type"] = record.Account.PlanType
	}
	if record.Token.ExpiresAt > 0 {
		expires := record.Token.ExpiresAt
		doc.ExpiresAt = &expires
		autoPause := record.Token.RefreshToken == ""
		doc.AutoPauseOnExpired = &autoPause
	}
	return doc, warnings, nil
}

func (s *Server) handleStrictAccountExportPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req strictAccountExportRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req, err := normalizeStrictAccountExportRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	candidates, err := s.strictExportCandidates(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var proxyPlan sub2APIProxyExportPlan
	if req.IncludeProxies && req.Format == strictExportSub2API {
		proxyPlan, err = s.planSub2APIExportProxies(r.Context(), req.AccountIDs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	compatible := 0
	items := make([]strictAccountExportItem, 0, len(candidates))
	for _, candidate := range candidates {
		if req.IncludeProxies && req.Format != strictExportSub2API {
			candidate.Item.Warnings = append(candidate.Item.Warnings, "include_proxies applies only to sub2api-v1")
		}
		if req.IncludeProxies && req.Format == strictExportSub2API {
			candidate.Item.Warnings = append(candidate.Item.Warnings, proxyPlan.Warnings[candidate.ID]...)
			candidate.Item.SecretTypes = append(candidate.Item.SecretTypes, proxyPlan.SecretTypes[candidate.ID]...)
		}
		sort.Strings(candidate.Item.Warnings)
		sort.Strings(candidate.Item.SecretTypes)
		if candidate.Item.Compatible {
			compatible++
		}
		items = append(items, candidate.Item)
	}
	expiresAt := time.Now().Add(strictExportNonceTTL).Unix()
	nonce, err := s.issueStrictExportNonce(r, req, expiresAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	nonceDigest := sha256.Sum256([]byte(nonce))
	if err = s.store.ProvisionAccountExportConfirmation(r.Context(), hex.EncodeToString(nonceDigest[:]), expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"format": req.Format, "compatible": compatible, "incompatible": len(items) - compatible,
		"items": items, "confirmation_nonce": nonce, "confirmation_expires_at": expiresAt,
		"fixture_status": map[string]string{"sub2api_v1": "verified_against_pinned_open_source_parser", "private_connectors": "blocked_until_three_real_fixtures"},
	})
}

func (s *Server) handleStrictAccountExport(w http.ResponseWriter, r *http.Request) {
	var req strictAccountExportRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req, err := normalizeStrictAccountExportRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err = s.validateStrictExportNonce(r, req); err != nil {
		s.auditStrictAccountExport(r, req, "denied", err.Error(), 0, len(req.AccountIDs))
		writeError(w, http.StatusForbidden, err)
		return
	}
	nonceDigest := sha256.Sum256([]byte(req.ConfirmationNonce))
	if err = s.store.ConsumeAccountExportConfirmation(r.Context(), hex.EncodeToString(nonceDigest[:]), time.Now().Unix()); err != nil {
		s.auditStrictAccountExport(r, req, "denied", "confirmation_reused_or_expired", 0, len(req.AccountIDs))
		writeError(w, http.StatusForbidden, errors.New("confirmation_nonce is invalid, expired, or already used"))
		return
	}
	candidates, err := s.strictExportCandidates(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	compatible := make([]strictExportCandidate, 0, len(candidates))
	skipped := make([]string, 0)
	items := make([]strictAccountExportItem, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, candidate.Item)
		if candidate.Item.Compatible {
			compatible = append(compatible, candidate)
		} else {
			skipped = append(skipped, candidate.Item.AccountCode)
		}
	}
	if len(skipped) > 0 && req.IncompatiblePolicy == "fail_all" {
		s.auditStrictAccountExport(r, req, "denied", "incompatible_accounts", 0, len(skipped))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{"error": "one or more accounts are incompatible", "items": items})
		return
	}
	if len(compatible) == 0 {
		writeError(w, http.StatusUnprocessableEntity, errors.New("no compatible accounts to export"))
		return
	}
	if err = s.writeStrictAccountExport(w, r.Context(), req, compatible, skipped); err != nil {
		s.auditStrictAccountExport(r, req, "failed", err.Error(), 0, len(skipped))
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.auditStrictAccountExport(r, req, "success", "confirmed", len(compatible), len(skipped))
}

type strictRenderedFile struct {
	Name string
	Body []byte
}

func (s *Server) writeStrictAccountExport(w http.ResponseWriter, ctx context.Context, req strictAccountExportRequest, candidates []strictExportCandidate, skipped []string) error {
	exportedAt := time.Now().UTC()
	files := make([]strictRenderedFile, 0, len(candidates))
	if req.Format == strictExportSub2API {
		proxyKeys := map[string]*string{}
		proxies := []sub2APIDataProxy{}
		if req.IncludeProxies {
			plan, err := s.planSub2APIExportProxies(ctx, req.AccountIDs)
			if err != nil {
				return err
			}
			proxyKeys, proxies = plan.AccountProxyKeys, plan.Proxies
		}
		accounts := make([]sub2APIDataAccount, 0, len(candidates))
		for _, candidate := range candidates {
			doc, _, err := sub2APIAccountDocument(candidate.Record, proxyKeys[candidate.ID])
			if err != nil {
				return err
			}
			accounts = append(accounts, doc)
		}
		payload := sub2APIDataPayload{Type: "sub2api-data", Version: 1, ExportedAt: exportedAt.Format(time.RFC3339), Proxies: proxies, Accounts: accounts}
		body, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		files = append(files, strictRenderedFile{Name: "sub2api-data-v1.json", Body: append(body, '\n')})
	} else {
		for _, candidate := range candidates {
			var document interface{}
			var err error
			if req.Format == strictExportCodex {
				document, err = officialCodexAuthDocument(candidate.Record)
			} else {
				document, err = cliProxyDocument(candidate.Record)
			}
			if err != nil {
				return err
			}
			body, err := json.MarshalIndent(document, "", "  ")
			if err != nil {
				return err
			}
			name := candidate.Item.PlannedFilename
			if req.Format == strictExportCodex && len(candidates) == 1 {
				name = "auth.json"
			}
			files = append(files, strictRenderedFile{Name: name, Body: append(body, '\n')})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accounts-Exported", strconv.Itoa(len(candidates)))
	w.Header().Set("X-Accounts-Skipped", strconv.Itoa(len(skipped)))
	if len(files) == 1 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(files[0].Name))
		w.Header().Set("Content-Length", strconv.Itoa(len(files[0].Body)))
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(files[0].Body)
		return err
	}
	archive, err := strictExportZIP(req.Format, exportedAt, files, skipped)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(req.Format+"-accounts.zip"))
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(archive)
	return err
}

func strictExportZIP(format string, exportedAt time.Time, files []strictRenderedFile, skipped []string) ([]byte, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	type manifestFile struct {
		Filename string `json:"filename"`
		Size     int    `json:"size"`
		SHA256   string `json:"sha256"`
	}
	manifestFiles := make([]manifestFile, 0, len(files))
	for _, file := range files {
		digest := sha256.Sum256(file.Body)
		manifestFiles = append(manifestFiles, manifestFile{Filename: file.Name, Size: len(file.Body), SHA256: hex.EncodeToString(digest[:])})
	}
	manifest, err := json.MarshalIndent(map[string]interface{}{
		"format": format, "format_version": 1, "exported_at": exportedAt.Format(time.RFC3339),
		"account_count": len(files), "skipped_account_codes": skipped, "files": manifestFiles,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	files = append(files, strictRenderedFile{Name: "manifest.json", Body: append(manifest, '\n')})
	var archive bytes.Buffer
	bounded := &accountArchiveBoundedWriter{dst: &archive, remaining: accountArchiveMaxUploadBytes, err: errGeneratedAccountArchiveTooLarge}
	zw := zip.NewWriter(bounded)
	fixedTime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.Name, Method: zip.Deflate}
		header.SetModTime(fixedTime)
		header.SetMode(0o600)
		entry, createErr := zw.CreateHeader(header)
		if createErr == nil {
			_, createErr = entry.Write(file.Body)
		}
		if createErr != nil {
			_ = zw.Close()
			return nil, createErr
		}
	}
	if err = zw.Close(); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

func (s *Server) planSub2APIExportProxies(ctx context.Context, accountIDs []string) (sub2APIProxyExportPlan, error) {
	plan := sub2APIProxyExportPlan{
		AccountProxyKeys: make(map[string]*string, len(accountIDs)),
		Proxies:          []sub2APIDataProxy{},
		Warnings:         make(map[string][]string, len(accountIDs)),
		SecretTypes:      make(map[string][]string, len(accountIDs)),
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return plan, err
	}
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		return plan, err
	}
	profileByID := make(map[string]storage.EgressProfile, len(profiles))
	for _, profile := range profiles {
		profileByID[profile.ID] = profile
	}
	byKey := make(map[string]sub2APIDataProxy)
	for _, accountID := range accountIDs {
		binding, ok := bindings[accountID]
		if !ok || strings.TrimSpace(binding.PrimaryEgressID) == "" {
			continue
		}
		profile, ok := profileByID[binding.PrimaryEgressID]
		if !ok {
			plan.Warnings[accountID] = append(plan.Warnings[accountID], "bound egress profile is missing; proxy will be omitted")
			continue
		}
		if strings.EqualFold(strings.TrimSpace(profile.Type), "direct") {
			continue
		}
		proxy, conversionErr := sub2APIProxyFromEgress(profile)
		if conversionErr != nil {
			plan.Warnings[accountID] = append(plan.Warnings[accountID], conversionErr.Error()+"; proxy will be omitted")
			continue
		}
		byKey[proxy.ProxyKey] = proxy
		key := proxy.ProxyKey
		plan.AccountProxyKeys[accountID] = &key
		if proxy.Username != "" || proxy.Password != "" {
			plan.SecretTypes[accountID] = append(plan.SecretTypes[accountID], "proxy_credentials")
		}
	}
	plan.Proxies = make([]sub2APIDataProxy, 0, len(byKey))
	for _, proxy := range byKey {
		plan.Proxies = append(plan.Proxies, proxy)
	}
	sort.Slice(plan.Proxies, func(i, j int) bool { return plan.Proxies[i].ProxyKey < plan.Proxies[j].ProxyKey })
	return plan, nil
}

func sub2APIProxyFromEgress(profile storage.EgressProfile) (sub2APIDataProxy, error) {
	protocol := ""
	switch strings.ToLower(strings.TrimSpace(profile.Type)) {
	case "http_proxy":
		protocol = "http"
	case "https_proxy":
		protocol = "https"
	case "socks5_proxy":
		protocol = "socks5"
	case "socks5h_proxy":
		protocol = "socks5h"
	default:
		return sub2APIDataProxy{}, fmt.Errorf("egress type %q is not representable in Sub2API data v1", strings.TrimSpace(profile.Type))
	}
	raw := strings.TrimSpace(profile.Endpoint)
	if !strings.Contains(raw, "://") {
		raw = protocol + "://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return sub2APIDataProxy{}, errors.New("egress endpoint is not a valid proxy URL")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return sub2APIDataProxy{}, errors.New("egress endpoint has an invalid proxy port")
	}
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	keyDigest := sha256.Sum256([]byte("sub2api-proxy-key-v1\x00" + strings.TrimSpace(profile.ID)))
	key := "proxy-" + hex.EncodeToString(keyDigest[:8])
	status := "active"
	if strings.EqualFold(strings.TrimSpace(profile.Health), "disabled") || strings.EqualFold(strings.TrimSpace(profile.Health), "unhealthy") {
		status = "inactive"
	}
	return sub2APIDataProxy{ProxyKey: key, Name: firstNonEmpty(profile.Name, profile.ID, "micliproxy-egress"), Protocol: protocol,
		Host: parsed.Hostname(), Port: port, Username: username, Password: password, Status: status}, nil
}

// auditStrictAccountExport records the confirmed export path. The actor is the same
// value already bound into the confirmation nonce's HMAC, so a row that omitted it
// left the audit trail unable to answer "who" about a decision the nonce had already
// attributed.
func (s *Server) auditStrictAccountExport(r *http.Request, req strictAccountExportRequest, state, reason string, exported, skipped int) {
	ids := append([]string(nil), req.AccountIDs...)
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	s.enqueueAudit(storage.AuditLogRow{Action: "account_credentials_export", State: state, Reason: reason,
		Detail: fmt.Sprintf("actor=%s method=%s format=%s accounts_hash=%s requested=%d exported=%d skipped=%d include_proxies=%t",
			strictExportActor(s, r), http.MethodPost, req.Format,
			hex.EncodeToString(digest[:8]), len(ids), exported, skipped, req.IncludeProxies), CreatedAt: storage.Now()})
}
