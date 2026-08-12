# Codex Account Pool Server

独立账号池网关实现，不修改 `other_codex`、`other_cpa`、`other_Codex_Manager` 参考源码。

项目目录约定见 `docs/PROJECT_STRUCTURE.md`。

## 功能覆盖

- OpenAI/Codex 兼容入口：
  - `GET /v1/models`
  - `POST /v1/responses`
  - `POST /v1/responses/compact`
  - `POST /v1/chat/completions`（含工具调用完整透传，第三方客户端 Cline/Roo/opencode/Cursor 可直接接入）
- Anthropic/Claude 原生入口：
  - `POST /v1/messages`、`POST /v1/messages/count_tokens`（账号绑定身份虚拟化 + 敏感词擦除 + 官方 Claude Code 指纹）
  - **Claude Code → Codex/GPT 原生桥**：当 `/v1/messages` 的 `model` 为 `gpt-*`/`codex-*` 时，直接执行 Anthropic Messages ↔ Codex Responses 转换并路由到内置 Codex 账号池，不经过 Chat Completions 中间格式；保留 typed 文本/图片/文件、并行函数工具、tool result/error、Responses WebSocket/SSE、cache usage，以及可跨 Claude Code 多轮回放的 encrypted reasoning signature。
  - **Skills / code-execution 透传**：`/v1/files`（含 multipart 上传/下载）、`/v1/skills`、`/v1/agents`、`/v1/environments`、`/v1/sessions`（及其子路径）。这些不是消息轮次，**body 原样透传不改写**（保留客户端自己的 `Content-Type`/`Anthropic-Beta`/`Anthropic-Version`），仅附加账号鉴权 + Claude Code 身份头并走账号出口（含 sidecar JA3 / WARP）。解决「无法访问官方 skills」（端点 404）。同一下游 key 的透传请求会黏在同一账号上，使 `file_id` 等账号作用域资源在其生命周期内一致。
- SQLite WAL 存储，初始化默认 `cyber` 分组，系统提示词为空。
- 官方 ChatGPT Codex upstream 默认：`https://chatgpt.com/backend-api/codex`。
- **Codex 官方 Skills 兼容分层**：Tier 1 为官方 Codex 账号通道（完整官方 CLI skills / plugins / Browser Use / Responses 新字段优先兼容）；Tier 2 为第三方原生 Responses 供应商（原生透明转发，云插件 best-effort）；Tier 3 为第三方 Chat Completions 桥接（支持 function、namespace、custom 与客户端 tool-search；无法执行的 hosted/server tools 会删除并通过 `X-Pool-Compatibility-Losses` 及用量诊断显式报告）。诊断：`GET /admin/compat/skills`。
- 导入官方 `auth.json`、CPA 顶层凭据 JSON、ChatGPT `/api/auth/session` JSON、auth.json 数组及 other_sub2api `sub2api-data` v1 备份；无 `refresh_token` 的 Web session 按官方 Codex `chatgptAuthTokens` 语义保存，实际转发只使用真实 `accessToken`，可选 Session Cookie 加密保存用于续期；Agent Identity 私钥同样加密保存，并支持 task 自动注册/失效恢复。
- 空 prompt raw fast path：普通 responses 在不需要注入和不需要 Virtual 2M 时保持原始 body。
- affinity/sticky 路由：parent thread、thread/conversation、window、prompt_cache_key、turn metadata、下游 key/project/model、稳定消息 hash。
- strict sticky：compact、`previous_response_id`、`x-codex-turn-state`、`compaction_trigger`、tool-result continuation 不跨账号。
- 并发默认无固定数量上限：`account_token_budget=0` 与 egress `max_concurrency=0` 表示不设硬上限；接近 CPU、内存或 FD 安全线时只做无损排队。`max_concurrent_upstream` 仅保留旧配置兼容，不参与调度。
- 每账号模型窗口探测，`/v1/models` 根据能力返回 `native_2m` 或 `virtual_2m`。
- Virtual/Pseudo 2M ledger + materialization。
- Prompt cache 相关字段稳定透传，不降模型、不降 reasoning。
- **多出口 WARP CF 阶梯**（见下 "CF 防护" 节）：CF 检测已修复误判，撞 CF 自动丢入 WARP 备用出口（≤3账号/出口），出口自身被 CF 时自动调用 cf_clearance 解题器并重注册新 IP，全程不停服。
- Sidecar 代理链（`chain_proxy`）：`curl_cffi_sidecar` 出口现支持 `chain_proxy` 字段，同时保持真实 Codex/Claude JA3 **并**从 WARP/代理 IP 出口，两者兼得。
- 分组默认出口 (`default_egress_id`)：新导入账号自动继承所在分组的默认出口（不再强制 `egress_direct`）。
- 批量分配出口 `POST /admin/groups/<name>/assign-egress`：一次调用把整个分组的账号切换到代理/WARP 出口。
- L3 浏览器 repair/bootstrap：可向 `/admin/accounts/<id>/browser-repair` 注入隔离 cookie jar。
- Egress 类型：`direct`、`http_proxy`、`https_proxy`、`socks5h_proxy`、`socks5_proxy`、`warp_proxy`、`curl_cffi_sidecar`（+`chain_proxy`）。
- 管理面：账号导入、probe、refresh、egress、CF 事件、group prompt。
- 网页 OAuth 登录导入：管理端一键生成 OpenAI/Codex 与 Claude（Pro/Max）的登录链接，浏览器登录后把回调网址/授权码粘贴回来即可入池（PKCE，凭据只在服务端落库）。
- 租户、用户、项目、账号生命周期管理：`/admin/tenants`、`/admin/users`、`/admin/projects`、`/admin/accounts/:id/disable|enable|delete`。
- 下游响应过滤内部 header，避免泄露账号 ID、egress ID、pool/sidecar 私有 header。

