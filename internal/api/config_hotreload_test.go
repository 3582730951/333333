package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// config_hotreload_test.go is the end-to-end guard for Phase ① (requirement #1):
// the /admin/config registry lists the runtime-editable knobs, and a PATCH takes
// effect at REQUEST time with no restart. We prove the hot path with
// require_downstream_key — flipping it on must make a keyless gateway request 401,
// flipping it off must stop rejecting — which can only happen if the request path
// reads the setting live (not the boot config).

func adminConfigRows(t *testing.T, h *testHarness) []map[string]interface{} {
	t.Helper()
	resp, err := http.Get(h.pool.URL + "/admin/config")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/config = %d: %s", resp.StatusCode, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode /admin/config: %v (%s)", err, raw)
	}
	return rows
}

func findConfigRow(rows []map[string]interface{}, key string) map[string]interface{} {
	for _, r := range rows {
		if r["key"] == key {
			return r
		}
	}
	return nil
}

func patchConfig(t *testing.T, h *testHarness, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, h.pool.URL+"/admin/config", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /admin/config %s = %d: %s", body, resp.StatusCode, raw)
	}
}

func patchConfigStatus(t *testing.T, h *testHarness, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, h.pool.URL+"/admin/config", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

func postSettingsCenter(t *testing.T, h *testHarness, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.pool.URL+"/admin/settings-center", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

func postSettingsTemplate(t *testing.T, h *testHarness, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.pool.URL+"/admin/settings-center/apply-template", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

func getSettingsCenter(t *testing.T, h *testHarness, query string) (int, map[string]interface{}, string) {
	t.Helper()
	resp, err := http.Get(h.pool.URL + "/admin/settings-center" + query)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var body map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	return resp.StatusCode, body, string(raw)
}

func getAdminSettings(t *testing.T, h *testHarness) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(h.pool.URL + "/admin/settings")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/settings = %d: %s", resp.StatusCode, raw)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode /admin/settings: %v (%s)", err, raw)
	}
	return body
}

func TestSettingsCenterAppliesKiroNoDegradationTemplate(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsTemplate(t, h, `{"template_id":"kiro-no-degradation"}`)
	if status != http.StatusOK {
		t.Fatalf("apply optimal template status = %d: %s", status, body)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode template response: %v\n%s", err, body)
	}
	if got["id"] != "optimal-stable-models-v1" {
		t.Fatalf("template id = %#v", got["id"])
	}
	saved, _ := got["saved"].([]interface{})
	if len(saved) == 0 {
		t.Fatalf("template did not report saved diffs: %#v", got)
	}
	status, center, raw := getSettingsCenter(t, h, "?sections=config")
	if status != http.StatusOK {
		t.Fatalf("GET config after template = %d: %s", status, raw)
	}
	rows, _ := center["config"].([]interface{})
	values := map[string]interface{}{}
	for _, row := range rows {
		m, _ := row.(map[string]interface{})
		key, _ := m["key"].(string)
		if key != "" {
			values[key] = m["value"]
		}
	}
	wants := map[string]interface{}{
		"conversation_isolation":          true,
		"rate_limit_guard_enabled":        true,
		"seamless_failover":               true,
		"leak_scrub":                      true,
		"stream_keepalive_seconds":        float64(15),
		"stream_stall_recovery_seconds":   float64(360),
		"stream_auto_continue_enabled":    true,
		"codex_session_mapping_enabled":   true,
		"codex_cpa_strict":                true,
		"codex_stateless_passthrough":     false,
		"goal_continuity_enabled":         true,
		"token_save_enabled":              false,
		"kiro_version":                    "0.11.107",
		"kiro_node_version":               "22.22.0",
		"kiro_default_auth_region":        "us-east-1",
		"kiro_default_api_region":         "us-east-1",
		"kiro_default_thinking":           true,
		"kiro_cache_mode":                 "auto",
		"kiro_cache_unreported_threshold": float64(20),
		"kiro_affinity_wait_millis":       float64(0),
	}
	for key, want := range wants {
		if values[key] != want {
			t.Fatalf("template value %s = %#v, want %#v", key, values[key], want)
		}
	}
}

func TestLegacyOptimalTemplateIDMapsToKiroNoDegradation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	status, body := postSettingsTemplate(t, h, `{"template_id":"optimal-codex-pool"}`)
	if status != http.StatusOK {
		t.Fatalf("legacy template status = %d: %s", status, body)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "optimal-stable-models-v1" {
		t.Fatalf("legacy template resolved to %#v", got["id"])
	}
}

func patchAdminSettingsStatus(t *testing.T, h *testHarness, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, h.pool.URL+"/admin/settings", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

func settingsSection(t *testing.T, body map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	section, ok := body[key].(map[string]interface{})
	if !ok {
		t.Fatalf("settings-center response missing %q section: %#v", key, body)
	}
	return section
}

func settingsErrorContains(t *testing.T, section map[string]interface{}, key, sub string) bool {
	t.Helper()
	errors, ok := section["settings_errors"].(map[string]interface{})
	if !ok {
		t.Fatalf("settings_errors missing from section: %#v", section)
	}
	got, _ := errors[key].(string)
	return strings.Contains(got, sub)
}

func nodeRegistrarConfig(t *testing.T, h *testHarness) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(h.pool.URL + "/admin/register/node-config")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET node registrar config = %d: %s", resp.StatusCode, raw)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode node registrar config: %v (%s)", err, raw)
	}
	return cfg
}

