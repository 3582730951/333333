package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminEgressProfileTestDraftDoesNotPersist(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"203.0.113.10","country_code":"US","region":"CA","city":"Cupertino"}`))
	}))
	defer geo.Close()

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles/test", `{
		"profile":{"id":"draft_test","type":"direct","region":"BR"},
		"probe_url":"`+geo.URL+`"
	}`)
	if code != http.StatusOK {
		t.Fatalf("test draft profile = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	if got["ok"] != true || got["exit_ip"] != "203.0.113.10" || got["region"] != "US" {
		t.Fatalf("unexpected probe response: %#v", got)
	}
	warnings, _ := got["warnings"].([]interface{})
	if len(warnings) == 0 || !strings.Contains(warnings[0].(string), "BR") {
		t.Fatalf("expected region mismatch warning, got %#v", got["warnings"])
	}
	if _, err := h.store.GetEgressProfile(context.Background(), "draft_test"); err == nil {
		t.Fatal("draft egress profile was persisted")
	}
}

func TestAdminEgressProfileTestRejectsInvalidProxyEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles/test", `{
		"profile":{"type":"http_proxy","endpoint":"http://%"},
		"probe_url":"http://127.0.0.1/"
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid proxy endpoint = %d, want 400: %s", code, raw)
	}
	if !strings.Contains(string(raw), "invalid") && !strings.Contains(string(raw), "escape") {
		t.Fatalf("invalid proxy endpoint response should explain parse failure: %s", raw)
	}
}

func TestAdminEgressProfileTestExistingProfile(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"198.51.100.2","countryCode":"JP","regionName":"Tokyo"}`))
	}))
	defer geo.Close()

	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID: "direct_probe", Name: "direct probe", Type: "direct", Health: "healthy", MaxConcurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles/test", `{
		"egress_id":"direct_probe",
		"probe_url":"`+geo.URL+`"
	}`)
	if code != http.StatusOK {
		t.Fatalf("test existing profile = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	if got["egress_id"] != "direct_probe" || got["exit_ip"] != "198.51.100.2" || got["region"] != "JP" {
		t.Fatalf("unexpected existing profile probe response: %#v", got)
	}
}

func TestAdminEgressProfileTestCliproxyAPIWhitelist(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("geo probe should route through the extracted proxy, got direct request %s", r.URL.String())
	}))
	defer geo.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.String(); got != geo.URL+"/" && got != geo.URL {
			t.Fatalf("proxy received unexpected target URL %q, want %q", got, geo.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"192.0.2.55","country_code":"BR","region":"SP","city":"Sao Paulo"}`))
	}))
	defer proxy.Close()

	cliproxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/white/api" {
			t.Fatalf("unexpected cliproxy path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("region"); got != "BR" {
			t.Fatalf("cliproxy region = %q, want BR", got)
		}
		if got := r.URL.Query().Get("time"); got != "5" {
			t.Fatalf("cliproxy time = %q, want 5", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("cliproxy auth = %q", got)
		}
		_, _ = w.Write([]byte(strings.TrimPrefix(proxy.URL, "http://") + "\n"))
	}))
	defer cliproxy.Close()

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles/test", `{
		"profile":{
			"type":"http_proxy",
			"proxy_auth_mode":"api_whitelist",
			"proxy_api_key":"test-key",
			"api_base":"`+cliproxy.URL+`",
			"api_num":1,
			"api_time":5,
			"region":"BR"
		},
		"probe_url":"`+geo.URL+`/"
	}`)
	if code != http.StatusOK {
		t.Fatalf("test cliproxy api profile = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	if got["exit_ip"] != "192.0.2.55" || got["region"] != "BR" {
		t.Fatalf("unexpected cliproxy api probe response: %#v", got)
	}
}
