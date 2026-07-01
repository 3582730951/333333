package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// 复制到系统目录
	destPath := "/usr/local/share/ca-certificates/claude-gateway.crt"
	if err := exec.Command("sudo", "cp", caPath, destPath).Run(); err != nil {
		return err
	}
	// 更新证书
	return exec.Command("sudo", "update-ca-certificates").Run()
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
		fmt.Printf(`
Linux:
  sudo cp %s /usr/local/share/ca-certificates/claude-gateway.crt
  sudo update-ca-certificates
`, caPath)
	case "windows":
		fmt.Printf(`
Windows (管理员权限 PowerShell):
  certutil -addstore -f "ROOT" "%s"
`, caPath)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// GenerateWrapper 生成 claude 命令包装器
func GenerateWrapper(realClaude, wrapperPath, proxyPort string) error {
	script := fmt.Sprintf(`#!/bin/bash
# Claude Gateway Wrapper
export HTTPS_PROXY=http://127.0.0.1:%s
export NO_PROXY=localhost,127.0.0.1
exec "%s" "$@"
`, proxyPort, realClaude)

	return os.WriteFile(wrapperPath, []byte(script), 0755)
}

// InstallWrapper 安装 claude 命令包装器
func InstallWrapper() error {
	// 查找真实 claude 二进制
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found: %w", err)
	}

	// 备份原始 claude
	backupPath := realClaude + ".real"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err := os.Rename(realClaude, backupPath); err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
	}

	// 生成包装器
	if err := GenerateWrapper(backupPath, realClaude, "8765"); err != nil {
		return fmt.Errorf("generate wrapper failed: %w", err)
	}

	fmt.Printf("✓ Installed wrapper: %s → %s\n", realClaude, backupPath)
	return nil
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
