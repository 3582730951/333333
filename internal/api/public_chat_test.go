package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestPublicChatLinkServesNoLoginChatThroughConfiguredUserGroup(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if !strings.Contains(body, `"input"`) || !strings.Contains(body, "hello from public chat") {
			t.Fatalf("public chat body was not converted to Responses input: %s", body)
		}
		if strings.Contains(body, "conversation_id") || strings.Contains(body, "public-chat-test") {
			t.Fatalf("public API envelope leaked to upstream: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer access-public-chat" {
			t.Fatalf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-public-chat","model":"gpt-public-chat","output_text":"public chat answer","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`))
	})
	if err := h.store.SetSetting(t.Context(), "require_downstream_key", "true"); err != nil {
		t.Fatal(err)
	}
	accountID := h.importAccount(t, "public-chat", "up-public-chat", "access-public-chat")
	setTestCapability(t, h, accountID, "gpt-public-chat", 272000)

	if code, raw := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"Public Chat Users",
		"targets":[{"kind":"account_pool_group","id":"cyber"}],
		"force_model":"gpt-public-chat"
	}`); code != http.StatusCreated {
		t.Fatalf("create user group = %d: %s", code, raw)
	} else {
		var group storage.UserGroup
		if err := json.Unmarshal(raw, &group); err != nil {
			t.Fatal(err)
		}
		linkBody := `{
			"slug":"public-chat-test",
			"name":"Public Chat Test",
			"enabled":true,
			"route_type":"user_group",
			"user_group_id":"` + group.ID + `",
			"model":"gpt-will-be-forced",
			"title":"测试聊天",
			"welcome_message":"欢迎",
			"max_history_messages":8,
			"rate_limit_per_minute":30
		}`
		if code, raw = grpReq(t, h, http.MethodPost, "/admin/public-chat/links", linkBody); code != http.StatusCreated {
			t.Fatalf("create public chat link = %d: %s", code, raw)
		}
		var link map[string]interface{}
		if err := json.Unmarshal(raw, &link); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "/chat/public-chat-test") || link["public_url"] == "" {
			t.Fatalf("public url missing: %s", raw)
		}
	}

	unauthGateway, _ := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-public-chat","messages":[{"role":"user","content":"hi"}]}`))
	if unauthGateway.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(unauthGateway.Body)
		unauthGateway.Body.Close()
		t.Fatalf("unauthenticated gateway status = %d body=%s", unauthGateway.StatusCode, raw)
	}
	unauthGateway.Body.Close()

	pageResp, err := http.Get(h.pool.URL + "/chat/public-chat-test")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(page), "测试聊天") {
		t.Fatalf("public page status=%d body=%s", pageResp.StatusCode, page)
	}

	cfgResp, err := http.Get(h.pool.URL + "/public-chat/public-chat-test/config")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.NewDecoder(cfgResp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK || cfg["title"] != "测试聊天" || cfg["welcome_message"] != "欢迎" {
		t.Fatalf("public config status=%d cfg=%+v", cfgResp.StatusCode, cfg)
	}

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/public-chat/public-chat-test/messages", strings.NewReader(`{
		"conversation_id":"conv_1",
		"messages":[
			{"role":"system","content":"must not forward as system"},
			{"role":"user","content":"hello from public chat"}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public chat status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("chat response JSON: %v body=%s", err, raw)
	}
	if got["object"] != "chat.completion" || !strings.Contains(string(raw), "public chat answer") {
		t.Fatalf("chat completion mismatch: %s", raw)
	}
	if len(h.requests()) != 1 {
		t.Fatalf("upstream request count = %d", len(h.requests()))
	}
	if h.requests()[0].TurnState != "" {
		t.Fatalf("unexpected turn state: %+v", h.requests()[0])
	}
}

func TestPublicChatDisabledAndRateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-rate","model":"gpt","output_text":"ok"}`))
	})
	accountID := h.importAccount(t, "public-rate", "up-public-rate", "access-public-rate")
	setTestCapability(t, h, accountID, "gpt", 272000)

	enabled, err := h.store.UpsertPublicChatLink(t.Context(), storage.PublicChatLink{
		Slug:               "rate-chat",
		Enabled:            true,
		RouteType:          storage.PublicChatRouteAccountPoolGroup,
		GroupName:          "cyber",
		Model:              "gpt",
		RateLimitPerMinute: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := h.store.UpsertPublicChatLink(t.Context(), storage.PublicChatLink{
		Slug:      "disabled-chat",
		Enabled:   false,
		RouteType: storage.PublicChatRouteAccountPoolGroup,
		GroupName: "cyber",
		Model:     "gpt",
	})
	if err != nil || disabled.ID == "" || enabled.ID == "" {
		t.Fatalf("create links enabled=%+v disabled=%+v err=%v", enabled, disabled, err)
	}

	body := `{"conversation_id":"c","messages":[{"role":"user","content":"hi"}]}`
	if resp, err := http.Post(h.pool.URL+"/public-chat/disabled-chat/messages", "application/json", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled status=%d body=%s", resp.StatusCode, raw)
		}
	}
	if resp, err := http.Post(h.pool.URL+"/public-chat/rate-chat/messages", "application/json", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first rate request status=%d body=%s", resp.StatusCode, raw)
		}
	}
	if resp, err := http.Post(h.pool.URL+"/public-chat/rate-chat/messages", "application/json", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second rate request status=%d body=%s", resp.StatusCode, raw)
		}
	}
}

func TestPublicChatRateLimitUsesTrustedProxyChain(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.TrustedProxyCIDRs = []string{"127.0.0.1/32"}
	link := storage.PublicChatLink{ID: "public-client-ip", RateLimitPerMinute: 1}

	direct := httptest.NewRequest(http.MethodPost, "http://pool.test/public-chat/test/messages", nil)
	direct.RemoteAddr = "198.51.100.20:1234"
	direct.Header.Set("CF-Connecting-IP", "203.0.113.1")
	direct.Header.Set("X-Real-IP", "203.0.113.2")
	direct.Header.Set("X-Forwarded-For", "203.0.113.3")
	if got := h.app.clientIP(direct); got != "198.51.100.20" {
		t.Fatalf("direct request client IP = %q", got)
	}
	if !h.app.publicChatRateAllowed(link, h.app.clientIP(direct)) {
		t.Fatal("first direct request should be admitted")
	}

	spoofedAgain := direct.Clone(direct.Context())
	spoofedAgain.Header = direct.Header.Clone()
	spoofedAgain.Header.Set("CF-Connecting-IP", "203.0.113.11")
	spoofedAgain.Header.Set("X-Real-IP", "203.0.113.12")
	spoofedAgain.Header.Set("X-Forwarded-For", "203.0.113.13")
	if h.app.publicChatRateAllowed(link, h.app.clientIP(spoofedAgain)) {
		t.Fatal("forged forwarding headers bypassed the direct-client rate limit")
	}

	proxied := httptest.NewRequest(http.MethodPost, "http://pool.test/public-chat/test/messages", nil)
	proxied.RemoteAddr = "127.0.0.1:4321"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.20")
	if got := h.app.clientIP(proxied); got != "203.0.113.20" {
		t.Fatalf("trusted proxy client IP = %q", got)
	}
	if !h.app.publicChatRateAllowed(link, h.app.clientIP(proxied)) {
		t.Fatal("trusted proxy client identity should have its own rate window")
	}

	for index, test := range []struct {
		header string
		ip     string
	}{
		{header: "CF-Connecting-IP", ip: "203.0.113.21"},
		{header: "X-Real-IP", ip: "203.0.113.22"},
	} {
		t.Run(test.header, func(t *testing.T) {
			fallback := httptest.NewRequest(http.MethodPost, "http://pool.test/public-chat/test/messages", nil)
			fallback.RemoteAddr = "127.0.0.1:4321"
			fallback.Header.Set(test.header, test.ip)
			if got := h.app.clientIP(fallback); got != test.ip {
				t.Fatalf("trusted proxy fallback client IP = %q", got)
			}
			fallbackLink := link
			fallbackLink.ID += fmt.Sprintf("-%d", index)
			if !h.app.publicChatRateAllowed(fallbackLink, h.app.clientIP(fallback)) {
				t.Fatal("trusted proxy fallback identity should have its own rate window")
			}
		})
	}
}