func keylessResponsesStatus(t *testing.T, h *testHarness) int {
	t.Helper()
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAdminConfigListsFieldsAndHotApplies(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")

	// Registry lists the expected knobs (an upstream-effect field + a hot field).
	rows := adminConfigRows(t, h)
	for _, want := range []string{"require_downstream_key", "codex_ja3", "conversation_isolation", "failover_max_attempts", "claude_native_cache_breakpoint_inject"} {
		if findConfigRow(rows, want) == nil {
			t.Fatalf("/admin/config registry missing %q", want)
		}
	}
	if row := findConfigRow(rows, "claude_native_cache_breakpoint_inject"); row["value"] != true || row["effect"] != "hot" {
		t.Fatalf("native cache breakpoint default/effect = %v, want true/hot", row)
	}

	// Hot apply: enabling require_downstream_key must reject a keyless request NOW.
	patchConfig(t, h, `{"require_downstream_key": true}`)
	if code := keylessResponsesStatus(t, h); code != http.StatusUnauthorized {
		t.Fatalf("hot require_downstream_key not applied: keyless request = %d, want 401", code)
	}
	// Disabling it must stop the rejection (request proceeds past the key gate).
	patchConfig(t, h, `{"require_downstream_key": false}`)
	if code := keylessResponsesStatus(t, h); code == http.StatusUnauthorized {
		t.Fatalf("require_downstream_key still enforced after disabling (got 401)")
	}

	// Persistence + registry round-trip for an upstream-effect string field. The PATCH
	// also exercises upstream.Client.UpdateConfig (effectiveUpstreamConfig) without panic.
	patchConfig(t, h, `{"codex_ja3": "771,4865,0"}`)
	row := findConfigRow(adminConfigRows(t, h), "codex_ja3")
	if row == nil || row["value"] != "771,4865,0" || row["overridden"] != true {
		t.Fatalf("codex_ja3 override not reflected in /admin/config: %v", row)
	}
	patchConfig(t, h, `{"claude_native_cache_breakpoint_inject": false}`)
	row = findConfigRow(adminConfigRows(t, h), "claude_native_cache_breakpoint_inject")
	if row == nil || row["value"] != false || row["overridden"] != true {
		t.Fatalf("native cache breakpoint override not reflected in /admin/config: %v", row)
	}
}

func TestAdminConfigHotAppliesSchedulerFields(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	rows := adminConfigRows(t, h)
	for _, want := range []string{"sticky_wait_millis", "account_token_budget", "strict_sticky_max_cooldown_seconds", "cooldown_wait_max_seconds", "stateful_sticky_wait_seconds"} {
		row := findConfigRow(rows, want)
		if row == nil {
			t.Fatalf("/admin/config registry missing scheduler field %q", want)
		}
		if row["effect"] != "scheduler" {
			t.Fatalf("%s effect = %#v, want scheduler", want, row["effect"])
		}
	}

	patchConfig(t, h, `{"sticky_wait_millis":25,"account_token_budget":12345,"strict_sticky_max_cooldown_seconds":0,"cooldown_wait_max_seconds":2,"stateful_sticky_wait_seconds":3}`)
	cfg := h.app.scheduler.Config()
	if cfg.StickyWaitMillis != 25 {
		t.Fatalf("scheduler StickyWaitMillis = %d, want 25", cfg.StickyWaitMillis)
	}
	if cfg.AccountTokenBudget != 12345 {
		t.Fatalf("scheduler AccountTokenBudget = %d, want 12345", cfg.AccountTokenBudget)
	}
	if cfg.StrictStickyMaxCooldownSeconds != 0 {
		t.Fatalf("scheduler StrictStickyMaxCooldownSeconds = %d, want 0", cfg.StrictStickyMaxCooldownSeconds)
	}
	if cfg.CooldownWaitMaxSeconds != 2 {
		t.Fatalf("scheduler CooldownWaitMaxSeconds = %d, want 2", cfg.CooldownWaitMaxSeconds)
	}
	if cfg.StatefulStickyWaitSeconds != 3 {
		t.Fatalf("scheduler StatefulStickyWaitSeconds = %d, want 3", cfg.StatefulStickyWaitSeconds)
	}

	patchConfig(t, h, `{"stateful_sticky_wait_seconds":-1}`)
	cfg = h.app.scheduler.Config()
	if cfg.StatefulStickyWaitSeconds != 0 {
		t.Fatalf("scheduler StatefulStickyWaitSeconds after negative = %d, want 0", cfg.StatefulStickyWaitSeconds)
	}
}

func TestLegacyAdminSettingsReflectsRuntimeConfigOverrides(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"require_downstream_key":true,"web_search_enabled":true,"ban_detection_enabled":false,"ban_auto_delete":true,"rate_limit_guard_enabled":false,"identity_os_source":"diverse","claude_cache_ttl":"1h"}}]`)
	if status != http.StatusOK {
		t.Fatalf("settings-center config patch status = %d: %s", status, body)
	}
	got := getAdminSettings(t, h)
	wants := map[string]interface{}{
		"require_downstream_key":   true,
		"web_search_enabled":       true,
		"ban_detection_enabled":    false,
		"ban_auto_delete":          true,
		"rate_limit_guard_enabled": false,
		"identity_os_source":       "diverse",
		"claude_cache_ttl":         "1h",
	}
	for key, want := range wants {
		if got[key] != want {
			t.Fatalf("/admin/settings %s = %#v, want %#v (full=%v)", key, got[key], want, got)
		}
	}
}

func TestLegacyAdminSettingsRejectsUnknownAndInvalidValues(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchAdminSettingsStatus(t, h, `{"not_a_legacy_setting":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown /admin/settings key status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "not_a_legacy_setting") {
		t.Fatalf("unknown /admin/settings body = %q", body)
	}

	status, body = patchAdminSettingsStatus(t, h, `{"allow_registration":"maybe"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid /admin/settings bool status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "expected boolean") {
		t.Fatalf("invalid /admin/settings bool body = %q", body)
	}

	status, body = patchAdminSettingsStatus(t, h, `{"allow_registration":false,"not_a_legacy_setting":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("mixed invalid /admin/settings status = %d, want 400: %s", status, body)
	}
	got := getAdminSettings(t, h)
	if got["allow_registration"] != true {
		t.Fatalf("invalid mixed /admin/settings patch partially persisted allow_registration: %#v", got)
	}

	status, body = patchAdminSettingsStatus(t, h, `{"allow_registration":"off"}`)
	if status != http.StatusOK {
		t.Fatalf("valid string bool /admin/settings status = %d: %s", status, body)
	}
	got = getAdminSettings(t, h)
	if got["allow_registration"] != false {
		t.Fatalf("allow_registration = %#v, want false after valid string bool", got["allow_registration"])
	}
}