## 多供应商（OpenAI / Responses / Anthropic 兼容）

- 通用「自定义供应商」框架：任何 **OpenAI Chat-Completions 兼容**、**OpenAI Responses 原生**或 **Anthropic Messages** 上游都能接入。新数据库不预置第三方供应商；升级只清理从未编辑且没有账号引用的旧版精确示例，用户配置保持不变。
- 每个供应商显式声明 `upstream_protocol`：默认 `chat_completions`（Tier 3，Responses→Chat 桥接，支持稳定版 function/namespace/custom/client tool-search，兼容性损失显式报告）；可选 `responses`（Tier 2，`/v1/responses` 原生透明转发，保留 typed tools、`include`、`previous_response_id` 与未来字段/事件）。
- 一个供应商可为 `/v1/chat/completions`、`/v1/responses`、`/v1/messages` 和其他资源路径分别配置 Base URL、协议与传输画像；精确路径优先于 `*` 回退。每条路径的粘性会话和 Cookie jar 均按下游身份隔离。
- 模型可**自动发现**（探测 `{base_url}/models` 并回写）或手动维护；管理端「供应商」页用普通表单新增，不依赖厂商预设。
- REST:`GET/POST /admin/providers`、`DELETE /admin/providers/{id}`、`POST /admin/accounts/import-key`(裸 API Key 入池)。

## Web 控制台 / 多用户门户

管理前端由固定版本的 Node 工具链构建，并作为确定性静态产物嵌入 Go 二进制：

