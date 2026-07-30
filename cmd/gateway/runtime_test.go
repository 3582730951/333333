package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBubblewrapArgsPreservesRuntimeCWD(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:8765"
	cfg.PoolServerURL = "http://165.254.109.23:8787/"
	cfg.DownstreamKey = "cap_secret"
	id := &CachedIdentity{
		Local: &LocalEnvironment{
			WorkDir: "/workspace/project",
		},
		Virtual: &VirtualIdentity{
			Username:   "virtuser",
			Hostname:   "virt-host",
			HomeDir:    "/home/virtuser",
			DNSServers: []string{"1.1.1.1"},
			ProcessInfo: ProcessInfo{
				CWD: "/home/virtuser/workspace/project",
			},
		},
	}
	paths := strictRuntimePaths{
		VirtualHomeHost: "/tmp/runtime/home",
		ResolvConf:      "/tmp/runtime/resolv.conf",
		Passwd:          "/tmp/runtime/passwd",
		Group:           "/tmp/runtime/group",
		RuntimeCWD:      "/workspace/project",
	}

	args := buildBubblewrapArgs(cfg, id, paths, "/bin/claude", []string{"--version"})
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	for _, want := range []string{
		"\x00--dir\x00/etc\x00",
		"\x00--dir\x00/home\x00",
		"\x00--bind\x00/tmp/runtime/home\x00/home/virtuser\x00",
		"\x00--bind\x00/workspace/project\x00/workspace/project\x00",
		"\x00--chdir\x00/workspace/project\x00",
		"\x00--ro-bind\x00/tmp/runtime/resolv.conf\x00/etc/resolv.conf\x00",
		"\x00--setenv\x00HTTP_PROXY\x00http://127.0.0.1:8765\x00",
		"165.254.109.23,165.254.109.23:8787,api.openai.com",
		"\x00--setenv\x00ANTHROPIC_BASE_URL\x00http://165.254.109.23:8787\x00",
		"\x00--setenv\x00ANTHROPIC_AUTH_TOKEN\x00cap_secret\x00",
		"\x00--unsetenv\x00ANTHROPIC_API_KEY\x00",
		"\x00--unsetenv\x00CLAUDE_CODE_USE_BEDROCK\x00",
		"\x00--setenv\x00CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY\x001\x00",
		"\x00--setenv\x00CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING\x001\x00",
		"\x00--\x00/bin/claude\x00--version\x00",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("bubblewrap args missing %q\nargs=%q", want, args)
		}
	}
	if strings.Contains(joined, "\x00--chdir\x00/home/virtuser/workspace/project\x00") {
		t.Fatalf("runtime cwd must not be rewritten to virtual workspace\nargs=%q", args)
	}
}

func TestStrictRuntimeEnvPointsClaudeCodeAtPoolAPI(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:8765"
	cfg.PoolServerURL = "https://pool.example:1455/"
	cfg.DownstreamKey = "cap_secret"
	id := &CachedIdentity{
		Virtual: &VirtualIdentity{
			Username: "virtuser",
			HomeDir:  "/home/virtuser",
		},
	}

	joined := "\n" + strings.Join(strictRuntimeEnv(cfg, id), "\n") + "\n"
	for _, want := range []string{
		"\nANTHROPIC_BASE_URL=https://pool.example:1455\n",
		"\nANTHROPIC_AUTH_TOKEN=cap_secret\n",
		"\nCLAUDE_CODE_ENABLE_AUTO_MODE=1\n",
		"\nCLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1\n",
		"\nCLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1\n",
		"pool.example,pool.example:1455,api.openai.com",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("strict runtime env missing %q\n---\n%s", want, joined)
		}
	}
	assertNoClaudeModelRuntimeEnv(t, joined)
}

func TestClaudeRuntimeModeDefaultsToCompat(t *testing.T) {
	t.Setenv("POOL_CLIENT_RUNTIME", "")
	t.Setenv("POOL_STRICT_LINUX", "")
	if strictLinuxRequested() {
		t.Fatal("Claude runtime should default to compat; strict must be explicit")
	}
}

