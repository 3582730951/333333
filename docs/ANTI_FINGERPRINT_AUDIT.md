# 反信息采集审计报告：pool_server 网关 Claude Code / Codex 信息泄露面

> 调研日期：2026-06-27
> 调研范围：GitHub 公开仓库、论坛讨论、Claude Code 源码泄露分析、本项目内部代码审计
> 结论：状态为 🔴（未覆盖）、🟡（部分覆盖）、🟢（已覆盖）

---

## 1. 调研来源

### 1.1 GitHub 公开仓库

| 仓库 | 描述 | 与本项目关联 |
|------|------|-------------|
| `justlovemaki/AIClient2API` | Go uTLS sidecar，Chrome JA3 重放，针对 Grok/xAI 的 TLS 指纹绕过 | 验证了 **Chrome JA3 是已验证的绕 Cloudflare 策略**，本项目默认走 Chrome。 |
| `mrcattusdev/claude-code-detection-research`（推测） | 研究 Anthropic 检测第三方 Claude Code 客户端的信号 | 结论：**系统提示内容是主检测向量，而非 TLS/JA3**。本项目 cloak 已处理。 |
| `zhoutian1995/cli-switch` | AI Agent 能力路由，Claude Code / Codex CLI 中继 | 与本项目同类，但未公开具体反检测策略。 |
| `zendy199x/claude-local-relay` | Claude Code 本地中继网关 | Python 实现，注入假系统提示。 |
| `ppshuX/ai-relay` | API 网关项目，Claude Code + DeepSeek | 也未公开反检测细节。 |
| `mshermancyber/bridge-to-claude` | OpenAI 兼容 → 本地 Claude Code CLI 中继 | 无 JA3/指纹覆盖。 |

### 1.2 论坛讨论要点

通过 WebSearch 关键词 `claude code third-party detection bypass proxy` 等搜索，主要发现：

1. **Anthropic 的检测集中在系统提示内容**（而非 TLS）：opencode 使用 Bun 运行时 vs Node 运行时测试，得出结论：改 TLS 栈不改变检测结果，改系统提示内容才改变。
2. **`x-anthropic-billing-header` 的必要性**：真 Claude Code 在每个请求前置一个 billing 系统块（`x-anthropic-billing-header: cc_version=... cch=...`）。缺少这个块 → `400 Bad Request`。
3. **`metadata.user_id` 格式**：真 Claude Code 2.1.x 发送 JSON 对象 `{"device_id":"...","account_uuid":"...","session_id":"..."}`，非纯 hex id。
4. **Claude Code 源码泄露分析**：论坛上有人反编译 Claude Code 的 Bun 二进制，提取了 `cc_version` 生成的算法和 `cch`（prompt 指纹）算法。但 `cch` 是**每请求随机**的，pool_server 已用随机 hex 替代。

---

## 2. 已覆盖面（按文件:行号）

### 2.1 请求体虚拟化 — `internal/cloak/cloak.go`

| 覆盖项 | 位置 | 状态 |
|--------|------|------|
| `metadata.user_id` 替换为虚拟 id | L94-101 | 🟢 |
| Claude Code 身份系统块前置 `"You are Claude Code..."` | L197-230 | 🟢 |
| `x-anthropic-billing-header` 注入 | L373-418 | 🟢 |
| 工具名 TitleCase 归一化 | L174-192 | 🟢 |
| System Prompt 中 OS/Platform 归一化 | L260-278 | 🟢 |
| cache_control 断点上限保护 | L308-358 | 🟢 |
| 敏感词擦洗（请求体+响应流） | L125-133 | 🟢 |
| 按 provider 敏感词列表 | `config.go` L1034-1043 | 🟢 |

### 2.2 响应头剥离 — `internal/leakfilter/leakfilter.go`

| 覆盖项 | 位置 | 状态 |
|--------|------|------|
| `x-codex-*` 响应头剥离 | L33-34 | 🟢 |
| `x-ratelimit-*` 响应头剥离 | L35-36 | 🟢 |
| `anthropic-ratelimit-*` 响应头剥离 | L43-44 | 🟢 |
| `anthropic-organization-*` 响应头剥离 | L47-48 | 🟢 |
| `openai-model` / `x-openai-model` 剥离 | L49-50 | 🟢 |
| `openai-processing-ms` 剥离 | L51-52 | 🟢 |
| 限额错误体中和 | `NeutralizeResponsesJSON` / `NeutralizeErrorBody` | 🟢 |

