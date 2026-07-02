package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminSessionsRequiresExistingAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts/missing/sessions", "")
	if code != http.StatusNotFound {
		t.Fatalf("admin sessions missing account = %d, want 404: %s", code, raw)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, raw)
	}
	if body["error"] == "" {
		t.Fatalf("missing account response should include an error: %v", body)
	}
}

func TestAdminSessionsDoesNotSwallowStoreErrors(t *testing.T) {
	source := readAPISource(t, "admin_config.go")
	body := functionBody(t, source, "adminSessions")
	for _, bad := range []string{"bindings, _ :=", "account, _ :="} {
		if strings.Contains(body, bad) {
			t.Fatalf("adminSessions must handle store errors explicitly; found %q", bad)
		}
	}
	accountLookup := strings.Index(body, ".GetAccount(")
	sessionLookup := strings.Index(body, ".ListAffinityBindingsByAccount(")
	if accountLookup < 0 || sessionLookup < 0 {
		t.Fatal("adminSessions should load both the account and the session affinity bindings")
	}
	if sessionLookup < accountLookup {
		t.Fatal("adminSessions should verify the account exists before building the session view")
	}
	if !strings.Contains(body, "writeError(w, http.StatusNotFound, err)") {
		t.Fatal("adminSessions should return a 404 when the account is missing")
	}
	if !strings.Contains(body, "egress_binding_error") {
		t.Fatal("adminSessions should surface non-fatal egress binding lookup errors")
	}
}
