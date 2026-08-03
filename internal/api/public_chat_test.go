package api

import (
	"encoding/json"
	"io"
	"net/http"
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