- **new-api 风格布局**:可折叠侧栏(用户端 / 管理端分组)+ 顶栏(主题切换、语言切换、用户菜单),**浅色 / 深色双主题** + **中文 / English 双语**(偏好持久化)。
- **真多用户登录**:终端用户可**注册 / 登录 / 登出**(`/auth/register|login|logout|me`),首个注册用户自动成为管理员;PBKDF2(stdlib,无新依赖)口令 + `user_sessions` 会话(HttpOnly Cookie、SameSite=Lax、TLS 下 Secure)+ **双提交 CSRF** + 登录限流。旧的单一 `admin_token`(Bearer)仍然兼容(`/auth/me` 会以 `via=admin_token`/`open` 标识)。
- **用户端**(`/user/*`,会话鉴权、按属主隔离):「我的密钥」自助创建/增删 `cap_` Key(明文仅显示一次)、「我的用量」(按用户归属的 token 用量 + 按模型汇总)、「模型广场」、「我的设置」(改昵称/改密码/主题/语言)。
- **管理端**:概览、账号、供应商、出口、用量、隔离/会话、分组、密钥、**用户管理**(改角色/封禁/重置密码/删除,带自锁保护)、CF、审计、注册与设置。
- **按用户用量归属**:下游 Key 绑定到创建它的用户,每条 `usage_records` 记录 `user_id`/`api_key_hash`,用户端只看自己的用量、管理端看全局。

## CF 防护（WARP 多出口阶梯）

chatgpt.com 位于 Cloudflare 后，同一 VPS IP 一旦被标记，所有账号都会被 cooling — 这是「动不动就冷却」的根因。新版实现了完整阶梯：

```
直连/sidecar → WARP 出口（≤3账号/出口） → cf_clearance 解题器 → 重注册换新 IP → 隔离
```

**原理：**
- `cf.Detect` 修复了误判（JSON 错误正文里含 "cloudflare"/"captcha" 不再触发 CF）。
- 首次撞 CF → `StormBreaker` 记录事件 → **自动**将账号丢入一个 WARP 备用出口（`scheduler.selectEgress` 走 standby → 同一请求重试，下游无感知）。
- 每个 WARP 出口最多绑 3 个账号（`warp_accounts_per_exit`），爆炸半径小。
- 出口自身被 CF 标记 → 先调 cf_clearance 解题器（FlareSolverr/Byparr/Solvearr，同一 WARP 出口 IP 求解，cookie+UA+IP 一起入库）；解题失败 → 重注册 wgcf profile 换新 WARP IP → 更新 `exit_ip` + 清 cooldown。
- `curl_cffi_sidecar` + `chain_proxy=<warp socks>` 同时保持 Codex/Claude JA3 **并**走 WARP 出口 IP。

**安装（一键完整安装含 WARP）：**
```bash
sudo scripts/install.sh --with-warp --warp-exits 8
# 验证出口（每个出口 IP 应不同）
for i in $(seq 1 8); do
  ip=$(curl -s --max-time 10 -x socks5h://127.0.0.1:$((40000+i-1)) https://ipapi.co/ip)
  echo "exit-${i}: ${ip}"
done
```

**配置（config.json）：**
```jsonc
{
  "warp_enabled": true,
  "warp_exit_count": 8,
  "warp_exit_base_port": 40000,
  "warp_accounts_per_exit": 3,
  "warp_exit_script": "/var/lib/codex-pool/warp/warp-exit.sh",
  "cf_solver_enabled": true,
  "cf_solver_url": "http://127.0.0.1:8191"
}
```

**升级 CF 求解器（已对齐 FlareSolverr v3.5.0）：**

v3.5.0 包含新的 Turnstile 处理修复。本项目现在优先使用
`sessions.create → request.get → sessions.destroy`，把代理（含账号密码）、浏览器实例、
cookie 和出口固定在同一会话中；旧的 FlareSolverr-compatible 实现不支持 session 时会自动
退回 stateless `request.get`。推荐固定版本标签，不使用漂移的 `latest`：

