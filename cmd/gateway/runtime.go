package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const strictRuntimeMissingMessage = "strict Linux runtime requires bubblewrap (bwrap) with user/mount/UTS namespace support"

type strictRuntimePaths struct {
	GatewayDir      string
	RuntimeDir      string
	VirtualHomeHost string
	ResolvConf      string
	Passwd          string
	Group           string
	RuntimeCWD      string
}

func handleProbeIdentity(configPath string) int {
	cfg, identity, err := loadRuntimeIdentity(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := saveRuntimeIdentity(identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("Gateway identity")
	fmt.Println("  hostname:", identity.Virtual.Hostname)
	fmt.Println("  user:", identity.Virtual.Username)
	fmt.Println("  home:", identity.Virtual.HomeDir)
	fmt.Println("  cwd:", preservedRuntimeCWD(identity))
	fmt.Println("  dns:", strings.Join(identity.Virtual.DNSServers, ","))

	if err := runStrictRuntimeSelfCheck(cfg, identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("  strict runtime: ok")
	return 0
}

func handleRunClaude(configPath string) int {
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if !strictLinuxRequested() {
		fmt.Fprintln(os.Stderr, "Claude gateway only supports Linux strict mode; set POOL_STRICT_LINUX=1")
		return 1
	}
	cfg, identity, err := loadRuntimeIdentity(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := saveRuntimeIdentity(identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	realClaude, err := findRealClaudeBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := runStrictRuntimeSelfCheck(cfg, identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd, err := buildStrictRuntimeCommand(cfg, identity, realClaude, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return exitCode(err)
	}
	return 0
}

func loadRuntimeIdentity(configPath string) (Config, *CachedIdentity, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return Config{}, nil, fmt.Errorf("load gateway config: %w", err)
	}
	if strings.TrimSpace(cfg.DownstreamKey) == "" {
		return Config{}, nil, errors.New("downstream_key not configured. Run: gateway init --key cap_xxx")
	}
	cache := NewIdentityCache(cfg.PoolServerURL, cfg.DownstreamKey, cfg.IdentityTTL, newGatewayPoolClient())
	identity, err := cache.Get("claude")
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, identity, nil
}

func saveRuntimeIdentity(identity *CachedIdentity) error {
	path, err := runtimeIdentityPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, gatewayPrivateDirMode); err != nil {
		return err
	}
	if err := chmodGatewayPrivateDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, gatewayConfigFileMode); err != nil {
		return err
	}
	return os.Chmod(path, gatewayConfigFileMode)
}

func runtimeIdentityPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude-gateway", "identity.json"), nil
}

func identityCachePresent() bool {
	path, err := runtimeIdentityPath()
	if err != nil {
		return false
	}
	return fileExists(path)
}

func strictLinuxRequested() bool {
	v := strings.TrimSpace(os.Getenv("POOL_STRICT_LINUX"))
	return v == "" || v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func checkStrictRuntimeSupport() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%s; current OS is %s", strictRuntimeMissingMessage, runtime.GOOS)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("%s: %w", strictRuntimeMissingMessage, err)
	}
	return nil
}

func runStrictRuntimeSelfCheck(cfg Config, identity *CachedIdentity) error {
	if err := checkStrictRuntimeSupport(); err != nil {
		return err
	}
	script := strictSelfCheckScript(identity)
	cmd, err := buildStrictRuntimeCommand(cfg, identity, "/bin/sh", []string{"-c", script})
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s; self-check failed: %s", strictRuntimeMissingMessage, msg)
	}
	return nil
}

func strictSelfCheckScript(identity *CachedIdentity) string {
	dnsChecks := ""
	for _, dns := range identity.Virtual.DNSServers {
		if dns == "" {
			continue
		}
		dnsChecks += fmt.Sprintf("grep -q %s /etc/resolv.conf || exit 15\n", shellSingleQuote("nameserver "+dns))
	}
	return fmt.Sprintf(`set -eu
[ "$(hostname)" = %s ] || exit 10
[ "$(whoami)" = %s ] || exit 11
[ "$HOME" = %s ] || exit 12
[ "$(pwd)" = %s ] || exit 13
grep -q '^nameserver ' /etc/resolv.conf || exit 14
%s`, shellSingleQuote(identity.Virtual.Hostname), shellSingleQuote(identity.Virtual.Username), shellSingleQuote(identity.Virtual.HomeDir), shellSingleQuote(preservedRuntimeCWD(identity)), dnsChecks)
}

