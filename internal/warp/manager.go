// Package warp manages WARP egress profiles that operators may select explicitly
// for an account-pool group or account. The exits are provisioned by scripts/install.sh
// --with-warp (wgcf generates N free WARP WireGuard profiles; wireproxy exposes each
// as a local SOCKS5 listener on a distinct port → a distinct WARP exit IP). This
// Manager describes that topology to the server and maintains explicitly selected exits:
//
//   - EnsurePool seeds one egress profile per exit (plus a JA3 variant routed through
//     the sidecar so a fingerprinted account keeps its TLS identity while changing IP).
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
	"strconv"
	"strings"
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
	m.logf("warp: ensured %d operator-selectable exits (base port %d)", m.cfg.WarpExitCount, m.cfg.WarpExitBasePort)
	return nil
}

// AssignCFAccount is retained for source compatibility with older integrations.
// Automatic WARP assignment is intentionally retired: outlet selection is owned by
// the account's group unless the account has an explicit override.
func (m *Manager) AssignCFAccount(ctx context.Context, accountID string) (string, error) {
	_ = ctx
	_ = accountID
	return "", nil
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