```bash
# Docker Compose 部署
docker compose pull flaresolverr
docker compose up -d --no-deps flaresolverr

# Docker CLI 部署（首次安装或按原参数重建）
docker pull ghcr.io/flaresolverr/flaresolverr:v3.5.0
docker rm -f flaresolverr 2>/dev/null || true
docker run -d --name flaresolverr \
  -p 127.0.0.1:8191:8191 \
  -e LOG_LEVEL=info \
  --restart unless-stopped \
  ghcr.io/flaresolverr/flaresolverr:v3.5.0

# 健康检查；服务只绑定本机，pool 使用 http://127.0.0.1:8191
curl -fsS http://127.0.0.1:8191/health
```

浏览器拿到 `cf_clearance` 后，pool 会先用临时 jar 在**同一出口**发送真实
Codex/Claude 应用指纹的模型探测（Codex 会覆盖全部五个兼容版本）。只有这些探测确认不再是 CF challenge，cookie 才会进入
正式 jar/sidecar/数据库并清 cooldown；浏览器 UA 与 CLI UA 不可复用时会继续执行 WARP
换 IP，避免把浏览器 UA 混进 Codex/Claude 请求。

**手动操作（无 install.sh）：**
```bash
# 预置 8 个出口（需出网到 Cloudflare 注册 WARP）
WARP_DIR=/var/lib/codex-pool/warp bash scripts/warp-exit.sh provision 8
# 验证单个出口
bash scripts/warp-exit.sh verify 3
# 重注册出口（换新 IP）
bash scripts/warp-exit.sh reregister 3
```

**批量把整组账号切到 WARP：**
```bash
curl -X POST http://127.0.0.1:8787/admin/groups/cyber/assign-egress \
  -H 'content-type: application/json' \
  -d '{"standby_egress_ids":["warp-1","warp-2","warp-3"]}'
```


## 本地运行

```bash
cd pool_server
go mod tidy
go test ./...
go run ./cmd/pool-server --config config.example.json
```

导入账号：

```bash
curl -sS http://127.0.0.1:8787/admin/accounts/import-auth-json \
  -H 'content-type: application/json' \
  -d '{"label":"main","auth_json":{"OPENAI_API_KEY":"token","tokens":{"access_token":"token","refresh_token":"refresh","account_id":"chatgpt-account-id"}}}'
```

同一接口也可直接粘贴 `{"type":"sub2api-data","version":1,"proxies":[...],"accounts":[...]}`。OpenAI OAuth 账号逐条导入并隔离错误；备份中的 HTTP/SOCKS 出口会创建或复用本地 egress，再按 `proxy_key` 绑定账号。两套调度器语义不同，因此 `concurrency`、`priority`、`rate_multiplier` 等字段只返回可见警告，不会静默套用。

也可粘贴 CPA 格式 `{"type":"codex","access_token":"...","id_token":"...","account_id":"..."}`，或 ChatGPT Web session：

```json
{
  "user": {"id": "user-...", "email": "name@example.com"},
  "account": {"id": "workspace-...", "planType": "pro"},
  "expires": "2026-07-28T00:00:00Z",
  "accessToken": "eyJ...",
  "sessionToken": "<encrypted-session-token>"
}
```

缺少真实 `id_token` 时，导入器会生成仅承载 claims 的三段兼容 JWT；它不会被当作上游凭据。导入前会验证真实 `accessToken` 的 JWT 结构、`exp` 和 workspace ID，避免“能入池、调用才 401/403”。Web session 通常没有 `refresh_token`；可在请求外层同时传入 `"session_cookie":"完整 Cookie 头或 session-token 值"`，服务端会加密保存并在 bearer 失效后重新读取 `/api/auth/session`。未提供 Cookie 时必须在 access token 到期前重新导入。
原始 Web session 响应中的 `sessionToken` / `session_token` 会自动作为加密 session cookie 导入；请求外层显式提供的 `session_cookie` 优先。

探测模型：

```bash
curl -X POST http://127.0.0.1:8787/admin/accounts/<account_id>/probe-models
curl http://127.0.0.1:8787/v1/models
```

### 网页 OAuth 登录（推荐导入方式）

