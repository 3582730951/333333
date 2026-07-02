package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/supervisor"
)

func TestAdminSystemIncludesSupervisorEvents(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	supervisor.LogPanicWithLogf("system-test-module", "system-test-panic", func(string, ...any) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/system", "")
	if code != http.StatusOK {
		t.Fatalf("admin system = %d, want 200: %s", code, raw)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode admin system: %v\n%s", err, raw)
	}
	events, ok := payload["supervisor_events"].([]interface{})
	if !ok {
		t.Fatalf("supervisor_events missing or wrong type: %#v", payload["supervisor_events"])
	}
	modules, ok := payload["supervisor_modules"].([]interface{})
	if !ok {
		t.Fatalf("supervisor_modules missing or wrong type: %#v", payload["supervisor_modules"])
	}
	for _, event := range events {
		row, _ := event.(map[string]interface{})
		if row["module"] == "system-test-module" && row["panic"] == "system-test-panic" {
			for _, module := range modules {
				mod, _ := module.(map[string]interface{})
				if mod["name"] == "system-test-module" && mod["last_panic"] == "system-test-panic" {
					return
				}
			}
			t.Fatalf("supervisor_modules did not include system-test-module panic: %#v", modules)
		}
	}
	t.Fatalf("supervisor_events did not include system-test-module panic: %#v", events)
}
