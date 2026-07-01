package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config 网关配置
type Config struct {
	ListenAddr    string        `json:"listen_addr"`
	PoolServerURL string        `json:"pool_server_url"`
	DownstreamKey string        `json:"downstream_key"`
	Providers     []string      `json:"providers"`
	IdentityTTL   time.Duration `json:"identity_ttl_seconds"`
	LogLevel      string        `json:"log_level"`
	MITM          MITMConfig    `json:"mitm"`
}

type MITMConfig struct {
	CACert string `json:"ca_cert"`
	CAKey  string `json:"ca_key"`
}

type gatewayConfigDisk struct {
	ListenAddr         string     `json:"listen_addr"`
	PoolServerURL      string     `json:"pool_server_url"`
	DownstreamKey      string     `json:"downstream_key"`
	Providers          []string   `json:"providers"`
	IdentityTTLSeconds int64      `json:"identity_ttl_seconds"`
	LogLevel           string     `json:"log_level"`
	MITM               MITMConfig `json:"mitm"`
}

type gatewayConfigPatch struct {
	ListenAddr         *string     `json:"listen_addr"`
	PoolServerURL      *string     `json:"pool_server_url"`
	DownstreamKey      *string     `json:"downstream_key"`
	Providers          []string    `json:"providers"`
	IdentityTTLSeconds *int64      `json:"identity_ttl_seconds"`
	LogLevel           *string     `json:"log_level"`
	MITM               *MITMConfig `json:"mitm"`
}

const (
	gatewayPrivateDirMode  os.FileMode = 0700
	gatewayConfigFileMode  os.FileMode = 0600
	gatewayPublicCertMode  os.FileMode = 0644
	gatewayPrivateFileMode os.FileMode = 0600
)

// DefaultConfig 默认配置
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	gatewayDir := filepath.Join(home, ".claude-gateway")

	return Config{
		ListenAddr:    "127.0.0.1:8765",
		PoolServerURL: "https://localhost:1455",
		DownstreamKey: "",
		Providers:     []string{"claude", "codex"},
		IdentityTTL:   5 * time.Minute,
		LogLevel:      "info",
		MITM: MITMConfig{
			CACert: filepath.Join(gatewayDir, "ca-cert.pem"),
			CAKey:  filepath.Join(gatewayDir, "ca-key.pem"),
		},
	}
}

// LoadConfig 加载配置文件
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := hardenGatewayConfigPath(path); err != nil {
		return cfg, err
	}

	if err := applyGatewayConfigJSON(&cfg, data); err != nil {
		return cfg, err
	}

	// 展开 ~ 路径
	cfg.MITM.CACert = ExpandPath(cfg.MITM.CACert)
	cfg.MITM.CAKey = ExpandPath(cfg.MITM.CAKey)

	return cfg, nil
}

// SaveConfig 保存配置文件
func SaveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(gatewayConfigForDisk(cfg), "", "  ")
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

	if err := os.WriteFile(path, data, gatewayConfigFileMode); err != nil {
		return err
	}
	return os.Chmod(path, gatewayConfigFileMode)
}

func applyGatewayConfigJSON(cfg *Config, data []byte) error {
	var patch gatewayConfigPatch
	if err := json.Unmarshal(data, &patch); err != nil {
		return err
	}
	if patch.ListenAddr != nil {
		cfg.ListenAddr = *patch.ListenAddr
	}
	if patch.PoolServerURL != nil {
		cfg.PoolServerURL = *patch.PoolServerURL
	}
	if patch.DownstreamKey != nil {
		cfg.DownstreamKey = *patch.DownstreamKey
	}
	if patch.Providers != nil {
		cfg.Providers = patch.Providers
	}
	if patch.IdentityTTLSeconds != nil {
		cfg.IdentityTTL = gatewayTTLFromDisk(*patch.IdentityTTLSeconds)
	}
	if patch.LogLevel != nil {
		cfg.LogLevel = *patch.LogLevel
	}
	if patch.MITM != nil {
		cfg.MITM = *patch.MITM
	}
	return nil
}

func gatewayConfigForDisk(cfg Config) gatewayConfigDisk {
	ttlSeconds := int64(cfg.IdentityTTL / time.Second)
	if cfg.IdentityTTL > 0 && ttlSeconds == 0 {
		ttlSeconds = 1
	}
	return gatewayConfigDisk{
		ListenAddr:         cfg.ListenAddr,
		PoolServerURL:      cfg.PoolServerURL,
		DownstreamKey:      cfg.DownstreamKey,
		Providers:          cfg.Providers,
		IdentityTTLSeconds: ttlSeconds,
		LogLevel:           cfg.LogLevel,
		MITM:               cfg.MITM,
	}
}

func gatewayTTLFromDisk(value int64) time.Duration {
	if value <= 0 {
		return 0
	}
	if value >= int64(time.Second) {
		return time.Duration(value)
	}
	return time.Duration(value) * time.Second
}

func hardenGatewayConfigPath(path string) error {
	if err := chmodGatewayPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return os.Chmod(path, gatewayConfigFileMode)
}

