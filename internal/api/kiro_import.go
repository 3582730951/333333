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
	"codex-account-pool/internal/storage"
)

type kiroImportItem struct {
	AuthMethod, RefreshToken, AccessToken, ClientID, ClientSecret, ProfileARN, AuthRegion, APIRegion, MachineID, APIKey, Endpoint, Email, Plan string
	ExpiresAt                                                                                                                                  int64
	ProxyIgnored                                                                                                                               bool
	ParseError                                                                                                                                 string
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

func (s *Server) adminImportKiroJSON(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		KiroJSONText string `json:"kiro_json_text"`
		Label        string `json:"label"`
		GroupName    string `json:"group_name"`
		EgressID     string `json:"egress_id"`
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
	s.kiro.UpdateConfig(s.effectiveKiroConfig(ctx))
	if err := s.store.UpsertAccount(ctx, account, token); err != nil {
		return err
	}
	if err := s.store.UpsertKiroCredentials(ctx, cred); err != nil {
		return err
	}
	if err := s.bindImportedAccountPrimaryEgress(ctx, account.ID, egressID); err != nil {
		return err
	}
	if err := s.store.UpsertCapabilities(ctx, capability.StaticKiroModels(account.ID)); err != nil {
		return err
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		return err
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
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
		item := kiroImportItem{AuthMethod: method, RefreshToken: get("refreshToken", "refresh_token"), AccessToken: get("accessToken", "access_token", "idToken", "id_token"), ClientID: get("clientId", "client_id"), ClientSecret: get("clientSecret", "client_secret"), ProfileARN: get("profileArn", "profile_arn"), AuthRegion: get("authRegion", "auth_region", "region"), APIRegion: get("apiRegion", "api_region"), MachineID: get("machineId", "machine_id"), APIKey: get("kiroApiKey", "kiro_api_key", "apiKey"), Endpoint: get("endpoint"), Email: get("email"), Plan: get("subscriptionTitle", "subscription_title", "plan")}
		if item.AuthMethod == "" {
			if item.APIKey != "" {
				item.AuthMethod = "api_key"
			} else if item.ClientID != "" || item.ClientSecret != "" {
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
