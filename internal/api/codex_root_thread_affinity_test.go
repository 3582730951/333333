package api

import (
	"net/http"
	"testing"

	"codex-account-pool/internal/bodysource"
)

// affinityForHeaders runs the real precedence chain used on the request path.
func affinityForHeaders(t *testing.T, headers map[string]string, meta *bodysource.BodyMeta) (string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	key := affinityWithMeta(req, []byte(`{"model":"gpt-5.6-sol","input":[]}`), meta)
	return key.Key, key.Source
}

// Upstream prompt caching only pays off when every turn of one conversation reaches
// the same account, which is what the affinity key decides. Diagnostics report
// `previous_response_id` at ~96% and `codex-root-thread-id` at ~55%, so the
// root-thread path is where the misses are. This characterizes the key across the
// header shapes one Codex conversation actually produces; every turn of the same
// conversation must yield the same key, or the turns are spread over accounts and
// each one pays a cold prefix.
func TestCodexRootThreadAffinityIsStableAcrossOneConversation(t *testing.T) {
	const root = "thread_root_aaa"
	const branch = "thread_branch_bbb"

	turns := []struct {
		name    string
		headers map[string]string
	}{
		{
			name:    "root turn 1",
			headers: map[string]string{"thread-id": root},
		},
		{
			name:    "root turn 2",
			headers: map[string]string{"thread-id": root},
		},
		{
			name: "branch turn, parent advertised",
			headers: map[string]string{
				"x-codex-parent-thread-id": root,
				"thread-id":                branch,
			},
		},
		{
			name: "branch turn, parent only in turn metadata",
			headers: map[string]string{
				"thread-id":             branch,
				"x-codex-turn-metadata": `{"parent_thread_id":"` + root + `"}`,
			},
		},
		{
			name: "branch turn, parent in metadata alongside its own thread_id",
			headers: map[string]string{
				"thread-id":             branch,
				"x-codex-turn-metadata": `{"thread_id":"` + branch + `","parent_thread_id":"` + root + `"}`,
			},
		},
	}

	meta := &bodysource.BodyMeta{}
	keys := make(map[string][]string)
	for _, turn := range turns {
		key, source := affinityForHeaders(t, turn.headers, meta)
		t.Logf("%-40s key=%-40s source=%s", turn.name, key, source)
		keys[key] = append(keys[key], turn.name)
	}

	if len(keys) != 1 {
		var report string
		for key, names := range keys {
			report += "\n  " + key + " <- "
			for i, n := range names {
				if i > 0 {
					report += ", "
				}
				report += n
			}
		}
		t.Errorf("one conversation produced %d distinct affinity keys, so its turns are "+
			"spread across accounts and each group pays a cold prefix:%s", len(keys), report)
	}
}

// The precedence must be root-before-branch in both directions: a turn that names
// only its own thread still keys on that thread, and a turn that names a parent
// keys on the parent even when it also names its own.
func TestCodexRootThreadAffinityPrefersParentOverBranch(t *testing.T) {
	meta := &bodysource.BodyMeta{}

	own, _ := affinityForHeaders(t, map[string]string{"thread-id": "solo"}, meta)
	if own != "codex-root-thread-id:solo" {
		t.Errorf("a turn with no parent must key on its own thread, got %q", own)
	}

	withParent, _ := affinityForHeaders(t, map[string]string{
		"thread-id":                "child",
		"x-codex-parent-thread-id": "parent",
	}, meta)
	if withParent != "codex-root-thread-id:parent" {
		t.Errorf("an advertised parent must win over the branch thread, got %q", withParent)
	}

	metaParent, _ := affinityForHeaders(t, map[string]string{
		"thread-id":             "child",
		"x-codex-turn-metadata": `{"parent_thread_id":"parent"}`,
	}, meta)
	if metaParent != "codex-root-thread-id:parent" {
		t.Errorf("a parent in turn metadata must win over the branch thread, got %q", metaParent)
	}
}

// A branch the pool has never seen advertised with a parent can only key on itself —
// nothing in the request identifies its root. That is the irreducible case.
func TestCodexNeverSeenBranchKeysOnItself(t *testing.T) {
	meta := &bodysource.BodyMeta{}
	key, _ := affinityForHeaders(t, map[string]string{"thread-id": "never_seen_branch"}, meta)
	if key != "codex-root-thread-id:never_seen_branch" {
		t.Fatalf("unexpected key %q", key)
	}
}

// Once any turn has advertised the parent, a later turn that advertises nothing joins
// the same conversation instead of starting a cold prefix. The mapping is learned
// from the advertising turns themselves, so this costs one map read rather than a
// storage lookup on the routing path.
func TestCodexParentlessBranchTurnJoinsTheRememberedConversation(t *testing.T) {
	meta := &bodysource.BodyMeta{}
	const root = "api_remember_root"
	const branch = "api_remember_branch"

	if key, _ := affinityForHeaders(t, map[string]string{
		"thread-id":                branch,
		"x-codex-parent-thread-id": root,
	}, meta); key != "codex-root-thread-id:"+root {
		t.Fatalf("advertised parent keyed on %q", key)
	}

	key, _ := affinityForHeaders(t, map[string]string{"thread-id": branch}, meta)
	if key != "codex-root-thread-id:"+root {
		t.Errorf("parentless branch turn keyed on %q, want the remembered root %q", key, root)
	}
}
