package api

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"strings"
	"testing"
)

// The spreadsheet-formula guard has exactly two call sites: diagnosticSafeCSVCell,
// which writes the archive, and validateDiagnosticEntry, which re-reads it before
// publishing. They have to agree on every value or the export fails validation
// instead of shipping, so both now call diagnosticNeedsFormulaGuard and these tests
// pin the agreement rather than trusting a comment.

func TestDiagnosticFormulaGuardExemptsNumericLiterals(t *testing.T) {
	// A negative number is a number in every spreadsheet, not an expression.
	// Guarding it turned numeric columns into text: a v3 bundle from the field
	// carried 154 route_attempts rows whose tier read `'-1`, so nothing downstream
	// could sort or aggregate that column.
	for _, value := range []string{"-1", "+1", "-0", "0", "42", "-3.5", "+2.25", ".5", "-.5", "1e9", "-1.2e-3", "+4E+2"} {
		if diagnosticNeedsFormulaGuard(value) {
			t.Errorf("%q is a numeric literal and must not be guarded", value)
		}
		if got := diagnosticSafeCSVCell(value); got != value {
			t.Errorf("diagnosticSafeCSVCell(%q) = %q, want it unchanged", value, got)
		}
	}
}

func TestDiagnosticFormulaGuardStillCatchesFormulas(t *testing.T) {
	// Every one of these begins with a character a spreadsheet treats as the start
	// of an expression and is NOT a bare number. The "-1+..." and "+1-..." cases are
	// the ones a naive "starts with a digit after the sign" check would wave through.
	for _, value := range []string{
		"=1+1",
		"=cmd|'/c calc'!A0",
		"-1+cmd|'/c calc'!A0",
		"+1-2+cmd|'/c calc'!A0",
		"@SUM(A1:A9)",
		"-",
		"+",
		"1-2", // does not lead with a sign, so it is not guarded...
		"\tx", // ...unlike a leading tab.
	} {
		guarded := diagnosticNeedsFormulaGuard(value)
		wantGuard := strings.ContainsRune("=+-@\t\r", rune(value[0]))
		if guarded != wantGuard {
			t.Errorf("diagnosticNeedsFormulaGuard(%q) = %v, want %v", value, guarded, wantGuard)
		}
		if !wantGuard {
			continue
		}
		if cell := diagnosticSafeCSVCell(value); !strings.HasPrefix(cell, "'") {
			t.Errorf("diagnosticSafeCSVCell(%q) = %q, want an apostrophe guard", value, cell)
		}
	}
}

// The invariant that matters operationally: anything the writer emits, the gate
// accepts. A one-sided exemption does not corrupt data -- it fails the whole export
// as dlp_validation_failed, which is how an operator loses diagnostics precisely
// when they need them.
//
// Driven through validateEmergencyDiagnosticArchive rather than through the shared
// predicate, deliberately. Asserting the predicate against itself proves nothing: it
// would still pass if someone re-inlined the old literal rune check into the gate,
// which is exactly the divergence this is here to catch.
func TestDiagnosticSanitizedCellsAlwaysSurviveTheDLPGate(t *testing.T) {
	corpus := []string{
		"", "-1", "+1", "-3.5", "1e9", "-1.2e-3", "0", "42",
		"=1+1", "-1+cmd", "@SUM(A1:A9)", "-", "+", "@", "=",
		"\tleading tab", "plain text", "structurally_unavailable", "availability_marker",
	}
	header := []string{"tier", "value"}
	rows := [][]string{header}
	for index, value := range corpus {
		rows = append(rows, []string{
			diagnosticSafeCSVCell(strconv.Itoa(index - 5)),
			diagnosticSafeCSVCell(value),
		})
	}
	var csvBuffer bytes.Buffer
	writer := csv.NewWriter(&csvBuffer)
	if err := writer.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}

	archive, err := emergencyDiagnosticZip(map[string]string{"route_attempts.csv": csvBuffer.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEmergencyDiagnosticArchive(archive); err != nil {
		t.Fatalf("the DLP gate rejected the sanitizer's own output: %v\n%s", err, csvBuffer.String())
	}

	// And the gate has not simply been loosened: an unguarded formula must still be
	// refused, or the exemption above would be a hole rather than a correction.
	var raw bytes.Buffer
	rawWriter := csv.NewWriter(&raw)
	if err := rawWriter.WriteAll([][]string{header, {"-1", "=cmd|'/c calc'!A0"}}); err != nil {
		t.Fatal(err)
	}
	rawWriter.Flush()
	hostile, err := emergencyDiagnosticZip(map[string]string{"route_attempts.csv": raw.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEmergencyDiagnosticArchive(hostile); err == nil {
		t.Fatal("the DLP gate accepted an unguarded spreadsheet formula")
	}
}

// Guarding is idempotent, so a value that has already been through the writer does
// not collect a second apostrophe on a re-export.
func TestDiagnosticFormulaGuardIsIdempotent(t *testing.T) {
	for _, value := range []string{"=1+1", "-1+cmd", "@SUM(A1)", "-1", "plain"} {
		once := diagnosticSafeCSVCell(value)
		if twice := diagnosticSafeCSVCell(once); twice != once {
			t.Errorf("re-sanitising %q changed it: %q -> %q", value, once, twice)
		}
	}
}
