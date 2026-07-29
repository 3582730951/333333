// Package identity generates a stable, per-account "virtual" client identity.
//
// Downstream CLIs (Codex, Claude Code, ...) embed real deployment/environment
// information in their requests: session IDs, a machine/user id, OS/arch, the
// terminal program, hostnames and absolute paths in the system prompt's <env>
// block, and so on. When many accounts are relayed through one server that
// information is inconsistent with each account and, taken together, is a strong
// "this is a proxy" signal to the upstream.
//
// identity.For(accountID) deterministically derives a complete, self-consistent
// synthetic identity for an account. It is stable across restarts (pure function
// of the server secret + account id, no storage needed) and distinct per account,
// so each account always presents the same plausible machine to the upstream.
// The values are fed to internal/streamrewrite to replace the real values found
// in the data stream.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Current official client versions. Kept here so a single edit updates the
// fingerprint everywhere. These should track the real shipping clients.
// Refreshed 2026-07-29 from the shipping Claude Code 2.1.220 binary and the
// official Codex rust-v0.146.0 source/release. Claude's shipping tuple remains
// Node v26.3.0 with Stainless package 0.94.0.
const (
	CodexCLIVersion  = "0.146.0"
	ClaudeCLIVersion = "2.1.220"
	// CodexOriginator is the interactive Codex CLI entrypoint id; CodexOriginatorExec
	// is the `codex exec` (non-interactive) entrypoint. The official client sends one
	// or the other depending on how it was launched, so the relay mirrors whichever
	// the downstream presents (see codexEntrypoint) instead of forcing a single one.
	CodexOriginator       = "codex_cli_rs"
	CodexOriginatorExec   = "codex_exec"
	CodexOriginatorTUI    = "codex-tui"
	CodexOriginatorVSCode = "codex_vscode"
	// CodexJA3 is the TLS ClientHello fingerprint (ja3 string) captured from the real
	// Codex (Rust) binary against api.openai.com — ja3 hash 69d274b521896ab1d71737c4d804e22c
	// (/tmp/pool-capture-20260710/manifest.json; current Codex source still uses stock
	// reqwest/rustls without ClientHello customization).
	// It is REFERENCE DATA, not a default or an alias target: verified against the Codex
	// source (other_codex), the real client does NO JA3 spoofing — it builds a stock
	// reqwest 0.12 + rustls 0.23 client with zero TLS customization, so this is simply
	// what vanilla rustls emits. Note the cipher list ends in 255 (0x00FF), the
	// TLS_EMPTY_RENEGOTIATION_INFO_SCSV *signalling* value, which curl_cffi/BoringSSL
	// emit via an extension and cannot list as a cipher — so this JA3 cannot be replayed
	// verbatim by the sidecar. The Codex sidecar egress therefore defaults to Chrome and
	// the "real Codex" aliases resolve to Chrome too (see upstream.resolveCodexJA3); an
	// operator can still paste this string into codex_ja3 to force a best-effort
	// (SCSV-sanitized) replay, which degrades to Chrome if curl_cffi still can't reproduce it.
	CodexJA3 = "771,4866-4865-4867-49196-49195-52393-49200-49199-52392-255,11-51-13-35-5-23-43-10-0-45,4588-29-23-24,0"
	// ClaudeJA3 is the TLS ClientHello fingerprint (ja3 string) of the real Claude
	// Code CLI (Node/undici) captured against api.anthropic.com — ja3 hash
	// d871d02cecbde59abbf8f4806134addf (capture/out_v2/manifest.json). It is an OPT-IN
	// fingerprint for the curl_cffi sidecar (selected via claude_ja3=claude-cli), NOT the
	// default. The default is Chrome impersonation — see upstream.resolveClaudeJA3 for the
	// full rationale: reference relays use Chrome, and the best available research shows
	// Anthropic detects third-party clients by SYSTEM-PROMPT CONTENT (which the cloak layer
	// already handles), not TLS, so matching this fingerprint buys no measurable
	// anti-detection benefit while a best-effort replay of a non-native (Node) profile
	// risks an imperfect third fingerprint. Kept available for operators who want explicit
	// TLS↔UA coherence. NOTE: JA3 does not encode ALPN (the real client negotiated
	// http/1.1), so an exact JA4/ALPN match is best-effort.
	ClaudeJA3 = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-21,29-23-24,0"
)

