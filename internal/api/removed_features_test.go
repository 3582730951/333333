package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestPaymentCompatibilitySurfaceIsGone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	for _, path := range []string{"/admin/gopay", "/admin/gopay/subscribe"} {
		code, raw := grpReq(t, h, http.MethodGet, path, "")
		if code != http.StatusGone || !strings.Contains(string(raw), "feature_removed") {
			t.Fatalf("%s = %d %s, want 410 feature_removed", path, code, raw)
		}
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/register/batch", `{
		"count":1,
		"upgrade_to_plus":true
	}`)
	if code != http.StatusGone || !strings.Contains(string(raw), "feature_removed") {
		t.Fatalf("upgrade_to_plus = %d %s, want 410 feature_removed", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/automation/policies", `{
		"type":"plus",
		"enabled":true,
		"config":{}
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("plus automation policy = %d %s, want 400", code, raw)
	}
}
