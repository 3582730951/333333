package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"codex-account-pool/internal/identity"
)

// GatewayIdentityResponse 返回给本地网关的完整虚拟身份
type GatewayIdentityResponse struct {
	AccountID string `json:"account_id"`

	// 核心标识符
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	MachineID string `json:"machine_id"`

	// 系统环境
	OSName    string `json:"os_name"`
	OSVersion string `json:"os_version"`
	OSRelease string `json:"os_release"`
	Arch      string `json:"arch"`
	Terminal  string `json:"terminal"`

	// 客户端版本
	NodeVersion             string `json:"node_version"`
	ClaudeCLIVersion        string `json:"claude_cli_version"`
	StainlessPackageVersion string `json:"stainless_package_version"`
	CodexCLIVersion         string `json:"codex_cli_version"`

	// 用户环境
	Username string `json:"username"`
	Hostname string `json:"hostname"`
	HomeDir  string `json:"home_dir"`

	// 环境变量映射
	EnvVars map[string]string `json:"env_vars"`

	// 网络环境
	DNSServers []string `json:"dns_servers"`
	GatewayIP  string   `json:"gateway_ip"`
	LocalIP    string   `json:"local_ip"`

	// 进程信息（虚拟）
	ProcessInfo ProcessInfo `json:"process_info"`

	// 本地 gateway 策略（管理员 UI 可编辑）
	GatewayPolicy GatewayPolicy `json:"gateway_policy"`
}