func TestClaudeRuntimeModeStrictIsExplicit(t *testing.T) {
	t.Setenv("POOL_CLIENT_RUNTIME", "strict")
	t.Setenv("POOL_STRICT_LINUX", "")
	if !strictLinuxRequested() {
		t.Fatal("POOL_CLIENT_RUNTIME=strict should request strict runtime")
	}
	t.Setenv("POOL_CLIENT_RUNTIME", "")
	t.Setenv("POOL_STRICT_LINUX", "1")
	if !strictLinuxRequested() {
		t.Fatal("legacy POOL_STRICT_LINUX=1 should still request strict runtime")
	}
}

func TestCompatRuntimeEnvPointsClaudeAtPoolAndKeepsHome(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	base := []string{
		"HOME=/home/real",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_BASE_URL=https://duplicate.example",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_AUTH_TOKEN=old",
		"ANTHROPIC_AUTH_TOKEN=duplicate-old",
		"CLAUDE_CODE_ENABLE_AUTO_MODE",
		"CLAUDE_CODE_ENABLE_AUTO_MODE=0",
		"CLAUDE_CODE_ENABLE_AUTO_MODE=duplicate",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=0",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=duplicate",
		"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=0",
		"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=duplicate",
		"POOL_CLIENT_RUNTIME=strict",
		"POOL_CLIENT_RUNTIME=duplicate",
		"POOL_STRICT_LINUX=1",
		"POOL_STRICT_LINUX=duplicate",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"ANTHROPIC_MODEL=claude-opus-old",
		"CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-old",
	}
	for _, key := range claudeGatewayConflictingEnvKeys() {
		base = append(base, key, key+"=host-value", key+"=duplicate-host-value")
	}
	env := compatRuntimeEnv(base, cfg)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	expected := map[string]string{
		"HOME":                         "/home/real",
		"ANTHROPIC_BASE_URL":           "https://pool.example",
		"ANTHROPIC_AUTH_TOKEN":         "cap_secret",
		"CLAUDE_CODE_ENABLE_AUTO_MODE": "1",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":     "1",
		"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING": "1",
		"POOL_CLIENT_RUNTIME":                            "compat",
		"POOL_STRICT_LINUX":                              "0",
	}
	for key, value := range expected {
		needle := "\n" + key + "="
		if strings.Count(joined, needle) != 1 || !strings.Contains(joined, needle+value+"\n") {
			t.Fatalf("compat runtime env must contain exactly one %s=%s\n---\n%s", key, value, joined)
		}
	}
	for _, key := range claudeGatewayConflictingEnvKeys() {
		if key == "ANTHROPIC_AUTH_TOKEN" {
			continue
		}
		if strings.Contains(joined, "\n"+key+"\n") || strings.Contains(joined, "\n"+key+"=") {
			t.Fatalf("compat runtime env retained conflicting %s\n---\n%s", key, joined)
		}
	}
	if strings.Contains(joined, "\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\n") ||
		strings.Contains(joined, "\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=") {
		t.Fatalf("compat parity mode retained total nonessential-traffic disable\n---\n%s", joined)
	}
	if !strings.Contains(joined, "ANTHROPIC_MODEL=claude-opus-old") || !strings.Contains(joined, "CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-old") {
		t.Fatalf("compat runtime must preserve user-maintained model variables\n---\n%s", joined)
	}
}