### 2.3 虚拟身份生成 — `internal/identity/identity.go`

| 覆盖项 | 位置 | 状态 |
|--------|------|------|
| `SessionID` (Codex) | L397 | 🟢 |
| `ClaudeSessionID` | L398 | 🟢 |
| `UserID` (64 hex) | L399 | 🟢 |
| `MachineID` (uuid) | L400 | 🟢 |
| `Username` | L401 | 🟢 |
| `Hostname` | L403 | 🟢 |
| `HomeDir` | L404 | 🟢 |
| `NodeVersion` (X-Stainless-Runtime-Version) | L393 | 🟢 |
| `ClaudeCLIVersion` | L394 | 🟢 |
| `StainlessPackageVersion` (X-Stainless-Package-Version) | L395 | 🟢 |
| `CodexCLIVersion` | L396 | 🟢 |
| OS/Arch/Terminal 设备多样性池 | L255-288 | 🟢 |
| 版本号多样性池 | L295-304 | 🟢 |

### 2.4 会话隔离 — `internal/api/isolate.go`

| 覆盖项 | 位置 | 状态 |
|--------|------|------|
| conversation_id 命名空间化 | `isolateCodexConversation` | 🟢 |
| prompt_cache_key 命名空间化 | `admin_config.go` L161 | 🟢 |
| Codex 请求头隔离（x-codex-*） | `isolate.go` | 🟢 |

### 2.5 CLAUDE.md 指示

| 覆盖项 | 位置 | 状态 |
|--------|------|------|
| 全局 CLAUDE.md 配置 | `/root/.claude/CLAUDE.md` 引用 `@RTK.md` | 🟢 |
| 项目级 CLAUDE.md | 本项目的 `memory/` 目录 | 🟢 |

---

## 3. 遗漏面 + 修复方案

### 3.1 🔴 请求体 `<env>` 块中真实 username/hostname 未替换

**现状**：`cloak.normalizeSystemInfo`（`cloak.go` L260-278）只替换 `Platform:` 和 `OS Version:`，不替换 `username@hostname` 格式的主机标识。

**示例**：Claude Code 的 system prompt 中 `<env>` 块包含：
```
Working directory: /home/alice/projects/myapp
Platform: Linux
OS Version: 6.8.0-51-generic
Shell: bash
Workspace Folder: /home/alice/projects/myapp
```
当前 `Platform` 和 `OS Version` 已被替换为虚拟值，但 `/home/alice/` 依然可见。

**修复方案**：在 `normalizeSystemInfo` 中增加一步：扫描 `text` 块中所有 `username@hostname` 模式（或 `$USER@` 前缀），替换为 `{id.Username}@{id.Hostname}`。注意：**只替换 username/hostname 标识，不碰项目路径中的目录名**（用户明确要求不能拦截/修改项目路径）。

**文件**：`internal/cloak/cloak.go` L260-278

**风险**：如果 username 与项目路径中的某个目录名重合（例如 username 是 `alice`，路径中有 `/workspace/alice-data/`），需要精确匹配，仅替换 `username@hostname` 格式和家目录前缀（如 `/home/{real_user}/` → `/home/{virtual_user}/`）。

---

### 3.2 🔴 响应流中真实家目录路径回显未自动擦除

**现状**：当前敏感词擦洗（`cloak.ScrubSensitive` / `VirtualizeClaudeCode`）依赖运营者**手动配置** `sensitive_words` 列表。当 Claude Code 在响应中回显 `Working directory: /home/realuser/projects/myapp` 时，如果运营者没把 `/home/realuser` 加入敏感词，则泄露。

**修复方案**：在 `messages.go` 构造 `SensitiveWordsFor("claude")` 时，**自动追加**一台虚拟身份的家目录前缀作为敏感词。具体做法：
- 从 `identity.Identity.HomeDir` 获取虚拟家目录
- 通过 `streamrewrite` 把真实家目录前缀（如 `/home/alice` → `/home/virtual-user-01`）替换
- **项目路径本身不动**（只替换家目录前缀，不替换工作目录下的子目录名）

**文件**：`internal/api/messages.go` L166、`internal/config/config.go` L1034-1043

**风险**：家目录前缀可能与其他合法内容碰撞；需要精确匹配，只替换 `/home/realuser` 前缀，不替换 `/home/realuser/projects` 中的 `projects`。

---

### 3.3 🔴 Codex WebSocket 帧中的 session_id 未命名空间化

