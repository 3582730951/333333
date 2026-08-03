package api

import (
	authparse "codex-account-pool/internal/auth"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func externalSessionJWTForTest(t *testing.T, userID, workspaceID string, expiresAt int64) string {
	t.Helper()
	header, err := json.Marshal(map[string]interface{}{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(map[string]interface{}{
		"email": "session@example.internal", "exp": expiresAt,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id": userID, "chatgpt_account_id": workspaceID, "chatgpt_plan_type": "pro",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(header) + "." + encode(claims) + "." + encode([]byte("sig"))
}

func importExternalSessionForTest(t *testing.T, h *testHarness, label, token, cookie string) (int, []byte) {
	t.Helper()
	payload := map[string]interface{}{
		"label": label,
		"auth_json": map[string]interface{}{
			"type": "codex", "access_token": token, "id_token": "legacy-placeholder",
		},
	}
	if cookie != "" {
		payload["session_cookie"] = cookie
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(raw))
}

func TestNormalizeImportedSessionCookie(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		parsed   string
		want     string
		wantErr  bool
	}{
		{name: "raw token", explicit: "raw-token", want: "__Secure-next-auth.session-token=raw-token"},
		{name: "Cookie prefix", explicit: "Cookie: a=b; c=d", want: "a=b; c=d"},
		{name: "explicit wins", explicit: "explicit=value", parsed: "embedded=value", want: "explicit=value"},
		{name: "embedded", parsed: "embedded=value", want: "embedded=value"},
		{name: "header injection", explicit: "a=b\r\nX-Leak: yes", wantErr: true},
		{name: "oversized", explicit: strings.Repeat("x", maxImportedSessionCookieBytes+1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeImportedSessionCookie(tc.explicit, authparse.ParsedAuth{SessionCookie: tc.parsed})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cookie %q unexpectedly accepted", tc.name)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("cookie = %q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestImportRawChatGPTSessionAutomaticallyStoresSessionToken(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	accessToken := externalSessionJWTForTest(
		t,
		"user-auto-session",
		"workspace-auto-session",
		time.Now().Add(time.Hour).Unix(),
	)
	const sessionToken = "automatic-session-secret"
	payload := map[string]interface{}{
		"label": "raw-web-session",
		"auth_json": map[string]interface{}{
			"user":         map[string]interface{}{"id": "user-auto-session", "email": "auto@example.internal"},
			"account":      map[string]interface{}{"id": "workspace-auto-session", "planType": "team"},
			"accessToken":  accessToken,
			"sessionToken": sessionToken,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", string(body))
	if code != http.StatusOK {
		t.Fatalf("raw Web session import = %d: %s", code, raw)
	}
	var imported struct {
		ID             string   `json:"id"`
		CredentialMode string   `json:"credential_mode"`
		Warnings       []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &imported); err != nil {
		t.Fatalf("decode import: %v (%s)", err, raw)
	}
	if imported.ID == "" || imported.CredentialMode != authparse.CredentialModeChatGPTAuthTokens {
		t.Fatalf("raw Web session result = %+v", imported)
	}
	if !strings.Contains(strings.Join(imported.Warnings, " "), "renewal will use the encrypted session cookie") {
		t.Fatalf("raw Web session warnings = %v", imported.Warnings)
	}
	if strings.Contains(string(raw), accessToken) || strings.Contains(string(raw), sessionToken) {
		t.Fatalf("raw Web session response leaked a credential: %s", raw)
	}
	storedToken, err := h.store.GetToken(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedToken.AccessToken != accessToken || storedToken.RefreshToken != "" ||
		storedToken.AuthMethod != "access_token" ||
		storedToken.CredentialMode != authparse.CredentialModeChatGPTAuthTokens {
		t.Fatalf("stored raw Web session credentials = %+v", storedToken)
	}
	storedCookie, err := h.store.GetSessionCookie(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedCookie != "__Secure-next-auth.session-token="+sessionToken {
		t.Fatalf("stored raw Web session cookie = %q", storedCookie)
	}
}

func TestImportedChatGPTWebSessionUsesRealBearerAndUpdatesOnReimport(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-session\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[]}}\n\n"+
			"data: [DONE]\n\n")
	})
	now := time.Now()
	firstAccess := externalSessionJWTForTest(t, "user-session", "workspace-session", now.Add(20*time.Minute).Unix())
	code, raw := importExternalSessionForTest(t, h, "session-first", firstAccess, "Cookie: raw-session-cookie")
	if code != http.StatusOK {
		t.Fatalf("session import = %d: %s", code, raw)
	}
	var first struct {
		ID             string   `json:"id"`
		ImportStatus   string   `json:"import_status"`
		CredentialMode string   `json:"credential_mode"`
		Warnings       []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode session import: %v (%s)", err, raw)
	}
	if first.ID == "" || first.ImportStatus != "imported" || first.CredentialMode != "chatgpt_auth_tokens" || len(first.Warnings) == 0 {
		t.Fatalf("session import response = %+v", first)
	}
	if strings.Contains(string(raw), firstAccess) || strings.Contains(string(raw), "raw-session-cookie") {
		t.Fatalf("session import response leaked a credential: %s", raw)
	}
	token, err := h.store.GetToken(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != firstAccess || token.RefreshToken != "" || token.AuthMethod != "access_token" ||
		token.CredentialMode != "chatgpt_auth_tokens" || token.IDTokenRaw == "" || token.IDTokenRaw == firstAccess {
		t.Fatalf("stored external credentials = %+v", token)
	}
	cookie, err := h.store.GetSessionCookie(context.Background(), first.ID)
	if err != nil || cookie != "__Secure-next-auth.session-token=raw-session-cookie" {
		t.Fatalf("stored session cookie = %q err=%v", cookie, err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	responseRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forwarded response = %d: %s", resp.StatusCode, responseRaw)
	}
	var forwarded *capturedRequest
	for _, request := range h.requests() {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/responses") {
			copy := request
			forwarded = &copy
		}
	}
	if forwarded == nil || forwarded.Auth != "Bearer "+firstAccess || forwarded.AccountID != "workspace-session" {
		t.Fatalf("forwarded session headers = %+v", forwarded)
	}
	if strings.Contains(forwarded.Body, token.IDTokenRaw) {
		t.Fatal("metadata-only id_token leaked into the upstream request")
	}

	newerAccess := externalSessionJWTForTest(t, "user-session", "workspace-session", now.Add(40*time.Minute).Unix())
	code, raw = importExternalSessionForTest(t, h, "session-second", newerAccess, "")
	if code != http.StatusOK {
		t.Fatalf("newer session reimport = %d: %s", code, raw)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("decode updated session: %v", err)
	}
	if updated["updated"] != true || updated["import_status"] != "updated" || updated["id"] != first.ID {
		t.Fatalf("updated session response = %v", updated)
	}
	account, err := h.store.GetAccount(context.Background(), first.ID)
	if err != nil || account.Label != "session-first" {
		t.Fatalf("reimport changed account identity/label: %+v err=%v", account, err)
	}
	token, err = h.store.GetToken(context.Background(), first.ID)
	if err != nil || token.AccessToken != newerAccess {
		t.Fatalf("newer bearer was not persisted: %+v err=%v", token, err)
	}

	olderAccess := externalSessionJWTForTest(t, "user-session", "workspace-session", now.Add(30*time.Minute).Unix())
	code, raw = importExternalSessionForTest(t, h, "session-older", olderAccess, "")
	if code != http.StatusBadRequest || !strings.Contains(string(raw), `"code":"invalid_request"`) {
		t.Fatalf("older session reimport = %d: %s", code, raw)
	}
	token, err = h.store.GetToken(context.Background(), first.ID)
	if err != nil || token.AccessToken != newerAccess {
		t.Fatalf("older bearer replaced newer credential: %+v err=%v", token, err)
	}
}

func TestImportedChatGPTWebSessionCookieRemintRefreshesAllBearerMetadata(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	firstAccess := externalSessionJWTForTest(t, "user-remint", "workspace-remint", time.Now().Add(10*time.Minute).Unix())
	code, raw := importExternalSessionForTest(t, h, "session-remint", firstAccess, "raw-remint-cookie")
	if code != http.StatusOK {
		t.Fatalf("session import = %d: %s", code, raw)
	}
	var imported struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &imported); err != nil || imported.ID == "" {
		t.Fatalf("decode import: id=%q err=%v body=%s", imported.ID, err, raw)
	}
	refreshedExpiry := time.Now().Add(time.Hour).Unix()
	refreshedAccess := externalSessionJWTForTest(t, "user-remint", "workspace-remint", refreshedExpiry)
	originalClient := apiExternalHTTPClient
	t.Cleanup(func() { apiExternalHTTPClient = originalClient })
	apiExternalHTTPClient = &http.Client{Transport: oauthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://chatgpt.com/api/auth/session" {
			t.Fatalf("session refresh URL = %s", req.URL)
		}
		if req.Header.Get("Cookie") != "__Secure-next-auth.session-token=raw-remint-cookie" {
			t.Fatalf("session refresh Cookie = %q", req.Header.Get("Cookie"))
		}
		return oauthJSONResponse(http.StatusOK, `{"accessToken":"`+refreshedAccess+`"}`), nil
	})}
	token, err := h.store.GetToken(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	previousIDToken := token.IDTokenRaw
	result, err := h.app.refreshCodexToken(context.Background(), token)
	if err != nil || !result.Refreshed || result.Method != "session_cookie" {
		t.Fatalf("session cookie refresh = %+v err=%v", result, err)
	}
	stored, err := h.store.GetToken(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != refreshedAccess || stored.ExpiresAt != refreshedExpiry ||
		stored.IDTokenRaw == "" || stored.IDTokenRaw == previousIDToken ||
		stored.AuthMethod != "access_token" || stored.CredentialMode != "chatgpt_auth_tokens" {
		t.Fatalf("refreshed bearer metadata = %+v", stored)
	}
}

func TestImportedChatGPTWebSessionAuthenticatesUpstreamWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstreamErrors := make(chan error, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			upstreamErrors <- err
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			upstreamErrors <- err
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-session-ws","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}}`)); err != nil {
			upstreamErrors <- err
		}
	})
	accessToken := externalSessionJWTForTest(t, "user-session-ws", "workspace-session-ws", time.Now().Add(time.Hour).Unix())
	code, raw := importExternalSessionForTest(t, h, "session-ws", accessToken, "")
	if code != http.StatusOK {
		t.Fatalf("session import = %d: %s", code, raw)
	}

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		var responseBody []byte
		if response != nil && response.Body != nil {
			responseBody, _ = io.ReadAll(response.Body)
			response.Body.Close()
		}
		t.Fatalf("downstream WebSocket dial: %v body=%s", err, responseBody)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hello","stream":true}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, event, readErr := conn.ReadMessage()
		if readErr != nil {
			t.Fatalf("read downstream WebSocket: %v", readErr)
		}
		if strings.Contains(string(event), `"response.completed"`) {
			break
		}
	}
	select {
	case upstreamErr := <-upstreamErrors:
		t.Fatalf("upstream WebSocket: %v", upstreamErr)
	default:
	}
	var handshake *capturedRequest
	for _, request := range h.requests() {
		if request.Method == http.MethodGet && strings.HasSuffix(request.Path, "/responses") {
			copy := request
			handshake = &copy
		}
	}
	if handshake == nil || handshake.Auth != "Bearer "+accessToken || handshake.AccountID != "workspace-session-ws" {
		t.Fatalf("WebSocket session headers = %+v", handshake)
	}
}

func TestImportTopLevelSessionJSONAndDuplicateDoesNotOverwrite(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })

	body := `{"label":"first","auth_json_text":"{\"access_token\":\"access-one\",\"refresh_token\":\"refresh-one\",\"account_id\":\"acct-dup\",\"chatgpt_user_id\":\"user-dup\",\"email\":\"dup@example.internal\",\"plan_type\":\"plus\",\"last_refresh\":1750000000}"}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", body)
	if code != http.StatusOK {
		t.Fatalf("first import = %d: %s", code, raw)
	}
	var first map[string]interface{}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode first: %v (%s)", err, raw)
	}
	accountID, _ := first["id"].(string)
	if accountID == "" {
		t.Fatalf("first import missing id: %v", first)
	}
	if first["upstream_account_id"] != "acct-dup" || first["email"] != "dup@example.internal" || first["plan_type"] != "plus" {
		t.Fatalf("top-level metadata not stored: %v", first)
	}
	tok, err := h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-one" || tok.RefreshToken != "refresh-one" || tok.LastRefresh != 1750000000 {
		t.Fatalf("top-level token not stored: %+v", tok)
	}
	caps, err := h.store.ListCapabilities(context.Background(), accountID)
	if err != nil || len(caps) == 0 {
		t.Fatalf("auth.json import must synchronously seed model capabilities: caps=%+v err=%v", caps, err)
	}

	dupBody := strings.ReplaceAll(body, "first", "second")
	dupBody = strings.ReplaceAll(dupBody, "access-one", "access-two")
	code, raw = grpReq(t, h, http.MethodPost, "/admin/accounts/import-auth-json", dupBody)
	if code != http.StatusOK {
		t.Fatalf("duplicate import = %d: %s", code, raw)
	}
	var dup map[string]interface{}
	if err := json.Unmarshal(raw, &dup); err != nil {
		t.Fatalf("decode duplicate: %v (%s)", err, raw)
	}
	if dup["duplicate"] != true || dup["import_status"] != "duplicate" {
		t.Fatalf("duplicate response missing duplicate marker: %v", dup)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != "first" {
		t.Fatalf("duplicate import overwrote label: %+v", account)
	}
	tok, err = h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-one" {
		t.Fatalf("duplicate import overwrote token: %+v", tok)
	}
}
