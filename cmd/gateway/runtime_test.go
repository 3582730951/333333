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
		"\x00--setenv\x00NO_PROXY\x00localhost,127.0.0.1,165.254.109.23,165.254.109.23:8787\x00",
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
		"\nNO_PROXY=localhost,127.0.0.1,pool.example,pool.example:1455\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("strict runtime env missing %q\n---\n%s", want, joined)
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