func buildStrictRuntimeCommand(cfg Config, identity *CachedIdentity, command string, args []string) (*exec.Cmd, error) {
	if err := checkStrictRuntimeSupport(); err != nil {
		return nil, err
	}
	paths, err := prepareStrictRuntimePaths(identity)
	if err != nil {
		return nil, err
	}
	bwrapArgs := buildBubblewrapArgs(cfg, identity, paths, command, args)
	cmd := exec.Command("bwrap", bwrapArgs...)
	cmd.Env = os.Environ()
	return cmd, nil
}

func prepareStrictRuntimePaths(identity *CachedIdentity) (strictRuntimePaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return strictRuntimePaths{}, err
	}
	gatewayDir := filepath.Join(home, ".claude-gateway")
	runtimeDir := filepath.Join(gatewayDir, "runtime")
	virtualHomeHost := filepath.Join(runtimeDir, "home")
	if err := os.MkdirAll(filepath.Join(virtualHomeHost, ".claude"), gatewayPrivateDirMode); err != nil {
		return strictRuntimePaths{}, err
	}
	if err := os.MkdirAll(filepath.Join(virtualHomeHost, "workspace"), gatewayPrivateDirMode); err != nil {
		return strictRuntimePaths{}, err
	}
	if err := os.MkdirAll(runtimeDir, gatewayPrivateDirMode); err != nil {
		return strictRuntimePaths{}, err
	}
	if err := chmodGatewayPrivateDir(gatewayDir); err != nil {
		return strictRuntimePaths{}, err
	}
	if err := chmodGatewayPrivateDir(runtimeDir); err != nil {
		return strictRuntimePaths{}, err
	}
	resolvPath := filepath.Join(runtimeDir, "resolv.conf")
	if err := os.WriteFile(resolvPath, []byte(renderResolvConf(identity.Virtual.DNSServers)), gatewayPublicCertMode); err != nil {
		return strictRuntimePaths{}, err
	}
	passwdPath := filepath.Join(runtimeDir, "passwd")
	groupPath := filepath.Join(runtimeDir, "group")
	uid := os.Getuid()
	gid := os.Getgid()
	passwd, group := renderStrictRuntimeIdentityFiles(identity, uid, gid)
	if err := os.WriteFile(passwdPath, []byte(passwd), gatewayPublicCertMode); err != nil {
		return strictRuntimePaths{}, err
	}
	if err := os.WriteFile(groupPath, []byte(group), gatewayPublicCertMode); err != nil {
		return strictRuntimePaths{}, err
	}
	return strictRuntimePaths{
		GatewayDir:      gatewayDir,
		RuntimeDir:      runtimeDir,
		VirtualHomeHost: virtualHomeHost,
		ResolvConf:      resolvPath,
		Passwd:          passwdPath,
		Group:           groupPath,
		RuntimeCWD:      preservedRuntimeCWD(identity),
	}, nil
}

func renderStrictRuntimeIdentityFiles(identity *CachedIdentity, uid, gid int) (string, string) {
	username := identity.Virtual.Username
	home := identity.Virtual.HomeDir
	// Put the virtual identity first. When the installer is run as root, both root
	// and the virtual identity use uid 0 inside bwrap; libc/whoami returns the first
	// matching passwd entry, so root-first made the strict self-check fail.
	passwd := fmt.Sprintf("%s:x:%d:%d:Pool User:%s:/bin/bash\nroot:x:0:0:root:/root:/bin/sh\n", username, uid, gid, home)
	group := fmt.Sprintf("%s:x:%d:\nroot:x:0:\n", username, gid)
	return passwd, group
}

