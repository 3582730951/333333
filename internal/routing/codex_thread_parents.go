package routing

import "sync"

// Codex advertises a branch's parent inconsistently: some turns carry
// x-codex-parent-thread-id, some carry it only inside x-codex-turn-metadata, and
// some carry nothing but the branch's own thread-id. The last shape cannot be
// attributed to its conversation from the request alone, so it keys on the branch
// and starts a cold prompt-cache prefix on whichever account it happens to land.
//
// The mapping does not need a database. The turns that *do* advertise a parent
// already carry the answer, and a branch is normally created by one of them — so
// remembering what those turns said is enough to answer for the ones that stay
// silent. That keeps the routing hot path at one map read with no storage query.
//
// This is a routing hint, not a record of truth: losing it on restart returns the
// previous behaviour (key on the branch) rather than producing a wrong answer, and
// a stale entry can only send a turn to the account its own conversation already
// prefers. Goal-continuity storage remains the durable owner of thread identity.
const maxCodexThreadParents = 8192

var codexThreadParents = struct {
	sync.RWMutex
	roots map[string]string
}{roots: make(map[string]string, 256)}

// RememberCodexThreadParent records that branch belongs to root. Self-references and
// empty values are ignored, so a root thread never maps to itself.
func RememberCodexThreadParent(branch, root string) {
	if branch == "" || root == "" || branch == root {
		return
	}
	codexThreadParents.Lock()
	defer codexThreadParents.Unlock()
	if existing, ok := codexThreadParents.roots[branch]; ok {
		if existing == root {
			return
		}
		// A branch that reports a different parent than before: trust the newest turn.
		codexThreadParents.roots[branch] = root
		return
	}
	if len(codexThreadParents.roots) >= maxCodexThreadParents {
		// Bounded, and cheap: drop one arbitrary entry rather than tracking recency.
		// A dropped mapping degrades to keying on the branch, which is what would have
		// happened without this table at all.
		for key := range codexThreadParents.roots {
			delete(codexThreadParents.roots, key)
			break
		}
	}
	codexThreadParents.roots[branch] = root
}

// CodexRootForBranch returns the remembered root for a branch thread, or "" when
// none is known.
func CodexRootForBranch(branch string) string {
	if branch == "" {
		return ""
	}
	codexThreadParents.RLock()
	defer codexThreadParents.RUnlock()
	return codexThreadParents.roots[branch]
}

// CodexRootOrSelf returns the affinity value to use for a turn whose own thread is
// branch: the remembered root when there is one, otherwise the branch itself.
func CodexRootOrSelf(branch string) string {
	if root := CodexRootForBranch(branch); root != "" {
		return root
	}
	return branch
}
