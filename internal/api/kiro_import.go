package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/storage"
)

type kiroImportItem struct {
	AuthMethod, RefreshToken, AccessToken, ClientID, ClientSecret, ClientIDHash, ProfileARN, AuthRegion, APIRegion, MachineID, APIKey, Endpoint, Email, Plan string
	ExpiresAt                                                                                                                                                int64
	ProxyIgnored                                                                                                                                             bool
	ParseError                                                                                                                                               string
}

type kiroClientRegistration struct {
	ClientIDHash string
	ClientID     string
	ClientSecret string
}
type kiroImportResult struct {
	Index      int      `json:"index"`
	Status     string   `json:"status"`
	AccountID  string   `json:"account_id,omitempty"`
	Label      string   `json:"label,omitempty"`
	AuthMethod string   `json:"auth_method,omitempty"`
	Error      string   `json:"error,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

const (
	kiroImportValidationPending = "kiro_import_validation_pending"
	kiroImportValidationFailed  = "kiro_import_validation_failed"
)

func (s *Server) adminImportKiroJSON(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		KiroJSONText       string `json:"kiro_json_text"`
		KiroClientJSONText string `json:"kiro_client_json_text"`
		Label              string `json:"label"`
		GroupName          string `json:"group_name"`
		EgressID           string `json:"egress_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	items, err := parseKiroImportJSON([]byte(req.KiroJSONText))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.KiroClientJSONText) != "" {
		if err := mergeKiroClientRegistrationJSON(items, []byte(req.KiroClientJSONText)); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Kiro client registration JSON: %w", err))
			return
		}
	}
	if req.GroupName == "" {
		req.GroupName = s.cfg.DefaultGroup
	}
	results := make([]kiroImportResult, 0, len(items))
	counts := map[string]int{"imported": 0, "duplicate": 0, "failed": 0}
	for i, item := range items {
		res := kiroImportResult{Index: i, AuthMethod: item.AuthMethod}
		if item.ProxyIgnored {
			res.Warnings = append(res.Warnings, "JSON 中的代理字段已忽略；仅使用导入请求选择的 egress_id")
		}
		if item.ParseError != "" {
			res.Status = "failed"
			res.Error = item.ParseError
			counts[res.Status]++
			results = append(results, res)
			continue
		}
		hash := kiroCredentialHash(item)
		exists, e := s.store.KiroCredentialHashExists(r.Context(), hash)
		if e != nil {
			res.Status = "failed"
			res.Error = e.Error()
		} else if exists {
			res.Status = "duplicate"
		} else {
			accountID := "kiro-" + hash[:24]
			label := strings.TrimSpace(req.Label)
			if label == "" {
				label = firstNonEmpty(item.Email, "Kiro "+hash[:8])
			} else if len(items) > 1 {
				label = fmt.Sprintf("%s #%d", label, i+1)
			}
			res.Label = label
			res.AccountID = accountID
			account := storage.Account{ID: accountID, Label: label, GroupName: req.GroupName, Email: item.Email, PlanType: item.Plan, Provider: "kiro", Status: "active"}
			token := storage.AccountToken{AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, ExpiresAt: item.ExpiresAt, LastRefresh: storage.Now()}
			cred := storage.KiroCredentials{AccountID: accountID, AuthMethod: item.AuthMethod, ClientID: item.ClientID, ClientSecret: item.ClientSecret, ProfileARN: item.ProfileARN, AuthRegion: firstNonEmpty(item.AuthRegion, s.cfg.KiroDefaultAuthRegion, "us-east-1"), APIRegion: firstNonEmpty(item.APIRegion, s.cfg.KiroDefaultAPIRegion, "us-east-1"), MachineID: item.MachineID, KiroAPIKey: item.APIKey, Endpoint: item.Endpoint, CredentialHash: hash}
			if e = s.importAndValidateKiro(r.Context(), account, token, cred, req.EgressID); e != nil {
				_ = s.store.DeleteAccount(r.Context(), accountID)
				res.Status = "failed"
				res.Error = safeKiroImportError(e, item)
			} else {
				res.Status = "imported"
				s.scheduler.InvalidateAccountCache()
			}
		}
		counts[res.Status]++
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"recognized": len(items), "imported": counts["imported"], "duplicate": counts["duplicate"], "failed": counts["failed"], "results": results})
}

