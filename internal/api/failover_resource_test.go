package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHasCodexFailoverCandidateUsesSchedulerEligibilitySnapshot(t *testing.T) {
	source := readAPISource(t, "server.go")
	body := functionBody(t, source, "hasCodexFailoverCandidate")
	if !strings.Contains(body, ".EligibleCandidateCount(") {
		t.Fatal("hasCodexFailoverCandidate should reuse scheduler eligibility")
	}
	for _, forbidden := range []string{".ListActiveAccountsByGroup(", ".ListTokensByAccountIDs(", ".GetToken("} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("hasCodexFailoverCandidate must not duplicate scheduler scans via %s", forbidden)
		}
	}
}

func TestCodexFailoverAttemptsAreBoundedByDistinctEligibleAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var accountIDs []string
	for _, name := range []string{"a", "b", "c"} {
		accountIDs = append(accountIDs, h.importAccount(t, "failover-"+name, "upstream-"+name, "access-"+name))
	}
	h.app.scheduler.InvalidateAccountCache()
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "10000"); err != nil {
		t.Fatal(err)
	}
	if got := h.app.codexFailoverAttempts(context.Background(), true, h.app.cfg.DefaultGroup, "", map[string]bool{}); got != 3 {
		t.Fatalf("attempts=%d, want three distinct eligible accounts", got)
	}
	if got := h.app.codexFailoverAttempts(context.Background(), true, h.app.cfg.DefaultGroup, "", map[string]bool{accountIDs[0]: true}); got != 2 {
		t.Fatalf("excluded account was counted, attempts=%d", got)
	}
}