无需手动找 token：在管理 UI 的「导入账号」里选 **Codex · 网页登录** 或 **Claude · 网页登录**，
点「生成登录链接」→ 在浏览器打开并登录账号 → 登录成功后地址栏会变成回调网址（Claude 还会直接
显示一段授权码）→ 把整段网址或授权码粘贴回来 → 入池。等价的 REST 接口：

```bash
# 1) 生成登录链接（provider: codex | claude）
curl -sS http://127.0.0.1:8787/admin/oauth/start \
  -H 'content-type: application/json' -d '{"provider":"codex"}'
# -> {"session_id":"...","auth_url":"https://auth.openai.com/oauth/authorize?...","expires_in":900}

# 2) 浏览器打开 auth_url 登录后，把回调网址（或 code#state / 纯 code）连同 session_id 提交
curl -sS http://127.0.0.1:8787/admin/oauth/complete \
  -H 'content-type: application/json' \
  -d '{"session_id":"...","redirected":"http://localhost:1455/auth/callback?code=...&state=...","label":"main","group_name":"cyber"}'
```

PKCE verifier/state 只存在于服务端内存（15 分钟有效，单次使用），授权码不可重放。OpenAI/Anthropic
的 authorize/token 端点、client_id、redirect、scope 均可在 `config.json` 覆盖（默认即官方客户端值）。

> 外部 HTTP 访问：面板默认监听 `0.0.0.0:8787`，可直接经 `http://<服务器IP>:8787` 远程打开使用。
> 回调里的 `http://localhost:1455/...` 是浏览器登录后落地的地址，**无需服务器可达**——你只是从地址栏
> 复制它再粘回面板；PKCE 保证授权码即便走明文 HTTP 也无法被重放。复制按钮在非 HTTPS 源会自动改用
> 兜底方式（或直接选中文本框手动复制）。建议给面板配置 `admin_token` 并放行/限制对应端口。

Codex CLI 指向 pool（示例仅列必要连接字段；一键脚本还按服务端选项写入模型、推理强度、
Goal/实验开关与沙箱审批）：

```toml
[features]
goals = true

[model_providers.codex-pool]
name = "Codex Pool"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = "local-downstream-key"
```

Codex 分支采用配置白名单：只管理 Goal/实验模式、模型、推理强度、沙箱/审批、Pool URL
和 API key；不写静态 header，不创建额外 token/client-id 文件，不探测 WebSocket，不安装
RTK，也不改模型缓存或其他既有配置。服务端直接使用 Codex 自带的 `Session-Id`/`Thread-Id`
派生不透明命名空间，所以共享同一 API key 时仍能隔离上下文，下游无需再填任何标识。

Codex 专用的一键入口不会包含 Claude gateway、RTK、skills/plugins 或其他安装分支：

```bash
curl -fsSL 'https://<POOL_HOST>/file/<API_KEY>?client=codex' | bash
```

它会备份并原子合并 `~/.codex/config.toml`，只写上述白名单。当前 Codex 的
`/experimental` 是逐项 feature 开关而不是全局模式；脚本只显式写入与本需求对应的
`[features] goals = true`，不顺带开启 `network_proxy`、`prevent_idle_sleep` 等无关实验项。

> 兼容提示：上面的 Codex CLI 配置始终使用 `wire_api = "responses"`。完整官方 Codex skills / plugins / Browser Use 体验需要下游模型路由到官方 Codex 账号池；第三方 API 供应商按 `upstream_protocol` 分层 best-effort。

### Claude Code 模型选择

本地 gateway 只配置池地址、认证、证书和运行策略，不设置 Claude Code 的默认模型、tier
映射、子 Agent 模型或自定义模型菜单。初始化方式：

```bash
gateway init --pool-url http://127.0.0.1:8787 --key <downstream-key>
```

