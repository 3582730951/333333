package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustCALinuxRootDoesNotRequireSudo(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	if err := trustCALinuxWith("/tmp/ca.pem", "/system/ca.crt", 0, run); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mkdir -p /system",
		"cp /tmp/ca.pem /system/ca.crt",
		"update-ca-certificates",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("root trust commands = %#v, want %#v", calls, want)
	}
}

func TestTrustCALinuxNonRootUsesSudo(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	if err := trustCALinuxWith("/tmp/ca.pem", "/system/ca.crt", 1000, run); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sudo mkdir -p /system",
		"sudo cp /tmp/ca.pem /system/ca.crt",
		"sudo update-ca-certificates",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("non-root trust commands = %#v, want %#v", calls, want)
	}
}

func TestTrustCALinuxPropagatesRefreshFailure(t *testing.T) {
	refreshErr := errors.New("refresh failed")
	run := func(name string, args ...string) error {
		if name == "update-ca-certificates" {
			return refreshErr
		}
		return nil
	}
	err := trustCALinuxWith("/tmp/ca.pem", "/system/ca.crt", 0, run)
	if !errors.Is(err, refreshErr) {
		t.Fatalf("trust error = %v, want wrapped refresh error", err)
	}
}

func TestPerformTrustCAReturnsNonzeroOnTrustFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.MITM.CACert = filepath.Join(dir, "ca.pem")
	cfg.MITM.CAKey = filepath.Join(dir, "ca-key.pem")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("trust failed")
	code := performTrustCA(path, false, func(gotPath string) error {
		if gotPath != cfg.MITM.CACert {
			t.Fatalf("CA path = %q, want %q", gotPath, cfg.MITM.CACert)
		}
		return wantErr
	})
	if code == 0 {
		t.Fatal("trust-ca failure returned success")
	}
}

func TestNormalizeWrapperProxy(t *testing.T) {
	tests := map[string]string{
		"19443":             "http://127.0.0.1:19443",
		"127.0.0.1:19443":   "http://127.0.0.1:19443",
		"0.0.0.0:19443":     "http://127.0.0.1:19443",
		"[::1]:19443":       "http://[::1]:19443",
		"http://host:19443": "http://host:19443",
	}
	for input, want := range tests {
		got, err := normalizeWrapperProxy(input)
		if err != nil {
			t.Fatalf("normalizeWrapperProxy(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeWrapperProxy(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "host", "0", "70000", "socks5://host:1234", "http://host:1234/path"} {
		if got, err := normalizeWrapperProxy(input); err == nil {
			t.Fatalf("normalizeWrapperProxy(%q) = %q, want error", input, got)
		}
	}
}

func TestInstallWrapperFromConfigUsesConfiguredListenAddr(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:19443"
	cfg.MITM.CACert = filepath.Join(dir, "ca.pem")
	cfg.MITM.CAKey = filepath.Join(dir, "ca-key.pem")
	configPath := filepath.Join(dir, "config.json")
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := installWrapperFromConfig(configPath); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{claudePath, filepath.Join(binDir, "claude-plan")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		script := string(data)
		if !strings.Contains(script, `HTTP_PROXY="http://127.0.0.1:19443"`) {
			t.Fatalf("%s does not use configured listen_addr:\n%s", path, script)
		}
		if strings.Contains(script, "127.0.0.1:8765") {
			t.Fatalf("%s retained hard-coded default port:\n%s", path, script)
		}
	}
}

func TestInstallWrapperFallsBackToUserBinAndVerifiesPATH(t *testing.T) {
	dir := t.TempDir()
	systemBin := filepath.Join(dir, "system-bin")
	home := filepath.Join(dir, "home")
	userBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(systemBin, 0755); err != nil {
		t.Fatal(err)
	}
	realClaude := filepath.Join(systemBin, "claude")
	original := "#!/bin/sh\necho real\n"
	if err := os.WriteFile(realClaude, []byte(original), 0755); err != nil {
		t.Fatal(err)
	}
	pathEnv := userBin + string(os.PathListSeparator) + systemBin

	err := installWrapperAt(
		realClaude,
		"127.0.0.1:19443",
		home,
		pathEnv,
		func(string, string) error { return os.ErrPermission },
	)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(realClaude); err != nil || string(data) != original {
		t.Fatalf("fallback modified real Claude binary: data=%q err=%v", data, err)
	}
	wrapperPath := filepath.Join(userBin, "claude")
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, "CLAUDE_REAL_BIN="+strconvQuote(realClaude)) {
		t.Fatalf("fallback wrapper lost real Claude path:\n%s", script)
	}
	if !strings.Contains(script, `HTTP_PROXY="http://127.0.0.1:19443"`) {
		t.Fatalf("fallback wrapper lost configured proxy:\n%s", script)
	}
	resolved, err := lookPathIn("claude", pathEnv)
	if err != nil || !sameCleanPath(resolved, wrapperPath) {
		t.Fatalf("PATH resolved %q (%v), want %q", resolved, err, wrapperPath)
	}
}

func TestInstallWrapperFallbackReportsIneffectivePATH(t *testing.T) {
	dir := t.TempDir()
	systemBin := filepath.Join(dir, "system-bin")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(systemBin, 0755); err != nil {
		t.Fatal(err)
	}
	realClaude := filepath.Join(systemBin, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	err := installWrapperAt(
		realClaude,
		"127.0.0.1:19443",
		home,
		systemBin,
		func(string, string) error { return os.ErrPermission },
	)
	if err == nil {
		t.Fatal("fallback accepted PATH that still selects the system Claude binary")
	}
	if !strings.Contains(err.Error(), "export PATH=") {
		t.Fatalf("fallback error does not provide PATH repair command: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", "claude")); statErr != nil {
		t.Fatalf("fallback wrapper was not created before PATH diagnostic: %v", statErr)
	}
}

func strconvQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
