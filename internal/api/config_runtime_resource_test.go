package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"testing"
)

func TestRuntimeSettingsDoNotIgnoreStoreErrors(t *testing.T) {
	configSource := readAPISource(t, "config_runtime.go")
	isolateSource := readAPISource(t, "isolate.go")
	for name, source := range map[string]string{
		"config_runtime.go": configSource,
		"isolate.go":        isolateSource,
	} {
		for _, bad := range []string{
			"v, ok, _ := s.store.GetSetting",
			"if v, ok, _ := s.store.GetSetting",
			"v, ok, _ := h.store.GetSetting",
			"if v, ok, _ := h.store.GetSetting",
		} {
			if strings.Contains(source, bad) {
				t.Fatalf("%s should route runtime setting reads through an error-logging helper; found %q", name, bad)
			}
		}
	}
	for _, fn := range []string{"settingString", "settingInt", "settingInt64", "settingCSV"} {
		if !strings.Contains(functionBody(t, configSource, fn), ".runtimeSetting(") {
			t.Fatalf("%s should use runtimeSetting", fn)
		}
	}
	if !strings.Contains(functionBody(t, isolateSource, "flagEnabled"), ".runtimeSetting(") {
		t.Fatal("flagEnabled should use runtimeSetting")
	}
	if !strings.Contains(functionBody(t, configSource, "runtimeSetting"), "[CONFIG-RUNTIME-ERROR]") {
		t.Fatal("runtimeSetting should log setting read errors")
	}
}

func TestInvalidRuntimeSettingsLogAndFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	var logs bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	if err := h.store.SetSetting(ctx, "failover_max_attempts", "not-an-int"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.settingInt(ctx, "failover_max_attempts", 3); got != 3 {
		t.Fatalf("invalid integer setting = %d, want fallback 3", got)
	}
	if err := h.store.SetSetting(ctx, "conversation_isolation", "maybe"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.flagEnabled(ctx, "conversation_isolation", true); got != true {
		t.Fatal("invalid boolean setting should fall back to the configured default")
	}
	out := logs.String()
	for _, want := range []string{
		`[CONFIG-RUNTIME-WARN] setting "failover_max_attempts" has invalid integer value "not-an-int"`,
		`[CONFIG-RUNTIME-WARN] setting "conversation_isolation" has invalid boolean value "maybe"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing log %q in:\n%s", want, out)
		}
	}
}

func TestRuntimeSettingsCacheIsRequestScoped(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, "runtime_cache_test", "one"); err != nil {
		t.Fatal(err)
	}

	requestCtx := contextWithRuntimeSettingsCache(ctx)
	if got := h.app.settingString(requestCtx, "runtime_cache_test", "fallback"); got != "one" {
		t.Fatalf("first cached setting read = %q, want one", got)
	}
	if err := h.store.SetSetting(ctx, "runtime_cache_test", "two"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.settingString(requestCtx, "runtime_cache_test", "fallback"); got != "one" {
		t.Fatalf("same request setting read = %q, want cached one", got)
	}
	if got := h.app.settingString(contextWithRuntimeSettingsCache(ctx), "runtime_cache_test", "fallback"); got != "two" {
		t.Fatalf("new request setting read = %q, want hot-updated two", got)
	}
}

func TestKiroThinkingCannotBeDisabledByLegacyHotOverride(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if !h.app.effectiveKiroConfig(ctx).KiroDefaultThinking {
		t.Fatal("Kiro adaptive thinking should be enabled by default")
	}
	if err := h.store.SetSetting(ctx, "kiro_default_thinking", "false"); err != nil {
		t.Fatal(err)
	}
	if !h.app.effectiveKiroConfig(contextWithRuntimeSettingsCache(ctx)).KiroDefaultThinking {
		t.Fatal("legacy false override disabled mandatory Kiro thinking")
	}
	field, ok := configFieldByKey("kiro_default_thinking")
	if !ok {
		t.Fatal("Kiro thinking field missing")
	}
	if _, err := validateSettingValue(field, false); err == nil {
		t.Fatal("runtime config accepted disabling mandatory Kiro thinking")
	}
}

func TestRegistrationSettingsDoNotIgnoreStoreErrors(t *testing.T) {
	regSource := readAPISource(t, "registration.go")
	for _, fn := range []string{"resolveConcurrency", "failureThreshold", "flagEnabledStr", "resolveMethod", "logEventDetail"} {
		body := functionBodyForReceiver(t, regSource, "h *Handler", fn)
		for _, bad := range []string{
			"v, ok, _ := h.store.GetSetting",
			"if v, ok, _ := h.store.GetSetting",
		} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s should route registration setting reads through an error-logging helper; found %q", fn, bad)
			}
		}
		if fn != "flagEnabledStr" && !strings.Contains(body, ".setting(") {
			t.Fatalf("%s should use Handler.setting", fn)
		}
	}
	if !strings.Contains(functionBodyForReceiver(t, regSource, "h *Handler", "setting"), "[REGISTRATION-CONFIG-ERROR]") {
		t.Fatal("Handler.setting should log setting read errors")
	}

	logsSource := readAPISource(t, "registration_logs.go")
	if !strings.Contains(functionBodyForReceiver(t, logsSource, "h *Handler", "LogRetentionCleanup"), ".setting(") {
		t.Fatal("LogRetentionCleanup should use Handler.setting")
	}

	registrarSource := readAPISource(t, "registrar_config.go")
	registrarBody := functionBody(t, registrarSource, "adminNodeRegistrarConfig")
	if strings.Contains(registrarBody, "if v, ok, _ := s.store.GetSetting") {
		t.Fatal("adminNodeRegistrarConfig should handle GetSetting errors")
	}
	if !strings.Contains(registrarBody, "writeError(w, http.StatusInternalServerError, err)") {
		t.Fatal("adminNodeRegistrarConfig should return 500 for setting read errors")
	}

	settingsCenterSource := readAPISource(t, "settings_center.go")
	settingFloat := functionBody(t, settingsCenterSource, "settingFloat")
	if !strings.Contains(settingFloat, ".runtimeSetting(") {
		t.Fatal("settingFloat should use runtimeSetting")
	}
	if strings.Contains(settingFloat, "fmt.Sscanf") {
		t.Fatal("settingFloat should use strconv.ParseFloat and log invalid values")
	}
}

func TestInvalidRegistrationSettingsLogAndFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	var logs bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	if err := h.store.SetSetting(ctx, "registration_concurrency", "NaN"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.regHandler.resolveConcurrency(ctx); got < 1 {
		t.Fatalf("invalid registration_concurrency should fall back to a positive default, got %d", got)
	}
	if err := h.store.SetSetting(ctx, "reg_failure_threshold", "2.5"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.regHandler.failureThreshold(ctx); got != 0.6 {
		t.Fatalf("invalid reg_failure_threshold = %v, want fallback 0.6", got)
	}
	if err := h.store.SetSetting(ctx, "reg_verbose_logging", "sometimes"); err != nil {
		t.Fatal(err)
	}
	h.app.regHandler.logEventDetail(ctx, "task-invalid-config", "info", "ignored", nil)

	out := logs.String()
	for _, want := range []string{
		`[REGISTRATION-CONFIG-WARN] setting "registration_concurrency" has invalid integer >= 1 value "NaN"`,
		`[REGISTRATION-CONFIG-WARN] setting "reg_failure_threshold" has invalid float in (0,1] value "2.5"`,
		`[REGISTRATION-CONFIG-WARN] setting "reg_verbose_logging" has invalid boolean value "sometimes"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing log %q in:\n%s", want, out)
		}
	}
}

func functionBodyForReceiver(t *testing.T, source, receiver, name string) string {
	t.Helper()
	prefix := "func (" + receiver + ") " + name + "("
	start := strings.Index(source, prefix)
	if start < 0 {
		t.Fatalf("%s method %s not found", receiver, name)
	}
	rest := source[start+len(prefix):]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}
