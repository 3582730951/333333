package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"codex-account-pool/internal/config"
)

// config_fields.go is the registry behind the admin "系统配置 / System config" page:
// the single source of truth for which configuration knobs are runtime-editable, how
// each is rendered/validated, and how a change takes effect. It turns the ~config.json
// fields that were previously file-only + restart-required into a hot-editable surface
// (requirement #1: "管理员能配置和控制项目内的一切，含虚拟身份").
//
// Effect classification:
//   - effectHot:      read at request time through a settings-overlay getter
//     (config_runtime.go / isolate.go flagEnabled). Live immediately.
//   - effectUpstream: consumed inside upstream.Client; a PATCH recomputes the effective
//     config and pushes it via upstream.Client.UpdateConfig (atomic
//     overlay). Live on the next upstream request, no restart.
//   - effectScheduler: consumed inside scheduler.Scheduler; a PATCH recomputes the
//     effective scheduler config and hot-swaps its atomic selection-path snapshot.
//   - effectRestart:  bootstrap-only (listener, db path, semaphore size). Shown
//     read-only with a "需重启" badge; PATCH ignores it.
type configEffect string

const (
	effectHot       configEffect = "hot"
	effectUpstream  configEffect = "upstream"
	effectScheduler configEffect = "scheduler"
	effectRestart   configEffect = "restart"
)

type configFieldType string

const (
	fieldBool   configFieldType = "bool"
	fieldString configFieldType = "string"
	fieldInt    configFieldType = "int"
	fieldCSV    configFieldType = "csv"
	fieldSelect configFieldType = "select"
)

// configField describes one runtime-editable setting. Key is BOTH the settings-table
// key and (for most) the config.json key, so a stored override and the boot default
// line up. boot returns the boot/default value (the getter's fallback) so the page can
// show the effective value and its source.
type configField struct {
	Key      string
	Label    string
	Category string
	Type     configFieldType
	Effect   configEffect
	Options  []string
	Help     string
	boot     func(c config.Config) interface{}
}

type configFieldMetadata struct {
	Placement string
	Domain    interface{}
	Scope     string
	Section   string
	Order     int
}

const (
	configPlacementAI      = "ai_settings"
	configPlacementSystem  = "system_settings"
	configPlacementFeature = "feature_page"
)

// configMetadata is the single ownership map used by both the system settings and
// AI settings clients. A field is emitted once, with one placement and (for AI
// settings) one domain. Keeping the classification server-side prevents the two
// frontends from drifting into duplicate controls for the same setting key.
func configMetadata(f configField, index int) configFieldMetadata {
	meta := configFieldMetadata{
		Placement: configPlacementSystem,
		Domain:    nil,
		Scope:     "global",
		Section:   configSection(f),
		Order:     index + 1,
	}
	key := strings.ToLower(strings.TrimSpace(f.Key))

	if f.Category == catQuality {
		meta.Placement = configPlacementFeature
		return meta
	}

	domain := ""
	switch {
	case strings.HasPrefix(key, "openai_"):
		domain = "chatgpt"
	case strings.HasPrefix(key, "kiro_"):
		domain = "kiro"
	case strings.HasPrefix(key, "antigravity_"):
		domain = "antigravity"
	case strings.HasPrefix(key, "claude_gateway_"),
		key == "claude_cli_version", key == "claude_node_version",
		key == "claude_stainless_version", key == "claude_cch_signing":
		domain = "claude_code"
	case strings.HasPrefix(key, "claude_"):
		domain = "claude"
	case strings.HasPrefix(key, "codex_"):
		domain = "codex"
	case key == "conversation_isolation":
		domain = "claude_code"
	}
	if domain != "" {
		meta.Placement = configPlacementAI
		meta.Domain = domain
	}
	if strings.Contains(key, "model") || strings.Contains(key, "thinking") || strings.Contains(key, "effort") || strings.Contains(key, "reasoning") {
		meta.Scope = "model"
	}
	return meta
}

func configSection(f configField) string {
	switch f.Category {
	case catIdentity:
		return "client_profile"
	case catBehavior:
		return "behavior_cache"
	case catKiro:
		return "provider_cache"
	case catClaudeGateway:
		return "gateway_transport"
	case catLimits:
		return "routing_limits"
	case catQuality:
		return "model_quality"
	case catReg:
		return "registration"
	case catBoot:
		return "bootstrap"
	default:
		return "general"
	}
}

const (
	catIdentity      = "虚拟身份 / 指纹"
	catBehavior      = "行为 / 缓存"
	catKiro          = "Kiro / API Key / 缓存"
	catClaudeGateway = "本地 Gateway / Claude Code"
	catLimits        = "限流 / 封禁"
	catQuality       = "模型质量 / 降智检测"
	catReg           = "注册 / 引擎"
	catBoot          = "引导（需重启）"
)

