package api

import (
	"strings"
	"testing"
)

func TestAdminGroupAssignEgressUsesBatchBindingLookup(t *testing.T) {
	source := readAPISource(t, "admin_config.go")
	body := functionBody(t, source, "adminGroupAssignEgress")
	if !strings.Contains(body, ".ListEgressBindingsByAccountIDs(") {
		t.Fatal("adminGroupAssignEgress should batch-load egress bindings")
	}
	for _, bad := range []string{
		".GetEgressBinding(r.Context(), acc.ID)",
		".GetEgressBinding(ctx, acc.ID)",
	} {
		if strings.Contains(body, bad) {
			t.Fatalf("adminGroupAssignEgress must not query egress bindings per account; found %q", bad)
		}
	}
	if strings.Contains(body, "_ = s.store.UpdateGroup") {
		t.Fatal("adminGroupAssignEgress should not ignore group default update errors")
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