func buildBubblewrapArgs(cfg Config, identity *CachedIdentity, paths strictRuntimePaths, command string, args []string) []string {
	out := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-uts",
		"--unshare-ipc",
		"--dir", "/etc",
		"--dir", "/home",
		"--dir", filepath.Dir(identity.Virtual.HomeDir),
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--hostname", identity.Virtual.Hostname,
	}
	for _, p := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt"} {
		if fileExists(p) {
			out = append(out, "--ro-bind", p, p)
		}
	}
	if fileExists("/etc/ssl") {
		out = append(out, "--ro-bind", "/etc/ssl", "/etc/ssl")
	}
	if fileExists("/etc/alternatives") {
		out = append(out, "--ro-bind", "/etc/alternatives", "/etc/alternatives")
	}
	for _, dir := range destinationParentDirs(paths.RuntimeCWD) {
		out = append(out, "--dir", dir)
	}
	out = append(out,
		"--bind", paths.VirtualHomeHost, identity.Virtual.HomeDir,
		"--bind", identity.Local.WorkDir, paths.RuntimeCWD,
		"--ro-bind", paths.ResolvConf, "/etc/resolv.conf",
		"--ro-bind", paths.Passwd, "/etc/passwd",
		"--ro-bind", paths.Group, "/etc/group",
		"--chdir", paths.RuntimeCWD,
	)
	for _, kv := range strictRuntimeEnv(cfg, identity) {
		parts := strings.SplitN(kv, "=", 2)
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		out = append(out, "--setenv", parts[0], value)
	}
	out = append(out, "--", command)
	out = append(out, args...)
	return out
}

func preservedRuntimeCWD(identity *CachedIdentity) string {
	if identity != nil && identity.Local != nil && strings.TrimSpace(identity.Local.WorkDir) != "" {
		return identity.Local.WorkDir
	}
	wd, err := os.Getwd()
	if err == nil && strings.TrimSpace(wd) != "" {
		return wd
	}
	return "/"
}

func destinationParentDirs(target string) []string {
	target = filepath.Clean(strings.TrimSpace(target))
	if target == "" || target == "." || target == string(os.PathSeparator) {
		return nil
	}
	parent := filepath.Dir(target)
	if parent == "." || parent == string(os.PathSeparator) {
		return nil
	}
	parts := strings.Split(strings.Trim(parent, string(os.PathSeparator)), string(os.PathSeparator))
	dirs := make([]string, 0, len(parts))
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		dirs = append(dirs, string(os.PathSeparator)+current)
	}
	return dirs
}

func strictRuntimeEnv(cfg Config, identity *CachedIdentity) []string {
	proxy := gatewayProxyURL(cfg.ListenAddr)
	out := []string{
		"HOME=" + identity.Virtual.HomeDir,
		"USER=" + identity.Virtual.Username,
		"LOGNAME=" + identity.Virtual.Username,
		"SHELL=/bin/bash",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"HTTP_PROXY=" + proxy,
		"HTTPS_PROXY=" + proxy,
		"ALL_PROXY=" + proxy,
		"NO_PROXY=localhost,127.0.0.1",
		"ANTHROPIC_AUTH_TOKEN=" + cfg.DownstreamKey,
	}
	policy := defaultGatewayPolicy()
	if identity != nil && identity.Virtual != nil {
		policy = effectiveGatewayPolicy(identity.Virtual.GatewayPolicy)
	}
	if policy.DisableNonessentialEnv {
		out = append(out,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"DO_NOT_TRACK=1",
			"DISABLE_TELEMETRY=1",
			"DISABLE_ERROR_REPORTING=1",
			"DISABLE_AUTOUPDATER=1",
			"OTEL_METRICS_EXPORTER=none",
			"OTEL_LOGS_EXPORTER=none",
		)
	}
	return out
}

func gatewayProxyURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://" + listenAddr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func renderResolvConf(servers []string) string {
	var b strings.Builder
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		fmt.Fprintf(&b, "nameserver %s\n", server)
	}
	if b.Len() == 0 {
		b.WriteString("nameserver 1.1.1.1\n")
	}
	return b.String()
}

func findRealClaudeBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CLAUDE_REAL_BIN")); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("CLAUDE_REAL_BIN does not exist: %s", p)
	}
	if p, err := exec.LookPath("claude.real"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	return "", errors.New("claude not found; install Claude Code or set CLAUDE_REAL_BIN")
}

func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
