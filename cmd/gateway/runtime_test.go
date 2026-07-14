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
	cfg.ClaudeModel = "gpt-5.6-sol"
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
		"pool.example,pool.example:1455,api.openai.com",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("strict runtime env missing %q\n---\n%s", want, joined)
		}
	}
	assertClaudeModelRuntimeEnv(t, joined, "gpt-5.6-sol")
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
	cfg.ClaudeModel = "gpt-5.6-sol"
	env := compatRuntimeEnv([]string{
		"HOME=/home/real",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN=old",
		"ANTHROPIC_MODEL=claude-opus-old",
		"CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-old",
	}, cfg)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nHOME=/home/real\n",
		"\nANTHROPIC_BASE_URL=https://pool.example\n",
		"\nANTHROPIC_AUTH_TOKEN=cap_secret\n",
		"\nCLAUDE_CODE_ENABLE_AUTO_MODE=1\n",
		"\nPOOL_CLIENT_RUNTIME=compat\n",
		"\nPOOL_STRICT_LINUX=0\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("compat runtime env missing %q\n---\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "https://api.anthropic.com") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=old") {
		t.Fatalf("compat runtime env did not replace old Anthropic settings\n---\n%s", joined)
	}
	if strings.Contains(joined, "claude-opus-old") || strings.Contains(joined, "claude-haiku-old") {
		t.Fatalf("compat runtime env did not replace stale Claude model routing\n---\n%s", joined)
	}
	assertClaudeModelRuntimeEnv(t, joined, "gpt-5.6-sol")
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

func assertClaudeModelRuntimeEnv(t *testing.T, joined, model string) {
	t.Helper()
	for _, key := range claudeModelRuntimeEnvKeys() {
		want := model
		switch key {
		case "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":
			want = model + " via Pool"
		case "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION":
			want = "Anthropic Messages to Codex Responses via pool_server"
		case "ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES":
			want = "effort,xhigh_effort,max_effort"
		}
		if !strings.Contains(joined, "\n"+key+"="+want+"\n") {
			t.Fatalf("Claude model runtime env missing %s=%s\n---\n%s", key, want, joined)
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

func TestClaudeGatewayRuntimeArgsSkipUnavailableWebFetchPreflight(t *testing.T) {
	args := claudeGatewayRuntimeArgs([]string{"--model", "opus", "fetch docs"})
	if len(args) < 2 || args[0] != "--settings" || args[1] != gatewayClaudeSettingsJSON {
		t.Fatalf("gateway settings overlay missing: %q", args)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
		t.Fatalf("settings overlay is invalid JSON: %v", err)
	}
	if settings["skipWebFetchPreflight"] != true {
		t.Fatalf("WebFetch preflight is not disabled: %v", settings)
	}
	if got := strings.Join(args[2:], "\x00"); got != "--model\x00opus\x00fetch docs" {
		t.Fatalf("caller args changed: %q", args)
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
