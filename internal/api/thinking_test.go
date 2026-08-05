package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
)

func TestThinkingConfigPersistsAndFeedsUpstreamOverlay(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	h.app.cfg.AdminToken = "secret"

	payload := `{
		"enabled":true,
		"default_mode":"level",
		"default_level":"high",
		"default_budget":16384,
		"providers":{"claude":{"mode":"level","level":"max"}},
		"models":{}
	}`
	resp, body := thinkingAdminReq(t, h, http.MethodPost, "/admin/thinking", payload, "secret")
	if resp.StatusCode != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("save status=%d body=%v", resp.StatusCode, body)
	}
	if h.app.cfg.ThinkingEnabled {
		t.Fatal("save mutated the shared boot config")
	}
	if raw, ok, err := h.store.GetSetting(context.Background(), thinkingConfigSettingKey); err != nil || !ok || !strings.Contains(raw, `"default_level":"high"`) {
		t.Fatalf("persisted setting ok=%v err=%v raw=%q", ok, err, raw)
	}

	effective := h.app.effectiveUpstreamConfig(context.Background())
	if !effective.ThinkingEnabled || effective.ThinkingDefaultLevel != "high" || effective.ThinkingProviders["claude"].Level != "max" {
		t.Fatalf("upstream overlay did not load persisted config: %+v", thinkingSettingsFromConfig(effective))
	}

	resp, body = thinkingAdminReq(t, h, http.MethodGet, "/admin/thinking", "", "secret")
	if resp.StatusCode != http.StatusOK || body["enabled"] != true || body["default_level"] != "high" {
		t.Fatalf("get status=%d body=%v", resp.StatusCode, body)
	}
	providers, _ := body["providers"].(map[string]interface{})
	claude, _ := providers["claude"].(map[string]interface{})
	if claude["level"] != "max" {
		t.Fatalf("provider override was not reloaded: %v", body)
	}

	preview := `{"provider":"claude","model":"claude-opus-4-8","body":{"max_tokens":4096,"messages":[]}}`
	resp, body = thinkingAdminReq(t, h, http.MethodPost, "/admin/thinking/preview", preview, "secret")
	if resp.StatusCode != http.StatusOK || body["source"] != "provider override" {
		t.Fatalf("preview status=%d body=%v", resp.StatusCode, body)
	}
	applied, _ := body["applied_body"].(map[string]interface{})
	thinkingBody, _ := applied["thinking"].(map[string]interface{})
	outputConfig, _ := applied["output_config"].(map[string]interface{})
	if thinkingBody["type"] != "adaptive" || outputConfig["effort"] != "max" {
		t.Fatalf("preview did not apply persisted config: %v", applied)
	}
}

func TestThinkingConfigRejectsInvalidSemanticsWithoutPersistOrPublish(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.AdminToken = "secret"

	prior := thinkingSettings{
		Enabled:       true,
		DefaultMode:   "level",
		DefaultLevel:  "high",
		DefaultBudget: 16384,
		Providers:     map[string]config.ThinkingOverride{"claude": {Mode: "level", Level: "max"}},
		Models:        map[string]config.ThinkingOverride{},
	}
	priorRaw, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), thinkingConfigSettingKey, string(priorRaw)); err != nil {
		t.Fatal(err)
	}
	publishCount := 0
	h.app.upstreamConfigPublishHook = func(config.Config) {
		publishCount++
	}

	tests := []struct {
		name string
		body string
	}{
		{
			name: "invalid default mode",
			body: `{"enabled":true,"default_mode":"turbo","default_level":"high","default_budget":16384,"providers":{},"models":{}}`,
		},
		{
			name: "invalid default level",
			body: `{"enabled":true,"default_mode":"level","default_level":"extreme","default_budget":16384,"providers":{},"models":{}}`,
		},
		{
			name: "negative default budget",
			body: `{"enabled":true,"default_mode":"budget","default_level":"high","default_budget":-1,"providers":{},"models":{}}`,
		},
		{
			name: "invalid provider mode",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{"claude":{"mode":"turbo"}},"models":{}}`,
		},
		{
			name: "invalid provider level",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{"claude":{"mode":"level","level":"extreme"}},"models":{}}`,
		},
		{
			name: "negative provider budget",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{"claude":{"mode":"budget","budget":-2}},"models":{}}`,
		},
		{
			name: "invalid model mode",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{},"models":{"claude-opus":{"mode":"turbo"}}}`,
		},
		{
			name: "invalid model level",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{},"models":{"claude-opus":{"mode":"level","level":"extreme"}}}`,
		},
		{
			name: "negative model budget",
			body: `{"enabled":true,"default_mode":"level","default_level":"high","default_budget":16384,"providers":{},"models":{"claude-opus":{"mode":"budget","budget":-2}}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := thinkingAdminReq(t, h, http.MethodPost, "/admin/thinking", tc.body, "secret")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%v", resp.StatusCode, http.StatusBadRequest, body)
			}
			assertErrorEnvelope(t, body, http.StatusBadRequest)

			gotRaw, ok, err := h.store.GetSetting(context.Background(), thinkingConfigSettingKey)
			if err != nil || !ok {
				t.Fatalf("read prior setting: ok=%v err=%v", ok, err)
			}
			if gotRaw != string(priorRaw) {
				t.Fatalf("persisted config changed after invalid request:\n got %s\nwant %s", gotRaw, priorRaw)
			}
			if publishCount != 0 {
				t.Fatalf("invalid request published %d live configurations", publishCount)
			}
		})
	}
}