// DefaultSecret is used when the operator has not configured one. Identity
// derivation only needs determinism + per-account uniqueness, not secrecy, so a
// constant default is acceptable; set a real secret to make profiles unique to a
// deployment.
var DefaultSecret = []byte("codex-pool/identity/v1")

// ResolveSecret returns the effective identity secret. A configured non-empty
// secret is used verbatim. Otherwise it derives a STABLE per-deployment secret
// from durable host identifiers (machine-id / hostname) so two installs do not
// generate the SAME per-account virtual identities (which would make the synthetic
// identity predictable across deployments — a weak relay-correlation signal). When
// no host identifier is available it falls back to DefaultSecret, preserving the
// previous behavior with no regression. It is pure given the host state and is
// computed once by the caller.
func ResolveSecret(configured []byte) []byte {
	if len(configured) > 0 {
		return configured
	}
	host := hostSeed()
	if host == "" {
		return DefaultSecret
	}
	mac := hmac.New(sha256.New, DefaultSecret)
	mac.Write([]byte("deployment\x00"))
	mac.Write([]byte(host))
	return mac.Sum(nil)
}

// hostSeed reads a durable, deployment-stable identifier without executing any
// process: the Linux machine-id, else the hostname. Returns "" if neither is
// available (then ResolveSecret keeps the constant default).
func hostSeed() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if v := readTrim(p); v != "" {
			return v
		}
	}
	if h, err := os.Hostname(); err == nil {
		if h = strings.TrimSpace(h); h != "" && h != "localhost" {
			return h
		}
	}
	return ""
}

// profile is a self-consistent device fingerprint (a real machine wouldn't mix
// macOS with x86_64-linux terminals, so we pick whole tuples).
type profile struct {
	osName    string
	osVersion string
	arch      string
	terminal  string
	// osRelease is the kernel-style "OS Version" string a CLI reports in its
	// environment block (Node os.release()), kept consistent with osName.
	osRelease string
	// stainless runtime fields for the Anthropic SDK fingerprint
	stainlessOS   string
	stainlessArch string
}

// hostProfile is the system environment auto-detected from the VPS the relay
// runs on. Basing every account's reported OS/arch/version on the real serving
// machine makes the virtual identity a coherent, non-contradictory environment
// (consistent with the host's TLS/IP origin) rather than a fabricated one. It is
// detected once and shared by all accounts; only the per-account identifiers
// (session/user/machine ids) vary.
var (
	hostOnce sync.Once
	hostP    profile
)

func detectedHost() profile {
	hostOnce.Do(func() { hostP = detectHost() })
	return hostP
}

// detectHost reads the real OS/arch/kernel/terminal of the machine the relay is
// deployed on. It uses only runtime info, /proc, and environment variables (no
// process execution), with safe fallbacks.
func detectHost() profile {
	var p profile
	switch runtime.GOOS {
	case "darwin":
		p.osName, p.stainlessOS = "Mac OS", "MacOS"
		p.osVersion = "15.3.1"
		p.osRelease = "Darwin 24.3.0"
	case "windows":
		p.osName, p.stainlessOS = "Windows", "Windows"
		p.osVersion = "10.0.22631"
		p.osRelease = "Windows 10.0.22631"
	default:
		p.osName, p.stainlessOS = "Linux", "Linux"
		kernel := firstNonEmpty(readTrim("/proc/sys/kernel/osrelease"), "6.8.0")
		p.osVersion = kernel
		p.osRelease = "Linux " + kernel
	}
	switch runtime.GOARCH {
	case "amd64":
		p.arch, p.stainlessArch = "x86_64", "x64"
	case "arm64":
		p.arch, p.stainlessArch = "arm64", "arm64"
	case "386":
		p.arch, p.stainlessArch = "x86", "ia32"
	default:
		p.arch, p.stainlessArch = runtime.GOARCH, runtime.GOARCH
	}
	p.terminal = detectTerminal()
	return p
}