用户侧仍只传 URL 与 API key。`gateway init` 会在内部生成并持久化
`client_instance_id`；升级旧配置时第一次加载会自动补齐。直连时服务端使用 Claude Code
自带的 `X-Claude-Code-Session-Id`，经 gateway 时由 gateway 自动携带内部实例标识，两种路径
都不要求用户配置额外 header。

模型由 Claude Code 和用户自行选择。下游 Key 或分组的 `force_model` 是 VPS 侧策略：请求到达
服务器并完成认证后才可能被覆盖，不会写回本地 gateway 或 Claude Code 配置。旧版
`claude_model` 字段升级后会被忽略并由 gateway 尽力清理；兼容期内 `gateway init --model ...`
仍可解析，但参数只会输出弃用提示且不会生效。

`/effort` 可选 `low`、`medium`、`high`、`xhigh`、`max`；桥接层把 Claude Code 的
`output_config.effort` 映射为 Codex Responses 的 `reasoning.effort`，其中 Claude Code
的 `max` 会钳到 Codex 的最高合法档 `xhigh`。持久化配置可写入
`~/.claude/settings.json`，但本项目不会替用户写入模型字段。

Claude Code 固定发送的 Anthropic `max_tokens` 不会写入内置 Codex Responses 请求；因此
`xhigh`/`max` 不再连带产生 WHAM 的 `Unsupported parameter: max_output_tokens`。桥接请求固定
使用 `stream:true`、`store:false` 与 `include:["reasoning.encrypted_content"]`，非流式 Claude
请求由网关在返回前聚合为一个 Messages JSON。


`max` 是单会话档位，用 `/effort max` 或 `claude --effort max`；持久化 `effortLevel` 使用
`low`、`medium`、`high`、`xhigh`。

## 自测

```bash
chmod +x scripts/selftest.sh
scripts/selftest.sh
CODEX_POOL_RUN_SIDECAR_SELFTEST=1 scripts/selftest.sh
```

自测执行：

1. `gofmt` 检查；
2. `go test ./...`；
3. `go test ./... -count=2`；
4. `go vet ./...`；
5. 编译单二进制；
6. 可选 sidecar 自测（`CODEX_POOL_RUN_SIDECAR_SELFTEST=1`）。

## curl_cffi sidecar

```bash
cd pool_server/sidecar
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
python curl_cffi_sidecar.py --selftest
CODEX_POOL_SIDECAR_ADDR=127.0.0.1:8790 python curl_cffi_sidecar.py
```

添加 sidecar egress：

```bash
curl -sS http://127.0.0.1:8787/admin/egress-profiles \
  -H 'content-type: application/json' \
  -d '{"id":"sidecar-1","name":"sidecar-1","type":"curl_cffi_sidecar","endpoint":"http://127.0.0.1:8790","stream_capable":true,"health":"healthy","max_concurrency":16}'
```

浏览器 repair/bootstrap 注入 cookie：

```bash
curl -sS http://127.0.0.1:8787/admin/accounts/<account_id>/browser-repair \
  -H 'content-type: application/json' \
  -d '{"egress_id":"egress_direct","upstream_host":"https://chatgpt.com","cookie_header":"cf_clearance=..."}'
```

## 部署

- 一键完整安装（Linux；安装 Go 网关、前端产物、`curl_cffi` sidecar 及注册 worker）：
  ```bash
  sudo scripts/install.sh
  ```
  云服务器安装器始终随 release 部署 Super-Instruct 资源，但不提供部署级开关。
  管理员在“用户分组”中授予对应模型家族能力；用户复制该 API Key 的一键安装命令后，
  安装脚本在配置 Codex 时再询问是否启用。只有分组策略和客户端选择同时开启才生效。
- 只安装 Go 网关：
  ```bash
  sudo scripts/install.sh --minimal
  ```
- 明确完整安装所有组件：
  ```bash
  sudo scripts/install.sh --full
  ```
