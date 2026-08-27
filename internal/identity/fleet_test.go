package identity

import (
	"testing"

	"codex-account-pool/internal/config"
)

// TestPerAccountVersionTupleCoherentWithFingerprintLibrary ensures independently
// derived accounts can never combine stale version axes into a tuple no real
// release shipped: the cli→stainless→node tuple must always match a row of the
// fingerprint library, even though the pool intentionally spans several shipped
// releases for fleet diversity.
func TestPerAccountVersionTupleCoherentWithFingerprintLibrary(t *testing.T) {
	secret := []byte("diversity-test")
	nodeSet := map[string]bool{}
	cliSet := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := For(secret, accountN(i))
		nodeSet[id.NodeVersion] = true
		cliSet[id.ClaudeCLIVersion] = true
		want := config.ClaudeStainlessVersionForCLI(id.ClaudeCLIVersion, "")
		if want == "" || want != id.StainlessPackageVersion {
			t.Fatalf("account %d: incoherent tuple cli=%s stainless=%s (library says %q)", i, id.ClaudeCLIVersion, id.StainlessPackageVersion, want)
		}
		if _, ok := config.ClaudeCLIFingerprintForVersion(id.ClaudeCLIVersion); !ok {
			t.Fatalf("account %d: cli version %q not in fingerprint library", i, id.ClaudeCLIVersion)
		}
	}
	if !nodeSet[config.DefaultClaudeNodeVersion] {
		t.Fatalf("node runtime not the captured ground truth: %v", nodeSet)
	}
	if len(cliSet) < 2 {
		t.Fatalf("pool should span multiple shipped CLI releases for diversity, got %v", cliSet)
	}
}

// TestStainlessVersionPopulated ensures every account's SDK version is coherent
// with its CLI version via the fingerprint library (the head being the captured
// ground truth), never a free-running axis that could combine stale tuples.
func TestStainlessVersionPopulated(t *testing.T) {
	id := For([]byte("s"), "acc-x")
	if id.StainlessPackageVersion == "" {
		t.Fatal("StainlessPackageVersion is empty")
	}
	if want := config.ClaudeStainlessVersionForCLI(id.ClaudeCLIVersion, ""); want != "" && want != id.StainlessPackageVersion {
		t.Fatalf("StainlessPackageVersion %q not coherent with cli %q (library says %q)", id.StainlessPackageVersion, id.ClaudeCLIVersion, want)
	}
}

// TestDiverseSpansMultipleOSFamilies asserts the "diverse" pool actually scatters
// accounts across OS families (so OS variety can match per-account IP variety).
func TestDiverseSpansMultipleOSFamilies(t *testing.T) {
	secret := []byte("diverse-os")
	fams := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := ForOS(secret, accountN(i), "diverse")
		fams[id.OSName] = true
	}
	if len(fams) < 2 {
		t.Fatalf("diverse pool produced only %d OS family(ies): %v", len(fams), fams)
	}
}

func accountN(i int) string {
	const digits = "0123456789abcdef"
	b := []byte("acc-0000")
	b[4] = digits[(i>>12)&0xf]
	b[5] = digits[(i>>8)&0xf]
	b[6] = digits[(i>>4)&0xf]
	b[7] = digits[i&0xf]
	return string(b)
}
