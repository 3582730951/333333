package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestAdminGroupAssignEgressUpdatesOnlyGroupEgress(t *testing.T) {
	source := readAPISource(t, "admin_config.go")
	body := functionBody(t, source, "adminGroupAssignEgress")
	if !strings.Contains(body, ".UpdateGroup(") || !strings.Contains(body, "group.EgressIDs") {
		t.Fatal("adminGroupAssignEgress should update the ordered group egress list")
	}
	for _, bad := range []string{
		".GetEgressBinding(r.Context(), acc.ID)",
		".GetEgressBinding(ctx, acc.ID)",
		".ListEgressBindingsByAccountIDs(",
		".UpsertEgressBinding(",
	} {
		if strings.Contains(body, bad) {
			t.Fatalf("adminGroupAssignEgress must not copy egresses into account bindings; found %q", bad)
		}
	}
	if strings.Contains(body, "_ = s.store.UpdateGroup") {
		t.Fatal("adminGroupAssignEgress should not ignore group default update errors")
	}
}

func TestAdminGroupAssignEgressLeavesAccountBindingUnchanged(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	upsertTestEgressProfile(t, h, "group-primary")
	upsertTestEgressProfile(t, h, "group-standby")
	account := storage.Account{ID: "dynamic-group-egress", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/groups/cyber/assign-egress", `{
		"primary_egress_id":"group-primary",
		"standby_egress_ids":["group-standby"]
	}`)
	if code != http.StatusOK {
		t.Fatalf("assign egress = %d: %s", code, raw)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response["accounts_updated"] != float64(0) || response["accounts_inheriting"] != float64(1) {
		t.Fatalf("unexpected compatibility response: %+v", response)
	}
	group, err := h.store.GetGroup(context.Background(), "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if len(group.EgressIDs) != 2 || group.EgressIDs[0] != "group-primary" || group.EgressIDs[1] != "group-standby" {
		t.Fatalf("group egress order = %#v", group.EgressIDs)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.PrimaryEgressID != storage.DefaultDirectEgressID || binding.StandbyEgressIDs != "" {
		t.Fatalf("account binding was rewritten: %+v", binding)
	}
}

func TestAutomationStatsUsesBatchBindingLookup(t *testing.T) {
	source := readAPISource(t, "settings_center.go")
	body := functionBody(t, source, "automationStats")
	if !strings.Contains(body, ".ListEgressBindingsByAccountIDs(") {
		t.Fatal("automationStats should batch-load egress bindings")
	}
	if strings.Contains(body, ".GetEgressBinding(") {
		t.Fatal("automationStats must not query egress bindings per account")
	}
}

func TestAdminDeleteReferencedEgressProfileReturnsConflict(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	upsertTestEgressProfile(t, h, "referenced-egress")
	if code, body := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"egress-reference","egress_ids":["referenced-egress"]}`); code != http.StatusOK {
		t.Fatalf("create referencing group = %d: %s", code, body)
	}

	code, body := grpReq(t, h, http.MethodDelete, "/admin/egress-profiles/referenced-egress", "")
	if code != http.StatusConflict || !strings.Contains(string(body), `"code":"egress_in_use"`) {
		t.Fatalf("delete referenced egress = %d: %s", code, body)
	}
	if _, err := h.store.GetEgressProfile(context.Background(), "referenced-egress"); err != nil {
		t.Fatalf("referenced profile was deleted: %v", err)
	}
}

func TestAdminEgressPoolDeleteCannotDeleteSameIDEgressProfile(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	upsertTestEgressProfile(t, h, "shared-resource-id")
	if err := h.store.UpsertEgressPool(context.Background(), storage.EgressPool{ID: "shared-resource-id", Purpose: "registration"}); err != nil {
		t.Fatal(err)
	}

	code, body := grpReq(t, h, http.MethodDelete, "/admin/egress-pools/shared-resource-id", "")
	if code != http.StatusNotFound {
		t.Fatalf("delete unsupported egress pool path = %d: %s", code, body)
	}
	if _, err := h.store.GetEgressProfile(context.Background(), "shared-resource-id"); err != nil {
		t.Fatalf("egress pool path deleted same-id profile: %v", err)
	}
}

func TestAdminEgressProfileMutationInvalidatesSchedulerCache(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	upsertTestEgressProfile(t, h, "cache-primary")
	upsertTestEgressProfile(t, h, "cache-standby")
	account := storage.Account{ID: "cache-publish-account", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	group, err := h.store.GetGroup(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.EgressIDs = []string{"cache-primary", "cache-standby"}
	if err := h.store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}

	first, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Egress.ID != "cache-primary" {
		first.Release()
		t.Fatalf("initial egress=%q, want cache-primary", first.Egress.ID)
	}
	first.Release()

	cooldownUntil := storage.Now() + 3600
	code, raw := grpReq(t, h, http.MethodPost, "/admin/egress-profiles", `{
		"id":"cache-primary","name":"cache-primary","type":"direct",
		"stream_capable":true,"health":"cooldown","cooldown_until":`+strconv.FormatInt(cooldownUntil, 10)+`,
		"max_concurrency":10,"detect_region":false
	}`)
	if code != http.StatusOK {
		t.Fatalf("update cached profile = %d: %s", code, raw)
	}

	second, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if second.Egress.ID != "cache-standby" {
		t.Fatalf("profile mutation remained cached: egress=%q", second.Egress.ID)
	}
}