- Docker：`docker build -t codex-pool-server .`
- systemd：`deploy/systemd/codex-pool.socket` 持有公开端口，`codex-pool-handoff.service`
  常驻转发到 release worker；更新时只短暂挂起**新请求**，已建立的 HTTP/SSE/WebSocket
  继续由旧 worker 排空，不再重启公开 listener。`codex-pool.service` 仅保留为兼容入口。
- TLS reverse proxy：`deploy/nginx/codex-pool.conf` 或 `deploy/caddy/Caddyfile`

### 上游中转特征防护：直连 vs sidecar 出口（重要）

向上游隐藏「这是中转」分两层，两层都要才完整：

1. **应用层（body / header 内容）** — 始终生效，无论出口类型：账号绑定的虚拟身份、Claude Code
   官方身份 system 块、`x-anthropic-billing-header`、`X-Stainless-*` / `User-Agent` 版本轴、敏感词擦除，
   以及**响应侧泄露过滤**（剥离 `x-codex-*`、`x-ratelimit-*`、`anthropic-ratelimit-*`、`anthropic-organization-*`
   等暴露逐账号配额/组织的头）。
2. **传输层（TLS/JA3、HTTP 协议、header 大小写/顺序）** — 默认 `inprocess` 指纹引擎可在直连、
   HTTP/SOCKS/WARP 代理以及 sidecar 链式出口上复现供应商线级形状；Claude 使用从 Claude Code
   2.1.226 / Bun 1.4.0 捕获的无 ALPN ClientHello 与 HTTP/1.1 请求，浏览器链路使用 Chrome_120。
   `curl_cffi_sidecar` 是可切换的等价后端。只有显式 `claude_force_direct` 的 Go 标准库逃生路径
   无法定制 cipher/curve 与 header 顺序，因此传输层是**不完整**的。

> **生产建议**：保持 `egress_fingerprint_engine=inprocess`（默认），并按账号绑定所需的真实 IP
> 出口；指纹引擎会保留 WARP/HTTP/SOCKS 代理出口。也可以绑定 `curl_cffi_sidecar` 并把
> `egress_fingerprint_engine` 切为 `sidecar` 作为外部后端。账号详情支持把
> `primary_egress_id`（真实 IP 出口）和 `sidecar_egress_id`（TLS/HTTP2 包装层）分别选择；运行时链路为
> `sidecar → primary/standby proxy → upstream`，出口健康、冷却、并发与 CF 审计仍归属真实出口。也可调用：
>
> ```json
> POST /admin/accounts/<id>/egress-binding
> {"primary_egress_id":"proxy_br","sidecar_egress_id":"egress_sidecar"}
> ```
>
> 显式绑定的 sidecar 丢失或无效时会失败关闭，不会静默退回 Go 直连。`claude_force_direct` 可紧急绕过
> 指纹引擎（保留基础代理出口，但会暴露 Go TLS）；`claude_ja3` 留空使用当前 Claude/Bun 指纹，
> `chrome` 是显式兼容逃生阀。用
> `POST /admin/groups/<name>/assign-egress` 可一次把整组账号切到 sidecar/WARP 出口。直连出口适合
> 配合默认进程内指纹引擎；不需要为 Claude 单独部署 Python sidecar。
>
> 当前 Claude 线级形状固定为 `Accept-Encoding: gzip, deflate, br, zstd`，两种指纹引擎均透明解压，
> SSE 扫描器读取的仍是明文；不会再通过 `identity` 编码暴露中转实现。

### 下游 Claude Code 降噪（防遥测，重要）

中转只能改写经过本服务的 `/v1/messages` 流量。Claude Code 自身还会**直连** Anthropic/Statsig 上报
遥测、错误、自动更新检查与会话质量问卷——这些携带 `user.id`（每安装一个的设备标识）、登录邮箱、
账号/组织 UUID、`terminal.type` 等，**不经过中转、虚拟身份系统无法触及**。因此请在**下游**机器上启动
Claude Code 前设置以下环境变量：