func detectTerminal() string {
	if tp := strings.TrimSpace(os.Getenv("TERM_PROGRAM")); tp != "" {
		if v := strings.TrimSpace(os.Getenv("TERM_PROGRAM_VERSION")); v != "" {
			return tp + "/" + v
		}
		return tp
	}
	return "unknown"
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// Identity is a fully-resolved synthetic identity for one account.
type Identity struct {
	AccountID string

	OSName    string
	OSVersion string
	OSRelease string
	Arch      string
	Terminal  string

	StainlessOS   string
	StainlessArch string

	// Client versions come from the one coherent shipping tuple captured for this
	// release. Per-account OS/device/session axes still differ; a config override,
	// when set, takes precedence at the upstream layer.
	NodeVersion      string // X-Stainless-Runtime-Version (Claude)
	ClaudeCLIVersion string // claude-cli/<v> (User-Agent)
	// StainlessPackageVersion is the @anthropic-ai/sdk (Stainless) version reported
	// in X-Stainless-Package-Version. It is a SEPARATE axis from the claude-cli
	// version (the real client sends claude-cli/2.1.220 with SDK 0.94.0), so
	// it must not be conflated with ClaudeCLIVersion.
	StainlessPackageVersion string
	CodexCLIVersion         string // codex_cli_rs/<v>

	// Stable identifiers.
	SessionID       string // Codex Session_id / generic session uuid
	ClaudeSessionID string // X-Claude-Code-Session-Id
	UserID          string // Anthropic metadata.user_id (64-hex)
	MachineID       string // generic device/machine id (uuid)

	// Plausible environment values. Username/Hostname/HomeDir describe the
	// virtual machine for display and header consistency; per project policy the
	// downstream's real working directory and file paths are NOT rewritten (that
	// would break tool use), so these are not used to scrub request bodies.
	Username string
	Hostname string
	HomeDir  string
}

// --- Device-profile diversity pools -----------------------------------------
//
// Real users scatter across OS builds, terminals, Node and CLI versions. If every
// pooled account reported the SAME tuple, N accounts would look like one
// orchestrated fleet (identical X-Stainless-OS/-Arch/-Runtime-Version + UA). So
// each account deterministically draws a self-consistent profile (and client
// versions) from these curated, plausible-recent pools. Head entries track the
// current shipping clients (captured from the real CLIs); trailing entries model
// the natural version lag of a real population. Coherence is preserved per entry
// (a macOS account gets a Darwin kernel + Mac terminal + matching stainless arch).
var macProfiles = []profile{
	{"Mac OS", "26.1", "arm64", "iTerm.app/3.5.13", "Darwin 25.1.0", "MacOS", "arm64"},
	{"Mac OS", "26.2", "arm64", "Apple_Terminal/455", "Darwin 25.2.0", "MacOS", "arm64"},
	{"Mac OS", "15.6.1", "arm64", "iTerm.app/3.5.14", "Darwin 24.6.0", "MacOS", "arm64"},
	{"Mac OS", "15.5", "arm64", "Ghostty/1.1.3", "Darwin 24.5.0", "MacOS", "arm64"},
	{"Mac OS", "14.7.4", "arm64", "iTerm.app/3.5.11", "Darwin 23.6.0", "MacOS", "arm64"},
	{"Mac OS", "26.1", "x86_64", "iTerm.app/3.5.13", "Darwin 25.1.0", "MacOS", "x64"},
}

var linuxProfiles = []profile{
	{"Linux", "6.8.0-51-generic", "x86_64", "unknown", "Linux 6.8.0-51-generic", "Linux", "x64"},
	{"Linux", "6.11.0-19-generic", "x86_64", "xterm-256color", "Linux 6.11.0-19-generic", "Linux", "x64"},
	{"Linux", "6.14.0-15-generic", "x86_64", "gnome-terminal", "Linux 6.14.0-15-generic", "Linux", "x64"},
	{"Linux", "6.6.87.2-microsoft-standard-WSL2", "x86_64", "WindowsTerminal", "Linux 6.6.87.2-microsoft-standard-WSL2", "Linux", "x64"},
	{"Linux", "6.12.10-arch1-1", "x86_64", "Alacritty", "Linux 6.12.10-arch1-1", "Linux", "x64"},
	{"Linux", "6.8.0-51-generic", "aarch64", "unknown", "Linux 6.8.0-51-generic", "Linux", "arm64"},
}

var windowsProfiles = []profile{
	{"Windows", "10.0.26100", "x86_64", "WindowsTerminal", "Windows 10.0.26100", "Windows", "x64"},
	{"Windows", "10.0.22631", "x86_64", "WindowsTerminal", "Windows 10.0.22631", "Windows", "x64"},
	{"Windows", "10.0.26100", "arm64", "WindowsTerminal", "Windows 10.0.26100", "Windows", "arm64"},
}

// allProfiles is weighted toward macOS/Linux, which dominate the real Claude Code
// / Codex user base.
var allProfiles = func() []profile {
	out := make([]profile, 0, len(macProfiles)*2+len(linuxProfiles)*2+len(windowsProfiles))
	out = append(out, macProfiles...)
	out = append(out, macProfiles...)
	out = append(out, linuxProfiles...)
	out = append(out, linuxProfiles...)
	out = append(out, windowsProfiles...)
	return out
}()

// Client-version pools. Head = current shipping (captured from the real CLIs);
// tail = plausible lag where the upstream allows lag. A config override, when set,
// supersedes these.
var (
	// Keep the three coupled Claude version axes on the one combination captured
	// from the shipping 2.1.220 binary. Independently rotating stale values can
	// create a cli/runtime/SDK tuple that no real Claude Code release ever shipped.
	claudeCLIVersionPool = []string{ClaudeCLIVersion}
	// Newer Codex models are actively client-version gated, so the Codex UA/version
	// axis must not lag behind the current client by default.
	codexCLIVersionPool = []string{CodexCLIVersion}
	nodeVersionPool     = []string{"v26.3.0"}
	// stainlessVersionPool is the @anthropic-ai/sdk (Stainless) version axis,
	// reported in X-Stainless-Package-Version. It is captured ground truth from
	// the real Claude Code client and is independent of the claude-cli version.
	stainlessVersionPool = []string{"0.94.0"}
)

func poolForFamily(osName string) []profile {
	switch normalizeOSName(osName) {
	case "Mac OS":
		return macProfiles
	case "Linux":
		return linuxProfiles
	case "Windows":
		return windowsProfiles
	}
	return nil
}

func (d deriver) index(label string, n int) int {
	if n <= 0 {
		return 0
	}
	return int(d.u64(label) % uint64(n))
}

func pickProfile(d deriver, pool []profile) profile {
	if len(pool) == 0 {
		return detectedHost()
	}
	return pool[d.index("profile", len(pool))]
}

func pickStr(d deriver, label string, pool []string, fallback string) string {
	if len(pool) == 0 {
		return fallback
	}
	return pool[d.index(label, len(pool))]
}

// For derives the deterministic identity for accountID, drawing a diverse but
// self-consistent device profile from the deployed host's OS family (so the
// reported OS stays coherent with the VPS exit IP) while still varying the
// build/terminal/versions per account so accounts are not byte-identical.
func For(secret []byte, accountID string) Identity {
	return forFamily(secret, accountID, detectedHost().osName)
}

// ForOS presents a specific OS. The sentinel "diverse" draws from the full
// cross-OS pool (use with per-account egress so OS variety matches IP variety); a
// family name ("Mac OS"/"Linux"/"Windows" or aliases) diversifies within that
// family; anything else falls back to the host family.
func ForOS(secret []byte, accountID, osName string) Identity {
	if strings.EqualFold(strings.TrimSpace(osName), "diverse") {
		return forPool(secret, accountID, allProfiles)
	}
	if pool := poolForFamily(osName); pool != nil {
		return forPool(secret, accountID, pool)
	}
	return For(secret, accountID)
}

// CodexDevice returns the stable virtual device profile for one account/egress
// boundary.  Codex's installation id is a device property, so a session that must
// stay on a particular exit gets a profile stable across restarts while another exit
// cannot accidentally present the same virtual installation.
func CodexDevice(secret []byte, accountID, egressID, osName string) Identity {
	return ForOS(secret, strings.TrimSpace(accountID)+"\x00codex-egress\x00"+strings.TrimSpace(egressID), osName)
}

// NewUUIDv7 creates a time-ordered UUIDv7 for a logical Codex session/thread/turn.
// Unlike DerivedUUIDv7 it intentionally has fresh randomness: persistent mapping
// rows retain the chosen values, while each new root/branch/turn mirrors the native
// client's newly allocated UUID lifecycle.
func NewUUIDv7() string {
	if value, err := uuid.NewV7(); err == nil {
		return value.String()
	}
	// uuid.NewRandom is still RFC4122-valid if a system clock/rand failure prevents
	// the library's v7 constructor. The caller persists it immediately, so this is
	// preferable to deterministic reuse. This path is exceptionally rare.
	if value, err := uuid.NewRandom(); err == nil {
		return value.String()
	}
	return DerivedUUIDv7At("codex-uuidv7-fallback", fmt.Sprintf("%d", time.Now().UnixNano()), time.Now().UnixMilli())
}

func forFamily(secret []byte, accountID, osName string) Identity {
	pool := poolForFamily(osName)
	if pool == nil {
		pool = allProfiles
	}
	return forPool(secret, accountID, pool)
}

func forPool(secret []byte, accountID string, pool []profile) Identity {
	if len(secret) == 0 {
		secret = DefaultSecret
	}
	d := deriver{secret: secret, account: accountID}
	return forProfile(secret, accountID, pickProfile(d, pool))
}

func forProfile(secret []byte, accountID string, p profile) Identity {
	if len(secret) == 0 {
		secret = DefaultSecret
	}
	d := deriver{secret: secret, account: accountID}

	id := Identity{
		AccountID:               accountID,
		OSName:                  p.osName,
		OSVersion:               p.osVersion,
		OSRelease:               p.osRelease,
		Arch:                    p.arch,
		Terminal:                p.terminal,
		StainlessOS:             p.stainlessOS,
		StainlessArch:           p.stainlessArch,
		NodeVersion:             pickStr(d, "node-version", nodeVersionPool, nodeVersionPool[0]),
		ClaudeCLIVersion:        pickStr(d, "claude-cli-version", claudeCLIVersionPool, ClaudeCLIVersion),
		StainlessPackageVersion: pickStr(d, "stainless-version", stainlessVersionPool, stainlessVersionPool[0]),
		CodexCLIVersion:         pickStr(d, "codex-cli-version", codexCLIVersionPool, CodexCLIVersion),
		SessionID:               d.uuid("session"),
		ClaudeSessionID:         d.uuid("claude-session"),
		UserID:                  d.hex("user-id", 32), // 64 hex chars
		MachineID:               d.uuid("machine"),
		Username:                d.username("username"),
	}
	id.Hostname = id.Username + "-" + p.hostSuffix(d)
	id.HomeDir = id.homeDir()
	return id
}

func normalizeOSName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "darwin", "mac os", "macos", "mac":
		return "Mac OS"
	case "linux":
		return "Linux"
	case "win32", "windows", "win":
		return "Windows"
	}
	return ""
}

