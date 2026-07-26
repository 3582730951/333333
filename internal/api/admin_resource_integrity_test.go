package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminRoutingTargetsCannotBeDeletedWhileReferenced(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"referenced-pool"}`); code != http.StatusOK {
		t.Fatalf("create pool group = %d: %s", code, body)
	}
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "referenced-provider", Name: "Referenced provider", BaseURL: "https://provider.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	code, body := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
		"name":"routing-reference",
		"targets":[
			{"kind":"account_pool_group","id":"referenced-pool"},
			{"kind":"model_provider","id":"referenced-provider"}
		],
		"model_routing":[{"model":"test-model","tiers":[
			[{"kind":"model_provider","id":"referenced-provider"}],
			[{"kind":"account_pool_group","id":"referenced-pool"}]
		]}]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create referencing user group = %d: %s", code, body)
	}

	for _, tc := range []struct {
		path string
		name string
	}{
		{path: "/admin/groups/referenced-pool", name: "account pool group"},
		{path: "/admin/providers/referenced-provider", name: "provider"},
	} {
		code, body = grpReq(t, h, http.MethodDelete, tc.path, "")
		if code != http.StatusConflict || !strings.Contains(string(body), `"code":"target_in_use"`) {
			t.Fatalf("delete referenced %s = %d: %s", tc.name, code, body)
		}
	}
	if _, err := h.store.GetGroup(t.Context(), "referenced-pool"); err != nil {
		t.Fatalf("referenced pool group was deleted: %v", err)
	}
	if _, ok, err := h.store.GetCustomProvider(t.Context(), "referenced-provider"); err != nil || !ok {
		t.Fatalf("referenced provider was deleted: ok=%v err=%v", ok, err)
	}
}

func TestAdminUnreferencedRoutingTargetsCanBeDeleted(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"unreferenced-pool"}`); code != http.StatusOK {
		t.Fatalf("create pool group = %d: %s", code, body)
	}
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "unreferenced-provider", Name: "Unreferenced provider", BaseURL: "https://provider.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if code, body := grpReq(t, h, http.MethodDelete, "/admin/groups/unreferenced-pool", ""); code != http.StatusOK {
		t.Fatalf("delete pool group = %d: %s", code, body)
	}
	if code, body := grpReq(t, h, http.MethodDelete, "/admin/providers/unreferenced-provider", ""); code != http.StatusOK {
		t.Fatalf("delete provider = %d: %s", code, body)
	}
	if _, err := h.store.GetGroup(t.Context(), "unreferenced-pool"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted pool group lookup err=%v", err)
	}
	if _, ok, err := h.store.GetCustomProvider(t.Context(), "unreferenced-provider"); err != nil || ok {
		t.Fatalf("deleted provider lookup: ok=%v err=%v", ok, err)
	}
}

func TestAdminDeleteEgressHonorsActiveAndExpiredRuntimeBindings(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "egress-runtime-account", Label: "Runtime binding", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()

	upsertTestEgressProfile(t, h, "active-affinity-egress")
	if err := h.store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: "active-affinity", RouteKey: "active-affinity", Source: "test",
		AccountID: account.ID, Provider: "codex", EgressID: "active-affinity-egress", ExpiresAt: now + 60,
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := grpReq(t, h, http.MethodDelete, "/admin/egress-profiles/active-affinity-egress", ""); code != http.StatusConflict || !strings.Contains(string(body), `"code":"egress_in_use"`) {
		t.Fatalf("delete active affinity egress = %d: %s", code, body)
	}

	upsertTestEgressProfile(t, h, "active-session-egress")
	insertCodexSessionBindingForEgress(t, h, "active-session", account.ID, "active-session-egress", now+60)
	if code, body := grpReq(t, h, http.MethodDelete, "/admin/egress-profiles/active-session-egress", ""); code != http.StatusConflict || !strings.Contains(string(body), `"code":"egress_in_use"`) {
		t.Fatalf("delete active Codex session egress = %d: %s", code, body)
	}

	upsertTestEgressProfile(t, h, "expired-runtime-egress")
	if err := h.store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: "expired-affinity", RouteKey: "expired-affinity", Source: "test",
		AccountID: account.ID, Provider: "codex", EgressID: "expired-runtime-egress", ExpiresAt: now - 1,
	}); err != nil {
		t.Fatal(err)
	}
	insertCodexSessionBindingForEgress(t, h, "expired-session", account.ID, "expired-runtime-egress", now-1)
	if code, body := grpReq(t, h, http.MethodDelete, "/admin/egress-profiles/expired-runtime-egress", ""); code != http.StatusOK {
		t.Fatalf("delete egress with expired runtime bindings = %d: %s", code, body)
	}
	if _, err := h.store.GetEgressProfile(ctx, "expired-runtime-egress"); err != sql.ErrNoRows {
		t.Fatalf("expired runtime egress still exists: %v", err)
	}
}