func TestAdminConfigRejectsOversizedAndMultiValueJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchConfigStatus(t, h, `{"require_downstream_key":true}`+strings.Repeat(" ", adminJSONBodyLimit))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized /admin/config status = %d, want 413: %s", status, body)
	}
	if !strings.Contains(body, `"code":"request_too_large"`) {
		t.Fatalf("oversized /admin/config body = %q, want size error", body)
	}

	status, body = patchConfigStatus(t, h, `{"require_downstream_key":true} {"web_search_enabled":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("multi-value /admin/config status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "single JSON value") {
		t.Fatalf("multi-value /admin/config body was not safely normalized: %q", body)
	}
	row := findConfigRow(adminConfigRows(t, h), "require_downstream_key")
	if row == nil || row["value"] != false || row["overridden"] != false {
		t.Fatalf("invalid /admin/config body should not persist require_downstream_key: %v", row)
	}
}

func TestSettingsCenterRejectsOversizedJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"require_downstream_key":true}}]`+strings.Repeat(" ", adminJSONBodyLimit))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settings-center status = %d, want 413: %s", status, body)
	}
	if !strings.Contains(body, `"code":"request_too_large"`) {
		t.Fatalf("oversized settings-center body = %q, want size error", body)
	}
	row := findConfigRow(adminConfigRows(t, h), "require_downstream_key")
	if row == nil || row["value"] != false || row["overridden"] != false {
		t.Fatalf("oversized settings-center body should not persist require_downstream_key: %v", row)
	}
}