func TestPublicChatRateLimiterIsStrictlyBounded(t *testing.T) {
	const nowMinute = int64(20_000)
	s := &Server{
		publicChatLimiter:    map[string]publicChatRateWindow{},
		publicChatLastMinute: nowMinute,
	}
	link := storage.PublicChatLink{ID: "bounded-public-chat", RateLimitPerMinute: 2}

	for i := 0; i < publicChatLimiterCapacity; i++ {
		if !s.publicChatRateAllowedAt(link, fmt.Sprintf("client-%d", i), nowMinute) {
			t.Fatalf("entry %d rejected before capacity", i)
		}
	}
	if got := len(s.publicChatLimiter); got != publicChatLimiterCapacity {
		t.Fatalf("limiter size = %d, want %d", got, publicChatLimiterCapacity)
	}
	if !s.publicChatRateAllowedAt(link, "client-0", nowMinute) {
		t.Fatal("oldest client did not receive its final allowed request")
	}
	if window := s.publicChatLimiter[link.ID+"\x00client-0"]; !window.Blocked {
		t.Fatal("window was not marked blocked when it reached the limit")
	}
	if !s.publicChatRateAllowedAt(link, "overflow-client", nowMinute) {
		t.Fatal("new client was rejected instead of evicting the oldest window")
	}
	if got := len(s.publicChatLimiter); got != publicChatLimiterCapacity {
		t.Fatalf("limiter grew past capacity: %d", got)
	}
	if _, ok := s.publicChatLimiter[link.ID+"\x00client-0"]; !ok {
		t.Fatal("blocked window was evicted under capacity pressure")
	}
	if _, ok := s.publicChatLimiter[link.ID+"\x00client-1"]; ok {
		t.Fatal("oldest non-blocked window survived capacity eviction")
	}
	if _, ok := s.publicChatLimiter[link.ID+"\x00overflow-client"]; !ok {
		t.Fatal("new client window was not admitted")
	}
	newestClient := fmt.Sprintf("client-%d", publicChatLimiterCapacity-1)
	if !s.publicChatRateAllowedAt(link, newestClient, nowMinute) || s.publicChatRateAllowedAt(link, newestClient, nowMinute) {
		t.Fatal("retained client lost its exact per-minute count")
	}

	if !s.publicChatRateAllowedAt(link, "next-minute-client", nowMinute+1) {
		t.Fatal("next-minute client was rejected after expired windows were swept")
	}
	if got := len(s.publicChatLimiter); got != 1 {
		t.Fatalf("limiter size after minute rollover = %d, want 1", got)
	}
}

func TestPublicChatRateLimiterRejectsNewIdentityWhenAllWindowsBlocked(t *testing.T) {
	const nowMinute = int64(21_000)
	s := &Server{
		publicChatLimiter:    make(map[string]publicChatRateWindow, publicChatLimiterCapacity),
		publicChatLastMinute: nowMinute,
	}
	for i := 0; i < publicChatLimiterCapacity; i++ {
		s.publicChatLimiter[fmt.Sprintf("blocked-%d", i)] = publicChatRateWindow{
			Minute: nowMinute, Count: 1, Sequence: uint64(i + 1), Blocked: true,
		}
	}
	link := storage.PublicChatLink{ID: "all-blocked", RateLimitPerMinute: 1}
	if s.publicChatRateAllowedAt(link, "new-client", nowMinute) {
		t.Fatal("new identity was admitted by evicting an active blocked window")
	}
	if got := len(s.publicChatLimiter); got != publicChatLimiterCapacity {
		t.Fatalf("limiter size = %d, want %d", got, publicChatLimiterCapacity)
	}
	if _, ok := s.publicChatLimiter[link.ID+"\x00new-client"]; ok {
		t.Fatal("rejected identity was inserted into a full blocked table")
	}
}

func TestPublicChatRateLimiterSweepsOncePerMinute(t *testing.T) {
	const nowMinute = int64(10_000)
	s := &Server{
		publicChatLimiter: map[string]publicChatRateWindow{
			"expired": {Minute: nowMinute - 1, Count: 1},
		},
		publicChatLastMinute: nowMinute - 1,
	}
	link := storage.PublicChatLink{ID: "sweep-public-chat", RateLimitPerMinute: 2}

	if !s.publicChatRateAllowedAt(link, "first", nowMinute) {
		t.Fatal("request after minute rollover should be admitted")
	}
	if _, ok := s.publicChatLimiter["expired"]; ok {
		t.Fatal("expired entry survived minute rollover cleanup")
	}

	// An entry inserted after this minute's sweep remains until the next minute,
	// proving requests do not repeatedly scan the whole table in the same minute.
	s.publicChatLimiter["late-expired"] = publicChatRateWindow{Minute: nowMinute - 1, Count: 1}
	if !s.publicChatRateAllowedAt(link, "second", nowMinute) {
		t.Fatal("second request should be admitted")
	}
	if _, ok := s.publicChatLimiter["late-expired"]; !ok {
		t.Fatal("limiter performed more than one cleanup sweep in the same minute")
	}
}