func TestCompatRuntimeEnvPrivacyPolicyReinstallsNonessentialControls(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	base := []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=old",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=duplicate",
		"DO_NOT_TRACK=old",
		"DO_NOT_TRACK=duplicate",
		"DISABLE_TELEMETRY=old",
		"DISABLE_ERROR_REPORTING=old",
		"DISABLE_AUTOUPDATER=old",
		"OTEL_METRICS_EXPORTER=old",
		"OTEL_LOGS_EXPORTER=old",
	}
	policy := GatewayPolicy{
		UnknownTargetPolicy:    "forward",
		DisableNonessentialEnv: true,
	}

	joined := "\n" + strings.Join(compatRuntimeEnv(base, cfg, policy), "\n") + "\n"
	expected := map[string]string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DO_NOT_TRACK":            "1",
		"DISABLE_TELEMETRY":       "1",
		"DISABLE_ERROR_REPORTING": "1",
		"DISABLE_AUTOUPDATER":     "1",
		"OTEL_METRICS_EXPORTER":   "none",
		"OTEL_LOGS_EXPORTER":      "none",
	}
	for key, value := range expected {
		needle := "\n" + key + "="
		if strings.Count(joined, needle) != 1 || !strings.Contains(joined, needle+value+"\n") {
			t.Fatalf("compat privacy policy must install exactly one %s=%s\n---\n%s", key, value, joined)
		}
	}
}

func TestCompatRuntimeEnvRemovesBareAnthropicAPIKeyWithoutMutatingInput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	base := []string{
		"HOME=/home/real",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_API_KEY=host-secret",
		"KEEP=value",
	}

	env := compatRuntimeEnv(base, cfg)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	if strings.Contains(joined, "\nANTHROPIC_API_KEY\n") ||
		strings.Contains(joined, "\nANTHROPIC_API_KEY=") {
		t.Fatalf("compat runtime did not remove ANTHROPIC_API_KEY\n---\n%s", joined)
	}
	if !strings.Contains(joined, "\nANTHROPIC_AUTH_TOKEN=cap_secret\n") ||
		!strings.Contains(joined, "\nKEEP=value\n") {
		t.Fatalf("compat runtime lost required environment\n---\n%s", joined)
	}
	if got := strings.Join(base, "\n"); !strings.Contains(got, "ANTHROPIC_API_KEY=host-secret") {
		t.Fatalf("compat runtime mutated caller environment: %q", base)
	}
}

func TestBubblewrapRemovesAnthropicAPIKeyAndUsesGatewayBearer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:8765"
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	id := &CachedIdentity{
		Local: &LocalEnvironment{WorkDir: "/workspace/project"},
		Virtual: &VirtualIdentity{
			Username: "virtuser",
			Hostname: "virt-host",
			HomeDir:  "/home/virtuser",
		},
	}
	paths := strictRuntimePaths{
		VirtualHomeHost: "/tmp/runtime/home",
		ResolvConf:      "/tmp/runtime/resolv.conf",
		Passwd:          "/tmp/runtime/passwd",
		Group:           "/tmp/runtime/group",
		RuntimeCWD:      "/workspace/project",
	}

	args := buildBubblewrapArgs(cfg, id, paths, "/bin/claude", nil)
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	if !strings.Contains(joined, "\x00--unsetenv\x00ANTHROPIC_API_KEY\x00") {
		t.Fatalf("strict runtime does not unset host ANTHROPIC_API_KEY\nargs=%q", args)
	}
	if strings.Contains(joined, "\x00--setenv\x00ANTHROPIC_API_KEY\x00") {
		t.Fatalf("strict runtime reinstalled direct Anthropic API key\nargs=%q", args)
	}
	if !strings.Contains(joined, "\x00--setenv\x00ANTHROPIC_AUTH_TOKEN\x00cap_secret\x00") {
		t.Fatalf("strict runtime does not install gateway auth token\nargs=%q", args)
	}
	for _, key := range claudeGatewayRuntimeUnsetEnvKeys() {
		if !strings.Contains(joined, "\x00--unsetenv\x00"+key+"\x00") {
			t.Fatalf("strict runtime did not remove %s\nargs=%q", key, args)
		}
	}
	if strings.Contains(joined, "\x00--setenv\x00CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\x00") {
		t.Fatalf("strict parity mode reinstalled total nonessential-traffic disable\nargs=%q", args)
	}
}