func TestAdminConfigRejectsInvalidSchedulerFields(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchConfigStatus(t, h, `{"account_token_budget":-1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("account_token_budget=-1 status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "account_token_budget") {
		t.Fatalf("account_token_budget=-1 body was not safely normalized: %q", body)
	}

	status, body = patchConfigStatus(t, h, `{"cooldown_wait_max_seconds":-1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("cooldown_wait_max_seconds=-1 status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "cooldown_wait_max_seconds") {
		t.Fatalf("cooldown_wait_max_seconds=-1 body was not safely normalized: %q", body)
	}
}

func TestAdminConfigRejectsClaudeCacheRolloutModelScope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchConfigStatus(t, h, `{"claude_cache_optimization_rollout":"{\"models\":[\"claude-opus-4-8\"]}"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("model-scoped claude cache rollout status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "models") {
		t.Fatalf("model-scoped claude cache rollout body = %q", body)
	}
}

func TestAdminConfigPatchDoesNotPartiallyPersistInvalidBatch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchConfigStatus(t, h, `{"require_downstream_key":true,"claude_cache_ttl":"7d"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("mixed valid/invalid config patch status = %d, want 400: %s", status, body)
	}
	row := findConfigRow(adminConfigRows(t, h), "require_downstream_key")
	if row == nil {
		t.Fatal("require_downstream_key missing from /admin/config")
	}
	if row["value"] != false || row["overridden"] != false {
		t.Fatalf("invalid batch partially persisted require_downstream_key: %v", row)
	}
}

func TestAdminConfigPatchRejectsUnknownAndRestartOnlyKeys(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := patchConfigStatus(t, h, `{"not_a_runtime_key":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown /admin/config key status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, "not_a_runtime_key") {
		t.Fatalf("unknown /admin/config body = %q", body)
	}

	status, body = patchConfigStatus(t, h, `{"listen_addr":":9999"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("restart-only /admin/config key status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, `"code":"invalid_request"`) || strings.Contains(body, `requires restart`) {
		t.Fatalf("restart-only /admin/config body = %q", body)
	}

	status, body = patchConfigStatus(t, h, `{"require_downstream_key":true,"not_a_runtime_key":false}`)
	if status != http.StatusBadRequest {
		t.Fatalf("mixed unknown /admin/config batch status = %d, want 400: %s", status, body)
	}
	row := findConfigRow(adminConfigRows(t, h), "require_downstream_key")
	if row == nil {
		t.Fatal("require_downstream_key missing from /admin/config")
	}
	if row["value"] != false || row["overridden"] != false {
		t.Fatalf("unknown-key batch partially persisted require_downstream_key: %v", row)
	}
}

func TestAdminConfigReportsStoredSettingErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	before := findConfigRow(adminConfigRows(t, h), "require_downstream_key")["value"]
	if err := h.store.SetSetting(context.Background(), "require_downstream_key", "maybe"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "claude_cache_ttl", "7d"); err != nil {
		t.Fatal(err)
	}

	rows := adminConfigRows(t, h)
	requireKey := findConfigRow(rows, "require_downstream_key")
	if requireKey == nil {
		t.Fatal("require_downstream_key missing from /admin/config")
	}
	if requireKey["overridden"] != true {
		t.Fatalf("require_downstream_key overridden = %#v, want true", requireKey["overridden"])
	}
	if requireKey["value"] != before {
		t.Fatalf("invalid require_downstream_key should fall back to boot value %v, got %#v", before, requireKey["value"])
	}
	if got, _ := requireKey["settings_error"].(string); !strings.Contains(got, "expected boolean") {
		t.Fatalf("require_downstream_key settings_error = %q, want expected boolean", got)
	}

	ttl := findConfigRow(rows, "claude_cache_ttl")
	if ttl == nil {
		t.Fatal("claude_cache_ttl missing from /admin/config")
	}
	if got, _ := ttl["settings_error"].(string); !strings.Contains(got, "must be one of") {
		t.Fatalf("claude_cache_ttl settings_error = %q, want option error", got)
	}
}

func TestAdminConfigReportsSettingsReadErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	// The settings snapshot is warmed by harness setup; clear it so the next read hits
	// the now-dropped table and surfaces the read error this test asserts.
	h.store.InvalidateSettingsCache()

	rows := adminConfigRows(t, h)
	row := findConfigRow(rows, "require_downstream_key")
	if row == nil {
		t.Fatal("require_downstream_key missing from /admin/config")
	}
	if row["overridden"] != false {
		t.Fatalf("require_downstream_key overridden = %#v, want false after read error", row["overridden"])
	}
	if got, _ := row["settings_error"].(string); !strings.Contains(got, "read require_downstream_key") {
		t.Fatalf("require_downstream_key settings_error = %q, want read error", got)
	}
}

func TestSettingsCenterConfigPatchFailsFastOnInvalidValue(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	before := findConfigRow(adminConfigRows(t, h), "claude_cache_ttl")["value"]
	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"claude_cache_ttl":"7d"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center invalid config status = %d, want 400: %s", status, body)
	}
	after := findConfigRow(adminConfigRows(t, h), "claude_cache_ttl")["value"]
	if after != before {
		t.Fatalf("invalid settings-center patch changed claude_cache_ttl: before=%v after=%v", before, after)
	}
}

func TestSettingsCenterConfigPatchValidatesSMSCountries(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"sms_manual_country":" br ","sms_preferred_countries":["br","CO","br"]}}]`)
	if status != http.StatusOK {
		t.Fatalf("settings-center valid SMS countries status = %d: %s", status, body)
	}
	rows := adminConfigRows(t, h)
	if got := findConfigRow(rows, "sms_manual_country")["value"]; got != "BR" {
		t.Fatalf("sms_manual_country = %v, want BR", got)
	}
	if got := findConfigRow(rows, "sms_preferred_countries")["value"]; got != "BR,CO" {
		t.Fatalf("sms_preferred_countries = %v, want BR,CO", got)
	}

	status, body = postSettingsCenter(t, h, `[{"section":"config","values":{"sms_manual_country":"ZZZ"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center invalid manual country status = %d, want 400: %s", status, body)
	}
	if got := findConfigRow(adminConfigRows(t, h), "sms_manual_country")["value"]; got != "BR" {
		t.Fatalf("invalid manual country changed setting to %v, want BR", got)
	}

	status, body = postSettingsCenter(t, h, `[{"section":"config","values":{"sms_preferred_countries":"BR,ZZZ"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center invalid preferred countries status = %d, want 400: %s", status, body)
	}
	if got := findConfigRow(adminConfigRows(t, h), "sms_preferred_countries")["value"]; got != "BR,CO" {
		t.Fatalf("invalid preferred countries changed setting to %v, want BR,CO", got)
	}
}

func TestSettingsCenterConfigPatchRequiresRegistrationPoolPurpose(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	_, runtimePool := configureRegistrationEgressPools(t, h)

	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"registration_egress_pool_id":"`+runtimePool+`"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center runtime registration_egress_pool_id status = %d, want 400: %s", status, body)
	}
}

func TestSettingsCenterConfigPatchRejectsUnknownKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"not_a_runtime_key":true}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center unknown config status = %d, want 400: %s", status, body)
	}
}

func TestSettingsCenterPatchArrayFailsFastBeforeWriting(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[
		{"section":"logging","values":{"verbose_logging":false}},
		{"section":"config","values":{"not_a_runtime_key":true}}
	]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center mixed valid/invalid status = %d, want 400: %s", status, body)
	}

	status, got, raw := getSettingsCenter(t, h, "?section=logging")
	if status != http.StatusOK {
		t.Fatalf("GET logging status = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if logging["verbose_logging"] != true {
		t.Fatalf("valid earlier patch was persisted despite later failure: %#v", logging)
	}
}

func TestSettingsCenterPatchArrayRollsBackWhenWriteFails(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if _, err := h.store.DB().ExecContext(context.Background(), `
CREATE TRIGGER fail_lifecycle_concurrency
BEFORE INSERT ON settings
WHEN NEW.key = 'lifecycle_concurrency'
BEGIN
	SELECT RAISE(ABORT, 'synthetic settings failure');
END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	status, body := postSettingsCenter(t, h, `[
		{"section":"logging","values":{"verbose_logging":false}},
		{"section":"memory","values":{"lifecycle_concurrency":4}}
	]`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("settings-center write failure status = %d, want 503: %s", status, body)
	}
	if !strings.Contains(body, `"code":"service_unavailable"`) || strings.Contains(body, "synthetic settings failure") {
		t.Fatalf("settings-center write failure was not safely normalized: %q", body)
	}

	status, got, raw := getSettingsCenter(t, h, "?sections=logging,memory")
	if status != http.StatusOK {
		t.Fatalf("GET settings after failed batch status = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if logging["verbose_logging"] != true {
		t.Fatalf("failed batch persisted earlier logging patch: %#v", logging)
	}
	memory := settingsSection(t, got, "memory")
	if memory["lifecycle_concurrency"] != float64(10) {
		t.Fatalf("failed batch persisted failing memory patch: %#v", memory)
	}
	if _, ok, err := h.store.GetSetting(context.Background(), "reg_verbose_logging"); err != nil || ok {
		t.Fatalf("reg_verbose_logging persisted after failed batch: ok=%v err=%v", ok, err)
	}
	if _, ok, err := h.store.GetSetting(context.Background(), "lifecycle_concurrency"); err != nil || ok {
		t.Fatalf("lifecycle_concurrency persisted after failed batch: ok=%v err=%v", ok, err)
	}
}

func TestSettingsCenterConfigPatchRejectsFractionalIntegers(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	before := findConfigRow(adminConfigRows(t, h), "failover_max_attempts")["value"]
	status, body := postSettingsCenter(t, h, `[{"section":"config","values":{"failover_max_attempts":1.5}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("settings-center fractional int status = %d, want 400: %s", status, body)
	}
	after := findConfigRow(adminConfigRows(t, h), "failover_max_attempts")["value"]
	if after != before {
		t.Fatalf("fractional integer patch changed failover_max_attempts: before=%v after=%v", before, after)
	}
}

func TestSettingsCenterGetCanLoadOnlyRequestedSections(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body, raw := getSettingsCenter(t, h, "?sections=logging,memory")
	if status != http.StatusOK {
		t.Fatalf("GET settings-center sections status = %d: %s", status, raw)
	}
	if _, ok := body["logging"]; !ok {
		t.Fatalf("filtered settings-center response missing logging: %s", raw)
	}
	if _, ok := body["memory"]; !ok {
		t.Fatalf("filtered settings-center response missing memory: %s", raw)
	}
	for _, unexpected := range []string{"config", "registrar", "automation", "lifecycle"} {
		if _, ok := body[unexpected]; ok {
			t.Fatalf("filtered settings-center response included %s: %s", unexpected, raw)
		}
	}
}

func TestSettingsCenterGetRejectsUnknownSection(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, _, raw := getSettingsCenter(t, h, "?section=missing")
	if status != http.StatusBadRequest {
		t.Fatalf("GET settings-center unknown section status = %d, want 400: %s", status, raw)
	}
}

func TestSettingsCenterLoggingPatchPersistsRuntimeKeys(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"logging","values":{"verbose_logging":false,"failure_threshold":0.8,"log_retention_days":14,"degraded":true}}]`)
	if status != http.StatusOK {
		t.Fatalf("settings-center logging patch status = %d: %s", status, body)
	}

	status, got, raw := getSettingsCenter(t, h, "?section=logging")
	if status != http.StatusOK {
		t.Fatalf("GET logging settings status = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if logging["verbose_logging"] != false {
		t.Fatalf("verbose_logging = %#v, want false", logging["verbose_logging"])
	}
	if logging["failure_threshold"] != 0.8 {
		t.Fatalf("failure_threshold = %#v, want 0.8", logging["failure_threshold"])
	}
	if logging["log_retention_days"] != float64(14) {
		t.Fatalf("log_retention_days = %#v, want 14", logging["log_retention_days"])
	}
	if logging["degraded"] != true {
		t.Fatalf("degraded = %#v, want true", logging["degraded"])
	}
	if _, ok, _ := h.store.GetSetting(context.Background(), "verbose_logging"); ok {
		t.Fatal("logging patch wrote UI key verbose_logging instead of reg_verbose_logging")
	}
	if stored, ok, _ := h.store.GetSetting(context.Background(), "reg_verbose_logging"); !ok || stored != "false" {
		t.Fatalf("reg_verbose_logging = %q ok=%v, want false", stored, ok)
	}
}

func TestSettingsCenterLoggingPatchRejectsInvalidValue(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"logging","values":{"failure_threshold":1.5}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid logging patch status = %d, want 400: %s", status, body)
	}
	status, got, raw := getSettingsCenter(t, h, "?section=logging")
	if status != http.StatusOK {
		t.Fatalf("GET logging settings status = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if logging["failure_threshold"] != 0.6 {
		t.Fatalf("invalid logging patch changed failure_threshold: %#v", logging["failure_threshold"])
	}
}

func TestSettingsCenterLoggingReportsStoredValueErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if err := h.store.SetSetting(context.Background(), "reg_verbose_logging", "maybe"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "reg_failure_threshold", "not-a-number"); err != nil {
		t.Fatal(err)
	}

	status, got, raw := getSettingsCenter(t, h, "?section=logging")
	if status != http.StatusOK {
		t.Fatalf("GET logging settings status = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if logging["verbose_logging"] != true {
		t.Fatalf("invalid verbose_logging should fall back to default true, got %#v", logging["verbose_logging"])
	}
	if logging["failure_threshold"] != 0.6 {
		t.Fatalf("invalid failure_threshold should fall back to default 0.6, got %#v", logging["failure_threshold"])
	}
	if !settingsErrorContains(t, logging, "verbose_logging", "reg_verbose_logging") {
		t.Fatalf("logging settings_errors missing verbose_logging storage key: %#v", logging["settings_errors"])
	}
	if !settingsErrorContains(t, logging, "failure_threshold", "expected number") {
		t.Fatalf("logging settings_errors missing failure_threshold parse error: %#v", logging["settings_errors"])
	}
}

func TestSettingsCenterMemoryPatchPersistsAndRejectsInvalidValue(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"memory","values":{"lifecycle_batch_size":128,"lifecycle_concurrency":4,"go_memory_limit_mb":512,"reg_combined_output_cap":262144}}]`)
	if status != http.StatusOK {
		t.Fatalf("settings-center memory patch status = %d: %s", status, body)
	}
	status, got, raw := getSettingsCenter(t, h, "?section=memory")
	if status != http.StatusOK {
		t.Fatalf("GET memory settings status = %d: %s", status, raw)
	}
	memory := settingsSection(t, got, "memory")
	if memory["lifecycle_batch_size"] != float64(128) || memory["lifecycle_concurrency"] != float64(4) ||
		memory["go_memory_limit_mb"] != float64(512) || memory["reg_combined_output_cap"] != float64(262144) {
		t.Fatalf("memory settings not persisted: %#v", memory)
	}

	status, body = postSettingsCenter(t, h, `[{"section":"memory","values":{"lifecycle_concurrency":0}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid memory patch status = %d, want 400: %s", status, body)
	}
	status, got, raw = getSettingsCenter(t, h, "?section=memory")
	if status != http.StatusOK {
		t.Fatalf("GET memory settings status = %d: %s", status, raw)
	}
	memory = settingsSection(t, got, "memory")
	if memory["lifecycle_concurrency"] != float64(4) {
		t.Fatalf("invalid memory patch changed lifecycle_concurrency: %#v", memory["lifecycle_concurrency"])
	}
}

func TestSettingsCenterMemoryReportsStoredValueErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if err := h.store.SetSetting(context.Background(), "lifecycle_concurrency", "0"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "reg_combined_output_cap", "bogus"); err != nil {
		t.Fatal(err)
	}

	status, got, raw := getSettingsCenter(t, h, "?section=memory")
	if status != http.StatusOK {
		t.Fatalf("GET memory settings status = %d: %s", status, raw)
	}
	memory := settingsSection(t, got, "memory")
	if memory["lifecycle_concurrency"] != float64(10) {
		t.Fatalf("invalid lifecycle_concurrency should fall back to default 10, got %#v", memory["lifecycle_concurrency"])
	}
	if memory["reg_combined_output_cap"] != float64(1<<20) {
		t.Fatalf("invalid reg_combined_output_cap should fall back to default %d, got %#v", 1<<20, memory["reg_combined_output_cap"])
	}
	if !settingsErrorContains(t, memory, "lifecycle_concurrency", "between 1 and 50") {
		t.Fatalf("memory settings_errors missing lifecycle_concurrency range error: %#v", memory["settings_errors"])
	}
	if !settingsErrorContains(t, memory, "reg_combined_output_cap", "expected integer") {
		t.Fatalf("memory settings_errors missing output cap parse error: %#v", memory["settings_errors"])
	}
}

func TestSettingsCenterRuntimeSectionsReportSettingsReadErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	// The settings snapshot is warmed by harness setup; clear it so the next read hits
	// the now-dropped table and surfaces the read error this test asserts.
	h.store.InvalidateSettingsCache()

	status, got, raw := getSettingsCenter(t, h, "?sections=logging,memory")
	if status != http.StatusOK {
		t.Fatalf("GET runtime settings with missing settings table = %d: %s", status, raw)
	}
	logging := settingsSection(t, got, "logging")
	if !settingsErrorContains(t, logging, "failure_threshold", "read reg_failure_threshold") {
		t.Fatalf("logging settings_errors missing settings read error: %#v", logging["settings_errors"])
	}
	memory := settingsSection(t, got, "memory")
	if !settingsErrorContains(t, memory, "lifecycle_concurrency", "read lifecycle_concurrency") {
		t.Fatalf("memory settings_errors missing settings read error: %#v", memory["settings_errors"])
	}
}

func TestSettingsCenterRegistrarReplaceDeletesOmittedAndBlankKeys(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"registrar","values":{"heroSmsApiKey":"old-key","proxyHost":"old-proxy","custom":"keep"}}]`)
	if status != http.StatusOK {
		t.Fatalf("seed registrar patch status = %d: %s", status, body)
	}

	status, body = postSettingsCenter(t, h, `[{"section":"registrar","mode":"replace","values":{"heroSmsApiKey":"","mailDomains":["one.test"],"proxyPort":3010}}]`)
	if status != http.StatusOK {
		t.Fatalf("registrar replace status = %d: %s", status, body)
	}
	cfg := nodeRegistrarConfig(t, h)
	for _, deleted := range []string{"proxyHost", "custom"} {
		if _, ok := cfg[deleted]; ok {
			t.Fatalf("registrar replace did not delete %s: %#v", deleted, cfg)
		}
	}
	if cfg["heroSmsApiKey"] != "old-key" {
		t.Fatalf("registrar replace did not preserve blank write-only credential: %#v", cfg)
	}
	if cfg["proxyPort"] != float64(3010) {
		t.Fatalf("proxyPort = %#v, want 3010", cfg["proxyPort"])
	}
	domains, ok := cfg["mailDomains"].([]interface{})
	if !ok || len(domains) != 1 || domains[0] != "one.test" {
		t.Fatalf("mailDomains = %#v, want [one.test]", cfg["mailDomains"])
	}
}

func TestSettingsCenterRegistrarRejectsUnknownMode(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"registrar","mode":"replace","values":{"heroSmsApiKey":"old-key"}}]`)
	if status != http.StatusOK {
		t.Fatalf("seed registrar patch status = %d: %s", status, body)
	}
	status, body = postSettingsCenter(t, h, `[{"section":"registrar","mode":"wipe","values":{"heroSmsApiKey":"new-key"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("registrar unknown mode status = %d, want 400: %s", status, body)
	}
	cfg := nodeRegistrarConfig(t, h)
	if cfg["heroSmsApiKey"] != "old-key" {
		t.Fatalf("unknown mode changed registrar config: %#v", cfg)
	}
}

func TestSettingsCenterRegistrarReportsCorruptStoredConfig(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if err := h.store.SetSetting(context.Background(), "node_registrar_config", "{"); err != nil {
		t.Fatal(err)
	}

	status, got, raw := getSettingsCenter(t, h, "?section=registrar")
	if status != http.StatusOK {
		t.Fatalf("GET registrar settings status = %d: %s", status, raw)
	}
	registrar := settingsSection(t, got, "registrar")
	if got, _ := registrar["registrar_error"].(string); !strings.Contains(got, "decode node_registrar_config") {
		t.Fatalf("registrar_error = %q, want decode node_registrar_config", got)
	}

	status, body := postSettingsCenter(t, h, `[{"section":"registrar","values":{"heroSmsApiKey":"new-key"}}]`)
	if status != http.StatusBadRequest {
		t.Fatalf("registrar patch with corrupt stored config status = %d, want 400: %s", status, body)
	}
	v, ok, err := h.store.GetSetting(context.Background(), "node_registrar_config")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != "{" {
		t.Fatalf("corrupt registrar config was changed: ok=%v value=%q", ok, v)
	}
}

func TestSettingsCenterRegistrarRejectsReadOnlyMetadataKeys(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"registrar","values":{"heroSmsApiKey":"old-key"}}]`)
	if status != http.StatusOK {
		t.Fatalf("seed registrar patch status = %d: %s", status, body)
	}

	for _, key := range []string{"registrar_error", "defaults_error"} {
		status, body = postSettingsCenter(t, h, `[{"section":"registrar","values":{"`+key+`":"should-not-persist"}}]`)
		if status != http.StatusBadRequest {
			t.Fatalf("registrar metadata key %s status = %d, want 400: %s", key, status, body)
		}
	}

	cfg := nodeRegistrarConfig(t, h)
	if cfg["heroSmsApiKey"] != "old-key" {
		t.Fatalf("metadata patch changed existing registrar config: %#v", cfg)
	}
	if _, ok := cfg["registrar_error"]; ok {
		t.Fatalf("registrar_error persisted into node registrar config: %#v", cfg)
	}
	if _, ok := cfg["defaults_error"]; ok {
		t.Fatalf("defaults_error persisted into node registrar config: %#v", cfg)
	}
}

