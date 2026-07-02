package api

import (
	"strings"
	"testing"
)

func TestHasCodexFailoverCandidateUsesBatchTokenLookup(t *testing.T) {
	source := readAPISource(t, "server.go")
	body := functionBody(t, source, "hasCodexFailoverCandidate")
	if !strings.Contains(body, ".ListTokensByAccountIDs(") {
		t.Fatal("hasCodexFailoverCandidate should batch-load candidate tokens")
	}
	if strings.Contains(body, ".GetToken(ctx, account.ID)") {
		t.Fatal("hasCodexFailoverCandidate must not query tokens per candidate account")
	}
}
