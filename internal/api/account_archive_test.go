package api

import (
	"archive/zip"
	"bytes"
	"codex-account-pool/internal/storage"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func uploadAccountArchiveForTest(t *testing.T, h *testHarness, filename string, raw []byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/admin/accounts/import-archive", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, responseBody
}

func exportAccountArchiveForTest(t *testing.T, h *testHarness, ids ...string) (*http.Response, []byte) {
	t.Helper()
	path := "/admin/accounts/export?format=backup"
	if len(ids) > 0 {
		path += "&ids=" + strings.Join(ids, ",")
	}
	resp, err := http.Get(h.pool.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func zipAccountBackupDocumentsForTest(t *testing.T, backups ...storage.AccountBackup) []byte {
	t.Helper()
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	for index, backup := range backups {
		entry, err := writer.Create(fmt.Sprintf("account-%d.json", index+1))
		if err != nil {
			t.Fatal(err)
		}
		document, err := json.Marshal(backupDocumentFromStorage(backup, storage.Now()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(document); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func accountArchiveAccessTokenForTest(t *testing.T, userID, workspaceID string) string {
	t.Helper()
	header, err := json.Marshal(map[string]interface{}{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(map[string]interface{}{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id": userID, "chatgpt_account_id": workspaceID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(header) + "." + encode(claims) + ".signature"
}

func seedPortableAccountsForTest(t *testing.T, h *testHarness) []string {
	t.Helper()
	ctx := context.Background()
	now := storage.Now()
	for _, profile := range []storage.EgressProfile{
		{ID: "direct", Name: "legacy direct", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
		{ID: "backup-a", Name: "backup A", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
		{ID: "backup-b", Name: "backup B", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
		{ID: "sidecar-a", Name: "sidecar A", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 16},
	} {
		if err := h.store.UpsertEgressProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.UpsertCustomProvider(ctx, storage.CustomProvider{
		ID: "custom-openai", Name: "Portable custom provider",
		BaseURL: h.upstream.URL + "/v1", UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		TransportProfile: storage.CustomProviderTransportGeneric,
		Enabled:          true, AutoDiscoverModels: false, Models: []string{"portable-custom-model"},
	}); err != nil {
		t.Fatal(err)
	}

	codex := storage.Account{
		ID: "archive-codex", Label: "Codex full", GroupName: "team-primary",
		UpstreamAccountID: "workspace-codex", ChatGPTUserID: "user-codex",
		Email: "codex@example.internal", PlanType: "pro", Provider: "codex",
		Status: "quarantine", IsFedramp: true, IgnoreRateLimitControls: true,
		QuarantineUntil: now + 600, QuarantineReason: "operator_test",
	}
	codexToken := storage.AccountToken{
		AuthMethod: "oauth", CredentialMode: "agent_identity",
		AccessToken: "codex-access", RefreshToken: "codex-refresh",
		OpenAIAPIKey: "codex-key-column", IDTokenRaw: "codex-id-token",
		AgentRuntimeID: "runtime-portable", AgentPrivateKey: "private-portable",
		AgentTaskID: "task-portable", LastRefresh: now - 10, ExpiresAt: now + 3600,
		Scopes: "openid profile", OAuthRateLimitTier: "tier-pro",
	}
	if err := h.store.UpsertAccount(ctx, codex, codexToken); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountGroup(ctx, codex.ID, codex.GroupName); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddAccountToGroup(ctx, codex.ID, "team-extra"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSessionCookie(ctx, codex.ID, "__Secure-next-auth.session-token=portable"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertInjectedCookie(ctx, storage.InjectedCookie{
		AccountID: codex.ID, EgressID: "direct", UpstreamHost: "chatgpt.com",
		CookieHeader: "cf_clearance=portable", UserAgent: "portable-UA", ExitIP: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID: codex.ID, PrimaryEgressID: "direct", StandbyEgressIDs: "backup-a,backup-b",
		SidecarEgressID: "sidecar-a", CookieJarKey: "portable-cookie-jar",
		CooldownUntil: now + 120, RecheckPending: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: codex.ID, ModelSlug: "portable-model", AvailabilityState: "verified",
		Context1MState: "supported", Context1MSource: "upstream",
		NativeContextWindow: 372000, NativeMaxContextWindow: 372000,
		EffectiveContextWindowPercent: 100, AutoCompactTokenLimit: 334800,
		Visibility: "list", ETag: "etag-portable", RawModelJSONHash: "hash-portable",
		RawModelJSON: `{"slug":"portable-model"}`, Source: "live", LastProbeAt: now - 5,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{
		AccountID: codex.ID, LoginEmail: "login@example.internal",
		Password: "password-portable", OTPURL: "otpauth://totp/portable",
		TargetWorkspaceID: "workspace-codex", AutoEnabled: true,
		LastStatus: "ready", LastError: "previous-detail",
	}); err != nil {
		t.Fatal(err)
	}

	claude := storage.Account{
		ID: "archive-claude", Label: "Claude OAuth", GroupName: "cyber",
		Email: "claude@example.internal", Provider: "claude", Status: "active",
	}
	if err := h.store.UpsertAccount(ctx, claude, storage.AccountToken{
		AuthMethod: "oauth", AccessToken: "sk-ant-oat-portable",
		RefreshToken: "claude-refresh", Scopes: "user:inference",
		OAuthRateLimitTier: "max", ExpiresAt: now + 7200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountGroup(ctx, claude.ID, claude.GroupName); err != nil {
		t.Fatal(err)
	}

	kiro := storage.Account{
		ID: "archive-kiro", Label: "Kiro IdC", GroupName: "kiro",
		Email: "kiro@example.internal", Provider: "kiro", Status: "active",
	}
	if err := h.store.UpsertAccount(ctx, kiro, storage.AccountToken{
		AccessToken: "kiro-access", RefreshToken: "kiro-refresh", ExpiresAt: now + 1800,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountGroup(ctx, kiro.ID, kiro.GroupName); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertKiroCredentials(ctx, storage.KiroCredentials{
		AccountID: kiro.ID, AuthMethod: "idc", ClientID: "kiro-client",
		ClientSecret: "kiro-client-secret", ProfileARN: "arn:aws:iam::123:role/portable",
		AuthRegion: "us-east-1", APIRegion: "eu-central-1", MachineID: "machine-portable",
		Endpoint: "https://q.eu-central-1.amazonaws.com", CredentialHash: "portable-kiro-hash",
	}); err != nil {
		t.Fatal(err)
	}

	antigravity := storage.Account{
		ID: "archive-antigravity", Label: "Antigravity", GroupName: "cyber",
		Email: "anti@example.internal", Provider: "antigravity", Status: "active",
	}
	antiToken := storage.AccountToken{
		AuthMethod: "oauth", AccessToken: "anti-access", RefreshToken: "anti-refresh",
		ExpiresAt: now + 3600, Scopes: "cloud-platform",
	}
	if err := h.store.UpsertAccountWithAntigravityCredentials(ctx, antigravity, antiToken, storage.AntigravityCredentials{
		AccountID: antigravity.ID, Email: antigravity.Email, ProjectID: "project-portable",
		AccessToken: "anti-access", RefreshToken: "anti-refresh", ExpiresAt: now + 3600,
		BaseURL: "https://example.internal/antigravity", UserAgent: "portable-antigravity/1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountGroup(ctx, antigravity.ID, antigravity.GroupName); err != nil {
		t.Fatal(err)
	}

	custom := storage.Account{
		ID: "archive-custom", Label: "Custom key", GroupName: "cyber",
		Provider: "custom-openai", Status: "disabled",
	}
	if err := h.store.UpsertAccount(ctx, custom, storage.AccountToken{
		AuthMethod: "api_key", OpenAIAPIKey: "custom-key-portable",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountGroup(ctx, custom.ID, custom.GroupName); err != nil {
		t.Fatal(err)
	}
	return []string{codex.ID, claude.ID, kiro.ID, antigravity.ID, custom.ID}
}

func TestAccountBackupSingleJSONAndMultiZIPRoundTripAllCredentialShapes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"data":[]}`) })
	ids := seedPortableAccountsForTest(t, h)

	singleResponse, singleRaw := exportAccountArchiveForTest(t, h, ids[0])
	if singleResponse.StatusCode != http.StatusOK ||
		!strings.Contains(singleResponse.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(singleResponse.Header.Get("Content-Disposition"), ".json") ||
		singleResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("single export headers = status:%d headers:%v body:%s", singleResponse.StatusCode, singleResponse.Header, singleRaw)
	}
	var single accountBackupDocumentV1
	if err := json.Unmarshal(singleRaw, &single); err != nil {
		t.Fatal(err)
	}
	if single.Type != accountBackupDocumentType || single.Version != accountBackupDocumentVersion ||
		single.Account.ID != ids[0] || single.Token.AgentPrivateKey != "private-portable" ||
		single.SessionCookie == nil || *single.SessionCookie != "__Secure-next-auth.session-token=portable" ||
		single.CodexReauthConfig == nil || single.CodexReauthConfig.Password != "password-portable" ||
		len(single.InjectedCookies) != 1 || len(single.GroupMemberships) != 2 ||
		len(single.Capabilities) != 1 || single.EgressBinding == nil ||
		len(single.EgressProfiles) != 4 {
		t.Fatalf("single account backup is incomplete: %+v", single)
	}

	multiResponse, archiveRaw := exportAccountArchiveForTest(t, h, ids...)
	if multiResponse.StatusCode != http.StatusOK ||
		!strings.Contains(multiResponse.Header.Get("Content-Type"), "application/zip") ||
		!strings.Contains(multiResponse.Header.Get("Content-Disposition"), ".zip") {
		t.Fatalf("multi export headers = status:%d headers:%v body:%s", multiResponse.StatusCode, multiResponse.Header, archiveRaw)
	}
	zr, err := zip.NewReader(bytes.NewReader(archiveRaw), int64(len(archiveRaw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != len(ids) {
		t.Fatalf("ZIP entries = %d, want %d", len(zr.File), len(ids))
	}
	customDefinitionFound := false
	for _, file := range zr.File {
		if !strings.HasSuffix(file.Name, ".json") {
			t.Fatalf("ZIP entry = %q, want one JSON per account", file.Name)
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		var document accountBackupDocumentV1
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		if document.Account.ID == ids[4] {
			customDefinitionFound = document.CustomProvider != nil &&
				document.CustomProvider.ID == "custom-openai" &&
				len(document.EgressProfiles) > 0
		}
	}
	if !customDefinitionFound {
		t.Fatal("custom account ZIP entry did not carry provider and egress definitions")
	}
	allResponse, allRaw := exportAccountArchiveForTest(t, h)
	if allResponse.StatusCode != http.StatusOK || !strings.Contains(allResponse.Header.Get("Content-Type"), "application/zip") {
		t.Fatalf("export all = status:%d headers:%v body:%s", allResponse.StatusCode, allResponse.Header, allRaw)
	}
	allZIP, err := zip.NewReader(bytes.NewReader(allRaw), int64(len(allRaw)))
	if err != nil {
		t.Fatalf("open export-all ZIP: %v", err)
	}
	if len(allZIP.File) != len(ids) {
		t.Fatalf("export-all ZIP entries=%d, want %d", len(allZIP.File), len(ids))
	}

	for _, id := range ids {
		if err := h.store.DeleteAccount(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	code, importRaw := uploadAccountArchiveForTest(t, h, "all-accounts.zip", allRaw)
	if code != http.StatusOK {
		t.Fatalf("ZIP import = %d: %s", code, importRaw)
	}
	var imported struct {
		Recognized int `json:"recognized"`
		Imported   int `json:"imported"`
		Replaced   int `json:"replaced"`
		Files      int `json:"files"`
	}
	if err := json.Unmarshal(importRaw, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Recognized != len(ids) || imported.Imported != len(ids) || imported.Replaced != 0 || imported.Files != len(ids) {
		t.Fatalf("import summary = %+v", imported)
	}

	codexToken, err := h.store.GetToken(context.Background(), ids[0])
	if err != nil || codexToken.AgentPrivateKey != "private-portable" || codexToken.RefreshToken != "codex-refresh" {
		t.Fatalf("restored Codex token = %+v err=%v", codexToken, err)
	}
	sessionCookie, err := h.store.GetSessionCookie(context.Background(), ids[0])
	if err != nil || sessionCookie != "__Secure-next-auth.session-token=portable" {
		t.Fatalf("restored session cookie = %q err=%v", sessionCookie, err)
	}
	reauth, found, err := h.store.GetCodexReauthConfig(context.Background(), ids[0])
	if err != nil || !found || reauth.Password != "password-portable" || reauth.OTPURL != "otpauth://totp/portable" {
		t.Fatalf("restored reauth = %+v found=%v err=%v", reauth, found, err)
	}
	kiroCredentials, err := h.store.GetKiroCredentials(context.Background(), ids[2])
	if err != nil || kiroCredentials.ClientSecret != "kiro-client-secret" || kiroCredentials.APIRegion != "eu-central-1" {
		t.Fatalf("restored Kiro credentials = %+v err=%v", kiroCredentials, err)
	}
	antiCredentials, err := h.store.GetAntigravityCredentials(context.Background(), ids[3])
	if err != nil || antiCredentials.ProjectID != "project-portable" || antiCredentials.RefreshToken != "anti-refresh" {
		t.Fatalf("restored Antigravity credentials = %+v err=%v", antiCredentials, err)
	}
	customToken, err := h.store.GetToken(context.Background(), ids[4])
	if err != nil || customToken.OpenAIAPIKey != "custom-key-portable" || customToken.AuthMethod != "api_key" {
		t.Fatalf("restored custom key = %+v err=%v", customToken, err)
	}
}

func TestAccountBackupFreshStoreRestoresCustomProviderAndProxyEgressForImmediateRouting(t *testing.T) {
	var proxyRequests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("proxy upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer portable-api-key" {
			t.Errorf("proxy Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-portable","object":"chat.completion","created":1,
			"model":"portable-route-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"portable routed"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer proxy.Close()

	source := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "source default upstream should not be used", http.StatusBadGateway)
	})
	ctx := context.Background()
	const (
		accountID  = "portable-custom-account"
		providerID = "portable-custom-provider"
		egressID   = "portable-http-proxy"
		model      = "portable-route-model"
	)
	provider := storage.CustomProvider{
		ID: providerID, Name: "Portable provider", BaseURL: "http://portable-relay.invalid/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		TransportProfile: storage.CustomProviderTransportGeneric,
		EgressIDs:        []string{egressID}, Enabled: true, AutoDiscoverModels: false,
		Models: []string{model},
	}
	if err := source.store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID: egressID, Name: "Portable HTTP proxy", Type: "http_proxy", Endpoint: proxy.URL,
		StreamCapable: true, Health: "healthy", MaxConcurrency: 16,
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertCustomProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: accountID, Label: "Portable custom", GroupName: source.app.cfg.DefaultGroup,
		Provider: providerID, Status: "active",
	}
	if err := source.store.UpsertAccount(ctx, account, storage.AccountToken{
		AuthMethod: "api_key", OpenAIAPIKey: "portable-api-key",
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: egressID, CookieJarKey: accountID + ":" + egressID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: accountID, ModelSlug: model, AvailabilityState: "verified",
		EffectiveContextWindowPercent: 100, Visibility: "list",
		Source: "portable_test", LastProbeAt: storage.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	exportResponse, exported := exportAccountArchiveForTest(t, source, accountID)
	if exportResponse.StatusCode != http.StatusOK {
		t.Fatalf("fresh-store source export = %d: %s", exportResponse.StatusCode, exported)
	}
	var document accountBackupDocumentV1
	if err := json.Unmarshal(exported, &document); err != nil {
		t.Fatal(err)
	}
	if document.CustomProvider == nil || document.CustomProvider.ID != providerID ||
		len(document.EgressProfiles) != 1 || document.EgressProfiles[0].ID != egressID {
		t.Fatalf("portable dependencies missing from JSON: %+v", document)
	}

	destination := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "destination default upstream should not be used", http.StatusBadGateway)
	})
	if _, found, err := destination.store.GetCustomProvider(ctx, providerID); err != nil || found {
		t.Fatalf("destination unexpectedly has provider before import: found=%v err=%v", found, err)
	}
	if _, err := destination.store.GetEgressProfile(ctx, egressID); !storage.NotFound(err) {
		t.Fatalf("destination unexpectedly has egress before import: %v", err)
	}
	code, imported := uploadAccountArchiveForTest(t, destination, "portable-account.json", exported)
	if code != http.StatusOK {
		t.Fatalf("fresh-store import = %d: %s", code, imported)
	}
	if restored, found, err := destination.store.GetCustomProvider(ctx, providerID); err != nil || !found ||
		restored.BaseURL != provider.BaseURL || len(restored.EgressIDs) != 1 || restored.EgressIDs[0] != egressID {
		t.Fatalf("restored provider = %+v found=%v err=%v", restored, found, err)
	}
	if restored, err := destination.store.GetEgressProfile(ctx, egressID); err != nil ||
		restored.Type != "http_proxy" || restored.Endpoint != proxy.URL {
		t.Fatalf("restored egress = %+v err=%v", restored, err)
	}
	requestBody, err := json.Marshal(map[string]interface{}{
		"model": model, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(destination.pool.URL+"/v1/chat/completions", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "portable routed") {
		t.Fatalf("immediate custom route = %d: %s", response.StatusCode, responseBody)
	}
	if got := proxyRequests.Load(); got != 1 {
		t.Fatalf("proxy request count = %d, want 1", got)
	}
}

func TestAccountBackupConflictingPortableDefinitionsRejectZIPAtomically(t *testing.T) {
	tests := []struct {
		name         string
		mutateSecond func(*storage.CustomProvider, *storage.EgressProfile)
	}{
		{
			name: "custom provider",
			mutateSecond: func(provider *storage.CustomProvider, _ *storage.EgressProfile) {
				provider.BaseURL = "https://conflicting-provider.example/v1"
			},
		},
		{
			name: "egress profile",
			mutateSecond: func(_ *storage.CustomProvider, profile *storage.EgressProfile) {
				profile.Endpoint = "http://127.0.0.1:19091"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unused", http.StatusBadGateway)
			})
			ctx := context.Background()
			provider := storage.CustomProvider{
				ID: "archive-conflict-provider", Name: "Existing provider",
				BaseURL:          "https://existing-provider.example/v1",
				UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
				TransportProfile: storage.CustomProviderTransportGeneric,
				EgressIDs:        []string{"archive-conflict-egress"},
				Enabled:          true, Models: []string{"archive-conflict-model"},
			}
			profile := storage.EgressProfile{
				ID: "archive-conflict-egress", Name: "Existing egress",
				Type: "http_proxy", Endpoint: "http://127.0.0.1:19090",
				StreamCapable: true, Health: "healthy", MaxConcurrency: 16,
			}
			if err := h.store.UpsertEgressProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
			if err := h.store.UpsertCustomProvider(ctx, provider); err != nil {
				t.Fatal(err)
			}
			backup := func(accountID string, providerDefinition storage.CustomProvider, egressDefinition storage.EgressProfile) storage.AccountBackup {
				return storage.AccountBackup{
					Account: storage.Account{
						ID: accountID, Label: accountID, GroupName: h.app.cfg.DefaultGroup,
						Provider: provider.ID, Status: "active",
					},
					Token: storage.AccountToken{
						AccountID: accountID, AuthMethod: "api_key", OpenAIAPIKey: "key-" + accountID,
					},
					CustomProvider: &providerDefinition,
					EgressProfiles: []storage.EgressProfile{egressDefinition},
					EgressBinding: &storage.AccountEgressBinding{
						AccountID: accountID, PrimaryEgressID: profile.ID,
						CookieJarKey: accountID + ":" + profile.ID,
					},
				}
			}
			secondProvider, secondProfile := provider, profile
			test.mutateSecond(&secondProvider, &secondProfile)
			archive := zipAccountBackupDocumentsForTest(t,
				backup("archive-conflict-a", provider, profile),
				backup("archive-conflict-b", secondProvider, secondProfile),
			)
			code, raw := uploadAccountArchiveForTest(t, h, "conflict.zip", archive)
			if code != http.StatusBadRequest || !strings.Contains(string(raw), `"code":"invalid_request"`) {
				t.Fatalf("conflicting ZIP = %d: %s", code, raw)
			}
			for _, accountID := range []string{"archive-conflict-a", "archive-conflict-b"} {
				if _, err := h.store.GetAccount(ctx, accountID); !storage.NotFound(err) {
					t.Fatalf("account %q was partially restored: %v", accountID, err)
				}
			}
			restoredProvider, found, err := h.store.GetCustomProvider(ctx, provider.ID)
			if err != nil || !found || restoredProvider.BaseURL != provider.BaseURL {
				t.Fatalf("existing provider changed after rejected import: %+v found=%v err=%v", restoredProvider, found, err)
			}
			restoredProfile, err := h.store.GetEgressProfile(ctx, profile.ID)
			if err != nil || restoredProfile.Endpoint != profile.Endpoint {
				t.Fatalf("existing egress changed after rejected import: %+v err=%v", restoredProfile, err)
			}
		})
	}
}

func TestAccountArchiveFiftyThousandIDsStayWithinSQLiteBatchLimit(t *testing.T) {
	ids := make([]string, 50_000)
	batches := accountArchiveIDBatches(ids)
	if len(batches) != 100 {
		t.Fatalf("batch count = %d, want 100", len(batches))
	}
	total := 0
	for index, batch := range batches {
		if len(batch) == 0 || len(batch) > accountArchiveQueryBatchSize {
			t.Fatalf("batch %d size = %d, want 1..%d", index, len(batch), accountArchiveQueryBatchSize)
		}
		total += len(batch)
	}
	if total != 50_000 {
		t.Fatalf("batched IDs = %d, want 50000", total)
	}
}

func TestAccountArchiveGeneratedSizeLimitsMatchImportFileContract(t *testing.T) {
	if accountArchiveMaxJSONBytes > accountArchiveMaxUploadBytes ||
		accountArchiveMaxUploadBytes+accountArchiveMaxFormOverhead <= accountArchiveMaxUploadBytes {
		t.Fatalf("invalid archive limits: JSON=%d file=%d form-overhead=%d",
			accountArchiveMaxJSONBytes, accountArchiveMaxUploadBytes, accountArchiveMaxFormOverhead)
	}
	var destination bytes.Buffer
	writer := &accountArchiveBoundedWriter{
		dst: &destination, remaining: 4, err: errGeneratedAccountArchiveTooLarge,
	}
	if n, err := writer.Write([]byte("1234")); err != nil || n != 4 {
		t.Fatalf("exact-limit write = %d, %v", n, err)
	}
	if n, err := writer.Write([]byte("5")); !errors.Is(err, errGeneratedAccountArchiveTooLarge) || n != 0 {
		t.Fatalf("over-limit write = %d, %v", n, err)
	}
	if destination.String() != "1234" {
		t.Fatalf("bounded destination contains a partial overflow: %q", destination.String())
	}
}

func TestAccountArchiveUploadSeparatesFileLimitFromMultipartEnvelope(t *testing.T) {
	request := func(t *testing.T, payload []byte) (*httptest.ResponseRecorder, *http.Request) {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", "accounts.zip")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/import-archive", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return recorder, req
	}

	recorder, req := request(t, bytes.Repeat([]byte("x"), 16))
	raw, filename, err := readAccountArchiveUploadWithLimits(recorder, req, 16, 1024)
	if err != nil || len(raw) != 16 || filename != "accounts.zip" {
		t.Fatalf("exact-limit multipart file = len:%d name:%q err:%v", len(raw), filename, err)
	}

	recorder, req = request(t, bytes.Repeat([]byte("x"), 17))
	if _, _, err := readAccountArchiveUploadWithLimits(recorder, req, 16, 1024); err == nil ||
		!strings.Contains(err.Error(), "exceeds 64 MiB") {
		t.Fatalf("over-limit multipart file error = %v", err)
	}
}

func TestAccountBackupImportReplacesMatchingAccountCompletely(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	ids := seedPortableAccountsForTest(t, h)
	_, original := exportAccountArchiveForTest(t, h, ids[0])

	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: ids[0], Label: "mutated", GroupName: "mutated", Provider: "custom",
		Status: "active",
	}, storage.AccountToken{AuthMethod: "api_key", OpenAIAPIKey: "mutated-key"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSessionCookie(ctx, ids[0], "mutated-cookie"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertKiroCredentials(ctx, storage.KiroCredentials{
		AccountID: ids[0], AuthMethod: "api_key", APIRegion: "us-east-1",
		AuthRegion: "us-east-1", KiroAPIKey: "mutated-kiro", CredentialHash: "mutated-hash",
	}); err != nil {
		t.Fatal(err)
	}

	code, raw := uploadAccountArchiveForTest(t, h, "account.json", original)
	if code != http.StatusOK {
		t.Fatalf("replace import = %d: %s", code, raw)
	}
	var summary struct {
		Recognized int `json:"recognized"`
		Imported   int `json:"imported"`
		Replaced   int `json:"replaced"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Recognized != 1 || summary.Imported != 0 || summary.Replaced != 1 {
		t.Fatalf("replace summary = %+v", summary)
	}
	account, err := h.store.GetAccount(ctx, ids[0])
	if err != nil || account.Label != "Codex full" || account.Provider != "codex" || account.Status != "quarantine" {
		t.Fatalf("replaced account = %+v err=%v", account, err)
	}
	if _, err := h.store.GetKiroCredentials(ctx, ids[0]); err == nil {
		t.Fatal("provider-specific row omitted by backup survived complete replacement")
	}
}

func TestAccountBackupImportCompatibilityFormatsAndVersions(t *testing.T) {
	tests := []struct {
		name       string
		filename   string
		document   string
		wantFormat string
		wantID     string
	}{
		{
			name: "legacy pool JSON v0", filename: "accounts.json",
			document:   `{"id":"legacy-v0","email":"legacy@example.internal","label":"legacy","group_name":"cyber","provider":"custom","status":"active","openai_api_key":"legacy-key"}`,
			wantFormat: "legacy-pool-json-v0", wantID: "legacy-v0",
		},
		{
			name: "historical auth export lower-case key", filename: "auth.json",
			document:   `[{"email":"auth-key@example.internal","access_token":"","refresh_token":"","openai_api_key":"sk-proj-legacy-auth"}]`,
			wantFormat: "auth-json-array",
		},
		{
			name: "single auth json", filename: "auth.json",
			document:   `{"access_token":"opaque-access","refresh_token":"opaque-refresh","email":"single@example.internal","provider":"codex"}`,
			wantFormat: "auth-json",
		},
		{
			name: "auth json array", filename: "auth-array.json",
			document:   `[{"access_token":"array-access-a","refresh_token":"array-refresh-a"},{"claudeAiOauth":{"accessToken":"sk-ant-oat-array","refreshToken":"claude-array-refresh","expiresAt":1999999999000}}]`,
			wantFormat: "auth-json-array",
		},
		{
			name: "sub2api version 1", filename: "sub2api.json",
			document:   `{"type":"sub2api-data","version":1,"proxies":[],"accounts":[{"name":"sub2api-account","platform":"openai","type":"oauth","credentials":{"access_token":"sub2api-access","refresh_token":"sub2api-refresh"}}]}`,
			wantFormat: "sub2api-data",
		},
		{
			name: "Kiro social JSON", filename: "kiro.json",
			document:   `{"authMethod":"social","refreshToken":"kiro-social-refresh","accessToken":"kiro-social-access","email":"kiro-social@example.internal","authRegion":"us-east-1","apiRegion":"us-west-2"}`,
			wantFormat: "kiro-json",
		},
		{
			name: "Kiro API key array", filename: "kiro-array.json",
			document:   `[{"auth_method":"api_key","kiro_api_key":"ksk_portable_one","api_region":"us-east-1"},{"authMethod":"apiKey","apiKey":"ksk_portable_two","apiRegion":"eu-west-1"}]`,
			wantFormat: "kiro-json",
		},
		{
			name: "provider API key request JSON", filename: "provider-key.json",
			document:   `{"provider_id":"claude","api_key":"sk-ant-api-portable","label":"Claude API","group_name":"paid"}`,
			wantFormat: "provider-api-key",
		},
		{
			name: "Antigravity credential JSON", filename: "antigravity.json",
			document:   `{"provider":"antigravity","access_token":"anti-compat-access","refresh_token":"anti-compat-refresh","email":"anti-compat@example.internal","project_id":"anti-project","expires_at":1999999999}`,
			wantFormat: "antigravity-json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
			code, raw := uploadAccountArchiveForTest(t, h, tc.filename, []byte(tc.document))
			if code != http.StatusOK {
				t.Fatalf("compatibility import = %d: %s", code, raw)
			}
			var result struct {
				Recognized int      `json:"recognized"`
				Formats    []string `json:"formats"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.Recognized == 0 || len(result.Formats) != 1 || result.Formats[0] != tc.wantFormat {
				t.Fatalf("compatibility result = %+v", result)
			}
			if tc.wantID != "" {
				account, err := h.store.GetAccount(context.Background(), tc.wantID)
				if err != nil || account.ID != tc.wantID {
					t.Fatalf("restored legacy account = %+v err=%v", account, err)
				}
			}
		})
	}
}

func TestAccountBackupImportCompatibilityCPAWebSessionAndKiroIdentityModes(t *testing.T) {
	cpaToken := accountArchiveAccessTokenForTest(t, "user-cpa-archive", "workspace-cpa-archive")
	webToken := accountArchiveAccessTokenForTest(t, "user-web-archive", "workspace-web-archive")
	tests := []struct {
		name       string
		document   string
		wantFormat string
		verify     func(*testing.T, *testHarness, string)
	}{
		{
			name:       "CPA external ChatGPT token",
			document:   `{"type":"codex","access_token":"` + cpaToken + `","account_id":"workspace-cpa-archive","id_token":"placeholder","email":"cpa-archive@example.internal"}`,
			wantFormat: "auth-json",
			verify: func(t *testing.T, h *testHarness, accountID string) {
				account, err := h.store.GetAccount(context.Background(), accountID)
				if err != nil || account.Provider != "codex" || account.UpstreamAccountID != "workspace-cpa-archive" || account.ChatGPTUserID != "user-cpa-archive" {
					t.Fatalf("CPA account = %+v err=%v", account, err)
				}
				token, err := h.store.GetToken(context.Background(), accountID)
				if err != nil || token.CredentialMode != "chatgpt_auth_tokens" || token.AccessToken != cpaToken ||
					token.IDTokenRaw == "" || token.IDTokenRaw == "placeholder" {
					t.Fatalf("CPA token = %+v err=%v", token, err)
				}
			},
		},
		{
			name: "ChatGPT web session",
			document: `{"session":{"user":{"id":"user-web-archive","email":"web-archive@example.internal"},"account":{"id":"workspace-web-archive","planType":"pro"},` +
				`"accessToken":"` + webToken + `","expires":"2099-01-01T00:00:00Z"}}`,
			wantFormat: "auth-json",
			verify: func(t *testing.T, h *testHarness, accountID string) {
				account, err := h.store.GetAccount(context.Background(), accountID)
				if err != nil || account.UpstreamAccountID != "workspace-web-archive" || account.ChatGPTUserID != "user-web-archive" ||
					account.Email != "web-archive@example.internal" || account.PlanType != "pro" {
					t.Fatalf("web-session account = %+v err=%v", account, err)
				}
				token, err := h.store.GetToken(context.Background(), accountID)
				if err != nil || token.CredentialMode != "chatgpt_auth_tokens" || token.AccessToken != webToken {
					t.Fatalf("web-session token = %+v err=%v", token, err)
				}
			},
		},
		{
			name:       "Kiro IdC",
			document:   `{"authMethod":"idc","refreshToken":"kiro-idc-refresh","clientId":"kiro-idc-client","clientSecret":"kiro-idc-secret","authRegion":"us-east-1","apiRegion":"us-west-2"}`,
			wantFormat: "kiro-json",
			verify: func(t *testing.T, h *testHarness, accountID string) {
				credentials, err := h.store.GetKiroCredentials(context.Background(), accountID)
				if err != nil || credentials.AuthMethod != "idc" || credentials.ClientID != "kiro-idc-client" ||
					credentials.ClientSecret != "kiro-idc-secret" || credentials.APIRegion != "us-west-2" {
					t.Fatalf("Kiro IdC credentials = %+v err=%v", credentials, err)
				}
			},
		},
		{
			name:       "Kiro Builder ID nested credentials",
			document:   `{"accounts":[{"email":"builder-archive@example.internal","credentials":{"authMethod":"builder-id","refresh_token":"kiro-builder-refresh","client_id":"kiro-builder-client","client_secret":"kiro-builder-secret","region":"eu-west-1"}}]}`,
			wantFormat: "kiro-json",
			verify: func(t *testing.T, h *testHarness, accountID string) {
				credentials, err := h.store.GetKiroCredentials(context.Background(), accountID)
				if err != nil || credentials.AuthMethod != "idc" || credentials.ClientID != "kiro-builder-client" ||
					credentials.ClientSecret != "kiro-builder-secret" || credentials.AuthRegion != "eu-west-1" {
					t.Fatalf("Kiro Builder ID credentials = %+v err=%v", credentials, err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
			code, raw := uploadAccountArchiveForTest(t, h, "account.json", []byte(tc.document))
			if code != http.StatusOK {
				t.Fatalf("compatibility import = %d: %s", code, raw)
			}
			var result struct {
				Recognized int      `json:"recognized"`
				Formats    []string `json:"formats"`
				Accounts   []struct {
					ID string `json:"id"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.Recognized != 1 || len(result.Formats) != 1 || result.Formats[0] != tc.wantFormat ||
				len(result.Accounts) != 1 || result.Accounts[0].ID == "" {
				t.Fatalf("compatibility result = %+v", result)
			}
			tc.verify(t, h, result.Accounts[0].ID)
		})
	}
}

func TestAccountBackupRejectsUnsupportedVersionDuplicateIDsAndUnsafeZIPAtomically(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	ids := seedPortableAccountsForTest(t, h)
	_, raw := exportAccountArchiveForTest(t, h, ids[0])

	var future map[string]interface{}
	if err := json.Unmarshal(raw, &future); err != nil {
		t.Fatal(err)
	}
	future["version"] = float64(accountBackupDocumentVersion + 1)
	futureRaw, _ := json.Marshal(future)
	if code, body := uploadAccountArchiveForTest(t, h, "future.json", futureRaw); code != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Fatalf("future version = %d: %s", code, body)
	}
	var validNew map[string]interface{}
	if err := json.Unmarshal(raw, &validNew); err != nil {
		t.Fatal(err)
	}
	validAccount := validNew["account"].(map[string]interface{})
	validAccount["id"] = "must-not-partially-import"
	validNewRaw, _ := json.Marshal(validNew)
	atomicBatch := []byte("[" + string(validNewRaw) + "," + string(futureRaw) + "]")
	if code, body := uploadAccountArchiveForTest(t, h, "atomic.json", atomicBatch); code != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Fatalf("atomic invalid batch = %d: %s", code, body)
	}
	if _, err := h.store.GetAccount(context.Background(), "must-not-partially-import"); err == nil {
		t.Fatal("valid prefix account was partially imported before a later file failed")
	}

	duplicateRaw := []byte("[" + string(raw) + "," + string(raw) + "]")
	if code, body := uploadAccountArchiveForTest(t, h, "duplicate.json", duplicateRaw); code != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Fatalf("duplicate IDs = %d: %s", code, body)
	}

	var duplicateArchive bytes.Buffer
	duplicateWriter := zip.NewWriter(&duplicateArchive)
	for i := 0; i < 2; i++ {
		entry, err := duplicateWriter.Create("account.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := duplicateWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := uploadAccountArchiveForTest(t, h, "duplicate-entry.zip", duplicateArchive.Bytes()); code != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Fatalf("duplicate ZIP entry = %d: %s", code, body)
	}

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("../account.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := uploadAccountArchiveForTest(t, h, "unsafe.zip", archive.Bytes()); code != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Fatalf("unsafe ZIP = %d: %s", code, body)
	}

	account, err := h.store.GetAccount(context.Background(), ids[0])
	if err != nil || account.Label != "Codex full" {
		t.Fatalf("failed imports changed account: %+v err=%v", account, err)
	}
}
