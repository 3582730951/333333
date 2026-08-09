package identity

import "testing"

// TestPerAccountVersionTupleMatchesCapturedRelease ensures independently-derived
// accounts cannot combine stale version axes into a tuple no real release shipped.
func TestPerAccountVersionTupleMatchesCapturedRelease(t *testing.T) {
	secret := []byte("diversity-test")
	node := map[string]bool{}
	cli := map[string]bool{}
	sdk := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := For(secret, accountN(i))
		node[id.NodeVersion] = true
		cli[id.ClaudeCLIVersion] = true
		sdk[id.StainlessPackageVersion] = true
	}
	if len(node) != 1 || len(cli) != 1 || len(sdk) != 1 {
		t.Fatalf("version tuple drift: node=%v cli=%v sdk=%v", node, cli, sdk)
	}
	if !node["v26.3.0"] || !cli[ClaudeCLIVersion] || !sdk["0.94.0"] {
		t.Fatalf("version tuple does not match captured 2.1.226 release: node=%v cli=%v sdk=%v", node, cli, sdk)
	}
}

// TestStainlessVersionPopulated ensures every account gets a non-empty SDK version
// drawn from the pool (the head being the captured ground truth).
func TestStainlessVersionPopulated(t *testing.T) {
	id := For([]byte("s"), "acc-x")
	if id.StainlessPackageVersion == "" {
		t.Fatal("StainlessPackageVersion is empty")
	}
	found := false
	for _, v := range stainlessVersionPool {
		if v == id.StainlessPackageVersion {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("StainlessPackageVersion %q not from pool", id.StainlessPackageVersion)
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