func (s *Server) adminImportKiroAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		KiroAPIKey string `json:"kiro_api_key"`
		Label      string `json:"label"`
		GroupName  string `json:"group_name"`
		EgressID   string `json:"egress_id"`
		APIRegion  string `json:"api_region"`
		AsyncProbe bool   `json:"async_probe"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.KiroAPIKey = strings.TrimSpace(req.KiroAPIKey)
	if !strings.HasPrefix(req.KiroAPIKey, "ksk_") || len(req.KiroAPIKey) <= len("ksk_") {
		writeError(w, http.StatusBadRequest, errors.New("kiro_api_key must start with ksk_"))
		return
	}
	kiroCfg := s.effectiveKiroConfig(r.Context())
	region := strings.ToLower(strings.TrimSpace(req.APIRegion))
	if region == "" {
		region = firstNonEmpty(kiroCfg.KiroDefaultAPIRegion, "us-east-1")
	}
	if _, err := kirowire.ValidateEndpoint("", region, nil); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid api_region"))
		return
	}
	if req.EgressID != "" {
		if _, err := s.store.GetEgressProfile(r.Context(), req.EgressID); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("unknown egress_id"))
			return
		}
	}
	item := kiroImportItem{AuthMethod: "api_key", APIKey: req.KiroAPIKey, APIRegion: region}
	hash := kiroCredentialHash(item)
	if exists, err := s.store.KiroCredentialHashExists(r.Context(), hash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if exists {
		writeError(w, http.StatusConflict, errors.New("Kiro API Key already imported"))
		return
	}
	group := strings.TrimSpace(req.GroupName)
	if group == "" {
		group = s.cfg.DefaultGroup
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Kiro API Key " + hash[:8]
	}
	accountID := "kiro-" + hash[:24]
	account := storage.Account{ID: accountID, Label: label, GroupName: group, Provider: "kiro", Status: "active"}
	credential := storage.KiroCredentials{
		AccountID: accountID, AuthMethod: "api_key", APIRegion: region,
		AuthRegion: firstNonEmpty(kiroCfg.KiroDefaultAuthRegion, "us-east-1"),
		KiroAPIKey: req.KiroAPIKey, CredentialHash: hash,
	}
	if req.AsyncProbe {
		account.QuarantineUntil = kiroSuspensionQuarantineUntil
		account.QuarantineReason = kiroImportValidationPending
		if err := s.persistKiroAPIKeyPending(r.Context(), account, credential, req.EgressID); err != nil {
			_ = s.store.DeleteAccount(r.Context(), accountID)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		timeout := 3 * s.cfg.RequestTimeout()
		if timeout <= 0 {
			timeout = 3 * time.Minute
		}
		launched := s.launchRuntimeTask("kiro-api-key-import-validation", timeout, func(ctx context.Context) {
			validationAccount := account
			if err := s.importAndValidateKiro(ctx, validationAccount, storage.AccountToken{}, credential, req.EgressID); err != nil {
				_ = s.store.SetAccountQuarantine(ctx, accountID, kiroSuspensionQuarantineUntil, kiroImportValidationFailed)
				_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
					AccountID: accountID, AccountLabel: label, Action: "kiro_api_key_import", State: "quarantined", Reason: "background_validation_failed",
				})
			} else {
				_ = s.store.SetAccountQuarantine(ctx, accountID, 0, "")
				_ = s.store.ClearBindingRecheck(ctx, accountID)
				_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
					AccountID: accountID, AccountLabel: label, Action: "kiro_api_key_import", State: "active", Reason: "background_validation_succeeded",
				})
			}
			if s.scheduler != nil {
				s.scheduler.InvalidateAccountCache()
			}
		})
		if !launched {
			account.QuarantineReason = kiroImportValidationFailed
			_ = s.store.SetAccountQuarantine(r.Context(), accountID, account.QuarantineUntil, account.QuarantineReason)
		}
		if s.scheduler != nil {
			s.scheduler.InvalidateAccountCache()
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id": accountID, "label": label, "group_name": group,
			"provider": "kiro", "auth_method": "api_key", "api_key_present": true, "billing_mode": "pay_as_you_go", "api_region": region,
			"validation_pending": launched, "quarantine_until": account.QuarantineUntil, "quarantine_reason": account.QuarantineReason,
		})
		return
	}
	if err := s.importAndValidateKiro(r.Context(), account, storage.AccountToken{}, credential, req.EgressID); err != nil {
		_ = s.store.DeleteAccount(r.Context(), accountID)
		writeError(w, http.StatusBadRequest, errors.New(safeKiroImportError(err, item)))
		return
	}
	s.scheduler.InvalidateAccountCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": accountID, "label": label, "group_name": group,
		"provider": "kiro", "auth_method": "api_key", "api_region": region,
	})
}

func (s *Server) persistKiroAPIKeyPending(ctx context.Context, account storage.Account, credential storage.KiroCredentials, egressID string) error {
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		return err
	}
	if err := s.store.UpsertKiroCredentials(ctx, credential); err != nil {
		return err
	}
	effectiveEgress := strings.TrimSpace(egressID)
	if effectiveEgress == "" {
		effectiveEgress = s.resolveKiroDefaultEgress(ctx, account.ID)
	}
	if err := s.bindImportedAccountPrimaryEgress(ctx, account.ID, effectiveEgress); err != nil {
		return err
	}
	return s.store.UpsertCapabilities(ctx, capability.StaticKiroModels(account.ID))
}

func safeKiroImportError(err error, item kiroImportItem) string {
	message := err.Error()
	for _, secret := range []string{item.RefreshToken, item.AccessToken, item.ClientSecret, item.APIKey} {
		if strings.TrimSpace(secret) != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func (s *Server) importAndValidateKiro(ctx context.Context, account storage.Account, token storage.AccountToken, cred storage.KiroCredentials, egressID string) error {
	kiroCfg := s.effectiveKiroConfig(ctx)
	s.kiro.UpdateConfig(kiroCfg)
	endpointHash, err := kirowire.EndpointHash(cred.Endpoint, firstNonEmpty(cred.APIRegion, kiroCfg.KiroDefaultAPIRegion, "us-east-1"), kiroCfg.KiroEndpointAllowlist)
	if err != nil {
		return err
	}
	if err := s.store.UpsertAccount(ctx, account, token); err != nil {
		return err
	}
	if err := s.store.UpsertKiroCredentials(ctx, cred); err != nil {
		return err
	}
	// When the operator did not pin an egress, default a Kiro account to a stealth
	// egress (healthy sidecar/WARP) instead of the shared host IP + Go TLS
	// fingerprint that trips Kiro anti-abuse. Falls back to direct if none exists;
	// overridable here and via /admin/groups/<name>/assign-egress afterward.
	effectiveEgress := egressID
	if strings.TrimSpace(effectiveEgress) == "" {
		effectiveEgress = s.resolveKiroDefaultEgress(ctx, account.ID)
	}
	if err := s.bindImportedAccountPrimaryEgress(ctx, account.ID, effectiveEgress); err != nil {
		return err
	}
	if err := s.store.UpsertCapabilities(ctx, capability.StaticKiroModels(account.ID)); err != nil {
		return err
	}
	staticModels := capability.StaticKiroModels(account.ID)
	modelNames := make([]string, 0, len(staticModels))
	for _, model := range staticModels {
		modelNames = append(modelNames, model.ModelSlug)
	}
	if err := s.store.EnsureKiroRuntimeModels(ctx, account.ID, endpointHash, modelNames); err != nil {
		return err
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		return err
	}
	binding, err = s.store.EffectiveEgressBinding(ctx, binding)
	if err != nil {
		return err
	}
	egress, err := s.store.ResolvePrimaryEgressBinding(ctx, binding)
	if err != nil {
		return err
	}
	token, err = s.store.GetToken(ctx, account.ID)
	if err != nil {
		return err
	}
	bearer, token, cred, err := s.kiro.Prepare(ctx, account, cred, token, egress, cred.AuthMethod != "api_key")
	if err != nil {
		return err
	}
	usage, err := s.kiro.UsageLimits(ctx, account, cred, bearer, egress)
	if err != nil {
		return err
	}
	if plan := kiroPlan(usage); plan != "" {
		account.PlanType = plan
		if err := s.store.UpsertAccount(ctx, account, token); err != nil {
			return err
		}
		if strings.Contains(strings.ToUpper(plan), "FREE") {
			caps := capability.StaticKiroModels(account.ID)
			filtered := caps[:0]
			for _, c := range caps {
				if !strings.Contains(c.ModelSlug, "opus") {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				_ = s.store.UpsertCapabilities(ctx, filtered)
			}
		}
	}
	return nil
}

func parseKiroImportJSON(raw []byte) ([]kiroImportItem, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	var raws []interface{}
	switch x := root.(type) {
	case []interface{}:
		raws = x
	case map[string]interface{}:
		if a, ok := lookup(x, "accounts").([]interface{}); ok {
			raws = a
		} else {
			raws = []interface{}{x}
		}
	default:
		return nil, errors.New("Kiro JSON must be an object, array, or {accounts:[...]}")
	}
	if len(raws) == 0 {
		return nil, errors.New("Kiro JSON contains no accounts")
	}
	out := make([]kiroImportItem, 0, len(raws))
	for i, v := range raws {
		m, ok := v.(map[string]interface{})
		if !ok {
			out = append(out, kiroImportItem{ParseError: fmt.Sprintf("account %d must be an object", i+1)})
			continue
		}
		flat := map[string]interface{}{}
		for k, x := range m {
			flat[k] = x
		}
		if c, ok := lookup(m, "credentials").(map[string]interface{}); ok {
			for k, x := range c {
				flat[k] = x
			}
		}
		get := func(names ...string) string {
			for _, n := range names {
				if v := lookup(flat, n); v != nil {
					return strings.TrimSpace(fmt.Sprint(v))
				}
			}
			return ""
		}
		method := normalizeKiroAuthMethod(get("authMethod", "auth_method", "method"))
		item := kiroImportItem{AuthMethod: method, RefreshToken: get("refreshToken", "refresh_token"), AccessToken: get("accessToken", "access_token", "idToken", "id_token"), ClientID: get("clientId", "client_id"), ClientSecret: get("clientSecret", "client_secret"), ClientIDHash: get("clientIdHash", "client_id_hash"), ProfileARN: get("profileArn", "profile_arn"), AuthRegion: get("authRegion", "auth_region", "region"), APIRegion: get("apiRegion", "api_region"), MachineID: get("machineId", "machine_id"), APIKey: get("kiroApiKey", "kiro_api_key", "apiKey"), Endpoint: get("endpoint"), Email: get("email"), Plan: get("subscriptionTitle", "subscription_title", "plan")}
		if item.AuthMethod == "" {
			if item.APIKey != "" {
				item.AuthMethod = "api_key"
			} else if item.ClientID != "" || item.ClientSecret != "" || item.ClientIDHash != "" {
				item.AuthMethod = "idc"
			} else {
				item.AuthMethod = "social"
			}
		}
		item.ExpiresAt = parseKiroExpiry(lookup(flat, "expiresAt"))
		for _, k := range []string{"proxy", "proxyUrl", "proxy_url", "proxyUsername", "proxyPassword"} {
			if lookup(flat, k) != nil {
				item.ProxyIgnored = true
			}
		}
		if err := validateKiroItem(item); err != nil {
			item.ParseError = err.Error()
		}
		out = append(out, item)
	}
	return out, nil
}

// mergeKiroClientRegistrationJSON joins Kiro IDE's two-file IdC credential
// layout: kiro-auth-token.json contains clientIdHash while <hash>.json contains
// clientId/clientSecret. A single registration is paired with a single missing
// IdC account; arrays pair by order; keyed objects or explicit clientIdHash fields
// pair by hash for batch imports.
func mergeKiroClientRegistrationJSON(items []kiroImportItem, raw []byte) error {
	registrations, err := parseKiroClientRegistrations(raw)
	if err != nil {
		return err
	}
	missing := make([]int, 0)
	for i := range items {
		if items[i].AuthMethod == "idc" && (items[i].ClientID == "" || items[i].ClientSecret == "") {
			missing = append(missing, i)
		}
	}
	used := make(map[int]bool)
	for _, itemIndex := range missing {
		registrationIndex := -1
		if hash := strings.TrimSpace(items[itemIndex].ClientIDHash); hash != "" {
			for i := range registrations {
				if !used[i] && registrations[i].ClientIDHash != "" && strings.EqualFold(registrations[i].ClientIDHash, hash) {
					registrationIndex = i
					break
				}
			}
		}
		if registrationIndex < 0 && len(missing) == 1 && len(registrations) == 1 {
			registrationIndex = 0
		}
		if registrationIndex < 0 && len(registrations) == len(items) && itemIndex < len(registrations) && !used[itemIndex] {
			registrationIndex = itemIndex
		}
		if registrationIndex < 0 && len(registrations) == len(missing) {
			for i := range registrations {
				if !used[i] {
					registrationIndex = i
					break
				}
			}
		}
		if registrationIndex >= 0 {
			registration := registrations[registrationIndex]
			items[itemIndex].ClientID = registration.ClientID
			items[itemIndex].ClientSecret = registration.ClientSecret
			used[registrationIndex] = true
		}
		items[itemIndex].ParseError = ""
		if err := validateKiroItem(items[itemIndex]); err != nil {
			items[itemIndex].ParseError = err.Error()
		}
	}
	return nil
}

func parseKiroClientRegistrations(raw []byte) ([]kiroClientRegistration, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	type candidate struct {
		hash  string
		value interface{}
	}
	candidates := make([]candidate, 0)
	switch value := root.(type) {
	case []interface{}:
		for _, item := range value {
			candidates = append(candidates, candidate{value: item})
		}
	case map[string]interface{}:
		for _, key := range []string{"clients", "registrations", "accounts"} {
			if values, ok := lookup(value, key).([]interface{}); ok {
				for _, item := range values {
					candidates = append(candidates, candidate{value: item})
				}
				break
			}
		}
		if len(candidates) == 0 && (lookup(value, "clientId") != nil || lookup(value, "clientSecret") != nil) {
			candidates = append(candidates, candidate{value: value})
		}
		if len(candidates) == 0 {
			for hash, item := range value {
				if _, ok := item.(map[string]interface{}); ok {
					candidates = append(candidates, candidate{hash: hash, value: item})
				}
			}
		}
	default:
		return nil, errors.New("client registration JSON must be an object or array")
	}
	registrations := make([]kiroClientRegistration, 0, len(candidates))
	for i, candidate := range candidates {
		value, ok := candidate.value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("client registration %d must be an object", i+1)
		}
		get := func(name string) string {
			field := lookup(value, name)
			if field == nil {
				return ""
			}
			return strings.TrimSpace(fmt.Sprint(field))
		}
		registration := kiroClientRegistration{
			ClientIDHash: firstNonEmpty(get("clientIdHash"), candidate.hash),
			ClientID:     get("clientId"),
			ClientSecret: get("clientSecret"),
		}
		if registration.ClientID == "" || registration.ClientSecret == "" {
			return nil, fmt.Errorf("client registration %d requires clientId and clientSecret", i+1)
		}
		registrations = append(registrations, registration)
	}
	if len(registrations) == 0 {
		return nil, errors.New("client registration JSON contains no credentials")
	}
	return registrations, nil
}
func normalizeKiroAuthMethod(v string) string {
	v = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), "-", "_"))
	switch v {
	case "builder_id", "iam", "idc":
		return "idc"
	case "apikey", "api_key":
		return "api_key"
	case "social":
		return "social"
	}
	return v
}
func validateKiroItem(i kiroImportItem) error {
	switch i.AuthMethod {
	case "social":
		if i.RefreshToken == "" {
			return errors.New("social requires refreshToken")
		}
	case "idc":
		if i.RefreshToken == "" || i.ClientID == "" || i.ClientSecret == "" {
			return errors.New("idc requires refreshToken, clientId and clientSecret")
		}
	case "api_key":
		if i.APIKey == "" {
			return errors.New("api_key requires kiroApiKey")
		}
	default:
		return fmt.Errorf("unsupported authMethod %q", i.AuthMethod)
	}
	return nil
}
func kiroCredentialHash(i kiroImportItem) string {
	secret := i.RefreshToken
	if i.AuthMethod == "api_key" {
		secret = i.APIKey
	}
	sum := sha256.Sum256([]byte(i.AuthMethod + "\x00" + i.ClientID + "\x00" + secret))
	return hex.EncodeToString(sum[:])
}
func lookup(m map[string]interface{}, name string) interface{} {
	want := normalizeJSONKey(name)
	for k, v := range m {
		if normalizeJSONKey(k) == want {
			return v
		}
	}
	return nil
}
func normalizeJSONKey(v string) string {
	v = strings.ToLower(v)
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(v)
}
func parseKiroExpiry(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		if x > 1e12 {
			return int64(x / 1000)
		}
		return int64(x)
	case string:
		if n, e := strconv.ParseInt(x, 10, 64); e == nil {
			if n > 1e12 {
				return n / 1000
			}
			return n
		}
		if t, e := time.Parse(time.RFC3339, x); e == nil {
			return t.Unix()
		}
	}
	return 0
}
func kiroPlan(m map[string]interface{}) string {
	for _, k := range []string{"subscriptionTitle", "subscription_title", "planType", "plan_type"} {
		if v := lookup(m, k); v != nil {
			return fmt.Sprint(v)
		}
	}
	if sub, ok := lookup(m, "subscriptionInfo").(map[string]interface{}); ok {
		return kiroPlan(sub)
	}
	return ""
}