func TestRuntimeEnvDoesNotForceClaudeModelWhenUnconfigured(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	compat := "\n" + strings.Join(compatRuntimeEnv([]string{"HOME=/home/real"}, cfg), "\n") + "\n"
	id := &CachedIdentity{Virtual: &VirtualIdentity{Username: "virtuser", HomeDir: "/home/virtuser"}}
	strict := "\n" + strings.Join(strictRuntimeEnv(cfg, id), "\n") + "\n"
	for _, key := range claudeModelRuntimeEnvKeys() {
		needle := "\n" + key + "="
		if strings.Contains(compat, needle) || strings.Contains(strict, needle) {
			t.Fatalf("empty claude_model must not inject %s\ncompat:\n%s\nstrict:\n%s", key, compat, strict)
		}
	}
}

func assertNoClaudeModelRuntimeEnv(t *testing.T, joined string) {
	t.Helper()
	for _, key := range claudeModelRuntimeEnvKeys() {
		if strings.Contains(joined, "\n"+key+"=") {
			t.Fatalf("Claude model runtime env unexpectedly contains %s\n---\n%s", key, joined)
		}
	}
}

func claudeModelRuntimeEnvKeys() []string {
	return []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"ANTHROPIC_CUSTOM_MODEL_OPTION",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES",
	}
}

