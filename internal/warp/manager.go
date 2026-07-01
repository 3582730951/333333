// Package warp manages the multi-exit WARP fallback pool used as a CF (Cloudflare)
// mitigation rung. The exits themselves are provisioned by scripts/install.sh
// --with-warp (wgcf generates N free WARP WireGuard profiles; wireproxy exposes each
// as a local SOCKS5 listener on a distinct port → a distinct WARP exit IP). This
// Manager describes that topology to the server and implements the runtime policy:
//
//   - EnsurePool seeds one egress profile per exit (plus a JA3 variant routed through
//     the sidecar so a fingerprinted account keeps its TLS identity while changing IP).
//   - AssignCFAccount moves a CF-hit account onto the least-loaded healthy exit,
//     packing at most WarpAccountsPerExit (default 3) accounts per exit IP so a single
//     flagged exit's blast radius stays small (the "一个组3个号" requirement). It adds
//     the exit as a STANDBY egress, so the existing scheduler.selectEgress /
//     doWithCFRetry machinery reroutes the (cooled) account through WARP — no new
//     dispatch code.
//   - ReregisterExit re-registers an exit's wgcf profile for a fresh WARP IP (the
//     recovery step when an exit is itself CF-blocked and the solver could not clear
//     it), via scripts/warp-exit.sh, then re-probes and clears its cooldown.
//
// The official Cloudflare client is deliberately NOT used: it gives only one exit per
// host and imposes a 10s proxy-mode request timeout that breaks long SSE — neither
// works for "每个组不同的 warp 出口".
package warp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// Prober re-detects an egress's exit IP/region. It is wired from
// upstream.Client.ProbeEgress so the manager can refresh exit_ip after a
// re-registration without importing the upstream package.
type Prober func(ctx context.Context, egress storage.EgressProfile) (ip, region string, err error)

// Manager owns the WARP fallback pool lifecycle/policy.
type Manager struct {
	cfg    config.Config
	store  *storage.Store
	prober Prober
	logf   func(string, ...interface{})
	mu     sync.Mutex // serializes AssignCFAccount so two concurrent CF events don't both claim the last slot
}

func NewManager(cfg config.Config, store *storage.Store, prober Prober, logf func(string, ...interface{})) *Manager {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	return &Manager{cfg: cfg, store: store, prober: prober, logf: logf}
}

// Enabled reports whether the WARP fallback is configured (flag on + at least one
// provisioned exit).
func (m *Manager) Enabled() bool {
	return m != nil && m.cfg.WarpEnabled && m.cfg.WarpExitCount > 0
}

func plainExitID(i int) string { return fmt.Sprintf("warp-%d", i) }
func ja3ExitID(i int) string   { return fmt.Sprintf("warp-%d-ja3", i) }

// exitIndexOf parses the 1-based exit index from a warp egress id (warp-3 or
// warp-3-ja3 → 3), or 0 when egressID is not a warp exit.
func exitIndexOf(egressID string) int {
	s := strings.TrimPrefix(egressID, "warp-")
	if s == egressID {
		return 0
	}
	s = strings.TrimSuffix(s, "-ja3")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// IsWarpExit reports whether an egress id belongs to the WARP pool.
func (m *Manager) IsWarpExit(egressID string) bool { return exitIndexOf(egressID) > 0 }

func (m *Manager) exitPort(i int) int { return m.cfg.WarpExitBasePort + i - 1 }

// EnsurePool idempotently seeds the warp-* egress profiles for every provisioned
// exit. It only CREATES missing profiles (never overwrites a live one), so runtime
// state — exit_ip, cooldown, health — set after a re-registration survives a restart.
func (m *Manager) EnsurePool(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}
	sidecar := strings.TrimSpace(m.cfg.DefaultSidecarEndpoint)
	for i := 1; i <= m.cfg.WarpExitCount; i++ {
		socks := fmt.Sprintf("socks5h://127.0.0.1:%d", m.exitPort(i))
		if _, err := m.store.GetEgressProfile(ctx, plainExitID(i)); err != nil {
			plain := storage.EgressProfile{
				ID: plainExitID(i), Name: plainExitID(i), Type: "socks5h_proxy",
				Endpoint: socks, StreamCapable: true, Health: "healthy",
				MaxConcurrency: 16, Region: "warp",
			}
			if err := m.store.UpsertEgressProfile(ctx, plain); err != nil {
				return err
			}
		}
		// JA3 variant: a sidecar egress whose impersonated request chains through this
		// exit's SOCKS, so a fingerprinted account keeps its real Codex/Claude JA3 AND
		// leaves from the WARP IP. Only when a sidecar endpoint is configured.
		if sidecar != "" {
			if _, err := m.store.GetEgressProfile(ctx, ja3ExitID(i)); err != nil {
				ja3 := storage.EgressProfile{
					ID: ja3ExitID(i), Name: ja3ExitID(i), Type: "curl_cffi_sidecar",
					Endpoint: sidecar, ChainProxy: socks, StreamCapable: true,
					Health: "healthy", MaxConcurrency: 16, Region: "warp",
				}
				if err := m.store.UpsertEgressProfile(ctx, ja3); err != nil {
					return err
				}
			}
		}
	}
	m.logf("warp: ensured pool of %d exits (base port %d, ≤%d accounts/exit)", m.cfg.WarpExitCount, m.cfg.WarpExitBasePort, m.cfg.WarpAccountsPerExit)
	return nil
}

