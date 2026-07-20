package api

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"sort"
	"strings"

	"codex-account-pool/internal/storage"
)

func requestedImportEgressID(egressID, primaryEgressID string) string {
	if id := strings.TrimSpace(egressID); id != "" {
		return id
	}
	return strings.TrimSpace(primaryEgressID)
}

func (s *Server) resolveImportPrimaryEgress(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return storage.DefaultDirectEgressID, nil
	}
	if _, err := s.store.GetEgressProfile(ctx, requested); err == nil {
		return requested, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return storage.DefaultDirectEgressID, nil
}

func (s *Server) bindImportedAccountPrimaryEgress(ctx context.Context, accountID, requested string) error {
	primary, err := s.resolveImportPrimaryEgress(ctx, requested)
	if err != nil {
		return err
	}
	// Re-importing credentials for an existing account must not silently remove its
	// independently configured sidecar (or standby list). UpsertAccount creates a
	// default binding for new accounts, so this also covers the fresh-import path.
	binding, getErr := s.store.GetEgressBinding(ctx, accountID)
	if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		return getErr
	}
	binding.AccountID = accountID
	binding.PrimaryEgressID = primary
	binding.CookieJarKey = accountID + ":" + primary
	return s.store.UpsertEgressBinding(ctx, binding)
}

// resolveKiroDefaultEgress picks a stealth-preferred egress for a newly imported
// Kiro account when the operator did not choose one. Kiro's anti-abuse flags a
// fleet of accounts that all leave from one datacenter IP with a stock Go TLS
// fingerprint; binding each account to a real egress mitigates both. Preference:
//  1. a healthy curl_cffi_sidecar egress (real JA3 + optional WARP chain proxy),
//  2. otherwise any other healthy non-direct egress (at least a distinct exit IP).
//
// Accounts are spread deterministically across the eligible set (hash of the
// account id) so a bulk import fans out over the available exits instead of
// stacking on one. Falls back to DefaultDirectEgressID when NO eligible egress
// exists, so a deployment without a sidecar/WARP behaves exactly as before. The
// operator can always override at import time or re-assign afterward via
// POST /admin/groups/<name>/assign-egress.
func (s *Server) resolveKiroDefaultEgress(ctx context.Context, accountID string) string {
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil || len(profiles) == 0 {
		return storage.DefaultDirectEgressID
	}
	now := storage.Now()
	var sidecars, others []storage.EgressProfile
	for _, p := range profiles {
		if p.ID == storage.DefaultDirectEgressID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(p.Health), "healthy") {
			continue
		}
		if p.CooldownUntil > now {
			continue
		}
		lt := strings.ToLower(p.Type)
		if strings.Contains(lt, "sidecar") || strings.Contains(lt, "curl_cffi") {
			sidecars = append(sidecars, p)
		} else {
			// Any other non-direct egress still gives a distinct exit IP.
			others = append(others, p)
		}
	}
	pick := sidecars
	if len(pick) == 0 {
		pick = others
	}
	if len(pick) == 0 {
		return storage.DefaultDirectEgressID
	}
	sort.SliceStable(pick, func(i, j int) bool { return pick[i].ID < pick[j].ID })
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountID))
	return pick[int(h.Sum32())%len(pick)].ID
}
