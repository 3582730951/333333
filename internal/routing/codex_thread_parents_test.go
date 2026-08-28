package routing

import (
	"net/http"
	"testing"
)

func affinityFor(t *testing.T, headers map[string]string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return ExtractAffinityKey(req, []byte(`{"model":"gpt-5.6-sol","input":[]}`)).Key
}

// The gap the ordering fix could not close: a branch turn carrying no parent
// anywhere keys on the branch and starts a cold prefix. The turns that DO advertise
// a parent already know the answer, so remembering what they said lets the silent
// ones join the same conversation — with no storage query on the routing path.
//
// Thread ids are unique per test because the table is process-wide by design.
func TestParentlessBranchTurnInheritsARememberedRoot(t *testing.T) {
	const root = "thread_remember_root"
	const branch = "thread_remember_branch"

	// Before anything is known, the branch can only key on itself.
	if got := affinityFor(t, map[string]string{"thread-id": branch}); got != "codex-root-thread-id:"+branch {
		t.Fatalf("unknown branch keyed on %q, want the branch itself", got)
	}

	// A turn that advertises the parent teaches the mapping.
	if got := affinityFor(t, map[string]string{
		"thread-id":                branch,
		"x-codex-parent-thread-id": root,
	}); got != "codex-root-thread-id:"+root {
		t.Fatalf("advertised parent keyed on %q, want the root", got)
	}

	// Now the silent turn joins the same conversation.
	if got := affinityFor(t, map[string]string{"thread-id": branch}); got != "codex-root-thread-id:"+root {
		t.Errorf("parentless branch turn keyed on %q, want the remembered root %q", got, root)
	}
}

// A parent advertised only in turn metadata must teach the mapping too, since that
// is one of the shapes Codex actually sends.
func TestTurnMetadataParentAlsoTeachesTheMapping(t *testing.T) {
	const root = "thread_meta_root"
	const branch = "thread_meta_branch"

	if got := affinityFor(t, map[string]string{
		"thread-id":             branch,
		"x-codex-turn-metadata": `{"parent_thread_id":"` + root + `"}`,
	}); got != "codex-root-thread-id:"+root {
		t.Fatalf("metadata parent keyed on %q, want the root", got)
	}
	if got := affinityFor(t, map[string]string{"thread-id": branch}); got != "codex-root-thread-id:"+root {
		t.Errorf("mapping learned from turn metadata was not used: %q", got)
	}
}

// A root thread must never map to itself, or the table would grow one useless entry
// per conversation and a root turn would depend on it.
func TestRootThreadIsNotRememberedAsItsOwnParent(t *testing.T) {
	const root = "thread_selfref"
	RememberCodexThreadParent(root, root)
	if got := CodexRootForBranch(root); got != "" {
		t.Errorf("a self-reference was stored: %q", got)
	}
	if got := affinityFor(t, map[string]string{"thread-id": root}); got != "codex-root-thread-id:"+root {
		t.Errorf("root turn keyed on %q", got)
	}
}

func TestRememberCodexThreadParentIgnoresEmptyValuesAndUpdatesOnChange(t *testing.T) {
	RememberCodexThreadParent("", "root")
	RememberCodexThreadParent("branch", "")
	if got := CodexRootForBranch(""); got != "" {
		t.Errorf("empty branch stored: %q", got)
	}

	const branch = "thread_reparent_branch"
	RememberCodexThreadParent(branch, "thread_reparent_first")
	RememberCodexThreadParent(branch, "thread_reparent_second")
	if got := CodexRootForBranch(branch); got != "thread_reparent_second" {
		t.Errorf("newest parent did not win: %q", got)
	}
}

// The table is bounded: a long-lived process must not accumulate one entry per
// branch thread forever.
func TestCodexThreadParentTableIsBounded(t *testing.T) {
	for i := 0; i < maxCodexThreadParents+512; i++ {
		RememberCodexThreadParent(
			"bounded_branch_"+itoa(i),
			"bounded_root_"+itoa(i),
		)
	}
	codexThreadParents.RLock()
	size := len(codexThreadParents.roots)
	codexThreadParents.RUnlock()
	if size > maxCodexThreadParents {
		t.Errorf("table grew to %d entries, cap is %d", size, maxCodexThreadParents)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