// AssignCFAccount moves a CF-hit account onto the least-loaded healthy WARP exit that
// still has capacity, adding it as a standby egress so the scheduler reroutes the
// (cooled) account through WARP. It picks the JA3 variant when the account's primary
// egress is the sidecar (keep the fingerprint), else the plain SOCKS variant. Returns
// the chosen egress id, or "" when the pool is full / disabled (the caller then falls
// back to the normal cooldown/quarantine ladder). Idempotent: if the account already
// has a WARP standby, that id is returned unchanged.
func (m *Manager) AssignCFAccount(ctx context.Context, accountID string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	binding, err := m.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return "", err
	}
	for _, id := range binding.StandbyIDs() {
		if m.IsWarpExit(id) {
			return id, nil
		}
	}
	wantJA3 := false
	if eg, err := m.store.GetEgressProfile(ctx, binding.PrimaryEgressID); err == nil {
		wantJA3 = strings.EqualFold(strings.TrimSpace(eg.Type), "curl_cffi_sidecar")
	}

	counts := m.exitCounts(ctx)
	now := storage.Now()
	type cand struct{ idx, count int }
	var cands []cand
	for i := 1; i <= m.cfg.WarpExitCount; i++ {
		if counts[i] >= m.cfg.WarpAccountsPerExit {
			continue
		}
		if !m.exitHealthy(ctx, i, now) {
			continue
		}
		cands = append(cands, cand{idx: i, count: counts[i]})
	}
	if len(cands) == 0 {
		m.logf("warp: no exit with capacity for account %s (pool full or all cooled)", accountID)
		return "", nil
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].count < cands[b].count })
	idx := cands[0].idx
	egressID := plainExitID(idx)
	if wantJA3 && m.ja3VariantExists(ctx, idx) {
		egressID = ja3ExitID(idx)
	}
	if _, err := m.store.AddStandbyEgress(ctx, accountID, egressID); err != nil {
		return "", err
	}
	m.logf("warp: assigned account %s → exit %s", accountID, egressID)
	return egressID, nil
}

// exitCounts returns exit index → number of distinct accounts whose binding
// references that exit (primary or standby, either variant counted once per account).
func (m *Manager) exitCounts(ctx context.Context) map[int]int {
	counts := map[int]int{}
	bindings, err := m.store.ListEgressBindings(ctx)
	if err != nil {
		m.logf("warp: list bindings: %v", err)
		return counts
	}
	for _, b := range bindings {
		seen := map[int]bool{}
		ids := append([]string{b.PrimaryEgressID}, b.StandbyIDs()...)
		for _, id := range ids {
			if idx := exitIndexOf(id); idx > 0 && !seen[idx] {
				seen[idx] = true
				counts[idx]++
			}
		}
	}
	return counts
}

// exitHealthy mirrors scheduler.EgressHealthy for the exit's plain profile: usable
// when its cooldown has passed and its health is not a hard-down state.
func (m *Manager) exitHealthy(ctx context.Context, i int, now int64) bool {
	eg, err := m.store.GetEgressProfile(ctx, plainExitID(i))
	if err != nil {
		return false
	}
	if eg.CooldownUntil > now {
		return false
	}
	switch eg.Health {
	case "", "healthy", "cooldown", "tripped":
		return true
	}
	return false
}

func (m *Manager) ja3VariantExists(ctx context.Context, idx int) bool {
	_, err := m.store.GetEgressProfile(ctx, ja3ExitID(idx))
	return err == nil
}

// ReregisterExit re-registers the wgcf profile behind a warp exit so it gets a new
// WARP IP, restarts that wireproxy instance via scripts/warp-exit.sh, then re-probes
// and clears the cooldown on both variants of the exit. egressID may be either
// variant (warp-i or warp-i-ja3). It is the recovery step for a CF-blocked exit that
// the solver could not clear.
func (m *Manager) ReregisterExit(ctx context.Context, egressID string) error {
	idx := exitIndexOf(egressID)
	if idx <= 0 {
		return fmt.Errorf("not a warp exit: %s", egressID)
	}
	script := strings.TrimSpace(m.cfg.WarpExitScript)
	if script == "" {
		return errors.New("warp_exit_script not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, script, "reregister", strconv.Itoa(idx))
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logf("warp: reregister exit %d failed: %v: %s", idx, err, strings.TrimSpace(string(out)))
		return err
	}
	m.logf("warp: reregistered exit %d (new IP)", idx)
	for _, id := range []string{plainExitID(idx), ja3ExitID(idx)} {
		eg, gerr := m.store.GetEgressProfile(ctx, id)
		if gerr != nil {
			continue
		}
		if m.prober != nil {
			if ip, region, perr := m.prober(ctx, eg); perr == nil {
				eg.ExitIP = ip
				if region != "" {
					eg.Region = region
				}
			}
		}
		eg.Health = "healthy"
		eg.CooldownUntil = 0
		_ = m.store.UpsertEgressProfile(ctx, eg)
	}
	return nil
}
