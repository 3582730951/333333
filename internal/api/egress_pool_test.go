package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminEgressPoolsExposeRegistrationPoolsOnly(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles", `{
		"id":"registration_proxy",
		"name":"registration proxy",
		"type":"curl_cffi_sidecar",
		"endpoint":"http://127.0.0.1:8790",
		"ip_mode":"static_residential",
		"provider_key":"cuff",
		"dynamic_config_json":{"note":"runtime"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("create egress profile = %d: %s", code, raw)
	}

	if err := h.store.UpsertEgressPool(ctx, storage.EgressPool{ID: "pool_runtime", Name: "legacy runtime pool", Purpose: "runtime", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressPoolMember(ctx, storage.EgressPoolMember{PoolID: "pool_runtime", EgressID: "registration_proxy", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools", `{
		"id":"pool_runtime_new",
		"name":"Runtime cuff pool",
		"purpose":"runtime",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create runtime egress pool = %d, want 400: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools", `{
		"id":"pool_registration",
		"name":"Registration proxy pool",
		"purpose":"registration",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusOK {
		t.Fatalf("create registration egress pool = %d: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools/pool_registration/members", `{
		"egress_id":"registration_proxy",
		"enabled":true,
		"capacity":12
	}`)
	if code != http.StatusOK {
		t.Fatalf("add registration pool member = %d: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools/pool_runtime/members", `{
		"egress_id":"registration_proxy",
		"enabled":true
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("add member to runtime pool = %d, want 400: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/egress-pools", "")
	if code != http.StatusOK {
		t.Fatalf("list egress pools = %d: %s", code, raw)
	}
	if !strings.Contains(string(raw), `"pool_registration"`) || strings.Contains(string(raw), `"pool_runtime"`) {
		t.Fatalf("registration pool list should hide runtime pools: %s", raw)
	}
	var pools []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &pools); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	member := pools[0]["members"].([]interface{})[0].(map[string]interface{})
	egress := member["egress"].(map[string]interface{})
	if egress["provider_key"] != "cuff" || egress["dynamic_config_json"] == "" {
		t.Fatalf("hydrated egress metadata missing from member: %#v", egress)
	}
}

func TestAdminGroupEgressPolicyRejectsRuntimePools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertEgressPool(ctx, storage.EgressPool{ID: "pool_registration", Purpose: "registration", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressPool(ctx, storage.EgressPool{ID: "pool_runtime", Purpose: "runtime", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/groups/cyber/egress-policy", `{
		"registration_pool_id":"pool_registration",
		"runtime_pool_id":"pool_runtime",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("save runtime group egress policy = %d, want 400: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/groups/cyber/egress-policy", `{
		"registration_pool_id":"pool_registration",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusOK {
		t.Fatalf("save registration group policy = %d: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/groups/cyber/egress-policy", "")
	if code != http.StatusOK {
		t.Fatalf("get group policy = %d: %s", code, raw)
	}
	if strings.Contains(string(raw), `"runtime_pool_id":"pool_runtime"`) {
		t.Fatalf("policy response should not include runtime pool: %s", raw)
	}
}
