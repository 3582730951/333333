package api

import (
	"reflect"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestModelProbeSweepUsesBatchDependencies(t *testing.T) {
	source := readAPISource(t, "account_probe.go")
	body := functionBody(t, source, "probeAllAccounts")
	for _, required := range []string{
		".ListTokensByAccountIDs(",
		".ListEgressBindingsByAccountIDs(",
		".ListEgressProfiles(",
		".probeAccountModelsWithDeps(",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("probeAllAccounts must use %s", required)
		}
	}
	for _, forbidden := range []string{
		".probeAccountModels(",
		".GetToken(",
		".GetEgressBinding(",
		".GetEgressProfile(",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("probeAllAccounts must not use per-account %s", forbidden)
		}
	}
}

func TestModelProbeDependencyHelpers(t *testing.T) {
	accounts := []storage.Account{
		{ID: "active-a", Status: "active"},
		{ID: "disabled", Status: "disabled"},
		{ID: "active-b", Status: "active"},
	}
	active := activeModelProbeAccounts(accounts)
	if got, want := accountIDsFromAccounts(active), []string{"active-a", "active-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active account ids = %#v, want %#v", got, want)
	}

	profiles := modelProbeEgressProfilesByID([]storage.EgressProfile{
		{ID: "direct", Type: "direct"},
		{ID: "", Type: "http_proxy"},
	})
	if len(profiles) != 1 || profiles["direct"].Type != "direct" {
		t.Fatalf("profiles by id = %#v, want one direct profile", profiles)
	}
}

func TestAdminRefreshHandlesAccountLookupErrors(t *testing.T) {
	source := readAPISource(t, "account_probe.go")
	body := functionBody(t, source, "adminRefresh")
	if strings.Contains(body, "account, _ := s.store.GetAccount") {
		t.Fatal("adminRefresh should handle account lookup errors")
	}
	if strings.Contains(body, "if account, aerr := s.store.GetAccount") {
		t.Fatal("adminRefresh should reuse the loaded account instead of querying it again on refresh failure")
	}
	if !strings.Contains(body, "writeError(w, http.StatusInternalServerError, err)") {
		t.Fatal("adminRefresh should return a 500 for non-not-found account lookup errors")
	}
}
