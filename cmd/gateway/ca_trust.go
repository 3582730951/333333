package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// TrustCA 自动信任 CA 证书
func TrustCA(caPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return trustCAMacOS(caPath)
	case "linux":
		return trustCALinux(caPath)
	case "windows":
		return trustCAWindows(caPath)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// trustCAMacOS macOS 信任 CA
func trustCAMacOS(caPath string) error {
	cmd := exec.Command("sudo", "security", "add-trusted-cert",
		"-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain",
		caPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// trustCALinux Linux 信任 CA
func trustCALinux(caPath string) error {
	return trustCALinuxWith(
		caPath,
		"/usr/local/share/ca-certificates/claude-gateway.crt",
		os.Geteuid(),
		runTrustCommand,
	)
}

type trustCommandRunner func(name string, args ...string) error

func runTrustCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func trustCALinuxWith(caPath, destPath string, euid int, run trustCommandRunner) error {
	if run == nil {
		return fmt.Errorf("trust command runner is nil")
	}
	runPrivileged := func(name string, args ...string) error {
		if euid == 0 {
			return run(name, args...)
		}
		return run("sudo", append([]string{name}, args...)...)
	}

	if err := runPrivileged("mkdir", "-p", filepath.Dir(destPath)); err != nil {
		return fmt.Errorf("create system CA directory: %w", err)
	}
	if err := runPrivileged("cp", caPath, destPath); err != nil {
		return fmt.Errorf("install CA certificate: %w", err)
	}
	if err := runPrivileged("update-ca-certificates"); err != nil {
		return fmt.Errorf("refresh system CA certificates: %w", err)
	}
	return nil
}

// trustCAWindows Windows 信任 CA
func trustCAWindows(caPath string) error {
	cmd := exec.Command("certutil", "-addstore", "-f", "ROOT", caPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// PrintTrustInstructions 打印手动信任指令
func PrintTrustInstructions(caPath string) {
	fmt.Println("\n📜 手动信任 CA 证书指令：")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	switch runtime.GOOS {
	case "darwin":
		fmt.Printf(`
macOS:
  sudo security add-trusted-cert -d -r trustRoot \
    -k /Library/Keychains/System.keychain \
    %s
`, caPath)
	case "linux":
		if os.Geteuid() == 0 {
			fmt.Printf(`
Linux:
  cp %s /usr/local/share/ca-certificates/claude-gateway.crt
  update-ca-certificates
`, caPath)
		} else {
			fmt.Printf(`
Linux:
  sudo cp %s /usr/local/share/ca-certificates/claude-gateway.crt
  sudo update-ca-certificates
`, caPath)
		}
	case "windows":
		fmt.Printf(`
Windows (管理员权限 PowerShell):
  certutil -addstore -f "ROOT" "%s"
`, caPath)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

func normalizeWrapperProxy(proxyListenAddr string) (string, error) {
	proxyListenAddr = strings.TrimSpace(proxyListenAddr)
	if proxyListenAddr == "" {
		return "", fmt.Errorf("gateway listen address is empty")
	}

	if strings.Contains(proxyListenAddr, "://") {
		parsed, err := url.Parse(proxyListenAddr)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("invalid gateway proxy URL %q", proxyListenAddr)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", fmt.Errorf("unsupported gateway proxy scheme %q", parsed.Scheme)
		}
		if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("gateway proxy URL must contain only scheme and host")
		}
		return parsed.String(), nil
	}

	// Preserve compatibility for callers that only supply a port. Config-driven
	// installs pass the complete listen_addr.
	if port, err := strconv.Atoi(proxyListenAddr); err == nil {
		if port <= 0 || port > 65535 {
			return "", fmt.Errorf("invalid gateway proxy port %d", port)
		}
		return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}

	host, portText, err := net.SplitHostPort(proxyListenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid gateway listen address %q: %w", proxyListenAddr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid gateway proxy port %q", portText)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), portText), nil
}

// GenerateWrapper 生成 claude 命令包装器
func GenerateWrapper(realClaude, wrapperPath, proxyListenAddr string, disableNonessentialEnvOption ...bool) error {
	proxyURL, err := normalizeWrapperProxy(proxyListenAddr)
	if err != nil {
		return err
	}
	disableNonessentialEnv := true
	if len(disableNonessentialEnvOption) > 0 {
		disableNonessentialEnv = disableNonessentialEnvOption[0]
	}
	script := fmt.Sprintf(`#!/bin/bash
# Claude Gateway Wrapper
export CLAUDE_REAL_BIN=%q
export HTTP_PROXY=%q
export HTTPS_PROXY=%q
export ALL_PROXY=%q
export NO_PROXY=%s
export CLAUDE_CODE_ENABLE_AUTO_MODE=1
%s
exec gateway run-claude -- "$@"
`, realClaude, proxyURL, proxyURL, proxyURL, defaultWrapperNoProxy(), renderWrapperDisableNonessentialEnvExports(disableNonessentialEnv))

	return os.WriteFile(wrapperPath, []byte(script), 0755)
}

// GeneratePlanWrapper generates a convenience launcher that starts Claude Code
// directly in plan mode while still using the gateway strict runtime.
func GeneratePlanWrapper(realClaude, wrapperPath, proxyListenAddr string, disableNonessentialEnvOption ...bool) error {
	proxyURL, err := normalizeWrapperProxy(proxyListenAddr)
	if err != nil {
		return err
	}
	disableNonessentialEnv := true
	if len(disableNonessentialEnvOption) > 0 {
		disableNonessentialEnv = disableNonessentialEnvOption[0]
	}
	script := fmt.Sprintf(`#!/bin/bash
# Claude Gateway Plan Wrapper
export CLAUDE_REAL_BIN=%q
export HTTP_PROXY=%q
export HTTPS_PROXY=%q
export ALL_PROXY=%q
export NO_PROXY=%s
export CLAUDE_CODE_ENABLE_AUTO_MODE=1
%s
exec gateway run-claude -- --permission-mode plan "$@"
`, realClaude, proxyURL, proxyURL, proxyURL, defaultWrapperNoProxy(), renderWrapperDisableNonessentialEnvExports(disableNonessentialEnv))

	return os.WriteFile(wrapperPath, []byte(script), 0755)
}

func renderWrapperDisableNonessentialEnvExports(enabled bool) string {
	if !enabled {
		return ""
	}
	return `export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export DO_NOT_TRACK=1
export DISABLE_TELEMETRY=1
export DISABLE_ERROR_REPORTING=1
export DISABLE_AUTOUPDATER=1
export OTEL_METRICS_EXPORTER=none
export OTEL_LOGS_EXPORTER=none`
}

// InstallWrapper 安装 claude 命令包装器
func InstallWrapper(proxyListenAddr string) error {
	if _, err := normalizeWrapperProxy(proxyListenAddr); err != nil {
		return err
	}

	// 查找真实 claude 二进制
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	return installWrapperAt(realClaude, proxyListenAddr, home, os.Getenv("PATH"), os.Rename)
}

func installWrapperAt(realClaude, proxyListenAddr, home, pathEnv string, rename func(string, string) error) error {
	// 备份原始 claude
	backupPath := realClaude + ".real"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err := rename(realClaude, backupPath); err != nil {
			if isWrapperInstallPermissionError(err) {
				return installUserWrapper(realClaude, proxyListenAddr, home, pathEnv)
			}
			return fmt.Errorf("backup failed: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect wrapper backup: %w", err)
	}

	disableNonessentialEnv := true
	if policy, ok := cachedGatewayPolicy(); ok {
		disableNonessentialEnv = policy.DisableNonessentialEnv
	}

	// 生成包装器
	if err := GenerateWrapper(backupPath, realClaude, proxyListenAddr, disableNonessentialEnv); err != nil {
		return fmt.Errorf("generate wrapper failed: %w", err)
	}
	planWrapper := filepath.Join(filepath.Dir(realClaude), "claude-plan")
	if err := GeneratePlanWrapper(backupPath, planWrapper, proxyListenAddr, disableNonessentialEnv); err != nil {
		return fmt.Errorf("generate plan wrapper failed: %w", err)
	}

	fmt.Printf("✓ Installed wrapper: %s → %s\n", realClaude, backupPath)
	fmt.Printf("✓ Installed plan wrapper: %s\n", planWrapper)
	return nil
}

func isWrapperInstallPermissionError(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EROFS)
}

func installUserWrapper(realClaude, proxyListenAddr, home, pathEnv string) error {
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("user home is empty")
	}
	userBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(userBin, 0755); err != nil {
		return fmt.Errorf("create user bin directory: %w", err)
	}
	wrapperPath := filepath.Join(userBin, "claude")
	planWrapper := filepath.Join(userBin, "claude-plan")
	if err := refuseUnmanagedWrapperOverwrite(wrapperPath, "# Claude Gateway Wrapper"); err != nil {
		return err
	}
	if err := refuseUnmanagedWrapperOverwrite(planWrapper, "# Claude Gateway Plan Wrapper"); err != nil {
		return err
	}

	disableNonessentialEnv := true
	if policy, ok := cachedGatewayPolicy(); ok {
		disableNonessentialEnv = policy.DisableNonessentialEnv
	}
	if err := GenerateWrapper(realClaude, wrapperPath, proxyListenAddr, disableNonessentialEnv); err != nil {
		return fmt.Errorf("generate user wrapper: %w", err)
	}
	if err := GeneratePlanWrapper(realClaude, planWrapper, proxyListenAddr, disableNonessentialEnv); err != nil {
		_ = os.Remove(wrapperPath)
		return fmt.Errorf("generate user plan wrapper: %w", err)
	}

	resolved, err := lookPathIn("claude", pathEnv)
	if err != nil || !sameCleanPath(resolved, wrapperPath) {
		return fmt.Errorf(
			"user wrapper installed at %s but PATH still resolves %q; prepend it now: export PATH=%q:$PATH",
			wrapperPath,
			resolved,
			userBin,
		)
	}
	fmt.Printf("✓ Installed user wrapper: %s → %s\n", wrapperPath, realClaude)
	fmt.Printf("✓ Installed plan wrapper: %s\n", planWrapper)
	fmt.Printf("✓ PATH verified: %s precedes the system Claude binary\n", userBin)
	return nil
}

func refuseUnmanagedWrapperOverwrite(path, marker string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing wrapper %s: %w", path, err)
	}
	if !strings.Contains(string(data), marker) {
		return fmt.Errorf("refusing to overwrite unmanaged command at %s", path)
	}
	return nil
}

func lookPathIn(file, pathEnv string) (string, error) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func sameCleanPath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
}

// UninstallWrapper 卸载包装器
func UninstallWrapper() error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found: %w", err)
	}

	backupPath := claudePath + ".real"
	if _, err := os.Stat(backupPath); err == nil {
		// 恢复原始 claude
		if err := os.Remove(claudePath); err != nil {
			return err
		}
		if err := os.Rename(backupPath, claudePath); err != nil {
			return err
		}
		fmt.Printf("✓ Restored original: %s\n", claudePath)
	}
	planWrapper := filepath.Join(filepath.Dir(claudePath), "claude-plan")
	if _, err := os.Stat(planWrapper); err == nil {
		_ = os.Remove(planWrapper)
	}

	return nil
}

// ExpandPath 展开 ~ 为用户主目录
func ExpandPath(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, path[1:])
}
