package api

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// resolveUserGroupRoute selects the routing target for a request that uses the
// two-layer group model (pol.UserGroupID != ""). Returns:
//   - resolvedGroup: base group name (empty for built-in providers like kiro/antigravity)
//   - resolvedProvider: provider hint override ("kiro", "antigravity", "custom:<id>", or "")
//
// Selection: affinity-spread (consistent-hash weighted by AffinityWeight), with
// session-sticky re-use when an affinity binding already exists for this route key.
// Falls back to pol.Group / pol.ProviderHint when no user_group targets are configured.
func resolveUserGroupRoute(ctx context.Context, store *storage.Store, pol downstreamPolicy, r *http.Request, raw []byte) (resolvedGroup, resolvedProvider string, err error) {
	if pol.UserGroupID == "" {
		return pol.Group, pol.ProviderHint, nil
	}
	targets, tErr := store.GetUserGroupTargets(ctx, pol.UserGroupID)
	if tErr != nil || len(targets) == 0 {
		return pol.Group, pol.ProviderHint, tErr
	}

	// Session-sticky: if there's an existing affinity binding, re-use that target.
	affinityKeyObj := routing.ExtractAffinityKey(r, raw)
	affinityKey := affinityKeyObj.Hash
	bindingKey := ""
	if affinityKey != "" {
		bindingKey = fnvHashString(pol.UserGroupID + ":" + affinityKey)
		if binding, bErr := store.GetAffinityBinding(ctx, bindingKey); bErr == nil {
			// binding.Provider encodes the resolved target as "base_group:<name>",
			// "kiro", "antigravity", or "relay:<id>".
			return decodeUserGroupTarget(binding.Provider)
		}
	}

	// Affinity-spread: weighted consistent-hash selection.
	seed := pol.UserGroupID + ":" + affinityKey
	target := weightedSelectTarget(targets, seed)
	rg, rp := userGroupTargetToRoute(target)
	return rg, rp, nil
}

// userGroupTargetToRoute converts a UserGroupTarget to (group, providerHint).
func userGroupTargetToRoute(t storage.UserGroupTarget) (string, string) {
	switch t.TargetType {
	case storage.UserGroupTargetTypeKiro:
		return "", "kiro"
	case storage.UserGroupTargetTypeAntigravity:
		return "", "antigravity"
	case storage.UserGroupTargetTypeRelay:
		return "", "custom:" + t.TargetRef
	default: // base_group
		return t.TargetRef, ""
	}
}

// decodeUserGroupTarget reverses the encoding stored in AffinityBinding.Provider.
func decodeUserGroupTarget(encoded string) (string, string, error) {
	switch {
	case encoded == "kiro":
		return "", "kiro", nil
	case encoded == "antigravity":
		return "", "antigravity", nil
	case len(encoded) > 7 && encoded[:7] == "relay::":
		return "", "custom:" + encoded[7:], nil
	default:
		return encoded, "", nil
	}
}

// encodeUserGroupTarget produces the AffinityBinding.Provider value for a target.
func encodeUserGroupTarget(rg, rp string) string {
	switch {
	case rp == "kiro":
		return "kiro"
	case rp == "antigravity":
		return "antigravity"
	case len(rp) > 7 && rp[:7] == "custom:":
		return "relay::" + rp[7:]
	default:
		return rg
	}
}

// weightedSelectTarget picks a target using consistent hashing over affinity weights.
func weightedSelectTarget(targets []storage.UserGroupTarget, seed string) storage.UserGroupTarget {
	if len(targets) == 1 {
		return targets[0]
	}
	sorted := make([]storage.UserGroupTarget, len(targets))
	copy(sorted, targets)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	total := 0
	for _, t := range sorted {
		w := t.AffinityWeight
		if w < 1 {
			w = 1
		}
		total += w
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	pick := int(h.Sum32()) % total
	cum := 0
	for _, t := range sorted {
		w := t.AffinityWeight
		if w < 1 {
			w = 1
		}
		cum += w
		if pick < cum {
			return t
		}
	}
	return sorted[len(sorted)-1]
}

func fnvHashString(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}