```bash
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1   # 总开关：关自动更新 + 反馈命令 + 错误上报 + 遥测
export DO_NOT_TRACK=1                               # 额外关会话质量问卷等
# 可选（已被总开关覆盖，单独设更明确）：
# export DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 DISABLE_AUTOUPDATER=1
```

> 这些是**官方文档**记录的环境变量（`code.claude.com/docs/en/env-vars`）。中转侧已尽力虚拟化 `/v1/messages`
> 内的会话/用户/机器/OS/终端及 `x-anthropic-billing-header`（`cc_version`/`cch` 指纹），但上述带外上报只能在
> 下游关闭。管理面板「接入 / 设置」页也有同样提示。

### 更新部署（保留池内账号）

把整个项目文件夹重新上传到 VPS 覆盖后，在项目目录里执行：

```bash
sudo ./update.sh             # 清空编译缓存 + 全量重编译 + 重装(含内嵌 UI/sidecar) + 原子热切换
sudo ./update.sh --minimal   # 只更新 Go 网关
sudo ./update.sh --no-tests  # 跳过 go test，加快更新
sudo ./update.sh --open-firewall  # 顺便在 ufw/firewalld 放行监听端口
```

- **零配置对外访问**：不带参数时默认绑定 `0.0.0.0`（复用当前端口，新装默认 8787），更新结束会自动
  探测公网 IP 并打印面板地址 `http://<公网IP>:<端口>/`。如需仅本机/反代：`LISTEN_ADDR=127.0.0.1:8787 sudo ./update.sh`。
- **保留账号 + 防丢**：更新前对账号库做**在线 gzip 压缩快照**（默认 `<data-dir>/backups/`，留最近 10 份），
  更新后校验账号数未减少；它从不写数据库。即使池里有几百上千个账号也无压力——账号本身在 SQLite 里只是
  少量行，占空间的是用量/账本历史，压缩后通常只剩零头。备份前会做**磁盘空间预检**：空间不足会直接中止
  （不会半途部署或塞满磁盘），可按提示清盘、`BACKUP_DIR=<更大分区>`、或 `SKIP_BACKUP=1` 后重试。
  恢复：停服后 `gunzip -c <备份>.sqlite3.gz > <db路径>` 再启动。
- **安装不等待长连接**：构建和安装期间旧版本继续服务；新 worker 通过健康检查后，稳定 handoff 在一个极短的
  admission barrier 内原子切换，新请求立即进入新版本。切换前已经建立的 HTTP/SSE/WebSocket 连接继续由旧
  worker 完成，`install.sh` 不等待它们；独立的 systemd reaper 在后台确认旧 worker 的 inflight 稳定归零后，
  再优雅停止旧进程并删除旧 socket、unit 状态、release 目录和失效的 `previous` 链接，全程不使用强制终止。
- **可调环境变量**：`LISTEN_ADDR`、`BACKUP_DIR`、`BACKUP_KEEP`（默认 10）、`SKIP_BACKUP=1`、
  `CLEAN_MODCACHE=1`、`DATA_DIR`/`DATABASE_PATH`/`SERVICE_NAME`（一般无需设置，脚本会从 systemd 单元自动发现）。

**为什么以后加新功能也不用改 `update.sh`**：它是**通用脚本**——只做「备份账号 → 清缓存 → 调用项目内的
`scripts/install.sh` 从源码重编译重装 → 重启 → 校验」。新功能随文件夹一起上传即可，因为：①二进制每次从
当前源码全量重编译（含内嵌 UI）；②新增的数据库表/列由启动时的**幂等增量迁移**自动建好，新增配置项由
`normalize()` 自动填默认值；③构建/部署、原子切流与后台回收都集中在随项目上传的 `install.sh` 里。所以无论日后加什么
功能，流程始终是「覆盖文件夹 → `sudo ./update.sh`」一条命令，`update.sh` 本身保持不变。
