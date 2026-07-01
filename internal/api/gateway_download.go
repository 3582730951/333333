package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
)

// handleDownloadGateway 提供网关二进制下载
func (s *Server) handleDownloadGateway(w http.ResponseWriter, r *http.Request) {
	// 根据客户端 OS 返回对应二进制
	clientOS := detectClientOS(r)
	clientArch := detectClientArch(r)

	binaryPath := fmt.Sprintf("./bin/gateway-%s-%s", clientOS, clientArch)

	// 如果不存在，尝试当前目录的 gateway
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		binaryPath = "./bin/gateway"
	}

	// 最后尝试：实时编译
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		if err := s.buildGateway(clientOS, clientArch); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("build gateway: %w", err))
			return
		}
	}

	// 设置下载头
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=gateway-%s-%s", clientOS, clientArch))

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
GATEWAY_URL="%s/download/gateway"
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

# 配置
echo ""
echo "[3/5] 配置网关..."
POOL_URL="%s"
%s

# 初始化配置
echo "[4/5] 初始化配置和 CA..."
gateway init --pool-url "$POOL_URL" --key "$API_KEY"

# 自动信任 CA
echo "[5/5] 信任 CA 证书..."
if ! gateway trust-ca; then
  echo ""
  echo "⚠️  自动信任失败，请手动执行："
  gateway trust-ca --print-commands
  echo ""
  echo "执行完成后，运行以下命令继续："
  echo "  gateway install-wrapper"
  echo "  gateway start &"
  exit 0
fi

# 安装包装器
echo ""
echo "安装 claude 命令包装器..."
if command -v claude >/dev/null 2>&1; then
  gateway install-wrapper
else
  echo "⚠️  未检测到 claude 命令，跳过包装器安装"
  echo "    如果稍后安装 Claude Code，运行: gateway install-wrapper"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ 安装完成！"
echo ""
echo "使用方法:"
echo "  1. 启动网关: gateway start &"
echo "  2. 使用 Claude: claude \"your prompt\""
echo ""
echo "查看状态: gateway status"
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

// buildGateway 实时编译网关二进制
func (s *Server) buildGateway(targetOS, targetArch string) error {
	outputPath := fmt.Sprintf("./bin/gateway-%s-%s", targetOS, targetArch)
	if err := os.MkdirAll("./bin", 0755); err != nil {
		return err
	}

	cmd := exec.Command("go", "build",
		"-o", outputPath,
		"./cmd/gateway")
	cmd.Env = append(os.Environ(),
		"GOOS="+targetOS,
		"GOARCH="+targetArch,
		"CGO_ENABLED=0",
	)

	return cmd.Run()
}

// detectClientOS 从 User-Agent 检测客户端 OS
func detectClientOS(r *http.Request) string {
	ua := r.UserAgent()
	switch {
	case contains(ua, "Mac"):
		return "darwin"
	case contains(ua, "Linux"):
		return "linux"
	case contains(ua, "Windows"):
		return "windows"
	default:
		return runtime.GOOS
	}
}

// detectClientArch 检测客户端架构
func detectClientArch(r *http.Request) string {
	ua := r.UserAgent()
	if contains(ua, "ARM") || contains(ua, "aarch64") {
		return "arm64"
	}
	return "amd64"
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