type ProcessInfo struct {
	PID       int      `json:"pid"`
	ParentPID int      `json:"parent_pid"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	CWD       string   `json:"cwd"`
}

type GatewayPolicy struct {
	InterceptHosts         []string `json:"intercept_hosts"`
	ForwardHosts           []string `json:"forward_hosts"`
	BlockedHostPatterns    []string `json:"blocked_host_patterns"`
	UnknownTargetPolicy    string   `json:"unknown_target_policy"`
	DisableNonessentialEnv bool     `json:"disable_nonessential_env"`
	StrictLinuxDefault     bool     `json:"strict_linux_default"`
}

// handleGatewayIdentity 返回本地网关需要的虚拟身份
// 注意：返回的是"用于改写的模板值"，不绑定到具体账号
func (s *Server) handleGatewayIdentity(w http.ResponseWriter, r *http.Request) {
	downstreamKey := gatewayDownstreamKey(r)
	_ = r.URL.Query().Get("provider") // 预留参数，暂不使用

	if downstreamKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing downstream_key"))
		return
	}
	if !s.allowGatewayIdentityKey(w, r, downstreamKey) {
		return
	}

	// 生成虚拟身份模板（不绑定具体账号）
	// 使用 downstream_key 作为种子，确保同一 key 获得稳定的虚拟身份
	id := s.virtualIdentity(r.Context(), downstreamKey, "")

	// 生成虚拟网络环境
	dnsServers := s.gatewayDNSServers(r.Context(), id)
	gatewayIP, localIP := generateVirtualNetwork(id)

	resp := GatewayIdentityResponse{
		AccountID:               "", // 不返回具体账号 ID
		SessionID:               id.SessionID,
		UserID:                  id.UserID,
		MachineID:               id.MachineID,
		OSName:                  id.OSName,
		OSVersion:               id.OSVersion,
		OSRelease:               id.OSRelease,
		Arch:                    id.Arch,
		Terminal:                id.Terminal,
		NodeVersion:             id.NodeVersion,
		ClaudeCLIVersion:        id.ClaudeCLIVersion,
		StainlessPackageVersion: id.StainlessPackageVersion,
		CodexCLIVersion:         id.CodexCLIVersion,
		Username:                id.Username,
		Hostname:                id.Hostname,
		HomeDir:                 id.HomeDir,
		EnvVars:                 buildVirtualEnvVars(id),
		DNSServers:              dnsServers,
		GatewayIP:               gatewayIP,
		LocalIP:                 localIP,
		ProcessInfo:             generateVirtualProcessInfo(id),
		GatewayPolicy:           s.gatewayPolicy(r.Context()),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) gatewayDNSServers(ctx context.Context, id identity.Identity) []string {
	servers := s.settingCSV(ctx, "claude_gateway_virtual_dns_servers", s.cfg.ClaudeGatewayVirtualDNSServers)
	if len(servers) == 0 {
		return generateVirtualDNS(id)
	}
	return servers
}

func (s *Server) gatewayPolicy(ctx context.Context) GatewayPolicy {
	unknown := normalizeGatewayUnknownTargetPolicy(s.settingString(ctx, "claude_gateway_unknown_target_policy", s.cfg.ClaudeGatewayUnknownTargetPolicy))
	return GatewayPolicy{
		InterceptHosts:         s.settingCSV(ctx, "claude_gateway_intercept_hosts", s.cfg.ClaudeGatewayInterceptHosts),
		ForwardHosts:           s.settingCSV(ctx, "claude_gateway_forward_hosts", s.cfg.ClaudeGatewayForwardHosts),
		BlockedHostPatterns:    s.settingCSV(ctx, "claude_gateway_blocked_host_patterns", s.cfg.ClaudeGatewayBlockedHostPatterns),
		UnknownTargetPolicy:    unknown,
		DisableNonessentialEnv: s.flagEnabled(ctx, "claude_gateway_disable_nonessential_env", s.cfg.ClaudeGatewayDisableNonessentialEnv),
		StrictLinuxDefault:     s.flagEnabled(ctx, "claude_gateway_strict_linux_default", s.cfg.ClaudeGatewayStrictLinuxDefault),
	}
}

func normalizeGatewayUnknownTargetPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "forward":
		return "forward"
	default:
		return "block"
	}
}

func (s *Server) allowGatewayIdentityKey(w http.ResponseWriter, r *http.Request, plain string) bool {
	if !s.flagEnabled(r.Context(), "require_downstream_key", s.cfg.RequireDownstreamKey) {
		return true
	}
	key, found, err := s.store.LookupAPIKey(r.Context(), hashAPIKey(plain))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	if !found {
		writeError(w, http.StatusUnauthorized, errors.New("unknown api key"))
		return false
	}
	if !key.Enabled {
		writeError(w, http.StatusUnauthorized, errors.New("api key disabled"))
		return false
	}
	return true
}

func gatewayDownstreamKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if key := strings.TrimSpace(r.Header.Get("X-Downstream-Key")); key != "" {
		return key
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
		if key := strings.TrimSpace(auth[len("Bearer "):]); key != "" {
			return key
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("downstream_key"))
}

// buildVirtualEnvVars 构建虚拟环境变量映射
func buildVirtualEnvVars(id identity.Identity) map[string]string {
	shell := "/bin/zsh"
	if id.OSName == "Linux" {
		shell = "/bin/bash"
	}

	return map[string]string{
		"HOME":   id.HomeDir,
		"USER":   id.Username,
		"SHELL":  shell,
		"TERM":   "xterm-256color",
		"LANG":   "en_US.UTF-8",
		"TMPDIR": "/tmp",
	}
}

// generateVirtualDNS 为账号生成稳定的虚拟 DNS 服务器列表
func generateVirtualDNS(id identity.Identity) []string {
	// 从账号 ID 派生，确保稳定性
	hash := identity.DerivedKey(id.MachineID, "dns")
	choice := int(hash[0]) % 3

	switch choice {
	case 0:
		return []string{"8.8.8.8", "8.8.4.4"} // Google DNS
	case 1:
		return []string{"1.1.1.1", "1.0.0.1"} // Cloudflare DNS
	default:
		return []string{"208.67.222.222", "208.67.220.220"} // OpenDNS
	}
}

// generateVirtualNetwork 生成虚拟网关 IP 和本地 IP
func generateVirtualNetwork(id identity.Identity) (gateway, local string) {
	// 从账号 ID 派生，确保稳定性
	hash := identity.DerivedKey(id.MachineID, "network")

	// 网关：192.168.x.1（x 从 hash 派生）
	subnet := int(hash[0])
	gateway = net.IPv4(192, 168, byte(subnet), 1).String()

	// 本地 IP：192.168.x.y（y 从 hash 派生）
	host := int(hash[1])%254 + 2 // 2-255
	local = net.IPv4(192, 168, byte(subnet), byte(host)).String()

	return
}

// generateVirtualProcessInfo 生成虚拟进程信息
func generateVirtualProcessInfo(id identity.Identity) ProcessInfo {
	// 从账号 ID 派生稳定的虚拟 PID
	hash := identity.DerivedKey(id.MachineID, "process")
	pid := int(hash[0])<<8 | int(hash[1])
	if pid < 1000 {
		pid += 10000
	}

	return ProcessInfo{
		PID:       pid,
		ParentPID: pid - 1,
		Command:   "claude",
		Args:      []string{"claude"}, // 实际参数由网关保留
	}
}

// isGatewayMode 检测请求是否来自本地网关
func isGatewayMode(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("X-Gateway-Mode")) == "local"
}
