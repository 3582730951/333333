package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// User-group outlet balancing and account balancing are independent controls.
// In particular, retaining an old account RPM threshold must not activate
// account balancing when that switch is disabled.
func TestUserGroupEgressRPMBalanceDoesNotEnableAccountBalance(t *testing.T) {
	const (
		groupName = "egress-only-balance-pool"
		groupID   = "ug-egress-only-balance"
		model     = "egress-only-balance-model"
	)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	for _, profile := range []storage.EgressProfile{
		{ID: "egress-only-a", Name: "Exit A", Type: "http_proxy", Endpoint: "http://a.invalid", Health: "healthy"},
		{ID: "egress-only-b", Name: "Exit B", Type: "http_proxy", Endpoint: "http://b.invalid", Health: "healthy"},
	} {
		if err := h.store.UpsertEgressProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.CreateGroup(ctx, storage.Group{
		Name: groupName, EgressIDs: []string{"egress-only-a", "egress-only-b"},
	}); err != nil {
		t.Fatal(err)
	}
	accountIDs := []string{"egress-only-account-a", "egress-only-account-b"}
	for _, accountID := range accountIDs {
		if err := h.store.UpsertAccount(ctx, storage.Account{
			ID: accountID, Label: accountID, GroupName: groupName, Provider: "codex", Status: "active",
		}, storage.AccountToken{AccessToken: "token-" + accountID}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
			AccountID: accountID, ModelSlug: model, AvailabilityState: "verified", Source: "egress_only_test",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.CreateUserGroupDefinition(ctx, storage.UserGroup{
		ID: groupID, Name: groupID, Targets: []storage.TargetRef{{
			Kind: storage.TargetKindAccountPoolGroup, ID: groupName,
		}},
		// The old account threshold remains persisted, but its switch is off.
		DynamicPoolBalanceRPMThreshold: 10,
		EgressRPMBalanceEnabled:        true,
		EgressRPMBalanceThreshold:      3,
		EgressRPMBalanceEgressIDs:      []string{"egress-only-a", "egress-only-b"},
	}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()
	// The router starts a background refresh; persist the same rate evidence so
	// a refresh cannot replace this fixture with an empty snapshot mid-selection.
	for i := range 12 {
		if err := h.store.RecordAccountRequestRateArrival(ctx, storage.AccountUsageRateEvent{
			EventID: fmt.Sprintf("egress-only-arrival-%d", i), AccountID: accountIDs[0],
			EgressID: "egress-only-a", OccurredAt: now - 1, AgentClass: storage.AgentClassRoot,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.app.scheduler.SetDynamicPoolBalanceSnapshot(now, map[string]storage.AccountRootRate{
		accountIDs[0]: {RootRPM: 12}, accountIDs[1]: {RootRPM: 0},
	})
	h.app.scheduler.SetEgressRPMBalanceSnapshot(now, map[string]int64{
		"egress-only-a": 12, "egress-only-b": 0,
	})

	raw := []byte(`{"model":"` + model + `","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req = req.WithContext(contextWithAccountAgentClass(req.Context(), storage.AgentClassRoot))
	response := httptest.NewRecorder()
	before := h.app.scheduler.Metrics()
	var selectedEgress string
	handled := h.app.dispatchUserGroupRouteCandidates(response, req, raw, raw, downstreamPolicy{
		UserGroupID: groupID, PolicyUserGroupID: groupID, Group: groupName,
	}, func(w http.ResponseWriter, candidate *http.Request) {
		lease, err := h.app.scheduler.Select(candidate.Context(), scheduler.Route{
			Provider: "codex", Model: model, SkipWait: true,
		})
		if err != nil {
			t.Fatalf("select routed account: %v", err)
		}
		selectedEgress = lease.Egress.ID
		lease.Release()
		w.WriteHeader(http.StatusOK)
	})
	if !handled || response.Code != http.StatusOK {
		t.Fatalf("handled=%v status=%d body=%s", handled, response.Code, response.Body.String())
	}
	after := h.app.scheduler.Metrics()
	if after.DynamicPoolBalanceSelections != before.DynamicPoolBalanceSelections {
		t.Fatalf("account balancing activated with switch disabled: before=%d after=%d", before.DynamicPoolBalanceSelections, after.DynamicPoolBalanceSelections)
	}
	if selectedEgress != "egress-only-b" {
		t.Fatalf("egress balancing did not select relief outlet: got=%q", selectedEgress)
	}
}