func TestThinkingConfigPersistenceAndPublishAreSerialized(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := thinkingSettings{Enabled: true, DefaultMode: "level", DefaultLevel: "low", Providers: map[string]config.ThinkingOverride{}, Models: map[string]config.ThinkingOverride{}}
	second := thinkingSettings{Enabled: true, DefaultMode: "level", DefaultLevel: "max", Providers: map[string]config.ThinkingOverride{}, Models: map[string]config.ThinkingOverride{}}
	firstRaw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}

	firstPublishing := make(chan struct{})
	releaseFirst := make(chan struct{})
	published := make([]string, 0, 2)
	h.app.upstreamConfigPublishHook = func(cfg config.Config) {
		published = append(published, cfg.ThinkingDefaultLevel)
		if cfg.ThinkingDefaultLevel == first.DefaultLevel {
			close(firstPublishing)
			<-releaseFirst
		}
	}

	done := make(chan error, 2)
	go func() {
		done <- h.app.persistAndPublishEffectiveUpstreamConfig(context.Background(), func(ctx context.Context) error {
			return h.store.SetSetting(ctx, thinkingConfigSettingKey, string(firstRaw))
		})
	}()
	select {
	case <-firstPublishing:
	case <-time.After(time.Second):
		t.Fatal("first configuration did not reach the publish hook")
	}

	secondStarted := make(chan struct{})
	secondPersistEntered := make(chan struct{})
	go func() {
		close(secondStarted)
		done <- h.app.persistAndPublishEffectiveUpstreamConfig(context.Background(), func(ctx context.Context) error {
			close(secondPersistEntered)
			return h.store.SetSetting(ctx, thinkingConfigSettingKey, string(secondRaw))
		})
	}()
	<-secondStarted
	select {
	case <-secondPersistEntered:
		t.Fatal("second persistence entered while the first snapshot was still publishing")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("serialized configuration update did not finish")
		}
	}
	if got := strings.Join(published, ","); got != "low,max" {
		t.Fatalf("published snapshots = %q, want low,max", got)
	}
	raw, ok, err := h.store.GetSetting(context.Background(), thinkingConfigSettingKey)
	if err != nil || !ok {
		t.Fatalf("read final persisted setting: ok=%v err=%v", ok, err)
	}
	var final thinkingSettings
	if err := json.Unmarshal([]byte(raw), &final); err != nil {
		t.Fatal(err)
	}
	if final.DefaultLevel != second.DefaultLevel {
		t.Fatalf("final persisted level = %q, want %q", final.DefaultLevel, second.DefaultLevel)
	}
}

func TestThinkingAdminErrorsUseJSONEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	h.app.cfg.AdminToken = "secret"

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		token      string
		wantStatus int
	}{
		{
			name:       "unauthorized",
			method:     http.MethodGet,
			path:       "/admin/thinking",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "method not allowed",
			method:     http.MethodPut,
			path:       "/admin/thinking",
			token:      "secret",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "invalid save json",
			method:     http.MethodPost,
			path:       "/admin/thinking",
			body:       "{",
			token:      "secret",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversized save json",
			method:     http.MethodPost,
			path:       "/admin/thinking",
			body:       `{"enabled":true}` + strings.Repeat(" ", adminJSONBodyLimit),
			token:      "secret",
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:       "preview missing provider model",
			method:     http.MethodPost,
			path:       "/admin/thinking/preview",
			body:       "{}",
			token:      "secret",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := thinkingAdminReq(t, h, tc.method, tc.path, tc.body, tc.token)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%v", resp.StatusCode, tc.wantStatus, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
			assertErrorEnvelope(t, body, tc.wantStatus)
		})
	}
}

func thinkingAdminReq(t *testing.T, h *testHarness, method, path, body, token string) (*http.Response, map[string]interface{}) {
	t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.pool.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return resp, decoded
}

func assertErrorEnvelope(t *testing.T, body map[string]interface{}, status int) {
	t.Helper()

	errBody, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing JSON error envelope: %v", body)
	}
	if typ, _ := errBody["type"].(string); typ != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", typ)
	}
	wantMessage, wantCode := safeClientError(status)
	if msg, _ := errBody["message"].(string); msg != wantMessage {
		t.Fatalf("error.message = %q, want %q", msg, wantMessage)
	}
	if code, _ := errBody["code"].(string); code != wantCode {
		t.Fatalf("error.code = %q, want %q", code, wantCode)
	}
	if requestID, _ := errBody["request_id"].(string); !strings.HasPrefix(requestID, "REQ-") {
		t.Fatalf("error.request_id = %q", requestID)
	}
}
