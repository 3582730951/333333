package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"github.com/google/uuid"
)

var ErrInvalidGrant = errors.New("kiro refresh token permanently invalid")

type Manager struct {
	store    *storage.Store
	upstream *upstream.Client
	cfg      atomic.Value
	mu       sync.Mutex
	gates    map[string]*sync.Mutex
}

func NewManager(store *storage.Store, up *upstream.Client, cfg config.Config) *Manager {
	m := &Manager{store: store, upstream: up, gates: map[string]*sync.Mutex{}}
	m.cfg.Store(cfg)
	return m
}
func (m *Manager) UpdateConfig(cfg config.Config) { m.cfg.Store(cfg) }
func (m *Manager) Config() config.Config          { return m.cfg.Load().(config.Config) }
func (m *Manager) gate(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gates[id]
	if g == nil {
		g = &sync.Mutex{}
		m.gates[id] = g
	}
	return g
}

func (m *Manager) Prepare(ctx context.Context, account storage.Account, cred storage.KiroCredentials, token storage.AccountToken, egress storage.EgressProfile, force bool) (string, storage.AccountToken, storage.KiroCredentials, error) {
	cfg := m.Config()
	if cred.AuthMethod == "api_key" {
		if cred.KiroAPIKey == "" {
			return "", token, cred, errors.New("kiro api key missing")
		}
		return cred.KiroAPIKey, token, cred, nil
	}
	if !force && token.AccessToken != "" && token.ExpiresAt > storage.Now()+300 {
		return token.AccessToken, token, cred, nil
	}
	g := m.gate(account.ID)
	g.Lock()
	defer g.Unlock()
	if !force {
		if latest, err := m.store.GetToken(ctx, account.ID); err == nil && latest.AccessToken != "" && latest.ExpiresAt > storage.Now()+300 {
			return latest.AccessToken, latest, cred, nil
		}
	}
	var endpoint string
	var payload interface{}
	region := normalizeRegion(first(cred.AuthRegion, cfg.KiroDefaultAuthRegion, "us-east-1"))
	if strings.TrimSpace(cred.Endpoint) == "" && !validRegion(region) {
		return "", token, cred, ErrEndpointNotAllowed
	}
	if cred.AuthMethod == "idc" {
		endpoint = fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
		payload = map[string]string{"clientId": cred.ClientID, "clientSecret": cred.ClientSecret, "refreshToken": token.RefreshToken, "grantType": "refresh_token"}
	} else {
		endpoint = fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
		payload = map[string]string{"refreshToken": token.RefreshToken}
	}
	if strings.TrimSpace(cred.Endpoint) != "" {
		allowed, validationErr := ValidateEndpoint(cred.Endpoint, first(cred.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1"), cfg.KiroEndpointAllowlist)
		if validationErr != nil {
			return "", token, cred, validationErr
		}
		base := strings.TrimSuffix(strings.TrimRight(allowed, "/"), "/generateAssistantResponse")
		if cred.AuthMethod == "idc" {
			endpoint = base + "/token"
		} else {
			endpoint = base + "/refreshToken"
		}
	}
	body, _ := json.Marshal(payload)
	headers := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/plain, */*"}, "Connection": {"close"}}
	if cred.AuthMethod == "idc" {
		headers.Set("x-amz-user-agent", "aws-sdk-js/3.980.0 KiroIDE")
		headers.Set("user-agent", fmt.Sprintf("aws-sdk-js/3.980.0 ua/2.1 os/linux lang/js md/nodejs#%s api/sso-oidc#3.980.0 m/E KiroIDE", first(cfg.KiroNodeVersion, "22.22.0")))
		headers.Set("amz-sdk-invocation-id", uuid.NewString())
		headers.Set("amz-sdk-request", "attempt=1; max=4")
	} else {
		headers.Set("user-agent", fmt.Sprintf("KiroIDE-%s-%s", first(cfg.KiroVersion, "0.11.107"), MachineID(cred, token)))
	}
	status, _, raw, err := m.do(ctx, egress, http.MethodPost, endpoint, headers, body, account.ID+":"+egress.ID)
	if err != nil {
		return "", token, cred, err
	}
	if status < 200 || status >= 300 {
		if status == 400 && strings.Contains(string(raw), "invalid_grant") {
			_ = m.store.SetAccountStatus(ctx, account.ID, "invalid")
			return "", token, cred, fmt.Errorf("%w: %s", ErrInvalidGrant, trimBody(raw))
		}
		return "", token, cred, fmt.Errorf("kiro token refresh status %d: %s", status, trimBody(raw))
	}
	var resp struct {
		AccessToken  string `json:"accessToken"`
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", token, cred, err
	}
	bearer := first(resp.IDToken, resp.AccessToken)
	if bearer == "" {
		return "", token, cred, errors.New("kiro refresh response had no token")
	}
	token.AccessToken = bearer
	if resp.IDToken != "" {
		token.IDTokenRaw = resp.IDToken
	}
	if resp.RefreshToken != "" {
		token.RefreshToken = resp.RefreshToken
	}
	if resp.ExpiresIn <= 0 {
		resp.ExpiresIn = 3600
	}
	token.ExpiresAt = storage.Now() + resp.ExpiresIn
	token.LastRefresh = storage.Now()
	if resp.ProfileARN != "" {
		cred.ProfileARN = resp.ProfileARN
	}
	// Freeze a canonical, stable machine-id the first time we hold the real refresh
	// token (it is not reachable from the per-request Headers() path). Set-once so a
	// later token rotation does not churn the device id, and so an operator-supplied
	// value is never overwritten.
	if strings.TrimSpace(cred.MachineID) == "" {
		if mid := canonicalMachineID(cred, token.RefreshToken); mid != "" {
			cred.MachineID = mid
		}
	}
	if err := m.store.UpdateToken(ctx, token); err != nil {
		return "", token, cred, err
	}
	if err := m.store.UpsertKiroCredentials(ctx, cred); err != nil {
		return "", token, cred, err
	}
	return bearer, token, cred, nil
}

func (m *Manager) UsageLimits(ctx context.Context, account storage.Account, cred storage.KiroCredentials, bearer string, egress storage.EgressProfile) (map[string]interface{}, error) {
	cfg := m.Config()
	region := first(cred.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1")
	base, err := ValidateEndpoint(cred.Endpoint, region, cfg.KiroEndpointAllowlist)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(base, "/generateAssistantResponse") {
		base = strings.TrimSuffix(base, "/generateAssistantResponse")
	}
	u := base + "/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST"
	if cred.ProfileARN != "" {
		u += "&profileArn=" + url.QueryEscape(cred.ProfileARN)
	}
	head := Headers(cfg, cred, bearer, false)
	status, _, raw, err := m.do(ctx, egress, http.MethodGet, u, head, nil, account.ID+":"+egress.ID)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("kiro usage status %d: %s", status, trimBody(raw))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func Headers(cfg config.Config, cred storage.KiroCredentials, bearer string, stream bool) http.Header {
	mid := MachineID(cred, storage.AccountToken{RefreshToken: bearer})
	version := first(cfg.KiroVersion, "0.11.107")
	node := first(cfg.KiroNodeVersion, "22.22.0")
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("accept", "application/vnd.amazon.eventstream")
	h.Set("x-amzn-codewhisperer-optout", "true")
	h.Set("x-amzn-kiro-agent-mode", "vibe")
	h.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", version, mid))
	h.Set("user-agent", fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", kiroOSToken(first(cred.AccountID, mid)), node, version, mid))
	h.Set("amz-sdk-invocation-id", uuid.NewString())
	h.Set("amz-sdk-request", "attempt=1; max=3")
	h.Set("authorization", "Bearer "+bearer)
	if cred.ProfileARN != "" {
		h.Set("x-amzn-kiro-profile-arn", cred.ProfileARN)
	}
	if cred.AuthMethod == "api_key" {
		h.Set("tokentype", "API_KEY")
	}
	return h
}
func MachineID(c storage.KiroCredentials, t storage.AccountToken) string {
	if raw := strings.TrimSpace(c.MachineID); raw != "" {
		// A real Kiro client presents a 64-hex machine-id on the wire. An imported
		// value may be a dashed UUID; normalize it to the canonical 64-hex shape
		// (matching the reference client's normalize_machine_id) rather than sending
		// a malformed dashed form. Unrecognized shapes are preserved as-is.
		if norm, ok := normalizeMachineID(raw); ok {
			return norm
		}
		return raw
	}
	// API-key credentials can derive the canonical machine-id directly from the
	// always-present key. OAuth's canonical value needs the refresh token, which is
	// persisted onto cred.MachineID during refresh (returned by the branch above).
	if c.AuthMethod == "api_key" {
		if k := strings.TrimSpace(c.KiroAPIKey); k != "" {
			return sha256String("KiroAPIKey/" + k)
		}
	}
	s := sha256String(first(c.CredentialHash, c.KiroAPIKey, t.RefreshToken, c.ClientID, c.AccountID))
	return s
}

// canonicalMachineID derives the machine-id the real Kiro client presents (ground
// truth: other/kiro.rs machine_id.rs): API-key => sha256("KiroAPIKey/"+key), OAuth
// => sha256("KotlinNativeAPI/"+refreshToken). Presenting this value makes the pool
// show the SAME device id AWS already associates with the account, instead of a
// divergent one that reads as a new/unknown device. Returns "" when the needed
// material is absent.
func canonicalMachineID(c storage.KiroCredentials, refreshToken string) string {
	if c.AuthMethod == "api_key" {
		if k := strings.TrimSpace(c.KiroAPIKey); k != "" {
			return sha256String("KiroAPIKey/" + k)
		}
		return ""
	}
	if r := strings.TrimSpace(refreshToken); r != "" {
		return sha256String("KotlinNativeAPI/" + r)
	}
	return ""
}

// normalizeMachineID mirrors the reference Kiro client: a 64-char hex string is
// returned lower-cased; a UUID (32 hex after removing dashes) is duplicated to 64
// hex. Anything else is rejected so the caller can fall back to derivation.
func normalizeMachineID(s string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(s))
	if len(t) == 64 && isHexString(t) {
		return t, true
	}
	nodash := strings.ReplaceAll(t, "-", "")
	if len(nodash) == 32 && isHexString(nodash) {
		return nodash + nodash, true
	}
	return "", false
}

func isHexString(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// kiroOSProfiles is the platform axis a real Kiro IDE (Electron/Node) reports.
// The reference client only ever emits darwin#/win32# builds — never linux — so a
// hardcoded "os/linux" shared across the whole fleet is a correlation tell. Each
// account draws one STABLE profile from a per-account seed (a rotating OS between
// requests would be more suspicious, not less).
var kiroOSProfiles = []string{
	"darwin#24.6.0", "darwin#24.5.0", "darwin#23.6.0",
	"win32#10.0.22631", "win32#10.0.22621", "win32#10.0.19045",
}

func kiroOSToken(seed string) string {
	if strings.TrimSpace(seed) == "" {
		return kiroOSProfiles[0]
	}
	sum := sha256Sum([]byte("kiro-os\x00" + seed))
	return kiroOSProfiles[int(sum[0])%len(kiroOSProfiles)]
}

func sha256String(s string) string { sum := sha256Sum([]byte(s)); return fmt.Sprintf("%x", sum) }
func sha256Sum(b []byte) [32]byte  { return sha256.Sum256(b) }

func (m *Manager) do(ctx context.Context, eg storage.EgressProfile, method, target string, headers http.Header, body []byte, jar string) (int, http.Header, []byte, error) {
	if eg.Type == "curl_cffi_sidecar" {
		resp, err := m.upstream.DoViaSidecar(ctx, eg, method, target, headers, body, jar)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return resp.StatusCode, resp.Header, raw, err
	}
	client, err := m.upstream.EgressHTTPClient(eg)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, resp.Header.Clone(), raw, err
}
func first(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
