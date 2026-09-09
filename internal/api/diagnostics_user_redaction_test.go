package api

import (
	"strings"
	"testing"
)

func TestDiagnosticAuditUserIdentifiersAreAliased(t *testing.T) {
	book := buildDiagnosticCodebookWithKey([]byte("diagnostic-fixture-key"), nil, nil, nil, nil, nil, nil)
	const rawID = "usr_1234567890123456789"
	const input = "actor=user:" + rawID + " user_id=" + rawID + " role=admin"
	if !diagnosticDLPMatch(input) {
		t.Fatal("DLP gate accepted a raw user identifier in audit details")
	}
	alias := diagnosticAlias(book.aliasKey, "USR", "user", rawID)
	want := "actor=user:" + alias + " user_id=" + alias + " role=admin"
	got := book.sanitize(input)
	if got != want || strings.Contains(got, rawID) {
		t.Fatalf("audit actor was not consistently aliased: %s", got)
	}
	if diagnosticDLPMatch(got) || book.sanitize(got) != got {
		t.Fatal("sanitized audit identity failed DLP or changed on a second pass")
	}
}