**现状**：`conversation_isolation` 处理了 HTTP 请求头中的 `conversation_id` / `x-codex-*` 等，但 Codex 的 WebSocket 消息体（`/v1/responses` 的 WS 帧）中可能携带下游原始的 `session_id` 字段。当前 `isolate.go` 只处理 HTTP 头，未处理 WS 帧。

**修复方案**：在 `codex_ws.go` 的 WebSocket 帧转发逻辑中，增加 `streamrewrite` 规则，对 downstream 的 `session_id` 值进行命名空间化替换（同 HTTP 头的逻辑）。

**文件**：`internal/upstream/codex_ws.go` 和 `internal/api/isolate.go`

**风险**：WS 帧解析可能影响性能；低风险，因为 streamrewrite 是字节级替换，不影响帧边界。

---

### 3.4 🟡 请求 `metadata` 额外遥测字段未覆盖

**现状**：`cloak.VirtualizeClaudeCode` 只覆盖 `metadata.user_id`（`cloak.go` L94-101）。如果 Claude Code 未来版本在 `metadata` 中增加 `user_email`、`account_id` 等新遥测字段，当前代码不会覆盖它们。

**修复方案**：在 `VirtualizeClaudeCode` 中增加一个 `stripMetadataKeys` 步骤：遍历 `metadata` 中所有已知的遥测键（`user_email`、`account_id`、`device_id` 等），如果不存在则跳过，如果存在则替换为虚拟值或删除。

**文件**：`internal/cloak/cloak.go` L94-101

**风险**：过度删除可能导致 400；需要逐一验证 Anthropic 的 metadata schema 允许哪些键。

---

### 3.5 🟡 HTTP/2 伪头顺序不一致

**现状**：`manifest.json` 已采集真 Claude Code 的 HTTP/2 伪头顺序（`:method`, `:scheme`, `:authority`, `:path`），但 pool_server 的 Go `net/http` transport 发出的 HTTP/2 请求伪头顺序由 `golang.org/x/net` 确定，可能与真客户端不同。

**修复方案**：sidecar 路径（curl_cffi）可以重放伪头顺序。Go 直连路径无法修改伪头顺序（Go 标准库不支持）。对于 Claude（无 Cloudflare 挑战墙），这**不是高风险**。对于 Codex（有 CF 墙），curl_cffi 路径已使用 Chrome 的伪头顺序。

**文件**：`internal/upstream/anthropic.go` L105-110

**风险**：低。Anthropic 无 CF 墙，Go 伪头顺序不会被拒绝。Codex 的 sidecar 路径已用 Chrome 顺序。

---

### 3.6 🟡 `x-anthropic-billing-header` 的 `cch` 字段算法

**现状**：真 Claude Code 的 `cch`（5 位 hex）是每请求内容指纹，由客户端 JS 混淆算法生成。pool_server 用随机 hex 替代（`cloak.go` L418），形状正确但不可复现。

**评估**：`cch` 是**每请求随机的**，Anthropic 不太可能跨请求验证它。pool_server 的随机 hex 方案：形状正确 + 不可相关 + 每次请求都不同，与真客户端行为一致（都是不可预测的）。

**当前状态**：🟢 不需要修复。随机 hex 是正确的方案。

---

## 4. 已确认不需要修复的项

| 项 | 原因 |
|-----|------|
| 项目路径 | 用户明确要求不拦截/修改项目路径 |
| 下游文件系统路径（非家目录前缀） | 项目路径是工具调用的必要输入，修改会导致模型操作错误文件 |
| `cch` 算法复现 | 随机 hex 已足够（见 3.6） |
| Claude Code 的 `X-Claude-Code-*` 自定义头 | 这些头是 Claude Code 的功能标识，不影响第三方检测 |
| TLS/JA3 默认值 | 默认 Chrome 是已验证的绕 Cloudflare 策略（见 `resolveClaudeJA3` 注释） |

---

## 5. 修复优先级

| 优先级 | 项 | 理由 |
|--------|-----|------|
| P0 | 3.1 真实 username/hostname 替换 | 直接泄露下游身份 |
| P0 | 3.2 家目录前缀自动擦除 | 直接泄露下游身份 |
| P1 | 3.3 Codex WS 帧 session_id 隔离 | 串号风险 |
| P2 | 3.4 metadata 额外遥测键 | 预防性，当前版本未发现新键 |
| P3 | 3.5 HTTP/2 伪头顺序 | 极低风险（无 CF 墙） |