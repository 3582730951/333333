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

const (
	catIdentity      = "虚拟身份 / 指纹"
	catBehavior      = "行为 / 缓存"
	catClaudeGateway = "本地 Gateway / Claude Code"
	catLimits        = "限流 / 封禁"
	catReg           = "注册 / 引擎"
	catBoot          = "引导（需重启）"
)

// configFields is the ordered registry rendered by the System config page. Thinking
// has its own dedicated page (/admin/thinking); sensitive-word lists are managed
// elsewhere — neither is duplicated here.
func configFields() []configField {
	return []configField{
		// ── 虚拟身份 / 指纹 ─────────────────────────────────────────────────────
		{Key: "codex_ja3", Label: "Codex JA3 覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=Chrome(默认，更安全)。设置则 sidecar 尝试重放该 JA3。", boot: func(c config.Config) interface{} { return c.CodexJA3Override }},
		{Key: "claude_ja3", Label: "Claude JA3 覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=Chrome(默认)。真实 claude-cli/Node JA3 为显式 opt-in。", boot: func(c config.Config) interface{} { return c.ClaudeJA3Override }},
		{Key: "codex_cli_version", Label: "Codex CLI 版本覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=内置默认。覆盖上游 Codex 客户端版本指纹。", boot: func(c config.Config) interface{} { return c.CodexCLIVersionOverride }},
		{Key: "claude_cli_version", Label: "Claude CLI 版本覆盖", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "留空=内置默认。覆盖上游 claude-cli 版本指纹。", boot: func(c config.Config) interface{} { return c.ClaudeCLIVersionOverride }},
		{Key: "claude_node_version", Label: "Claude Node 版本", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "X-Stainless-Runtime-Version 上报的 Node 版本。", boot: func(c config.Config) interface{} { return c.ClaudeNodeVersion }},
		{Key: "claude_stainless_version", Label: "Claude Stainless 版本", Category: catIdentity, Type: fieldString, Effect: effectUpstream,
			Help: "X-Stainless-Package-Version 上报的 SDK 版本。", boot: func(c config.Config) interface{} { return c.ClaudeStainlessVersion }},
		{Key: "claude_force_direct", Label: "Claude 强制直连", Category: catIdentity, Type: fieldBool, Effect: effectUpstream,
			Help: "开=即使账号绑定 sidecar 出口也走直连(部署无法运行 sidecar 时用)。", boot: func(c config.Config) interface{} { return c.ClaudeForceDirect }},
		{Key: "identity_os_source", Label: "身份 OS 来源", Category: catIdentity, Type: fieldSelect, Effect: effectHot,
			Options: []string{"vps", "downstream", "diverse"},
			Help:    "vps=与主机一致(随出口IP多样而多样)；downstream=按请求体推断；diverse=始终跨OS池。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.IdentityOSSource, "vps") }},
		{Key: "codex_identity_scrub", Label: "Codex 身份擦除", Category: catIdentity, Type: fieldBool, Effect: effectHot,
			Help: "开=按敏感词擦洗 Codex 请求体与响应流。", boot: func(c config.Config) interface{} { return c.CodexIdentityScrub }},
		{Key: "codex_prefer_sidecar_ja3_over_ws", Label: "Codex 优先 sidecar JA3", Category: catIdentity, Type: fieldBool, Effect: effectHot,
			Help: "开(默认)=sidecar 出口的 Codex 账号走 SSE 路径(保真 JA3)而非 WebSocket。", boot: func(c config.Config) interface{} { return c.CodexPreferSidecarJA3OverWS }},

		// ── 行为 / 缓存 ────────────────────────────────────────────────────────
		{Key: "conversation_isolation", Label: "会话隔离 (串号隔离)", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=每账号独立命名会话标识，避免串号导致的跨账号 401 级联。", boot: func(c config.Config) interface{} { return c.ConversationIsolation }},
		{Key: "claude_cache_control_inject", Label: "Claude 缓存注入", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=在 OpenAI 兼容→Claude 路径自动注入 cache_control。", boot: func(c config.Config) interface{} { return c.ClaudeCacheControlInject }},
		{Key: "claude_cache_affinity_policy", Label: "Claude 缓存亲和策略", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"legacy", "balanced"},
			Help:    "legacy=旧路由；balanced=优先 Claude session 与稳定前缀，减少粗路由缓存写入。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheAffinityPolicy, "balanced") }},
		{Key: "claude_cache_breakpoint_policy", Label: "Claude 缓存断点策略", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"legacy", "aggressive", "balanced", "coarse_safe"},
			Help:    "legacy=旧注入；aggressive=质量不变前提下尽量写入缓存；balanced=粗路由自动降级；coarse_safe=仅标记 tools 与非 billing system。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheBreakpointPolicy, "aggressive") }},
		{Key: "claude_cache_optimization_rollout", Label: "Claude 缓存灰度 JSON", Category: catBehavior, Type: fieldString, Effect: effectHot,
			Help: "JSON 灰度范围：groups/api_key_hash_prefixes/percent，断点还支持 account_ids。{}=全量；不支持按具体 Claude 模型灰度。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.ClaudeCacheOptimizationRollout, "{}") }},
		{Key: "claude_native_cache_breakpoint_inject", Label: "Claude 原生缓存断点", Category: catBehavior, Type: fieldBool, Effect: effectHot,
			Help: "开=对已识别的 Claude Code auto-context 前缀保守补充 cache_control 断点。", boot: func(c config.Config) interface{} { return c.ClaudeNativeCacheBreakpointInject }},
		{Key: "claude_cch_signing", Label: "Claude CCH 签名", Category: catBehavior, Type: fieldBool, Effect: effectUpstream,
			Help: "开=OAuth Claude 请求按最终 body 生成确定性 cch。", boot: func(c config.Config) interface{} { return c.ClaudeCCHSigning }},
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
			Help: "开=/file 脚本默认 POOL_STRICT_LINUX=1；关=默认 0，仍可由环境变量覆盖。", boot: func(c config.Config) interface{} { return c.ClaudeGatewayStrictLinuxDefault }},
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
		{Key: "stream_failover_hold_memory_bytes", Label: "流式无痕内存上限", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "流式响应为隐藏上游错误而暂存的内存字节上限；默认 8MiB。超出后若未配置磁盘兜底则换号。", boot: func(c config.Config) interface{} { return int(c.StreamFailoverHoldMemoryBytes) }},
		{Key: "stream_failover_hold_disk_bytes", Label: "流式无痕磁盘上限", Category: catLimits, Type: fieldInt, Effect: effectHot,
			Help: "流式暂存允许落盘的最大字节数；默认 0=完全不落盘。超出后换号，不继续占磁盘。", boot: func(c config.Config) interface{} { return int(c.StreamFailoverHoldDiskBytes) }},
		{Key: "stream_failover_hold_temp_dir", Label: "流式暂存目录", Category: catLimits, Type: fieldString, Effect: effectHot,
			Help: "可选落盘目录；磁盘上限为 0 时忽略。可指向 tmpfs 或更大分区。", boot: func(c config.Config) interface{} { return c.StreamFailoverHoldTempDir }},
		{Key: "sticky_wait_millis", Label: "Sticky 等待毫秒", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "同会话绑定账号暂不可用时短暂等待的毫秒数。", boot: func(c config.Config) interface{} { return c.StickyWaitMillis }},
		{Key: "account_token_budget", Label: "账号并发 Token 预算", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "同账号已有在途请求时允许叠加的估算输入 token 上限。", boot: func(c config.Config) interface{} { return int(c.AccountTokenBudget) }},
		{Key: "strict_sticky_max_cooldown_seconds", Label: "严格 Sticky 冷却阈值", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "严格绑定账号冷却超过该秒数时允许换号；0=永不因长冷却换号。", boot: func(c config.Config) interface{} { return c.StrictStickyMaxCooldownSeconds }},
		{Key: "cooldown_wait_max_seconds", Label: "短冷却等待秒数", Category: catLimits, Type: fieldInt, Effect: effectScheduler,
			Help: "全组账号都在冷却时最多等待最短冷却的秒数；0=不等待。", boot: func(c config.Config) interface{} { return c.CooldownWaitMaxSeconds }},
		{Key: "force_failover_on_429", Label: "429 强制换号", Category: catLimits, Type: fieldBool, Effect: effectHot,
			Help: "开=即使带服务端状态(previous_response_id)的请求遇 429 也尝试换号(自包含请求默认即会换号)。", boot: func(c config.Config) interface{} { return c.ForceFailoverOn429 }},
		{Key: "codex_prompt_cache_retention", Label: "Codex 提示缓存保留", Category: catBehavior, Type: fieldSelect, Effect: effectHot,
			Options: []string{"24h", "in_memory", ""},
			Help:    "24h=扩展缓存(gpt-5.x/4.1 命中率最高)，in_memory=~5-10分钟，空=不注入。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.CodexPromptCacheRetention, "24h") }},

		// ── 注册 / 引擎 ───────────────────────────────────────────────────────
		{Key: "default_register_method", Label: "默认注册引擎", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"node", "protocol_v2", "browser_v3", "protocol", "browser"},
			Help:    "node=移植的 puppeteer-real-browser 注册机(每任务独立 IP/指纹/Cookie 清理)。触发注册未指定引擎时使用。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.DefaultRegisterMethod, "node") }},
		{Key: "registration_egress_pool_id", Label: "默认注册池", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "注册任务发起阶段使用的默认注册代理池。注册成功的账号仍默认直连，账号详情页单独修改运行出口。", boot: func(c config.Config) interface{} { return c.RegistrationEgressPoolID }},
		{Key: "registration_concurrency", Label: "注册并发上限", Category: catReg, Type: fieldInt, Effect: effectHot,
			Help: "单批最多并行的注册数(每个浏览器独立隔离)。仅在轮换出口上提高，固定 IP 出口保持 1。", boot: func(c config.Config) interface{} { return c.RegistrationConcurrency }},
		{Key: "codex_install_model", Label: "Codex 安装默认模型", Category: catReg, Type: fieldString, Effect: effectHot,
			Help: "一键脚本写入 config.toml 的 model（默认 gpt-5.5）。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.CodexInstallModel, "gpt-5.5") }},
		{Key: "codex_install_effort", Label: "Codex 安装推理强度", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"xhigh", "high", "medium", "low", "minimal", ""},
			Help:    "写入 model_reasoning_effort（默认 xhigh）。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.CodexInstallEffort, "xhigh") }},
		{Key: "codex_install_approval_policy", Label: "Codex 审批策略", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"never", "on-request", "on-failure", "untrusted", ""},
			Help:    "never=全自动审批(目标模式)。与沙箱共同决定全自动程度。", boot: func(c config.Config) interface{} { return firstNonEmpty(c.CodexInstallApprovalPolicy, "never") }},
		{Key: "codex_install_sandbox_mode", Label: "Codex 沙箱模式", Category: catReg, Type: fieldSelect, Effect: effectHot,
			Options: []string{"danger-full-access", "workspace-write", "read-only", ""},
			Help:    "danger-full-access=完整权限(目标模式)。保守可改 workspace-write。", boot: func(c config.Config) interface{} {
				return firstNonEmpty(c.CodexInstallSandboxMode, "danger-full-access")
			}},

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

		// ── 引导（需重启）─────────────────────────────────────────────────────
		{Key: "listen_addr", Label: "监听地址", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "进程监听地址，改后需重启。", boot: func(c config.Config) interface{} { return c.ListenAddr }},
		{Key: "database_path", Label: "数据库路径", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "SQLite 路径，改后需重启。", boot: func(c config.Config) interface{} { return c.DatabasePath }},
		{Key: "default_sidecar_chain_proxy", Label: "Sidecar 链式代理", Category: catBoot, Type: fieldString, Effect: effectRestart,
			Help: "curl_cffi sidecar 出口链接的上游代理(仅本地测试需要，如 http://127.0.0.1:7897)。VPS 上必须留空。改后需重启。", boot: func(c config.Config) interface{} { return c.DefaultSidecarChainProxy }},
		{Key: "max_concurrent_upstream", Label: "最大并发上游", Category: catBoot, Type: fieldInt, Effect: effectRestart,
			Help: "全局在途上游请求上限(信号量在启动时构建)，改后需重启。", boot: func(c config.Config) interface{} { return c.MaxConcurrentUpstream }},
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
	c.CodexJA3Override = s.settingString(ctx, "codex_ja3", c.CodexJA3Override)
	c.ClaudeJA3Override = s.settingString(ctx, "claude_ja3", c.ClaudeJA3Override)
	c.ClaudeForceDirect = s.flagEnabled(ctx, "claude_force_direct", c.ClaudeForceDirect)
	c.CodexCLIVersionOverride = s.settingString(ctx, "codex_cli_version", c.CodexCLIVersionOverride)
	c.ClaudeCLIVersionOverride = s.settingString(ctx, "claude_cli_version", c.ClaudeCLIVersionOverride)
	c.ClaudeNodeVersion = s.settingString(ctx, "claude_node_version", c.ClaudeNodeVersion)
	c.ClaudeStainlessVersion = s.settingString(ctx, "claude_stainless_version", c.ClaudeStainlessVersion)
	c.ClaudeCCHSigning = s.flagEnabled(ctx, "claude_cch_signing", c.ClaudeCCHSigning)
	return c
}

// effectiveSchedulerConfig builds the scheduler's live selection config from boot
// defaults plus DB overrides. It intentionally covers only scheduler-owned knobs;
// bootstrap-only limits such as max_concurrent_upstream still require restart.
func (s *Server) effectiveSchedulerConfig(ctx context.Context) config.Config {
	c := s.cfg
	c.StickyWaitMillis = s.settingInt(ctx, "sticky_wait_millis", c.StickyWaitMillis)
	c.AccountTokenBudget = s.settingInt64(ctx, "account_token_budget", c.AccountTokenBudget)
	c.StrictStickyMaxCooldownSeconds = s.settingInt(ctx, "strict_sticky_max_cooldown_seconds", c.StrictStickyMaxCooldownSeconds)
	c.CooldownWaitMaxSeconds = s.settingInt(ctx, "cooldown_wait_max_seconds", c.CooldownWaitMaxSeconds)
	if c.StickyWaitMillis <= 0 {
		c.StickyWaitMillis = config.DefaultStickyWaitMillis
	}
	if c.AccountTokenBudget <= 0 {
		c.AccountTokenBudget = config.DefaultAccountTokenBudget
	}
	if c.StrictStickyMaxCooldownSeconds < 0 {
		c.StrictStickyMaxCooldownSeconds = 0
	}
	if c.CooldownWaitMaxSeconds < 0 {
		c.CooldownWaitMaxSeconds = 0
	}
	return c
}

// settingsViewJSON renders the registry with each field's current effective value and
// whether a DB override is in place (so the UI can show "已覆盖 / 默认").
func (s *Server) settingsViewJSON(ctx context.Context) []map[string]interface{} {
	fields := configFields()
	out := make([]map[string]interface{}, 0, len(fields))
	for _, f := range fields {
		value, overridden, err := s.configFieldResolvedValue(ctx, f)
		settingsError := ""
		if err != nil {
			settingsError = err.Error()
		}
		opts := f.Options
		if opts == nil {
			opts = []string{}
		}
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
	if err := s.store.SetSettings(ctx, updates); err != nil {
		return false, err
	}
	if changedUpstream {
		s.upstream.UpdateConfig(s.effectiveUpstreamConfig(ctx))
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
	case "sticky_wait_millis", "account_token_budget":
		raw, err := validateIntegerSetting(v)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		if n <= 0 {
			return "", fmt.Errorf("must be greater than 0")
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
