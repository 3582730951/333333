package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminEgressPoolsAndGroupPolicyRoundTrip(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles", `{
		"id":"runtime_sidecar",
		"name":"runtime sidecar",
		"type":"curl_cffi_sidecar",
		"endpoint":"http://127.0.0.1:8790",
		"ip_mode":"static_residential",
		"provider_key":"cuff",
		"dynamic_config_json":{"note":"runtime"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("create egress profile = %d: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools", `{
		"id":"pool_runtime",
		"name":"Runtime cuff pool",
		"purpose":"runtime",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusOK {
		t.Fatalf("create egress pool = %d: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/egress-pools/pool_runtime/members", `{
		"egress_id":"runtime_sidecar",
		"enabled":true,
		"capacity":12
	}`)
	if code != http.StatusOK {
		t.Fatalf("add egress pool member = %d: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodPost, "/admin/groups/cyber/egress-policy", `{
		"registration_pool_id":"pool_runtime",
		"runtime_pool_id":"pool_runtime",
		"assignment_strategy":"sticky_least_used"
	}`)
	if code != http.StatusOK {
		t.Fatalf("save group policy = %d: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/egress-pools", "")
	if code != http.StatusOK {
		t.Fatalf("list egress pools = %d: %s", code, raw)
	}
	if !strings.Contains(string(raw), `"pool_runtime"`) || !strings.Contains(string(raw), `"runtime_sidecar"`) {
		t.Fatalf("pool list missing pool/member: %s", raw)
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

	code, raw = grpReq(t, h, http.MethodGet, "/admin/groups/cyber/egress-policy", "")
	if code != http.StatusOK {
		t.Fatalf("get group policy = %d: %s", code, raw)
	}
	if !strings.Contains(string(raw), `"runtime_pool_id":"pool_runtime"`) {
		t.Fatalf("policy response missing runtime pool: %s", raw)
	}
}