func TestClaudeGatewayRuntimeArgsUsePrivateAuthoritativeSettingsOverlay(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PoolServerURL = "https://pool.example/"
	cfg.DownstreamKey = "cap_secret"
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit.json")
	if err := os.WriteFile(explicit, []byte(`{
		"permissions":{"defaultMode":"plan"},
		"env":{
			"KEEP_SETTING":"yes",
			"ANTHROPIC_BASE_URL":"https://stale.example",
			"CLAUDE_CODE_USE_BEDROCK":"1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"
		},
		"apiKeyHelper":"/tmp/stale-helper"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args, cleanup, err := prepareClaudeGatewayRuntimeArgs(
		[]string{"--settings", explicit, "--model", "opus", "fetch docs"},
		cfg,
		defaultGatewayPolicy(),
		dir,
		dir,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(args) < 2 || args[0] != "--settings" || args[1] == explicit {
		t.Fatalf("private gateway settings overlay missing: %q", args)
	}
	info, err := os.Stat(args[1])
	if err != nil || info.Mode().Perm() != gatewayConfigFileMode {
		t.Fatalf("settings overlay mode=%v err=%v", info, err)
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings overlay is invalid JSON: %v", err)
	}
	env, _ := settings["env"].(map[string]interface{})
	for key, want := range map[string]interface{}{
		"KEEP_SETTING":                                   "yes",
		"ANTHROPIC_BASE_URL":                             "https://pool.example",
		"ANTHROPIC_AUTH_TOKEN":                           "cap_secret",
		"ANTHROPIC_API_KEY":                              "",
		"CLAUDE_CODE_USE_BEDROCK":                        "",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":           "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":       "",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":     "1",
		"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING": "1",
	} {
		if env[key] != want {
			t.Fatalf("gateway settings env[%s]=%#v, want %#v\n%s", key, env[key], want, raw)
		}
	}
	if settings["skipWebFetchPreflight"] != true || settings["apiKeyHelper"] != "" {
		t.Fatalf("gateway settings did not neutralize preflight/helper: %v", settings)
	}
	permissions, _ := settings["permissions"].(map[string]interface{})
	if permissions["defaultMode"] != "plan" {
		t.Fatalf("explicit non-gateway settings were not preserved: %v", settings)
	}
	if got := strings.Join(args[2:], "\x00"); got != "--model\x00opus\x00fetch docs" {
		t.Fatalf("caller args changed: %q", args)
	}
	cleanup()
	if _, err := os.Stat(args[1]); !os.IsNotExist(err) {
		t.Fatalf("ephemeral settings overlay survived cleanup: %v", err)
	}
}

func TestStrictRuntimeEnvBypassesCodexAndAdvisoryHosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:8765"
	cfg.PoolServerURL = "https://pool.example:1455/"
	cfg.DownstreamKey = "cap_secret"
	id := &CachedIdentity{
		Virtual: &VirtualIdentity{
			Username: "virtuser",
			HomeDir:  "/home/virtuser",
		},
	}

	joined := "\n" + strings.Join(strictRuntimeEnv(cfg, id), "\n") + "\n"
	for _, want := range []string{
		"api.openai.com",
		"chatgpt.com",
		".chatgpt.com",
		"chat.openai.com",
		"api.github.com",
		"pypi.org",
		"api.osv.dev",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("strict runtime NO_PROXY should bypass %q for child tools\n---\n%s", want, joined)
		}
	}
}

func TestPrepareStrictRuntimePathsWritesClaudeAutoPlanSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := &CachedIdentity{
		Local: &LocalEnvironment{WorkDir: "/workspace/project"},
		Virtual: &VirtualIdentity{
			Username:   "virtuser",
			Hostname:   "virt-host",
			HomeDir:    "/home/virtuser",
			DNSServers: []string{"1.1.1.1"},
		},
	}

	paths, err := prepareStrictRuntimePaths(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(paths.VirtualHomeHost, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("strict runtime must write Claude Code settings.json: %v", err)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, raw)
	}
	env, _ := settings["env"].(map[string]interface{})
	if env["CLAUDE_CODE_ENABLE_AUTO_MODE"] != "1" {
		t.Fatalf("settings env must enable auto mode: %s", raw)
	}
	if settings["useAutoModeDuringPlan"] != true {
		t.Fatalf("settings must enable auto mode during plan: %s", raw)
	}
	if settings["skipWebFetchPreflight"] != true {
		t.Fatalf("strict gateway settings must skip unavailable WebFetch preflight: %s", raw)
	}
	permissions, _ := settings["permissions"].(map[string]interface{})
	if permissions["defaultMode"] != "auto" {
		t.Fatalf("settings permissions.defaultMode must be auto: %s", raw)
	}
}

func TestStrictRuntimeEnvCanOmitNonessentialTrafficControls(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:8765"
	cfg.DownstreamKey = "cap_secret"
	id := &CachedIdentity{
		Virtual: &VirtualIdentity{
			Username: "virtuser",
			HomeDir:  "/home/virtuser",
			GatewayPolicy: GatewayPolicy{
				DisableNonessentialEnv: false,
				UnknownTargetPolicy:    "block",
			},
		},
	}

	joined := "\n" + strings.Join(strictRuntimeEnv(cfg, id), "\n") + "\n"
	for _, forbidden := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DO_NOT_TRACK=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
		"OTEL_METRICS_EXPORTER=none",
		"OTEL_LOGS_EXPORTER=none",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("strict runtime env should omit admin-disabled %q\n---\n%s", forbidden, joined)
		}
	}
}

func TestStrictRuntimePasswdPrefersVirtualUserWhenHostUIDIsRoot(t *testing.T) {
	id := &CachedIdentity{
		Virtual: &VirtualIdentity{
			Username: "virtuser",
			HomeDir:  "/home/virtuser",
		},
	}

	passwd, group := renderStrictRuntimeIdentityFiles(id, 0, 0)
	if !strings.HasPrefix(passwd, "virtuser:x:0:0:Pool User:/home/virtuser:/bin/bash\n") {
		t.Fatalf("virtual user must be first when host uid is root so whoami resolves correctly\n---\n%s", passwd)
	}
	if strings.Index(passwd, "virtuser:x:0:0:") > strings.Index(passwd, "root:x:0:0:") {
		t.Fatalf("root entry precedes virtual user entry\n---\n%s", passwd)
	}
	if !strings.HasPrefix(group, "virtuser:x:0:\n") {
		t.Fatalf("virtual group should be first for root gid\n---\n%s", group)
	}
}
