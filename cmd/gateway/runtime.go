package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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

	if strictLinuxRequested() {
		if err := runStrictRuntimeSelfCheck(cfg, identity); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("  strict runtime: ok")
	} else {
		fmt.Println("  runtime: compat")
	}
	return 0
}

func handleRunClaude(configPath string) int {
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
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
	policy := defaultGatewayPolicy()
	if identity != nil && identity.Virtual != nil {
		policy = effectiveGatewayPolicy(identity.Virtual.GatewayPolicy)
	}
	if !strictLinuxRequested() {
		settingsDir, dirErr := gatewayRuntimeSettingsDir()
		if dirErr != nil {
			fmt.Fprintln(os.Stderr, dirErr)
			return 1
		}
		var cleanup func()
		args, cleanup, err = prepareClaudeGatewayRuntimeArgs(args, cfg, policy, settingsDir, settingsDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer cleanup()
		return runCompatClaude(cfg, identity, realClaude, args)
	}
	if err := runStrictRuntimeSelfCheck(cfg, identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	paths, err := prepareStrictRuntimePaths(identity)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hostSettingsDir := filepath.Join(paths.VirtualHomeHost, ".claude")
	clientSettingsDir := filepath.Join(identity.Virtual.HomeDir, ".claude")
	var cleanup func()
	args, cleanup, err = prepareClaudeGatewayRuntimeArgs(args, cfg, policy, hostSettingsDir, clientSettingsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()
	cmd := strictRuntimeCommandWithPaths(cfg, identity, paths, realClaude, args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return exitCode(err)
	}
	return 0
}

func gatewayRuntimeSettingsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude-gateway", "runtime")
	if err := os.MkdirAll(dir, gatewayPrivateDirMode); err != nil {
		return "", err
	}
	if err := chmodGatewayPrivateDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := chmodGatewayPrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// prepareClaudeGatewayRuntimeArgs materializes a private, process-scoped
// --settings overlay. Claude Code reapplies settings.json "env" after reading the
// launcher environment, so process env alone is insufficient: a stale user/project
// provider selector can silently route around URL + pool key. Existing explicit
// --settings JSON/files are merged first and all non-gateway keys are preserved.
func prepareClaudeGatewayRuntimeArgs(
	args []string,
	cfg Config,
	policy GatewayPolicy,
	hostDir, clientDir string,
) ([]string, func(), error) {
	cleanArgs, settings, err := mergeExplicitClaudeSettings(args)
	if err != nil {
		return nil, func() {}, err
	}
	env := mapSetting(settings, "env")
	for _, key := range claudeGatewayConflictingEnvKeys() {
		env[key] = ""
	}
	env["ANTHROPIC_BASE_URL"] = poolServerBaseURL(cfg)
	env["ANTHROPIC_AUTH_TOKEN"] = strings.TrimSpace(cfg.DownstreamKey)
	env["CLAUDE_CODE_ENABLE_AUTO_MODE"] = "1"
	env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	env["CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING"] = "1"
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = ""
	if policy.DisableNonessentialEnv {
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		env["DO_NOT_TRACK"] = "1"
		env["DISABLE_TELEMETRY"] = "1"
		env["DISABLE_ERROR_REPORTING"] = "1"
		env["DISABLE_AUTOUPDATER"] = "1"
		env["OTEL_METRICS_EXPORTER"] = "none"
		env["OTEL_LOGS_EXPORTER"] = "none"
	}
	settings["env"] = env
	// A user apiKeyHelper can otherwise install a different credential after env
	// resolution. Empty explicitly disables it for this gateway launch only.
	settings["apiKeyHelper"] = ""
	// Pool-backed providers cannot answer Claude Code's independent
	// api.anthropic.com domain-safety preflight. Gateway CONNECT policy remains the
	// egress authority for the actual requested URL.
	settings["skipWebFetchPreflight"] = true

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, func() {}, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(hostDir, gatewayPrivateDirMode); err != nil {
		return nil, func() {}, err
	}
	file, err := os.CreateTemp(hostDir, ".pool-claude-settings-*.json")
	if err != nil {
		return nil, func() {}, err
	}
	hostPath := file.Name()
	cleanup := func() { _ = os.Remove(hostPath) }
	if err := file.Chmod(gatewayConfigFileMode); err != nil {
		_ = file.Close()
		cleanup()
		return nil, func() {}, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return nil, func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := os.Chmod(hostPath, gatewayConfigFileMode); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	clientPath := filepath.Join(clientDir, filepath.Base(hostPath))
	out := make([]string, 0, len(cleanArgs)+2)
	out = append(out, "--settings", clientPath)
	out = append(out, cleanArgs...)
	return out, cleanup, nil
}

func mergeExplicitClaudeSettings(args []string) ([]string, map[string]interface{}, error) {
	settings := map[string]interface{}{}
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		var value string
		switch {
		case arg == "--settings":
			if i+1 >= len(args) {
				return nil, nil, errors.New("--settings requires a JSON object or file path")
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--settings="):
			value = strings.TrimPrefix(arg, "--settings=")
		default:
			clean = append(clean, arg)
			continue
		}
		explicit, err := readClaudeSettingsValue(value)
		if err != nil {
			return nil, nil, err
		}
		deepMergeClaudeSettings(settings, explicit)
	}
	return clean, settings, nil
}

func readClaudeSettingsValue(value string) (map[string]interface{}, error) {
	value = strings.TrimSpace(value)
	var raw []byte
	if strings.HasPrefix(value, "{") {
		raw = []byte(value)
	} else {
		file, err := os.Open(value)
		if err != nil {
			return nil, fmt.Errorf("read Claude Code --settings %q: %w", value, err)
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, 2<<20))
		if err != nil {
			return nil, fmt.Errorf("read Claude Code --settings %q: %w", value, err)
		}
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse Claude Code --settings: %w", err)
	}
	return out, nil
}

func deepMergeClaudeSettings(dst, src map[string]interface{}) {
	for key, value := range src {
		incoming, incomingMap := value.(map[string]interface{})
		current, currentMap := dst[key].(map[string]interface{})
		if incomingMap && currentMap {
			deepMergeClaudeSettings(current, incoming)
			continue
		}
		dst[key] = value
	}
}

func loadRuntimeIdentity(configPath string) (Config, *CachedIdentity, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return Config{}, nil, fmt.Errorf("load gateway config: %w", err)
	}
	if strings.TrimSpace(cfg.DownstreamKey) == "" {
		return Config{}, nil, errors.New("downstream_key not configured. Run: gateway init --key sk-...")
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
	switch strings.ToLower(strings.TrimSpace(os.Getenv("POOL_CLIENT_RUNTIME"))) {
	case "strict":
		return true
	case "compat":
		return false
	}
	v := strings.TrimSpace(os.Getenv("POOL_STRICT_LINUX"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func runCompatClaude(cfg Config, identity *CachedIdentity, realClaude string, args []string) int {
	cmd := exec.Command(realClaude, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	policy := defaultGatewayPolicy()
	if identity != nil && identity.Virtual != nil {
		policy = effectiveGatewayPolicy(identity.Virtual.GatewayPolicy)
	}
	cmd.Env = compatRuntimeEnv(os.Environ(), cfg, policy)
	if err := cmd.Run(); err != nil {
		return exitCode(err)
	}
	return 0
}

func compatRuntimeEnv(base []string, cfg Config, policies ...GatewayPolicy) []string {
	// Use the bearer-token path recommended for custom gateways. Remove every
	// inherited direct/provider credential selector first so an existing cloud
	// provider or API-key login cannot outrank URL + pool key.
	policy := defaultGatewayPolicy()
	if len(policies) > 0 {
		policy = effectiveGatewayPolicy(policies[0])
	}
	env := append([]string(nil), base...)
	for _, key := range claudeGatewayRuntimeUnsetEnvKeys() {
		env = removeRuntimeEnvKey(env, key)
	}
	set := func(key, value string) {
		// Remove both bare and KEY=value occurrences before installing exactly
		// one gateway-owned value. Exec environments can contain duplicates.
		env = removeRuntimeEnvKey(env, key)
		env = append(env, key+"="+value)
	}
	set("ANTHROPIC_BASE_URL", strings.TrimRight(strings.TrimSpace(cfg.PoolServerURL), "/"))
	set("ANTHROPIC_AUTH_TOKEN", strings.TrimSpace(cfg.DownstreamKey))
	set("CLAUDE_CODE_ENABLE_AUTO_MODE", "1")
	set("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1")
	set("CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING", "1")
	set("POOL_CLIENT_RUNTIME", "compat")
	set("POOL_STRICT_LINUX", "0")
	if policy.DisableNonessentialEnv {
		set("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
		set("DO_NOT_TRACK", "1")
		set("DISABLE_TELEMETRY", "1")
		set("DISABLE_ERROR_REPORTING", "1")
		set("DISABLE_AUTOUPDATER", "1")
		set("OTEL_METRICS_EXPORTER", "none")
		set("OTEL_LOGS_EXPORTER", "none")
	}
	return env
}

func claudeGatewayConflictingEnvKeys() []string {
	return []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_AWS_API_KEY",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_VERTEX",
		"ANTHROPIC_AWS_BASE_URL",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"AWS_BEARER_TOKEN_BEDROCK",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"ANTHROPIC_WORKSPACE_ID",
	}
}

func claudeGatewayRuntimeUnsetEnvKeys() []string {
	keys := append([]string(nil), claudeGatewayConflictingEnvKeys()...)
	return append(keys, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC")
}

func removeRuntimeEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, item := range env {
		if item == key || strings.HasPrefix(item, prefix) {
			continue
		}
		out = append(out, item)
	}
	return out
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
	return strictRuntimeCommandWithPaths(cfg, identity, paths, command, args), nil
}

func strictRuntimeCommandWithPaths(
	cfg Config,
	identity *CachedIdentity,
	paths strictRuntimePaths,
	command string,
	args []string,
) *exec.Cmd {
	bwrapArgs := buildBubblewrapArgs(cfg, identity, paths, command, args)
	cmd := exec.Command("bwrap", bwrapArgs...)
	cmd.Env = os.Environ()
	return cmd
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
	if err := ensureClaudeAutoPlanSettings(virtualHomeHost); err != nil {
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

func ensureClaudeAutoPlanSettings(virtualHomeHost string) error {
	path := filepath.Join(virtualHomeHost, ".claude", "settings.json")
	settings := map[string]interface{}{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("read Claude Code settings: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	env := mapSetting(settings, "env")
	if _, ok := env["CLAUDE_CODE_ENABLE_AUTO_MODE"]; !ok {
		env["CLAUDE_CODE_ENABLE_AUTO_MODE"] = "1"
	}
	settings["env"] = env

	if _, ok := settings["useAutoModeDuringPlan"]; !ok {
		settings["useAutoModeDuringPlan"] = true
	}
	// The strict virtual HOME is used only for gateway-backed Claude Code, where
	// api.anthropic.com is not the model provider. Skip its independent WebFetch
	// hostname preflight; the gateway's own CONNECT policy still controls egress.
	settings["skipWebFetchPreflight"] = true

	permissions := mapSetting(settings, "permissions")
	if _, ok := permissions["defaultMode"]; !ok {
		permissions["defaultMode"] = "auto"
	}
	settings["permissions"] = permissions

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, gatewayConfigFileMode); err != nil {
		return err
	}
	return os.Chmod(path, gatewayConfigFileMode)
}

func mapSetting(settings map[string]interface{}, key string) map[string]interface{} {
	if existing, ok := settings[key].(map[string]interface{}); ok {
		return existing
	}
	return map[string]interface{}{}
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
	// bwrap otherwise inherits the launcher environment. Remove the host
	// credentials/provider selectors first; strictRuntimeEnv then installs the
	// single bearer-token gateway credential.
	for _, key := range claudeGatewayRuntimeUnsetEnvKeys() {
		out = append(out, "--unsetenv", key)
	}
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
	policy := defaultGatewayPolicy()
	if identity != nil && identity.Virtual != nil {
		policy = effectiveGatewayPolicy(identity.Virtual.GatewayPolicy)
	}
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
		"NO_PROXY=" + gatewayNoProxy(cfg, policy),
		"ANTHROPIC_BASE_URL=" + poolServerBaseURL(cfg),
		"ANTHROPIC_AUTH_TOKEN=" + cfg.DownstreamKey,
		"CLAUDE_CODE_ENABLE_AUTO_MODE=1",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1",
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

func poolServerBaseURL(cfg Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.PoolServerURL), "/")
	if base == "" {
		base = strings.TrimRight(DefaultConfig().PoolServerURL, "/")
	}
	return base
}

func gatewayNoProxy(cfg Config, policies ...GatewayPolicy) string {
	policy := defaultGatewayPolicy()
	if len(policies) > 0 {
		policy = effectiveGatewayPolicy(policies[0])
	}
	entries := []string{"localhost", "127.0.0.1"}
	entries = append(entries, poolServerNoProxyEntries(cfg.PoolServerURL)...)
	entries = append(entries, gatewayPolicyNoProxyEntries(policy)...)
	return strings.Join(uniqueNonEmpty(entries), ",")
}

func defaultWrapperNoProxy() string {
	entries := []string{"localhost", "127.0.0.1"}
	entries = append(entries, gatewayPolicyNoProxyEntries(defaultGatewayPolicy())...)
	return strings.Join(uniqueNonEmpty(entries), ",")
}

func gatewayPolicyNoProxyEntries(policy GatewayPolicy) []string {
	out := make([]string, 0, len(policy.ForwardHosts)*2)
	for _, host := range policy.ForwardHosts {
		out = append(out, noProxyEntriesForGatewayPattern(host)...)
	}
	return out
}

func noProxyEntriesForGatewayPattern(pattern string) []string {
	host := normalizeTargetHost(pattern)
	if host == "" {
		return nil
	}
	if strings.HasPrefix(host, "*.") {
		domain := strings.TrimPrefix(host, "*.")
		return []string{domain, "." + domain}
	}
	if strings.Contains(host, "*") {
		return nil
	}
	return noProxyEntriesForHostPort(host, "")
}

func poolServerNoProxyEntries(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return noProxyEntriesForHostPort(u.Hostname(), u.Port())
	}
	host := raw
	port := ""
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host = h
		port = p
	}
	return noProxyEntriesForHostPort(host, port)
}

func noProxyEntriesForHostPort(host, port string) []string {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return nil
	}
	entries := []string{host}
	if port != "" {
		if strings.Contains(host, ":") {
			entries = append(entries, "["+host+"]:"+port)
		} else {
			entries = append(entries, host+":"+port)
		}
	}
	return entries
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
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
