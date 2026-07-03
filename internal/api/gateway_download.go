package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// handleDownloadGateway 提供网关二进制下载
func (s *Server) handleDownloadGateway(w http.ResponseWriter, r *http.Request) {
	// 根据客户端 OS 返回对应二进制
	clientOS, err := detectClientOS(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	clientArch, err := detectClientArch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	binaryPath, err := s.gatewayBinaryPath(clientOS, clientArch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 设置下载头
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", gatewayBinaryName(clientOS, clientArch)))

	http.ServeFile(w, r, binaryPath)
}

// handleGatewayInstallScript 返回一键安装脚本（带 API Key 预填充）
func (s *Server) handleGatewayInstallScript(w http.ResponseWriter, r *http.Request) {
	// 从 query 或 header 获取 API Key
	apiKey := r.URL.Query().Get("key")
	if apiKey == "" {
		apiKey = extractAPIKey(r)
	}

	// 获取当前 VPS 地址
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	// 如果有 API Key，自动填充；否则提示输入
	apiKeyLine := ""
	if apiKey != "" {
		apiKeyLine = fmt.Sprintf(`API_KEY="%s"`, apiKey)
	} else {
		apiKeyLine = `read -p "请输入下游 API Key (cap_xxx): " API_KEY
if [ -z "$API_KEY" ]; then
  echo "❌ API Key 不能为空"
  exit 1
fi`
	}

	// 生成安装脚本
	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "🚀 Claude Gateway 自动安装脚本"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# 检测 OS 和架构
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac

# 下载网关二进制
echo "[1/5] 下载网关..."
GATEWAY_URL="%s/download/gateway?os=$OS&arch=$ARCH"
curl -fsSL "$GATEWAY_URL" -o /tmp/gateway || {
  echo "❌ 下载失败，请检查网络或 VPS 地址"
  exit 1
}
chmod +x /tmp/gateway

# 移动到系统目录
echo "[2/5] 安装到 /usr/local/bin..."
sudo mv /tmp/gateway /usr/local/bin/gateway || {
  echo "⚠️  需要 sudo 权限，请手动执行："
  echo "    sudo mv /tmp/gateway /usr/local/bin/gateway"
  exit 1
}
GATEWAY_BIN="/usr/local/bin/gateway"

# 配置
echo ""
echo "[3/5] 配置网关..."
POOL_URL="%s"
%s
export CLAUDE_CODE_ENABLE_AUTO_MODE=1

# 初始化配置
echo "[4/5] 初始化配置和 CA..."
"$GATEWAY_BIN" init --pool-url "$POOL_URL" --key "$API_KEY"

# 自动信任 CA
echo "[5/5] 信任 CA 证书..."
if ! "$GATEWAY_BIN" trust-ca; then
  echo ""
  echo "⚠️  自动信任失败，请手动执行："
  "$GATEWAY_BIN" trust-ca --print-commands
  echo ""
  echo "执行完成后，运行以下命令继续："
  echo "  $GATEWAY_BIN install-wrapper"
  echo "  $GATEWAY_BIN start &"
  exit 0
fi

# 安装包装器
echo ""
echo "安装 claude 命令包装器..."
if command -v claude >/dev/null 2>&1; then
  "$GATEWAY_BIN" install-wrapper
else
  echo "⚠️  未检测到 claude 命令，跳过包装器安装"
  echo "    如果稍后安装 Claude Code，运行: $GATEWAY_BIN install-wrapper"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ 安装完成！"
echo ""
echo "使用方法:"
echo "  1. 启动网关: $GATEWAY_BIN start &"
echo "  2. 使用 Claude: claude \"your prompt\""
echo "  3. 计划模式: claude-plan \"your prompt\""
echo ""
echo "查看状态: $GATEWAY_BIN status"
echo "查看日志: tail -f ~/.claude-gateway/gateway.log"
`, baseURL, baseURL, apiKeyLine)

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-gateway.sh")
	w.Write([]byte(script))
}

// extractAPIKey 从请求头提取 API Key
func extractAPIKey(r *http.Request) string {
	// 尝试多种来源
	if auth := r.Header.Get("Authorization"); auth != "" {
		// Bearer cap_xxx 或 cap_xxx
		if len(auth) > 7 && auth[:7] == "Bearer " {
			return auth[7:]
		}
		return auth
	}
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}
	return ""
}

func (s *Server) gatewayBinaryPath(targetOS, targetArch string) (string, error) {
	name := gatewayBinaryName(targetOS, targetArch)
	dirs := gatewayBinaryDirs()
	for _, dir := range dirs {
		for _, candidate := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, genericGatewayBinaryName(targetOS)),
		} {
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}

	outputPath, err := s.buildGateway(targetOS, targetArch)
	if err != nil {
		return "", fmt.Errorf("gateway binary %s not found in %s and live build failed: %w", name, strings.Join(dirs, ", "), err)
	}
	if st, err := os.Stat(outputPath); err == nil && !st.IsDir() {
		return outputPath, nil
	}
	return "", fmt.Errorf("live build completed but output binary is missing: %s", outputPath)
}

func gatewayBinaryDirs() []string {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv("GATEWAY_BIN_DIR")); env != "" {
		dirs = append(dirs, filepath.SplitList(env)...)
	}
	dirs = append(dirs, "./bin")
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		dirs = append(dirs,
			filepath.Join(exeDir, "bin"),
			filepath.Clean(filepath.Join(exeDir, "..", "lib", "codex-pool", "bin")),
		)
	}
	dirs = append(dirs, "/usr/local/lib/codex-pool/bin")

	seen := make(map[string]bool, len(dirs))
	out := dirs[:0]
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

func gatewayBinaryName(targetOS, targetArch string) string {
	name := fmt.Sprintf("gateway-%s-%s", targetOS, targetArch)
	if targetOS == "windows" {
		name += ".exe"
	}
	return name
}

func genericGatewayBinaryName(targetOS string) string {
	if targetOS == "windows" {
		return "gateway.exe"
	}
	return "gateway"
}

// buildGateway 实时编译网关二进制
func (s *Server) buildGateway(targetOS, targetArch string) (string, error) {
	outputPath := filepath.Join("./bin", gatewayBinaryName(targetOS, targetArch))
	if err := os.MkdirAll("./bin", 0755); err != nil {
		return "", err
	}

	cmd := exec.Command("go", "build",
		"-o", outputPath,
		"./cmd/gateway")
	cmd.Env = append(os.Environ(),
		"GOOS="+targetOS,
		"GOARCH="+targetArch,
		"CGO_ENABLED=0",
	)

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return outputPath, nil
}

// detectClientOS 从 User-Agent 检测客户端 OS
func detectClientOS(r *http.Request) (string, error) {
	if raw := r.URL.Query().Get("os"); raw != "" {
		return normalizeGatewayOS(raw)
	}
	ua := r.UserAgent()
	switch {
	case strings.Contains(ua, "Mac"):
		return "darwin", nil
	case strings.Contains(ua, "Linux"):
		return "linux", nil
	case strings.Contains(ua, "Windows"):
		return "windows", nil
	default:
		return normalizeGatewayOS(runtime.GOOS)
	}
}

// detectClientArch 检测客户端架构
func detectClientArch(r *http.Request) (string, error) {
	if raw := r.URL.Query().Get("arch"); raw != "" {
		return normalizeGatewayArch(raw)
	}
	ua := r.UserAgent()
	if strings.Contains(ua, "ARM") || strings.Contains(ua, "arm64") || strings.Contains(ua, "aarch64") {
		return "arm64", nil
	}
	return normalizeGatewayArch(runtime.GOARCH)
}

func normalizeGatewayOS(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "linux":
		return "linux", nil
	case "darwin", "mac", "macos":
		return "darwin", nil
	case "windows", "win":
		return "windows", nil
	default:
		return "", fmt.Errorf("unsupported gateway os %q", v)
	}
}

func normalizeGatewayArch(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported gateway arch %q", v)
	}
}