// configFields is the ordered registry rendered by the System config page. Thinking
// has its own dedicated page (/admin/thinking); sensitive-word lists are managed
// elsewhere — neither is duplicated here.
func configFields() []configField {
	return []configField{
		{Key: "openai_api_upstream_base_url", Label: "OpenAI Platform Base URL", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "仅供 Codex/OpenAI API Key 账号使用；ChatGPT OAuth 继续使用 WHAM upstream_base_url。", boot: func(c config.Config) interface{} { return c.OpenAIAPIUpstreamBaseURL }},
		// ── 虚拟身份 / 指纹 ─────────────────────────────────────────────────────
		{Key: "codex_ja3", Label: "Codex JA3 覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=Chrome(默认，更安全)。设置则 sidecar 尝试重放该 JA3。", boot: func(c config.Config) interface{} { return c.CodexJA3Override }},
		{Key: "claude_ja3", Label: "Claude JA3 覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=Chrome(默认)。真实 claude-cli/Node JA3 为显式 opt-in。", boot: func(c config.Config) interface{} { return c.ClaudeJA3Override }},
		{Key: "egress_fingerprint_engine", Label: "出口指纹引擎", Category: catIdentity, Type: fieldSelect, Effect: effectUpstream,
			Options: []string{"inprocess", "sidecar"},
			Help:    "inprocess(默认)=进程内 Go tls-client(uTLS+fhttp)复现 JA3 与 HTTP/2 Akamai 指纹(Chrome_120，与边车 chrome120 一致)，无独立进程/本地回环跳/双倍套接字；sidecar=经外部 Python curl_cffi 边车，作为随时可切回的兜底。两引擎默认指纹按构造一致；部署后用 /admin/egress-fingerprint-check 校验。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.EgressFingerprintEngine, "inprocess") }},
		{Key: "codex_cli_version", Label: "Codex CLI 版本覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=内置默认。覆盖上游 Codex 客户端版本指纹。", boot: func(c config.Config) interface{} { return c.CodexCLIVersionOverride }},
		{Key: "claude_cli_version", Label: "Claude CLI 版本覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=内置默认。覆盖上游 claude-cli 版本指纹。", boot: func(c config.Config) interface{} { return c.ClaudeCLIVersionOverride }},
		{Key: "claude_node_version", Label: "Claude Node 版本", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "X-Stainless-Runtime-Version 上报的 Node 版本。", boot: func(c config.Config) interface{} { return c.ClaudeNodeVersion }},
		{Key: "claude_stainless_version", Label: "Claude Stainless 版本", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "X-Stainless-Package-Version 上报的 SDK 版本。", boot: func(c config.Config) interface{} { return c.ClaudeStainlessVersion }},
		{Key: "claude_force_direct", Label: "Claude 强制直连", Category: catIdentity, Type: fieldBool, Effect: effectUpstream,
			Help: "开=绕过账号 Sidecar 包装并恢复原 HTTP/SOCKS/WARP 出口；主出口本身是 Sidecar 时退回直连。仅作逃生阀。", boot: func(c config.Config) interface{} { return c.ClaudeForceDirect }},
		{Key: "identity_os_source", Label: "身份 OS 来源", Category: catIdentity, Type: fieldSelect, Effect: effectHot,
			Options: []string{"vps", "downstream", "diverse"},
			Help:    "vps=与主机一致(随出口IP多样而多样)；downstream=按请求体推断；diverse=始终跨OS池。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.IdentityOSSource, "vps") }},
		{Key: "codex_identity_scrub", Label: "Codex 身份擦除", Category: catIdentity, Type: fieldBool, Effect: effectHot,
			Help: "开=按敏感词擦洗 Codex 请求体与响应流。", boot: func(c config.Config) interface{} { return c.CodexIdentityScrub }},
		{Key: "codex_prefer_sidecar_ja3_over_ws", Label: "Codex 优先 sidecar JA3", Category: catIdentity, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=sidecar 出口的 Codex 账号走 SSE 路径(保真 JA3)而非 WebSocket。", boot: func(c config.Config) interface{} { return c.CodexPreferSidecarJA3OverWS }},
		{Key: "codex_reauth_worker_url", Label: "Codex 重登 worker URL", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "默认 http://127.0.0.1:8802；账号详情的自动修复/重登会调用该 HTTP worker。", boot: func(c config.Config) interface{} { return c.CodexReauthWorkerURL }},
		{Key: "codex_reauth_worker_concurrency", Label: "Codex 重登并发", Category: catBoot, Type: fieldInt, Effect: effectRestart,
			Help: "独立 worker 的建议并发；默认 1，避免同时启动多个 Chromium。", boot: func(c config.Config) interface{} { return c.CodexReauthWorkerConcurrency }},

		// ── 行为 / 缓存 ────────────────────────────────────────────────────────
		{Key: "conversation_isolation", Label: "会话隔离 (串号隔离)", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=每账号独立命名会话标识，避免串号导致的跨账号 401 级联。", boot: func(c config.Config) interface{} { return c.ConversationIsolation }},
		{Key: "claude_cache_control_inject", Label: "Claude 缓存注入", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=在 OpenAI 兼容→Claude 路径自动注入 cache_control。", boot: func(c config.Config) interface{} { return c.ClaudeCacheControlInject }},
		{Key: "claude_cache_mode", Label: "Claude 缓存模式", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"stable_safe", "max_hit"},
			Help:    "stable_safe=保守稳定前缀；max_hit=在 4 个断点内加入滚动历史与最新尾部写入点。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheMode, "stable_safe") }},
		{Key: "claude_cache_affinity_policy", Label: "Claude 缓存亲和策略", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"legacy", "balanced"},
			Help:    "legacy=旧路由；balanced=优先 Claude session 与稳定前缀，减少粗路由缓存写入。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheAffinityPolicy, "balanced") }},
		{Key: "claude_cache_breakpoint_policy", Label: "Claude 缓存断点策略", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"legacy", "stable_prefix_safe", "aggressive", "balanced", "coarse_safe", "max_hit"},
			Help:    "legacy 兼容旧策略；新部署优先使用 claude_cache_mode。stable_prefix_safe=优先稳定前缀；coarse_safe=仅标记 tools 与非 billing system。", boot: func(c config.Config) interface{} {
				return firstNonEmpty(c.ClaudeCacheBreakpointPolicy, "stable_prefix_safe")
			}},
		{Key: "claude_cache_optimization_rollout", Label: "Claude 缓存灰度 JSON", Category: catBehavior, Type: fieldString, Effect: effectHot,
			Help: "JSON 灰度范围：groups/api_key_hash_prefixes/percent，断点还支持 account_ids。{}=全量；不支持按具体 Claude 模型灰度。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheOptimizationRollout, "{}") }},
		{Key: "claude_native_cache_breakpoint_inject", Label: "Claude 原生缓存断点", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=对已识别的 Claude Code auto-context 前缀保守补充 cache_control 断点。", boot: func(c config.Config) interface{} { return c.ClaudeNativeCacheBreakpointInject }},
		{Key: "claude_cache_latest_tail_write", Label: "Claude 最新尾部写入", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "max_hit 模式下标记最新可缓存消息尾部，让下一轮能读取完整增长上下文。", boot: func(c config.Config) interface{} { return c.ClaudeCacheLatestTailWrite }},
		{Key: "claude_cache_prewarm_mode", Label: "Claude 预热模式", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"off", "async", "sync_extreme"},
			Help:    "off=关闭；async=后台 max_tokens=0 写缓存；sync_extreme=真实请求前同步预热。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCachePrewarmMode, "off") }},
		{Key: "claude_cache_diagnostics_enabled", Label: "Claude 缓存诊断", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=请求 Anthropic cache diagnostics beta 并记录 miss 原因。", boot: func(c config.Config) interface{} { return c.ClaudeCacheDiagnosticsEnabled }},
		{Key: "claude_cache_singleflight_enabled", Label: "Claude 缓存 Singleflight", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=同前缀并发请求等待首个写缓存请求开始返回，减少并发冷启动 miss。", boot: func(c config.Config) interface{} { return c.ClaudeCacheSingleflightEnabled }},
		{Key: "codex_cache_singleflight_enabled", Label: "Codex 缓存 Singleflight", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=同账号、模型和 prompt_cache_key 的并发冷请求等待 leader 返回响应头；超时后独立执行。", boot: func(c config.Config) interface{} { return c.CodexCacheSingleflightEnabled }},
		{Key: "claude_cache_lossless_block_split", Label: "Claude 无损块拆分", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=仅在拼接后逐字节一致时拆分巨型 text block，便于标记稳定上下文。", boot: func(c config.Config) interface{} { return c.ClaudeCacheLosslessBlockSplit }},
		{Key: "claude_cch_signing", Label: "Claude CCH 签名（已弃用）", Category: catBehavior, Type: fieldBool, Effect: effectUpstream,
			Help: "兼容旧配置；Claude Code 2.1.220 当前 wire 不含 cch，本设置不再改变请求。", boot: func(c config.Config) interface{} { return c.ClaudeCCHSigning }},
		{Key: "claude_cache_ttl", Label: "Claude 缓存 TTL", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"", "1h"}, Help: "注入缓存的 TTL：空=标准(5m)，1h=长缓存。", boot: func(c config.Config) interface{} {
				if strings.TrimSpace(c.ClaudeCacheTTL) == "1h" {
					return "1h"
				}
				return ""
			}},
		{Key: "leak_scrub", Label: "泄漏擦除", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=对下游隐藏池内上游信号(配额/限额头、限额SSE帧、限额错误体)。", boot: func(c config.Config) interface{} { return c.LeakScrubEnabled }},
		{Key: "token_save_enabled", Label: "Token 压缩 (服务端)", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=对请求中的大块工具结果做保守压缩(去ANSI/折叠空行与重复行/超长保留头尾)后再转发上游，降低上游输入 token。默认关(会改动请求内容，可能影响模型输出)。", boot: func(c config.Config) interface{} { return c.TokenSaveEnabled }},
		{Key: "web_search_enabled", Label: "Web 搜索", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=为非 compact 请求注入 web_search 工具。", boot: func(c config.Config) interface{} { return c.WebSearchEnabled }},
		{Key: "require_downstream_key", Label: "要求下游密钥", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=下游请求必须带有效 API Key，否则 401。", boot: func(c config.Config) interface{} { return c.RequireDownstreamKey }},

		// ── 模型质量 / 降智检测 ──────────────────────────────────────────────
		{Key: "model_quality_monitor_enabled", Label: "启用分组模型降智检测", Category: catQuality, Type: fieldBool, Effect: effectHot,
			Help: "按分组×模型检测，绝不逐账号遍历。每周期仅一条短答案主探针；答错才追加确认题。默认关闭以避免意外消耗额度。", boot: func(c config.Config) interface{} { return c.ModelQualityMonitorEnabled }},
		{Key: "model_quality_interval_minutes", Label: "检测周期（分钟）", Category: catQuality, Type: fieldInt, Effect: effectHot,
			Help: "最小 60 分钟；默认每小时一次。", boot: func(c config.Config) interface{} { return c.ModelQualityIntervalMinutes }},
		{Key: "model_quality_reasoning_effort", Label: "主探针推理强度", Category: catQuality, Type: fieldSelect, Effect: effectHot,
			Options: []string{"low", "medium", "high"},
			Help:    "默认 low 以降低每小时成本；若主探针异常，确认题会自动至少使用 medium。只提供 Codex/Claude 共同支持的档位。", boot: func(c config.Config) interface{} {
				return firstNonEmpty(c.ModelQualityReasoningEffort, config.DefaultModelQualityReasoningEffort)
			}},
		{Key: "model_quality_models", Label: "检测模型白名单", Category: catQuality, Type: fieldCSV, Effect: effectHot,
			Help: "留空=检测各分组已发布的全部模型；填写逗号分隔模型名可进一步降低成本。", boot: func(c config.Config) interface{} { return c.ModelQualityModels }},
		{Key: "model_quality_degraded_threshold", Label: "确认异常阈值", Category: catQuality, Type: fieldInt, Effect: effectHot,
			Help: "连续多少轮主探针+确认题都失败才标记降智；最小/默认 2，避免单次随机误判。", boot: func(c config.Config) interface{} { return c.ModelQualityDegradedThreshold }},
		{Key: "model_quality_history_days", Label: "历史保留天数", Category: catQuality, Type: fieldInt, Effect: effectHot,
			Help: "探针历史自动清理，默认 30 天。", boot: func(c config.Config) interface{} { return c.ModelQualityHistoryDays }},

		// ── 本地 Gateway / Claude Code ────────────────────────────────────────
		{Key: "claude_gateway_intercept_hosts", Label: "Gateway 拦截主机", Category: catClaudeGateway, Type: fieldCSV, Effect: effectHot,
			Help: "本地 gateway 会 MITM 并改写的 API 主机/通配符列表。默认只覆盖 Anthropic/Claude 必需 API，避免影响 Claude Bash 内的 Codex。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayInterceptHosts }},
		{Key: "claude_gateway_forward_hosts", Label: "Gateway 放行主机", Category: catClaudeGateway, Type: fieldCSV, Effect: effectHot,
			Help: "除 pool 服务端外额外允许 tunnel/bypass 的主机/通配符列表；默认放行 OpenAI/Codex、GitHub、PyPI、npm、OSV，不做 MITM。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayForwardHosts }},
		{Key: "claude_gateway_blocked_host_patterns", Label: "Gateway 阻断目标", Category: catClaudeGateway, Type: fieldCSV, Effect: effectHot,
			Help: "阻断的非必要遥测/更新目标关键词或通配符。空=不按关键词阻断。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayBlockedHostPatterns }},
		{Key: "claude_gateway_unknown_target_policy", Label: "未知目标策略", Category: catClaudeGateway, Type: fieldSelect, Effect: effectHot,
			Options: []string{"block", "forward"},
			Help:    "forward=默认只 MITM Claude/API，其他目标直连转发；block=只允许配置中的必要目标。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeGatewayUnknownTargetPolicy, "forward") }},
		{Key: "claude_gateway_disable_nonessential_env", Label: "写入禁遥测环境变量", Category: catClaudeGateway, Type: fieldBool, Effect: effectHot,
			Help: "开=脚本、wrapper 和 strict runtime 写入官方禁遥测/禁更新环境变量。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayDisableNonessentialEnv }},
		{Key: "claude_gateway_strict_linux_default", Label: "Claude strict Linux 默认启用", Category: catClaudeGateway, Type: fieldBool, Effect: effectHot,
			Help: "开=/file 脚本默认 POOL_CLIENT_RUNTIME=strict；关=默认 compat，保留真实 HOME 下的 Claude skills/plugins/MCP。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayStrictLinuxDefault }},
		{Key: "claude_gateway_virtual_dns_servers", Label: "虚拟 DNS 服务器", Category: catClaudeGateway, Type: fieldCSV, Effect: effectHot,
			Help: "可选覆盖 /v1/gateway/identity 下发的 DNS。留空=按虚拟身份稳定派生。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayVirtualDNSServers }},

		// ── 限流 / 封禁 ────────────────────────────────────────────────────────
		{Key: "ban_detection_enabled", Label: "封禁检测", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开=高置信终态判定为封禁时审计并删除/隔离账号。", boot: func(c config.Config) interface{} { return c.BanDetectionEnabled }},
		{Key: "ban_auto_delete", Label: "封禁自动删除", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开=确认封禁直接删除账号；关=改为隔离。", boot: func(c config.Config) interface{} { return c.BanAutoDelete }},
		{Key: "rate_limit_guard_enabled", Label: "限额守卫", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=依据成功响应的限额头主动冷却并轮换账号。", boot: func(c config.Config) interface{} { return c.RateLimitGuardEnabled }},
		{Key: "codex_reset_credits_auto_enabled", Label: "Codex 主动重置", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=Codex 7d quota 明确用尽时自动消耗主动重置次数。", boot: func(c config.Config) interface{} { return c.CodexResetCreditsAutoEnabled }},
		{Key: "codex_reset_credits_unknown_consume_enabled", Label: "未知次数允许重置", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=主动重置次数未知时，每个 7d window 允许一次受保护消耗并审计。", boot: func(c config.Config) interface{} { return c.CodexResetCreditsUnknownConsumeEnabled }},
		{Key: "codex_reset_credits_account_denylist", Label: "重置账号拒绝列表", Category: catLimits, Type: fieldCSV, Effect: effectHot,
			Help: "逗号分隔账号 ID。拒绝列表优先级最高。", boot: func(c config.Config) interface{} { return c.CodexResetCreditsAccountDenylist }},
		{Key: "codex_reset_credits_account_allowlist", Label: "重置账号允许列表", Category: catLimits, Type: fieldCSV, Effect: effectHot,
			Help: "逗号分隔账号 ID。账号/分组允许列表任一非空时，只启用命中项。", boot: func(c config.Config) interface{} { return c.CodexResetCreditsAccountAllowlist }},
		{Key: "codex_reset_credits_group_allowlist", Label: "重置分组允许列表", Category: catLimits, Type: fieldCSV, Effect: effectHot,
			Help: "逗号分隔分组名。账号/分组允许列表都为空时启用全部 Codex 账号。", boot: func(c config.Config) interface{} { return c.CodexResetCreditsGroupAllowlist }},
		{Key: "seamless_failover", Label: "无缝故障转移", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=自包含请求遇账号级错误时透明换号重试。", boot: func(c config.Config) interface{} { return c.SeamlessFailover }},
		{Key: "failover_max_attempts", Label: "故障转移最大次数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "单个请求最多换号重试的次数。", boot: func(c config.Config) interface{} { return c.FailoverMaxAttempts }},
		{Key: "sticky_wait_millis", Label: "Sticky 等待毫秒", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "同会话绑定账号暂不可用时短暂等待的毫秒数。", boot: func(c config.Config) interface{} { return c.StickyWaitMillis }},
		{Key: "stateful_sticky_wait_seconds", Label: "Stateful Sticky 等待秒数", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "仅用于 previous_response_id / X-Codex-Turn-State 等必须同账号请求等待本账号本地容量释放；0=跟随请求超时。", boot: func(c config.Config) interface{} { return c.StatefulStickyWaitSeconds }},
		{Key: "account_token_budget", Label: "账号并发 Token 预算", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "同账号已有在途请求时允许叠加的估算输入 token 上限；0=关闭。", boot: func(c config.Config) interface{} { return int(c.AccountTokenBudget) }},
		{Key: "resource_headroom_percent", Label: "资源安全余量", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "CPU、内存或 FD 达到安全线时暂停新准入；最小 10%。", boot: func(c config.Config) interface{} { return c.ResourceHeadroomPercent }},
		{Key: "scheduler_index_enabled", Label: "调度索引 v2", Category: catLimits, Type: fieldBool, Effect: effectScheduler,
			Help: "开(默认)=不可变候选索引与 power-of-two；关=逐请求全候选扫描兼容路径。", boot: func(c config.Config) interface{} { return c.SchedulerIndexEnabled }},
		{Key: "body_v2_enabled", Label: "请求体 v2", Category: catLimits, Type: fieldBool, Effect: effectRestart,
			Help: "开(默认)=有界内存、自动 spool 与可重放 BodySource；关=旧版整块内存读取，需重启。", boot: func(c config.Config) interface{} { return c.BodyV2Enabled }},
		{Key: "context_journal_ttl_seconds", Label: "上下文日志 TTL", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "加密 Responses 重建日志保留秒数，默认 3600 秒（1 小时）。命中(续写/恢复)会滑动续期，活跃长任务可无限恢复。", boot: func(c config.Config) interface{} { return c.ContextJournalTTLSeconds }},
		{Key: "context_journal_max_rows", Label: "上下文日志最大行数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "上下文重建日志表行数上限；超出按 expires_at 最早(最久未续期)优先清理，保护最可能恢复的活跃链。0=不限制。默认 50000。", boot: func(c config.Config) interface{} { return c.ContextJournalMaxRows }},
		{Key: "context_journal_max_mb", Label: "上下文日志最大MB", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "上下文重建日志表存储上限(MB，按加密负载字节)，超出按最早过期优先清理。低配 VPS 磁盘保护。0=不限制。默认 200。", boot: func(c config.Config) interface{} { return c.ContextJournalMaxMB }},
		{Key: "goal_continuity_enabled", Label: "长任务连续性", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=将目标链、检查点和运行租约持久化；旧 context_journal 仅保留读取回退。", boot: func(c config.Config) interface{} { return c.GoalContinuityEnabled }},
		{Key: "goal_legacy_journal_dual_write", Label: "旧日志双写", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "兼容迁移开关，默认关：停止旧 context_journal 的整段快照写入，但保留其读取回退直至 TTL 到期。", boot: func(c config.Config) interface{} { return c.GoalLegacyJournalDualWrite }},
		{Key: "goal_retention_days", Label: "目标保留天数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "目标、检查点和持久 working_state 的滑动保留期。默认 7 天；运行中的目标不会被驱逐。", boot: func(c config.Config) interface{} { return c.GoalRetentionDays }},
		{Key: "goal_storage_max_mb", Label: "目标存储上限MB", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "所有 v2 目标链的加密存储预算。默认 1024 MiB；空间不足时保留活跃上下文并返回可见错误。", boot: func(c config.Config) interface{} { return c.GoalStorageMaxMB }},
		{Key: "goal_compression_chunk_ratio", Label: "目标压缩块比例", Category: catLimits, Type: fieldString, Effect: effectHot,
			Help: "分段检查点的目标块比例(0 到 1)，默认 0.70。", boot: func(c config.Config) interface{} { return c.GoalCompressionChunkRatio }},
		{Key: "goal_compression_max_stages", Label: "目标最大分段数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "超过该完整 turn 数后归并为新的加密 checkpoint，默认 16。", boot: func(c config.Config) interface{} { return c.GoalCompressionMaxStages }},
		{Key: "goal_lease_seconds", Label: "目标租约秒数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "同一目标恢复的数据库租约，默认 90 秒。", boot: func(c config.Config) interface{} { return c.GoalLeaseSeconds }},
		{Key: "goal_heartbeat_seconds", Label: "目标心跳秒数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "压缩、账号等待及恢复期间的目标运行心跳，默认 15 秒。", boot: func(c config.Config) interface{} { return c.GoalHeartbeatSeconds }},
		{Key: "goal_compression_concurrency", Label: "目标压缩并发", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "全局可同时执行的目标压缩作业数，默认 1。", boot: func(c config.Config) interface{} { return c.GoalCompressionConcurrency }},
		{Key: "codex_session_mapping_enabled", Label: "Codex 会话映射", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "用于固定连接 WebSocket 和显式兼容模式的加密 UUIDv7 映射。HTTP 无状态直通开启时优先，不会建立或使用上游 continuation 映射。", boot: func(c config.Config) interface{} { return c.CodexSessionMappingEnabled }},
		{Key: "codex_session_mapping_retention_days", Label: "Codex 映射保留天数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "Codex 会话映射的滑动保留期，默认 7 天；仅保留 HMAC 别名和加密身份元数据。", boot: func(c config.Config) interface{} { return c.CodexSessionMappingRetentionDays }},
		{Key: "codex_cpa_strict", Label: "Codex 严格 CPA", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "默认开启：previous_response_id 与工具输出只交给原上游会话；额度耗尽时使用持久化 Goal 在健康账号重建，不改变原生缓存语义。", boot: func(c config.Config) interface{} { return c.CodexCPAStrict }},
		{Key: "codex_stateless_passthrough", Label: "Codex 无状态直通", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "显式兼容模式，默认关闭：HTTP 请求剥掉 previous_response_id / x-codex-turn-state，允许任意账号接管，但会放弃原生 continuation 与对应缓存收益；固定连接 WebSocket 不受影响。", boot: func(c config.Config) interface{} { return c.CodexStatelessPassthrough }},
		{Key: "strict_sticky_max_cooldown_seconds", Label: "严格 Sticky 冷却阈值", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "严格绑定账号冷却超过该秒数时允许换号；0=永不因长冷却换号。", boot: func(c config.Config) interface{} { return c.StrictStickyMaxCooldownSeconds }},
		{Key: "cooldown_wait_max_seconds", Label: "短冷却等待秒数", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "全组账号都在冷却时最多等待最短冷却的秒数；0=不等待。", boot: func(c config.Config) interface{} { return c.CooldownWaitMaxSeconds }},
		{Key: "scheduler_heartbeat_seconds", Label: "调度等待心跳秒数", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "流式推理在等待账号容量时发送 SSE 注释的间隔。", boot: func(c config.Config) interface{} { return c.SchedulerHeartbeatSeconds }},
		{Key: "stream_keepalive_seconds", Label: "流式保活间隔", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "上游长时间静默时向下游发送协议保活帧(Codex response.in_progress / Claude ping)的间隔秒数，避免中间层/客户端在长流式任务未完成前断开；读取时上限约束在中间层空闲超时之下。0=关闭。默认 15。", boot: func(c config.Config) interface{} { return c.StreamKeepAliveSeconds }},
		{Key: "stream_stall_recovery_seconds", Label: "流式停滞恢复秒数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "上游保持连接但持续无任何新事件达到该时长时，取消旧流并从已记录的 response/checkpoint 发送一次仅上游可见的继续指令。0=关闭；建议 360 秒。", boot: func(c config.Config) interface{} { return c.StreamStallRecoverySeconds }},
		{Key: "stream_auto_continue_enabled", Label: "流式自动续写", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开=流式响应在未收到终止事件(Codex response.completed / Claude message_stop)就中断时，自动携带一次“继续”指令按原上下文重发一次并无缝拼接(重发会消耗上游额度)。默认关。绝不伪造内容，仅向上游发送续写指令。", boot: func(c config.Config) interface{} { return c.StreamAutoContinueEnabled }},
		{Key: "stream_continue_text", Label: "续写指令文本", Category: catLimits, Type: fieldString, Effect: effectHot,
			Help: "自动续写/安全等待续写重发时追加的 user 轮指令。默认英文；可设为任意语言。仅发往上游，不下发给客户端。", boot: func(c config.Config) interface{} { return c.StreamContinueText }},
		{Key: "stream_auto_continue_max_attempts", Label: "自动续写最大次数", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "单个请求最多自动续写的次数，超过则干净结束(保留已产出内容)。默认 1，上限 3，防止重发放大。", boot: func(c config.Config) interface{} { return c.StreamAutoContinueMaxAttempts }},
		{Key: "kiro_version", Label: "Kiro IDE 版本", Category: catKiro, Type: fieldString, Effect: effectHot,
			Help: "Kiro 上游请求指纹中的 IDE 版本。", boot: func(c config.Config) interface{} { return c.KiroVersion }},
		{Key: "kiro_node_version", Label: "Kiro Node 版本", Category: catKiro, Type: fieldString, Effect: effectHot,
			Help: "Kiro 上游请求指纹中的 Node 版本。", boot: func(c config.Config) interface{} { return c.KiroNodeVersion }},
		{Key: "kiro_default_auth_region", Label: "Kiro 默认认证区域", Category: catKiro, Type: fieldString, Effect: effectHot,
			Help: "凭证未指定时使用的认证区域。", boot: func(c config.Config) interface{} { return c.KiroDefaultAuthRegion }},
		{Key: "kiro_default_api_region", Label: "Kiro 默认 API 区域", Category: catKiro, Type: fieldString, Effect: effectHot,
			Help: "凭证未指定时使用的推理区域。", boot: func(c config.Config) interface{} { return c.KiroDefaultAPIRegion }},
		{Key: "kiro_default_thinking", Label: "Kiro 强制深度思考", Category: catKiro, Type: fieldBool, Effect: effectHot,
			Help: "强制开启且不可关闭：所有 Kiro 推理使用原生 adaptive thinking、max effort；不会用提示词伪装思考。", boot: func(config.Config) interface{} { return true }},
		{Key: "kiro_cache_mode", Label: "Kiro 缓存模式", Category: catKiro, Type: fieldSelect, Effect: effectHot,
			Options: []string{"auto", "observe", "off"}, Help: "auto=按 max_hit 规划并发送 cachePoint，同时启用同前缀 singleflight；observe=仅观测；off=关闭请求侧缓存协调。三种模式都会解析真实 token/cache usage。", boot: func(c config.Config) interface{} { return c.KiroCacheMode }},
		{Key: "kiro_endpoint_allowlist", Label: "Kiro 自定义端点白名单", Category: catKiro, Type: fieldCSV, Effect: effectHot,
			Help: "官方 Kiro CLI 服务面（runtime.<region>.kiro.dev、management.<region>.kiro.dev；兼容旧 q.<region>.amazonaws.com 配置）无需加入；测试或私有端点必须精确列出 host:port，防止 bearer token 外发。", boot: func(c config.Config) interface{} { return c.KiroEndpointAllowlist }},
		{Key: "kiro_cache_unreported_threshold", Label: "Kiro 缓存未报告阈值", Category: catKiro, Type: fieldInt, Effect: effectHot,
			Help: "连续成功响应未出现缓存计量字段达到该次数后标记 unreported；按响应而非事件数量计数，且不计为缓存 miss。", boot: func(c config.Config) interface{} { return c.KiroCacheUnreportedThreshold }},
		{Key: "kiro_affinity_wait_millis", Label: "Kiro 亲和短等待毫秒", Category: catKiro, Type: fieldInt, Effect: effectScheduler,
			Help: "兼容旧配置；当前 Kiro 路径不注入代理侧等待，固定为 0。", boot: func(config.Config) interface{} { return 0 }},
		{Key: "codex_prompt_cache_retention", Label: "Codex 提示缓存保留", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"24h", "in_memory", ""},
			Help:    "兼容旧配置；Codex 0.144.x 已不发送该字段，网关会清除它。缓存复用由 prompt_cache_key 与账号亲和保证。", boot: func(c config.Config) interface{} { return c.CodexPromptCacheRetention }},

		// ── 注册 / 引擎 ───────────────────────────────────────────────────────
		{Key: "default_register_method", Label: "默认注册引擎", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"protocol_v2", "protocol", "node", "browser", "browser_v3"},
			Help:    "触发注册未指定引擎时使用。引擎只有在制品、provider、注册出口就绪且当前配置的单账号 canary 成功后才可批量运行。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.DefaultRegisterMethod, "protocol_v2") }},
		{Key: "registration_egress_pool_id", Label: "默认注册池", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "注册任务发起阶段使用的默认注册代理池。注册成功的账号仍默认直连，账号详情页单独修改运行出口。", boot: func(c config.Config) interface{} { return c.RegistrationEgressPoolID }},
		{Key: "registration_enabled", Label: "启用自动注册", Category: catReg, Type: fieldBool, Effect: effectHot,
			Help: "统一控制手动批量、旧版邮箱入口和自动补号。仍需通过出口、Provider 与单账号 canary 就绪检查。", boot: func(c config.Config) interface{} { return c.RegistrationEnabled }},
		{Key: "registration_concurrency", Label: "注册并发上限", Category: catReg, Type: fieldInt, Effect: effectHot,
			Help: "单批最多并行的注册数(每个浏览器独立隔离)。仅在轮换出口上提高，固定 IP 出口保持 1。", boot: func(c config.Config) interface{} { return c.RegistrationConcurrency }},
		{Key: "registration_timeout", Label: "单次注册超时（秒）", Category: catReg, Type: fieldInt, Effect: effectHot,
			Help: "每个注册尝试的总超时，覆盖邮箱轮询、浏览器或协议执行；默认 300 秒。", boot: func(c config.Config) interface{} { return c.RegistrationTimeout }},
		{Key: "registration_default_group", Label: "默认注册分组", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "请求未指定分组时使用；兼容旧 email_registration_group。", boot: func(c config.Config) interface{} { return c.RegistrationDefaultGroup }},
		{Key: "default_sms_provider", Label: "默认接码 Provider", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "请求与自动补号未指定接码商时使用。", boot: func(c config.Config) interface{} { return c.DefaultSMSProvider }},
		{Key: "default_mailbox_provider", Label: "默认邮箱 Provider", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "请求与自动补号未指定邮箱来源时使用；旧 Outlook 邮箱池使用 email_pool。", boot: func(c config.Config) interface{} { return c.DefaultMailboxProvider }},
		{Key: "default_captcha_provider", Label: "默认验证码 Provider", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "请求与自动补号未指定验证码服务时使用。", boot: func(c config.Config) interface{} { return c.DefaultCaptchaProvider }},
		{Key: "codex_install_model", Label: "Codex 安装默认模型", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "一键脚本写入 config.toml 的 model（默认 gpt-5.6-sol）。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.CodexInstallModel, "gpt-5.6-sol") }},
		{Key: "codex_install_effort", Label: "Codex 安装推理强度", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"", "ultra", "max", "xhigh", "high", "medium", "low", "minimal"},
			Help:    "空值=保留 Codex 配置；非空时显式写入 model_reasoning_effort。", boot: func(c config.Config) interface{} { return c.CodexInstallEffort }},
		{Key: "codex_install_approval_policy", Label: "Codex 审批策略", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"", "never", "on-request", "on-failure", "untrusted"},
			Help:    "空值=保留 Codex 配置；非空时作为显式审批策略覆盖。", boot: func(c config.Config) interface{} { return c.CodexInstallApprovalPolicy }},
		{Key: "codex_install_sandbox_mode", Label: "Codex 沙箱模式", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"", "danger-full-access", "workspace-write", "read-only"},
			Help:    "空值=保留 Codex 配置；非空时作为显式沙箱模式覆盖。", boot: func(c config.Config) interface{} { return c.CodexInstallSandboxMode }},

		// ── 接码多平台智能选国家 ───────────────────────────────────────────
		{Key: "sms_platform_strategy", Label: "接码国家策略", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"auto", "manual"},
			Help:    "auto=综合两平台当日统计(成功率排名+价格+库存)与推荐优先级自动选最优国家；manual=使用下方指定的国家。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.SMSPlatformStrategy, "auto") }},
		{Key: "sms_preferred_countries", Label: "推荐国家优先级", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "逗号分隔 ISO-2 代码，越靠前加权越高(默认 BR,CO,PL)。BR 经统计+实测成功率最优故默认最高。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.SMSPreferredCountries, "BR,CO,PL") }},
		{Key: "sms_manual_country", Label: "指定国家", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "策略为 manual 时使用此 ISO-2 国家(如 BR/CO/PL)。auto 时忽略。", boot: func(c config.Config) interface{} { return c.SMSManualCountry }},
		{Key: "sms_stats_top_n", Label: "候选接码商数", Category: catReg, Type: fieldInt, Effect: effectHot,
			Help: "auto 策略按序尝试的前 N 个候选(国家+平台)，失败回退下一个(默认 3)。", boot: func(c config.Config) interface{} {
				if c.SMSStatsTopN < 1 {
					return 3
				}
				return c.SMSStatsTopN
			}},
		{Key: "sms_min_price", Label: "接码最低单价 (USD)", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "auto 策略只考虑不低于该价格的号码；0 表示不设下限。", boot: func(c config.Config) interface{} { return c.SMSMinPrice }},
		{Key: "sms_max_price", Label: "接码最高单价 (USD)", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "auto 策略与实际购买都不会超过该价格；0 表示不设上限。", boot: func(c config.Config) interface{} { return c.SMSMaxPrice }},

		// ── 引导（需重启）─────────────────────────────────────────────────────
		{Key: "listen_addr", Label: "监听地址", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "进程监听地址，改后需重启。", boot: func(c config.Config) interface{} { return c.ListenAddr }},
		{Key: "database_path", Label: "数据库路径", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "SQLite 路径，改后需重启。", boot: func(c config.Config) interface{} { return c.DatabasePath }},
		{Key: "trusted_proxy_cidrs", Label: "可信反向代理 CIDR", Category: catBoot, Type: fieldCSV, Effect: effectRestart,
			Help: "仅这些直连代理可提供 X-Forwarded-*；默认只信任本机回环代理。改后需重启。", boot: func(c config.Config) interface{} { return c.TrustedProxyCIDRs }},
		{Key: "default_sidecar_chain_proxy", Label: "Sidecar 链式代理", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "curl_cffi sidecar 出口链接的上游代理(仅本地测试需要，如 http://127.0.0.1:7897)。VPS 上必须留空。改后需重启。", boot: func(c config.Config) interface{} { return c.DefaultSidecarChainProxy }},
	}
}

func configFieldByKey(key string) (configField, bool) {
	for _, f := range configFields() {
		if f.Key == key {
			return f, true
		}
	}
	return configField{}, false
}

// configFieldValue resolves a field's current EFFECTIVE value. Corrupt stored
// overrides fall back to the boot value; settingsViewJSON exposes the parse/read error.
func (s *Server) configFieldValue(ctx context.Context, f configField) interface{} {
	value, _, _ := s.configFieldResolvedValue(ctx, f)
	return value
}

func (s *Server) configFieldBootValue(f configField) interface{} {
	switch f.Type {
	case fieldBool:
		def, _ := f.boot(s.cfg).(bool)
		return def
	case fieldInt:
		def, _ := f.boot(s.cfg).(int)
		return def
	case fieldCSV:
		if def, ok := f.boot(s.cfg).([]string); ok {
			return def
		}
		return []string{}
	default: // string, select
		def, _ := f.boot(s.cfg).(string)
		return def
	}
}

func (s *Server) configFieldResolvedValue(ctx context.Context, f configField) (interface{}, bool, error) {
	boot := s.configFieldBootValue(f)
	raw, overridden, err := s.store.GetSetting(ctx, f.Key)
	if err != nil {
		return boot, false, fmt.Errorf("read %s: %w", f.Key, err)
	}
	if !overridden {
		return boot, false, nil
	}
	stored, err := validateSettingValue(f, raw)
	if err != nil {
		return boot, true, fmt.Errorf("%s=%q: %w", f.Key, raw, err)
	}
	return configFieldCanonicalValue(f, stored), true, nil
}

func configFieldCanonicalValue(f configField, stored string) interface{} {
	switch f.Type {
	case fieldBool:
		switch strings.ToLower(strings.TrimSpace(stored)) {
		case "1", "true", "on", "yes":
			return true
		default:
			return false
		}
	case fieldInt:
		n, err := strconv.Atoi(strings.TrimSpace(stored))
		if err != nil {
			return 0
		}
		return n
	case fieldCSV:
		parts := strings.Split(stored, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return stored
	}
}

// effectiveUpstreamConfig builds the config the upstream client should run with: the
// boot config with the current DB overrides for the upstream-consumed fingerprint /
// identity fields applied. Pushed via upstream.Client.UpdateConfig at startup and on
// every settings PATCH that touches an effectUpstream field.
func (s *Server) effectiveUpstreamConfig(ctx context.Context) config.Config {
	c := s.cfg
	c.OpenAIAPIUpstreamBaseURL = s.settingString(ctx, "openai_api_upstream_base_url", c.OpenAIAPIUpstreamBaseURL)
	c.CodexJA3Override = s.settingString(ctx, "codex_ja3", c.CodexJA3Override)
	c.ClaudeJA3Override = s.settingString(ctx, "claude_ja3", c.ClaudeJA3Override)
	c.EgressFingerprintEngine = s.settingString(ctx, "egress_fingerprint_engine", c.EgressFingerprintEngine)
	c.ClaudeForceDirect = s.flagEnabled(ctx, "claude_force_direct", c.ClaudeForceDirect)
	c.CodexCLIVersionOverride = s.settingString(ctx, "codex_cli_version", c.CodexCLIVersionOverride)
	c.ClaudeCLIVersionOverride = s.settingString(ctx, "claude_cli_version", c.ClaudeCLIVersionOverride)
	c.ClaudeNodeVersion = s.settingString(ctx, "claude_node_version", c.ClaudeNodeVersion)
	c.ClaudeStainlessVersion = s.settingString(ctx, "claude_stainless_version", c.ClaudeStainlessVersion)
	c.ClaudeCCHSigning = s.flagEnabled(ctx, "claude_cch_signing", c.ClaudeCCHSigning)
	return s.effectiveThinkingConfig(ctx, c)
}

// persistAndPublishEffectiveUpstreamConfig serializes the durable setting change,
// its resolved DB snapshot, and the live client swap as one operation. Every runtime
// writer of an effectUpstream field (including thinking_config_v1) must use this
// helper so an older snapshot cannot publish after a newer save.
func (s *Server) persistAndPublishEffectiveUpstreamConfig(ctx context.Context, persist func(context.Context) error) error {
	s.upstreamConfigPublishMu.Lock()
	defer s.upstreamConfigPublishMu.Unlock()
	if err := persist(ctx); err != nil {
		return err
	}
	// Once persistence succeeds, request cancellation must not replace the durable
	// value with a boot-default live snapshot.
	s.publishEffectiveUpstreamConfigLocked(context.WithoutCancel(ctx))
	return nil
}

func (s *Server) publishEffectiveUpstreamConfig(ctx context.Context) {
	s.upstreamConfigPublishMu.Lock()
	defer s.upstreamConfigPublishMu.Unlock()
	s.publishEffectiveUpstreamConfigLocked(ctx)
}

func (s *Server) publishEffectiveUpstreamConfigLocked(ctx context.Context) {
	if s.upstream == nil && s.upstreamConfigPublishHook == nil {
		return
	}
	cfg := s.effectiveUpstreamConfig(ctx)
	if s.upstream != nil {
		s.upstream.UpdateConfig(cfg)
	}
	if s.upstreamConfigPublishHook != nil {
		s.upstreamConfigPublishHook(cfg)
	}
}

// effectiveSchedulerConfig builds the scheduler's live selection config from boot
// defaults plus DB overrides. It intentionally covers only scheduler-owned knobs;
// Bootstrap-only listener/database fields still require restart; the deprecated
// max_concurrent_upstream field is intentionally absent from this registry.
func (s *Server) effectiveSchedulerConfig(ctx context.Context) config.Config {
	c := s.cfg
	c.StickyWaitMillis = s.settingInt(ctx, "sticky_wait_millis", c.StickyWaitMillis)
	c.StatefulStickyWaitSeconds = s.settingInt(ctx, "stateful_sticky_wait_seconds", c.StatefulStickyWaitSeconds)
	c.AccountTokenBudget = s.settingInt64(ctx, "account_token_budget", c.AccountTokenBudget)
	c.ResourceHeadroomPercent = s.settingInt(ctx, "resource_headroom_percent", c.ResourceHeadroomPercent)
	c.SchedulerIndexEnabled = s.flagEnabled(ctx, "scheduler_index_enabled", c.SchedulerIndexEnabled)
	c.StrictStickyMaxCooldownSeconds = s.settingInt(ctx, "strict_sticky_max_cooldown_seconds", c.StrictStickyMaxCooldownSeconds)
	c.CooldownWaitMaxSeconds = s.settingInt(ctx, "cooldown_wait_max_seconds", c.CooldownWaitMaxSeconds)
	c.SchedulerHeartbeatSeconds = s.settingInt(ctx, "scheduler_heartbeat_seconds", c.SchedulerHeartbeatSeconds)
	if c.StickyWaitMillis <= 0 {
		c.StickyWaitMillis = config.DefaultStickyWaitMillis
	}
	if c.StatefulStickyWaitSeconds < 0 {
		c.StatefulStickyWaitSeconds = 0
	}
	if c.AccountTokenBudget < 0 {
		c.AccountTokenBudget = 0
	}
	if c.ResourceHeadroomPercent < 10 {
		c.ResourceHeadroomPercent = 10
	}
	if c.StrictStickyMaxCooldownSeconds < 0 {
		c.StrictStickyMaxCooldownSeconds = 0
	}
	if c.CooldownWaitMaxSeconds < 0 {
		c.CooldownWaitMaxSeconds = 0
	}
	if c.SchedulerHeartbeatSeconds <= 0 {
		c.SchedulerHeartbeatSeconds = 15
	}
	return c
}

// settingsViewJSON renders the registry with each field's current effective value and
// whether a DB override is in place (so the UI can show "已覆盖 / 默认").
func (s *Server) settingsViewJSON(ctx context.Context) []map[string]interface{} {
	fields := configFields()
	out := make([]map[string]interface{}, 0, len(fields))
	for _, f := range fields {
		index := len(out)
		value, overridden, err := s.configFieldResolvedValue(ctx, f)
		settingsError := ""
		if err != nil {
			settingsError = err.Error()
		}
		opts := f.Options
		if opts == nil {
			opts = []string{}
		}
		meta := configMetadata(f, index)
		out = append(out, map[string]interface{}{
			"key":            f.Key,
			"label":          f.Label,
			"category":       f.Category,
			"type":           string(f.Type),
			"effect":         string(f.Effect),
			"options":        opts,
			"help":           f.Help,
			"value":          value,
			"overridden":     overridden,
			"settings_error": settingsError,
			"placement":      meta.Placement,
			"domain":         meta.Domain,
			"scope":          meta.Scope,
			"section":        meta.Section,
			"order":          meta.Order,
		})
	}
	return out
}

// applySettingsPatch validates and persists a settings patch (key→value), returning
// whether any upstream-consumed field changed (so the caller can refresh the upstream
// client). Unknown and restart-only keys fail fast: a PATCH that appears to succeed
// but does not hot-apply anything is worse than a clear operator error.
func (s *Server) applySettingsPatch(ctx context.Context, body map[string]interface{}) (bool, error) {
	updates := make(map[string]string, len(body))
	changedUpstream := false
	changedScheduler := false
	for k, v := range body {
		f, ok := configFieldByKey(k)
		if !ok {
			return false, fmt.Errorf("unknown config key %q", k)
		}
		if f.Effect == effectRestart {
			return false, fmt.Errorf("config key %q requires restart and is read-only at runtime", k)
		}
		val, err := validateSettingValue(f, v)
		if err != nil {
			return false, fmt.Errorf("%s: %w", k, err)
		}
		updates[k] = val
		if f.Effect == effectUpstream {
			changedUpstream = true
		}
		if f.Effect == effectScheduler {
			changedScheduler = true
		}
	}
	if changedUpstream {
		if err := s.persistAndPublishEffectiveUpstreamConfig(ctx, func(ctx context.Context) error {
			return s.store.SetSettings(ctx, updates)
		}); err != nil {
			return false, err
		}
	} else if err := s.store.SetSettings(ctx, updates); err != nil {
		return false, err
	}
	if changedScheduler && s.scheduler != nil {
		s.scheduler.UpdateConfig(s.effectiveSchedulerConfig(ctx))
	}
	return changedUpstream, nil
}

// validateSettingValue coerces a JSON-decoded value to the canonical string stored in
// the settings table, rejecting type/option mismatches.
func validateSettingValue(f configField, v interface{}) (string, error) {
	switch f.Key {
	case "registration_concurrency", "registration_timeout":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.Atoi(raw)
		if n < 1 {
			return "", fmt.Errorf("must be greater than 0")
		}
		if f.Key == "registration_timeout" && n > 86400 {
			return "", fmt.Errorf("must be at most 86400")
		}
		return raw, nil
	case "kiro_default_thinking":
		enabled := false
		switch value := v.(type) {
		case bool:
			enabled = value
		case string:
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "1", "true", "on", "yes":
				enabled = true
			case "0", "false", "off", "no", "":
				enabled = false
			default:
				return "", fmt.Errorf("expected boolean")
			}
		default:
			return "", fmt.Errorf("expected boolean")
		}
		if !enabled {
			return "", fmt.Errorf("Kiro adaptive thinking is mandatory and cannot be disabled")
		}
		return "true", nil
	case "sms_manual_country":
		str, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("expected ISO-2 country string")
		}
		return normalizePhoneCountryISO(str, true)
	case "sms_preferred_countries":
		switch t := v.(type) {
		case string:
			return normalizePhoneCountryCSV(t)
		case []interface{}:
			parts := make([]string, 0, len(t))
			for _, e := range t {
				s, ok := e.(string)
				if !ok {
					return "", fmt.Errorf("expected ISO-2 country strings")
				}
				parts = append(parts, s)
			}
			return normalizePhoneCountryCSV(strings.Join(parts, ","))
		default:
			return "", fmt.Errorf("expected comma-separated ISO-2 country list")
		}
	case "sms_min_price", "sms_max_price":
		var text string
		switch typed := v.(type) {
		case string:
			text = strings.TrimSpace(typed)
		case float64:
			text = strconv.FormatFloat(typed, 'f', -1, 64)
		case int:
			text = strconv.Itoa(typed)
		default:
			return "", fmt.Errorf("expected a decimal USD price")
		}
		if text == "" {
			return "0", nil
		}
		price, err := strconv.ParseFloat(text, 64)
		if err != nil || price < 0 || price > 1000 {
			return "", fmt.Errorf("must be a decimal between 0 and 1000")
		}
		return strconv.FormatFloat(price, 'f', -1, 64), nil
	case "account_token_budget":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		if n < 0 {
			return "", fmt.Errorf("must be zero (disabled) or greater")
		}
		return raw, nil
	case "resource_headroom_percent":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.Atoi(raw)
		if n < 10 || n > 50 {
			return "", fmt.Errorf("must be between 10 and 50")
		}
		return raw, nil
	case "context_journal_ttl_seconds":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.Atoi(raw)
		if n < 60 {
			return "", fmt.Errorf("must be at least 60")
		}
		return raw, nil
	case "stream_stall_recovery_seconds":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.Atoi(raw)
		if n != 0 && (n < 60 || n > 3600) {
			return "", fmt.Errorf("must be zero or between 60 and 3600")
		}
		return raw, nil
	case "goal_retention_days", "goal_storage_max_mb", "goal_compression_max_stages", "goal_lease_seconds", "goal_heartbeat_seconds", "goal_compression_concurrency":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.Atoi(raw)
		if n <= 0 {
			return "", fmt.Errorf("must be greater than 0")
		}
		if f.Key == "goal_heartbeat_seconds" && n > 60 {
			return "", fmt.Errorf("must be at most 60")
		}
		return raw, nil
	case "goal_compression_chunk_ratio":
		var raw string
		switch value := v.(type) {
		case string:
			raw = strings.TrimSpace(value)
		case float64:
			raw = strconv.FormatFloat(value, 'f', -1, 64)
		default:
			return "", fmt.Errorf("expected decimal between 0 and 1")
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n <= 0 || n >= 1 {
			return "", fmt.Errorf("must be greater than 0 and less than 1")
		}
		return raw, nil
	case "sticky_wait_millis", "scheduler_heartbeat_seconds":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		if n <= 0 {
			return "", fmt.Errorf("must be greater than 0")
		}
		return raw, nil
	case "stateful_sticky_wait_seconds":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		if n < 0 {
			return "0", nil
		}
		return raw, nil
	case "strict_sticky_max_cooldown_seconds", "cooldown_wait_max_seconds":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		if n < 0 {
			return "", fmt.Errorf("must be greater than or equal to 0")
		}
		return raw, nil
	case "claude_cache_optimization_rollout":
		str, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("expected JSON object string")
		}
		str = strings.TrimSpace(str)
		if str == "" {
			str = "{}"
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(str), &obj); err != nil {
			return "", fmt.Errorf("invalid JSON object: %w", err)
		}
		if obj == nil {
			return "", fmt.Errorf("expected JSON object")
		}
		allowed := map[string]bool{
			"groups":                true,
			"api_key_hash_prefixes": true,
			"account_ids":           true,
			"percent":               true,
		}
		for key := range obj {
			if !allowed[key] {
				return "", fmt.Errorf("unsupported rollout key %q", key)
			}
		}
		return str, nil
	}
	switch f.Type {
	case fieldBool:
		switch t := v.(type) {
		case bool:
			if t {
				return "true", nil
			}
			return "false", nil
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "1", "true", "on", "yes":
				return "true", nil
			case "0", "false", "off", "no", "":
				return "false", nil
			}
		}
		return "", fmt.Errorf("expected boolean")
	case fieldInt:
		switch t := v.(type) {
		case float64:
			n := int(t)
			if float64(n) != t {
				return "", fmt.Errorf("expected integer")
			}
			return strconv.Itoa(n), nil
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				return strconv.Itoa(n), nil
			}
		}
		return "", fmt.Errorf("expected integer")
	case fieldSelect:
		str, _ := v.(string)
		str = strings.TrimSpace(str)
		if f.Key == "default_register_method" {
			str = normalizeRegistrationMethodAlias(str)
		}
		for _, o := range f.Options {
			if o == str {
				return str, nil
			}
		}
		return "", fmt.Errorf("must be one of %v", f.Options)
	case fieldCSV:
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t), nil
		case []interface{}:
			parts := make([]string, 0, len(t))
			for _, e := range t {
				if sv, ok := e.(string); ok {
					if sv = strings.TrimSpace(sv); sv != "" {
						parts = append(parts, sv)
					}
				}
			}
			return strings.Join(parts, ","), nil
		}
		return "", fmt.Errorf("expected list or comma-separated string")
	default: // string
		str, _ := v.(string)
		return strings.TrimSpace(str), nil
	}
}

func validateIntegerSetting(v interface{}) (string, error) {
	switch t := v.(type) {
	case float64:
		n := int64(t)
		if float64(n) != t {
			return "", fmt.Errorf("expected integer")
		}
		return strconv.FormatInt(n, 10), nil
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return strconv.FormatInt(n, 10), nil
		}
	}
	return "", fmt.Errorf("expected integer")
}
