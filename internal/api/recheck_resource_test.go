package api

import (
	"strings"
	"testing"
)

func TestRecheckPendingAccountsUsesBatchAccountAndTokenLoads(t *testing.T) {
	source := readAPISource(t, "recheck.go")
	body := functionBody(t, source, "recheckPendingAccounts")
	for _, required := range []string{".ListAccountsByIDs(", ".ListTokensByAccountIDs("} {
		if !strings.Contains(body, required) {
			t.Fatalf("recheckPendingAccounts must batch-load with %s", required)
		}
	}
	for _, forbidden := range []string{".GetAccount(", ".GetToken("} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("recheckPendingAccounts must not use per-account %s", forbidden)
		}
	}
}