func TestSettingsCenterAutomationPatchPersistsValidPolicy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"automation","values":{"policy":{"type":"refill","enabled":true,"config":{"target":5,"threshold":2}}}}]`)
	if status != http.StatusOK {
		t.Fatalf("automation policy patch status = %d: %s", status, body)
	}
	status, got, raw := getSettingsCenter(t, h, "?section=automation")
	if status != http.StatusOK {
		t.Fatalf("GET automation settings status = %d: %s", status, raw)
	}
	automation := settingsSection(t, got, "automation")
	policies, ok := automation["policies"].([]interface{})
	if !ok || len(policies) != 1 {
		t.Fatalf("automation policies = %#v", automation["policies"])
	}
	policy, ok := policies[0].(map[string]interface{})
	if !ok || policy["type"] != "refill" || policy["enabled"] != true {
		t.Fatalf("unexpected automation policy: %#v", policies[0])
	}
	config, ok := policy["config"].(map[string]interface{})
	if !ok || config["target"] != float64(5) || config["threshold"] != float64(2) {
		t.Fatalf("unexpected automation config: %#v", policy["config"])
	}
}

func TestSettingsCenterAutomationPatchRejectsInvalidPolicy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"automation","values":{"policy":{"type":"refill","enabled":true,"config":{"target":5}}}}]`)
	if status != http.StatusOK {
		t.Fatalf("seed automation policy status = %d: %s", status, body)
	}
	cases := []string{
		`[{"section":"automation","values":{"policy":{"type":"unknown","enabled":true,"config":{}}}}]`,
		`[{"section":"automation","values":{"policy":{"type":"refill","enabled":"yes","config":{}}}}]`,
		`[{"section":"automation","values":{"policy":{"type":"refill","enabled":true,"config":"bad"}}}]`,
		`[{"section":"automation","values":{"bad":true}}]`,
	}
	for _, bodyJSON := range cases {
		status, body = postSettingsCenter(t, h, bodyJSON)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid automation patch %s status = %d, want 400: %s", bodyJSON, status, body)
		}
	}
	status, got, raw := getSettingsCenter(t, h, "?section=automation")
	if status != http.StatusOK {
		t.Fatalf("GET automation settings status = %d: %s", status, raw)
	}
	automation := settingsSection(t, got, "automation")
	policies, ok := automation["policies"].([]interface{})
	if !ok || len(policies) != 1 {
		t.Fatalf("automation policies after invalid patches = %#v", automation["policies"])
	}
	policy, _ := policies[0].(map[string]interface{})
	if policy["type"] != "refill" {
		t.Fatalf("invalid automation patch changed policy type: %#v", policy)
	}
}