// DerivedUUID returns a deterministic v4-shaped UUID from (seed, original). With
// seed bound to an account (e.g. its MachineID), it gives each account its own
// stable replacement for a downstream conversation identifier, so the upstream
// cannot correlate one account's (possibly rate-limited / risk-flagged) session
// with another account that later serves the same conversation.
func DerivedUUID(seed, original string) string {
	sum := sha256.Sum256([]byte(seed + "\x00" + original))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// DerivedUUIDv7 returns a deterministic, per-seed UUIDv7 replacement. When the
// downstream value is already UUIDv7, its 48-bit millisecond timestamp is kept
// while all identifying/random bits are replaced. That preserves Codex's
// chronological UUID shape without allowing the upstream to correlate the real
// downstream identifier across pool accounts.
//
// Non-Codex/legacy callers sometimes supply an opaque correlator rather than a
// UUID. For those values a deterministic historical timestamp is used so the
// replacement remains stable across requests and process restarts. Shipping
// Codex clients always take the timestamp-preserving path.
func DerivedUUIDv7(seed, original string) string {
	sum := sha256.Sum256([]byte(seed + "\x02" + original))
	unixMilli, ok := uuidV7TimestampMillis(original)
	if !ok {
		const (
			fallbackEpochMillis = int64(1735689600000) // 2025-01-01T00:00:00Z
			fallbackSpanMillis  = int64(365 * 24 * 60 * 60 * 1000)
		)
		unixMilli = fallbackEpochMillis + int64(binary.BigEndian.Uint64(sum[16:24])%uint64(fallbackSpanMillis))
	}
	return formatDerivedUUIDv7(sum, unixMilli)
}

// DerivedUUIDv7At is DerivedUUIDv7 with an explicit timestamp fallback. A valid
// UUIDv7 input still wins so callers can preserve the original chronological
// shape; unixMilli is used for newly-created IDs such as a turn that arrived
// without a downstream turn_id.
func DerivedUUIDv7At(seed, original string, unixMilli int64) string {
	sum := sha256.Sum256([]byte(seed + "\x02" + original))
	if originalMillis, ok := uuidV7TimestampMillis(original); ok {
		unixMilli = originalMillis
	}
	if unixMilli <= 0 {
		return DerivedUUIDv7(seed, original)
	}
	return formatDerivedUUIDv7(sum, unixMilli)
}

func formatDerivedUUIDv7(sum [sha256.Size]byte, unixMilli int64) string {
	var u [16]byte
	copy(u[:], sum[:16])
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(unixMilli))
	copy(u[:6], timestamp[2:])
	u[6] = (u[6] & 0x0f) | 0x70
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func uuidV7TimestampMillis(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '7' {
		return 0, false
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != 16 || raw[6]>>4 != 7 || raw[8]>>6 != 2 {
		return 0, false
	}
	var timestamp [8]byte
	copy(timestamp[2:], raw[:6])
	return int64(binary.BigEndian.Uint64(timestamp[:])), true
}

// DerivedKey returns a deterministic opaque key from (seed, original), used to
// namespace a downstream prompt_cache_key per account.
func DerivedKey(seed, original string) string {
	sum := sha256.Sum256([]byte(seed + "\x01" + original))
	return hex.EncodeToString(sum[:])[:24]
}

// CodexUserAgent returns the User-Agent the official Codex CLI would send for
// this profile, e.g. "codex_cli_rs/0.135.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9".
func (i Identity) CodexUserAgent() string {
	return i.CodexUserAgentVersion(CodexCLIVersion)
}

// CodexUserAgentVersion is CodexUserAgent with an explicit CLI version, so a
// deployment can track the real shipping client via config instead of the pinned
// default constant.
func (i Identity) CodexUserAgentVersion(version string) string {
	if version == "" {
		version = CodexCLIVersion
	}
	return fmt.Sprintf("codex_cli_rs/%s (%s %s; %s) %s", version, i.OSName, i.OSVersion, i.Arch, i.Terminal)
}

// CodexUserAgentExecVersion is the User-Agent shape the `codex exec` entrypoint
// sends. The exec/app-server initialize handshake sets the official client suffix
// to "codex_exec; <version>" in addition to changing the originator prefix.
func (i Identity) CodexUserAgentExecVersion(version string) string {
	if version == "" {
		version = CodexCLIVersion
	}
	return fmt.Sprintf("codex_exec/%s (%s %s; %s) %s (codex_exec; %s)", version, i.OSName, i.OSVersion, i.Arch, i.Terminal, version)
}

// CodexUserAgentForOriginator returns the process-originator-correct User-Agent.
// codex_exec keeps its app-server suffix; every other recognized process uses its
// own prefix while retaining the account-virtual OS, architecture, and terminal.
func (i Identity) CodexUserAgentForOriginator(originator, version string) string {
	if originator == CodexOriginatorExec {
		return i.CodexUserAgentExecVersion(version)
	}
	if version == "" {
		version = CodexCLIVersion
	}
	originator = strings.TrimSpace(originator)
	if originator == "" {
		originator = CodexOriginator
	}
	return fmt.Sprintf("%s/%s (%s %s; %s) %s", originator, version, i.OSName, i.OSVersion, i.Arch, i.Terminal)
}

// ClaudeUserAgent returns the Claude Code CLI User-Agent, e.g.
// "claude-cli/2.1.220 (external, cli)".
func (i Identity) ClaudeUserAgent() string {
	return i.ClaudeUserAgentVersion(ClaudeCLIVersion)
}

// ClaudeUserAgentVersion is ClaudeUserAgent with an explicit CLI version.
func (i Identity) ClaudeUserAgentVersion(version string) string {
	return i.ClaudeUserAgentVersionForEntrypoint(version, "cli")
}

// ClaudeUserAgentVersionForEntrypoint mirrors Claude Code's launch mode. Native
// interactive requests use "cli" while `claude -p` / Agent SDK requests use
// "sdk-cli". Relays should project the downstream mode instead of forcing one.
func (i Identity) ClaudeUserAgentVersionForEntrypoint(version, entrypoint string) string {
	if version == "" {
		version = ClaudeCLIVersion
	}
	if entrypoint != "sdk-cli" {
		entrypoint = "cli"
	}
	return fmt.Sprintf("claude-cli/%s (external, %s)", version, entrypoint)
}

func (i Identity) homeDir() string {
	if i.OSName == "Linux" {
		return "/home/" + i.Username
	}
	return "/Users/" + i.Username
}

func (p profile) hostSuffix(d deriver) string {
	if p.osName == "Linux" {
		return "dev"
	}
	return "macbook"
}

// deriver produces independent deterministic values from (secret, account, label).
type deriver struct {
	secret  []byte
	account string
}

func (d deriver) block(label string) [32]byte {
	mac := hmac.New(sha256.New, d.secret)
	mac.Write([]byte(d.account))
	mac.Write([]byte{0})
	mac.Write([]byte(label))
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

func (d deriver) u64(label string) uint64 {
	b := d.block(label)
	return binary.BigEndian.Uint64(b[:8])
}

func (d deriver) hex(label string, n int) string {
	b := d.block(label)
	if n > len(b) {
		n = len(b)
	}
	return hex.EncodeToString(b[:n])
}

// uuid formats a deterministic RFC-4122 v4-shaped UUID from derived bytes.
func (d deriver) uuid(label string) string {
	b := d.block(label)
	var u [16]byte
	copy(u[:], b[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

var usernameAdjs = []string{"alex", "sam", "jordan", "casey", "taylor", "morgan", "riley", "jamie", "devon", "quinn"}

// username derives a plausible developer username.
func (d deriver) username(label string) string {
	b := d.block(label)
	name := usernameAdjs[int(b[0])%len(usernameAdjs)]
	suffix := hex.EncodeToString(b[1:3]) // 4 hex chars, e.g. "7f3a"
	return name + suffix
}