func insertCodexSessionBindingForEgress(t *testing.T, h *testHarness, id, accountID, egressID string, expiresAt int64) {
	t.Helper()
	now := storage.Now()
	if _, err := h.store.DB().ExecContext(t.Context(), `
		INSERT INTO codex_session_binding(
			id, tree_id, namespace_hash, account_id, egress_id, epoch, state,
			encrypted_identity, created_at, updated_at, expires_at
		) VALUES(?,?,?,?,?,0,'active','test-identity',?,?,?)`,
		id, "tree-"+id, "namespace-"+id, accountID, egressID, now, now, expiresAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAdminLegacyTargetDeleteIsScopedAndUsesReturnedLegacyID(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	type legacyTargetResponse struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		LegacyID int64  `json:"legacy_id"`
	}
	create := func(name string) storage.UserGroup {
		t.Helper()
		code, body := grpReq(t, h, http.MethodPost, "/admin/user-groups", `{
			"name":"`+name+`",
			"targets":[{"kind":"account_pool_group","id":"cyber"}]
		}`)
		if code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", name, code, body)
		}
		var group storage.UserGroup
		if err := json.Unmarshal(body, &group); err != nil {
			t.Fatal(err)
		}
		return group
	}
	groupA := create("legacy-a")
	groupB := create("legacy-b")

	code, body := grpReq(t, h, http.MethodGet, "/admin/user-groups/"+groupB.ID+"/targets", "")
	if code != http.StatusOK {
		t.Fatalf("get legacy targets = %d: %s", code, body)
	}
	var targets []legacyTargetResponse
	if err := json.Unmarshal(body, &targets); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].LegacyID <= 0 || targets[0].Kind != storage.TargetKindAccountPoolGroup {
		t.Fatalf("legacy target response = %+v", targets)
	}
	targetPath := "/targets/" + strconv.FormatInt(targets[0].LegacyID, 10)

	code, body = grpReq(t, h, http.MethodDelete, "/admin/user-groups/"+groupA.ID+targetPath, "")
	if code != http.StatusNotFound {
		t.Fatalf("cross-group legacy delete = %d: %s", code, body)
	}
	code, body = grpReq(t, h, http.MethodGet, "/admin/user-groups/"+groupB.ID+"/targets", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"legacy_id":`+strconv.FormatInt(targets[0].LegacyID, 10)) {
		t.Fatalf("cross-group delete modified owner targets = %d: %s", code, body)
	}

	code, body = grpReq(t, h, http.MethodDelete, "/admin/user-groups/"+groupB.ID+targetPath, "")
	if code != http.StatusOK {
		t.Fatalf("delete returned legacy ID = %d: %s", code, body)
	}
	code, body = grpReq(t, h, http.MethodGet, "/admin/user-groups/"+groupB.ID+"/targets", "")
	var remaining []legacyTargetResponse
	if code != http.StatusOK || json.Unmarshal(body, &remaining) != nil || len(remaining) != 0 {
		t.Fatalf("targets after legacy delete = %d: %s", code, body)
	}
}