func TestSettingsCenterLifecyclePatchPersistsDefaults(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"lifecycle","values":{"defaults":{"sms":"herosms","mailbox":"1secmail","captcha":"yescaptcha","group":"default","egress":"egress_direct"}}}]`)
	if status != http.StatusOK {
		t.Fatalf("lifecycle defaults patch status = %d: %s", status, body)
	}
	status, got, raw := getSettingsCenter(t, h, "?section=lifecycle")
	if status != http.StatusOK {
		t.Fatalf("GET lifecycle settings status = %d: %s", status, raw)
	}
	lifecycle := settingsSection(t, got, "lifecycle")
	defaults := settingsSection(t, lifecycle, "defaults")
	for key, want := range map[string]interface{}{
		"sms":     "herosms",
		"mailbox": "1secmail",
		"captcha": "yescaptcha",
		"group":   "default",
		"egress":  "egress_direct",
	} {
		if defaults[key] != want {
			t.Fatalf("lifecycle default %s = %#v, want %#v (all defaults: %#v)", key, defaults[key], want, defaults)
		}
	}
}

func TestSettingsCenterLifecyclePatchRejectsInvalidPayload(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})

	status, body := postSettingsCenter(t, h, `[{"section":"lifecycle","values":{"defaults":{"sms":"herosms"}}}]`)
	if status != http.StatusOK {
		t.Fatalf("seed lifecycle defaults status = %d: %s", status, body)
	}
	cases := []string{
		`[{"section":"lifecycle","values":{"defaults":"bad"}}]`,
		`[{"section":"lifecycle","values":{"defaults":{"sms":123}}}]`,
		`[{"section":"lifecycle","values":{"defaults":{"unknown":"bad"}}}]`,
		`[{"section":"lifecycle","values":{"bad":true}}]`,
		`[{"section":"lifecycle","values":{"defaults":{},"extra":true}}]`,
	}
	for _, bodyJSON := range cases {
		status, body = postSettingsCenter(t, h, bodyJSON)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid lifecycle patch %s status = %d, want 400: %s", bodyJSON, status, body)
		}
	}
	status, got, raw := getSettingsCenter(t, h, "?section=lifecycle")
	if status != http.StatusOK {
		t.Fatalf("GET lifecycle settings status = %d: %s", status, raw)
	}
	lifecycle := settingsSection(t, got, "lifecycle")
	defaults := settingsSection(t, lifecycle, "defaults")
	if defaults["sms"] != "herosms" {
		t.Fatalf("invalid lifecycle patch changed defaults: %#v", defaults)
	}
}

func TestSettingsCenterLifecycleReportsDefaultsReadErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	if _, err := h.store.DB().ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	// The settings snapshot is warmed by harness setup; clear it so the next read hits
	// the now-dropped table and surfaces the read error this test asserts.
	h.store.InvalidateSettingsCache()

	status, got, raw := getSettingsCenter(t, h, "?section=lifecycle")
	if status != http.StatusOK {
		t.Fatalf("GET lifecycle settings with missing settings table = %d: %s", status, raw)
	}
	lifecycle := settingsSection(t, got, "lifecycle")
	if got, _ := lifecycle["defaults_error"].(string); !strings.Contains(got, "read registration default") {
		t.Fatalf("defaults_error = %q, want read registration default", got)
	}
	defaults := settingsSection(t, lifecycle, "defaults")
	if len(defaults) != 0 {
		t.Fatalf("defaults = %#v, want empty after read error", defaults)
	}
}