func chmodGatewayPrivateDir(dir string) error {
	clean := filepath.Clean(dir)
	if clean == "." || clean == string(os.PathSeparator) {
		return nil
	}
	return os.Chmod(clean, gatewayPrivateDirMode)
}

func main() {
	home, _ := os.UserHomeDir()
	defaultConfigPath := filepath.Join(home, ".claude-gateway", "config.json")

	// 子命令
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]

	switch cmd {
	case "init":
		handleInit(defaultConfigPath)
	case "start":
		handleStart(defaultConfigPath)
	case "status":
		os.Exit(handleStatus(defaultConfigPath))
	case "trust-ca":
		handleTrustCA(defaultConfigPath)
	case "install-wrapper":
		handleInstallWrapper()
	case "uninstall":
		handleUninstall()
	case "quick-install":
		handleQuickInstall(defaultConfigPath)
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Claude Gateway - Local MITM proxy for pool_server

Usage:
  gateway init [--pool-url URL] [--key KEY]    初始化配置和 CA
  gateway start                                 启动代理服务器
  gateway status                                显示运行状态
  gateway trust-ca [--print-commands]           信任 CA 证书
  gateway install-wrapper                       安装 claude 命令包装器
  gateway uninstall                             完整卸载
  gateway quick-install                         一键安装（推荐）

Examples:
  # 一键安装
  gateway quick-install --pool-url https://your-vps.com:1455 --key cap_xxx

  # 手动安装
  gateway init --pool-url https://your-vps.com:1455 --key cap_xxx
  gateway trust-ca
  gateway install-wrapper
  gateway start

  # 日常使用（包装器安装后）
  claude "your prompt"  # 自动走网关`)
}

func handleInit(configPath string) {
	poolURL := flag.String("pool-url", "", "Pool server URL")
	key := flag.String("key", "", "Downstream API key")
	flag.CommandLine.Parse(os.Args[2:])

	cfg := DefaultConfig()
	if *poolURL != "" {
		cfg.PoolServerURL = *poolURL
	}
	if *key != "" {
		cfg.DownstreamKey = *key
	}

	// 保存配置
	if err := SaveConfig(configPath, cfg); err != nil {
		log.Fatalf("Save config failed: %v", err)
	}

	// 生成 CA
	caMgr, err := NewCAManager(cfg.MITM.CACert, cfg.MITM.CAKey)
	if err != nil {
		log.Fatalf("Generate CA failed: %v", err)
	}
	_ = caMgr // 初始化会自动生成 CA

	fmt.Println("✓ Config saved:", configPath)
	fmt.Println("✓ CA generated:", cfg.MITM.CACert)
	fmt.Println("\n下一步:")
	fmt.Println("  1. gateway trust-ca        # 信任 CA 证书")
	fmt.Println("  2. gateway install-wrapper # 安装命令包装器")
	fmt.Println("  3. gateway start           # 启动网关")
}

func handleStart(configPath string) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}

	if cfg.DownstreamKey == "" {
		log.Fatal("downstream_key not configured. Run: gateway init --key cap_xxx")
	}

	proxy, err := NewProxy(cfg)
	if err != nil {
		log.Fatalf("Init proxy failed: %v", err)
	}
	log.Fatal(proxy.ListenAndServe())
}

type gatewayStatusReport struct {
	ConfigPath              string
	ConfigLoaded            bool
	ConfigError             string
	ListenAddr              string
	PoolServerURL           string
	DownstreamKeyConfigured bool
	CACertPresent           bool
	CAKeyPresent            bool
	GatewayReachable        bool
	GatewayError            string
	PoolReachable           bool
	PoolStatus              int
	PoolError               string
}

func handleStatus(configPath string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	report := inspectGatewayStatus(ctx, configPath)
	printGatewayStatus(report)
	if !report.ConfigLoaded || !report.GatewayReachable {
		return 1
	}
	return 0
}

func inspectGatewayStatus(ctx context.Context, configPath string) gatewayStatusReport {
	report := gatewayStatusReport{ConfigPath: configPath}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		report.ConfigError = err.Error()
		return report
	}
	report.ConfigLoaded = true
	report.ListenAddr = cfg.ListenAddr
	report.PoolServerURL = cfg.PoolServerURL
	report.DownstreamKeyConfigured = cfg.DownstreamKey != ""
	report.CACertPresent = fileExists(cfg.MITM.CACert)
	report.CAKeyPresent = fileExists(cfg.MITM.CAKey)

	if conn, err := net.DialTimeout("tcp", cfg.ListenAddr, 2*time.Second); err == nil {
		report.GatewayReachable = true
		_ = conn.Close()
	} else {
		report.GatewayError = err.Error()
	}

	if cfg.PoolServerURL != "" {
		status, err := probePoolHealth(ctx, cfg.PoolServerURL)
		if err == nil {
			report.PoolReachable = true
			report.PoolStatus = status
		} else {
			report.PoolError = err.Error()
		}
	}
	return report
}

func printGatewayStatus(report gatewayStatusReport) {
	fmt.Println("Gateway status")
	fmt.Println("  Config:", report.ConfigPath)
	if !report.ConfigLoaded {
		fmt.Println("  Config loaded: no")
		fmt.Println("  Error:", report.ConfigError)
		return
	}
	fmt.Println("  Config loaded: yes")
	fmt.Println("  Listen:", report.ListenAddr)
	fmt.Println("  Pool:", report.PoolServerURL)
	fmt.Println("  Downstream key:", yesNo(report.DownstreamKeyConfigured))
	fmt.Println("  CA cert:", yesNo(report.CACertPresent))
	fmt.Println("  CA key:", yesNo(report.CAKeyPresent))
	if report.GatewayReachable {
		fmt.Println("  Gateway TCP:", "reachable")
	} else {
		fmt.Println("  Gateway TCP:", "not reachable")
		fmt.Println("  Gateway error:", report.GatewayError)
	}
	if report.PoolServerURL == "" {
		fmt.Println("  Pool health:", "not configured")
	} else if report.PoolReachable {
		fmt.Printf("  Pool health: reachable (HTTP %d)\n", report.PoolStatus)
	} else {
		fmt.Println("  Pool health:", "not reachable")
		fmt.Println("  Pool error:", report.PoolError)
	}
}

func probePoolHealth(ctx context.Context, poolURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(poolURL, "/")+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	return resp.StatusCode, nil
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func handleTrustCA(configPath string) {
	printCommands := flag.Bool("print-commands", false, "Print manual commands")
	flag.CommandLine.Parse(os.Args[2:])

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}

	if *printCommands {
		PrintTrustInstructions(cfg.MITM.CACert)
		return
	}

	// 尝试自动信任
	fmt.Println("尝试自动信任 CA...")
	if err := TrustCA(cfg.MITM.CACert); err != nil {
		fmt.Printf("❌ 自动信任失败: %v\n", err)
		PrintTrustInstructions(cfg.MITM.CACert)
	} else {
		fmt.Println("✓ CA 已信任")
	}
}

func handleInstallWrapper() {
	if err := InstallWrapper(); err != nil {
		log.Fatalf("Install wrapper failed: %v", err)
	}
	fmt.Println("✓ Wrapper installed. Now run: gateway start")
}

func handleUninstall() {
	fmt.Println("卸载 Claude Gateway...")

	// 恢复 claude 命令
	if err := UninstallWrapper(); err != nil {
		fmt.Printf("⚠️  Wrapper uninstall: %v\n", err)
	}

	// TODO: 撤销 CA 信任、删除配置文件

	fmt.Println("✓ Uninstalled")
}

func handleQuickInstall(configPath string) {
	poolURL := flag.String("pool-url", "", "Pool server URL (required)")
	key := flag.String("key", "", "Downstream API key (required)")
	flag.CommandLine.Parse(os.Args[2:])

	if *poolURL == "" || *key == "" {
		log.Fatal("Usage: gateway quick-install --pool-url URL --key KEY")
	}

	fmt.Println("🚀 Claude Gateway 一键安装")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 1. 初始化配置和 CA
	fmt.Println("\n[1/4] 初始化配置...")
	cfg := DefaultConfig()
	cfg.PoolServerURL = *poolURL
	cfg.DownstreamKey = *key
	if err := SaveConfig(configPath, cfg); err != nil {
		log.Fatalf("Save config failed: %v", err)
	}
	caMgr, err := NewCAManager(cfg.MITM.CACert, cfg.MITM.CAKey)
	if err != nil {
		log.Fatalf("Generate CA failed: %v", err)
	}
	_ = caMgr
	fmt.Println("  ✓ 配置已保存:", configPath)
	fmt.Println("  ✓ CA 已生成:", cfg.MITM.CACert)

	// 2. 信任 CA
	fmt.Println("\n[2/4] 信任 CA 证书...")
	if err := TrustCA(cfg.MITM.CACert); err != nil {
		fmt.Printf("  ❌ 自动信任失败，请手动执行:\n")
		PrintTrustInstructions(cfg.MITM.CACert)
		fmt.Println("\n执行完成后，运行: gateway quick-install --pool-url", *poolURL, "--key", *key)
		os.Exit(1)
	}
	fmt.Println("  ✓ CA 已信任")

	// 3. 安装包装器
	fmt.Println("\n[3/4] 安装 claude 命令包装器...")
	if err := InstallWrapper(); err != nil {
		log.Fatalf("Install wrapper failed: %v", err)
	}
	fmt.Println("  ✓ 包装器已安装")

	// 4. 测试连接
	fmt.Println("\n[4/4] 测试连接...")
	fmt.Println("  ⏩ 跳过（手动测试: gateway start）")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("✅ 安装完成！")
	fmt.Println("\n使用方法:")
	fmt.Println("  1. 在新终端运行: gateway start")
	fmt.Println("  2. 在另一个终端直接使用: claude \"your prompt\"")
	fmt.Println("\n网关会自动拦截并改写请求，无需手动设置环境变量。")
}
