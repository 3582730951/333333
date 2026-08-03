# ChatGPT Web session `sessionToken` 导入与 Codex 0.146.0 Docker 验证记录

- 日期：2026-08-03（America/New_York）
- 基线提交：`343dcbf47c5d4f2379b5033804b4efa648afc63f`
- 隔离运行根目录：`RUN_ROOT=/tmp/codex-at-e2e-run-20260803T054148Z-211137`
- 最终工作区：`/workspace`
- 测试凭据：完全合成的 RS256 形状 JWT 与合成 `sessionToken`；对话中曾出现的凭据未进入命令、容器、数据库、日志或诊断包。
- Responses 上游：本地 Node mock，路径 `/backend-api/codex/responses`。后台 quota poller 仍按源码中的硬编码地址访问真实 `/backend-api/wham/usage`，合成 AT 在该独立探针上返回 401；本记录不代表真实 OpenAI 上游接受该合成 AT。
- 真实上游状态：等待 `/workspace/secrets/chatgpt-session.json` 中的新 session；当前工作区不存在该 secret 文件。

## 1. 结论与根因

旧的 `ParseAuthJSON` 已能从原始 `/api/auth/session` JSON 读取 `accessToken`，但只从
`cookie_header/cookieHeader/session_cookie/sessionCookie/cookie` 读取 cookie，忽略同一响应中的
`sessionToken`。因此导入结果是：

1. AT 正常入池；
2. RT 为空（Web session 的预期形态）；
3. session cookie 也为空；
4. AT 失效或上游拒绝后，刷新路径才会报告缺少 RT/cookie。

修改后的分支位于 `internal/auth/auth.go::ParseAuthJSON`：仅当文档已经识别为 ChatGPT Web
session 且显式 cookie 字段为空时，使用 `sessionToken` 或 `session_token` 作为 fallback。
显式 `session_cookie` 始终优先。导入层随后规范化为
`__Secure-next-auth.session-token=<VALUE>` 并加密保存。

官方 Codex CLI 0.146.0 的可用接法是把账号池配置为自定义 model provider。Web session 的
`eyJ...` AT 由账号池解密后作为上游 Bearer 使用，而不是直接交给 CLI 的登录存储。

## 2. 基线与修改哈希

基线命令：

```bash
sha256sum internal/auth/auth.go internal/auth/chatgpt_session.go \
  internal/auth/auth_test.go internal/api/accounts_import_test.go \
  web-spa/src/components/OAuthLoginModal.jsx README.md
```

基线输出（exit 0）：

```text
9cc0cada7629d969b71204fda4684949393d1d649c68df7573b4e02f454ba2bf  internal/auth/auth.go
471cb30f6726dfd14e7e266ed015978dda6e1060dc9ded1f04cc63b650420a47  internal/auth/chatgpt_session.go
5b7401835f1aa01e3d653d5594c32976b9576f4a16d6ab65061084bb5a96da99  internal/auth/auth_test.go
f34749c6a52a0382ab38734f7437a962d7b044f45fd4c699f02841306be4b201  internal/api/accounts_import_test.go
535815d3eff1825a41f48e23429ddd839a631ac0dd0e3d234a39af8fbb8e4f33  web-spa/src/components/OAuthLoginModal.jsx
8ee826c24a9d1616e6d5d4cac54d3921466ef42e3f14ca216e130a4b136c5edf  README.md
```

修改后输出（exit 0）：

```text
dc9e110783b67577ded9e098e10bbdf3d04a947a6eebabebb2940a471022c49e  internal/auth/auth.go
a38de52fcfe51553317e8df72d61f50ee8d56c62c38feed12d27920eabe0732e  internal/auth/auth_test.go
f3bf1fba7f772b96ce9cb2cbb292cd84c8f409cf56c9c233a96679789a372046  internal/api/accounts_import_test.go
032a567b0638beafb063c64e660d98510042ae3c29eeebe01f28b08c58a3925a  web-spa/src/components/OAuthLoginModal.jsx
728744f53a5dd1c9253156d3ebccdbad21cf76e219a8daf39b3a0dc55a005d61  README.md
```

补丁：

```text
SHA256 0096a7cb03d9b0380f10bbddb06db1e5e4d34f920f1690e75b9fb0765803095b
/workspace/verification/chatgpt-session-token-import.patch
```

## 3. 输入结构

Docker E2E 使用的原始导入结构如下；所有值均为本地合成值：

```json
{
  "label": "local-session-e2e",
  "auth_json": {
    "user": {"id": "user-local-e2e", "email": "local-e2e@example.internal"},
    "account": {"id": "workspace-local-e2e", "planType": "team"},
    "expires": "FUTURE_ISO8601",
    "accessToken": "SYNTHETIC_RS256_JWT",
    "sessionToken": "SYNTHETIC_SESSION_TOKEN"
  }
}
```

## 4. 定向测试

命令：

```bash
docker run --rm \
  -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
  -v /workspace:/src:ro -v "$RUN_ROOT/cache/go-build:/cache/build" \
  -v "$RUN_ROOT/cache/go-mod:/cache/mod" -w /src golang:1.25.12-trixie \
  /usr/local/go/bin/go test ./internal/auth ./internal/api \
  -run 'Test(ParseChatGPTWebSessionExtractsSessionTokenAliases|ParseChatGPTWebSessionBuildsUsableExternalCredentials|ImportRawChatGPTSessionAutomaticallyStoresSessionToken|NormalizeImportedSessionCookie|ImportedChatGPTWebSessionCookieRemintRefreshesAllBearerMetadata)$' \
  -count=1 -v
```

关键原样输出（exit 0）：

```text
--- PASS: TestParseChatGPTWebSessionExtractsSessionTokenAliases (0.00s)
PASS
ok      codex-account-pool/internal/auth  0.011s
--- PASS: TestImportRawChatGPTSessionAutomaticallyStoresSessionToken (0.45s)
--- PASS: TestImportedChatGPTWebSessionCookieRemintRefreshesAllBearerMetadata (0.19s)
PASS
ok      codex-account-pool/internal/api   0.762s
EXIT_STATUS=0
```

完整输出：`/workspace/verification/logs/final-main-targeted-tests.log`。

## 5. 完整 Docker 构建

命令：

```bash
DOCKER_BUILDKIT=0 docker build -t codex-pool:session-token-e2e "$RUN_ROOT/worktree"
```

原样结果（exit 0）：

```text
Test Files  23 passed (23)
Tests       122 passed (122)
ok  codex-account-pool/internal/api      200.205s
ok  codex-account-pool/internal/auth     0.009s
Successfully built 6c229ecd70c2
Successfully tagged codex-pool:session-token-e2e
```

镜像 ID：

```text
sha256:6c229ecd70c2d5bfcec9ee72e7fd382a941cfed71ad7dc03806e7571c5faae83
```

完整输出：`/workspace/verification/logs/docker-build.log`。

## 6. 导入、无 RT 测活与密文落盘

导入命令：

```bash
curl -sS -H 'Content-Type: application/json' \
  --data-binary @"$RUN_ROOT/runtime/import-request.json" \
  http://127.0.0.1:18787/admin/accounts/import-auth-json
```

关键原样结果（HTTP 200）：

```json
{
  "import_status": "imported",
  "credential_mode": "chatgpt_auth_tokens",
  "status": "active",
  "warnings": [
    "id_token is a local metadata-only compatibility JWT; upstream authentication always uses access_token",
    "Web session has no refresh_token; access_token renewal will use the encrypted session cookie"
  ]
}
```

测活命令：

```bash
curl -sS -X POST -H 'Content-Type: application/json' --data '{}' \
  http://127.0.0.1:18787/admin/accounts/ACCOUNT_ID/health-test
```

关键原样结果（HTTP 200）：

```json
{
  "alive": true,
  "http_status": 200,
  "model": "gpt-5.5",
  "model_checked": true,
  "probe_scope": "model_request",
  "ready": true,
  "state": "alive"
}
```

SQLite、WAL 与 SHM 扫描原样结果（exit 0）：

```json
{
  "integrity_check": "ok",
  "credential_mode": "chatgpt_auth_tokens",
  "auth_method": "access_token",
  "access_token_is_ciphertext": true,
  "refresh_token_empty": true,
  "session_cookie_is_ciphertext": true,
  "plaintext_session_token_storage_hits": [],
  "plaintext_access_token_storage_hits": [],
  "mock_received_authorization": true,
  "mock_received_account_id": true
}
```

完整输出：
- `/workspace/verification/logs/import.log`
- `/workspace/verification/logs/health-test.log`
- `/workspace/verification/logs/storage-check-final.log`

## 7. 官方 Codex 0.146.0 端到端

安装与版本：

```bash
npm view @openai/codex version
npm install @openai/codex@0.146.0
codex --version
```

原样输出（exit 0）：

```text
codex-cli 0.146.0
```

最新版本查询返回 `0.146.0`。本地持久化镜像也已构建并复验：

```bash
DOCKER_BUILDKIT=0 docker build \
  -f /workspace/verification/codex-cli-0.146.0.Dockerfile \
  -t codex-cli:0.146.0-local /workspace/verification
docker run --rm codex-cli:0.146.0-local --version
```

```text
Successfully tagged codex-cli:0.146.0-local
codex-cli 0.146.0
BUILD_EXIT_STATUS=0
VERSION_EXIT_STATUS=0
image=sha256:d5f51b7eac660fbc09fdfc29dbf3be1cf95a320425cfb3090bb7ef2a0de2adcd
```

CLI provider 配置：

```toml
model = "gpt-5.5"
model_provider = "poolserver"

[model_providers.poolserver]
base_url = "http://codex-at-pool:8787/v1"
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = "local-e2e-downstream-key"
supports_websockets = false
```

执行命令：

```bash
docker run --rm --network codex-at-e2e-54148Z211137 --user 0:0 -i \
  -e CODEX_HOME=/codex-home \
  -v "$RUN_ROOT/codex-home:/codex-home" -v "$RUN_ROOT/codex-work:/work" -w /work \
  codex-cli:0.146.0-local \
  exec --skip-git-repo-check --ephemeral --color never -m gpt-5.5 -C /work \
  'Reply with exactly AT_POOL_OK and do not call tools.'
```

原样结果（exit 0）：

```text
OpenAI Codex v0.146.0
provider: poolserver
codex
AT_POOL_OK
tokens used
12
AT_POOL_OK
EXIT_STATUS=0
ASSERTION=persistent_codex_image_pool_e2e_passed
```

此前直接挂载官方 vendor 二进制的第一次执行也返回 `AT_POOL_OK`，并记录
`ASSERTION=official_codex_pool_e2e_passed`。两种装载方式均为 exit 0。

mock 最终捕获到四次请求：初始账号测活、vendor 挂载 Codex、持久化镜像 Codex，以及
quota 401 后的复测活；四次的 Bearer SHA-256 前 12 位均为 `0782b189f807`，账号头均为
`workspace-local-e2e`。最后一次复测活仍为 `alive=true/state=alive/HTTP 200`；诊断 ZIP
生成时已捕获前两次。完整输出：
`/workspace/verification/logs/codex-pool-e2e.log` 与
`/workspace/verification/logs/mock-requests.jsonl`。

## 8. 诊断包

命令：

```bash
bash "$RUN_ROOT/worktree/scripts/fetch-diagnostics.sh" \
  --base-url http://127.0.0.1:18787 \
  --output "$RUN_ROOT/artifacts/codex-pool-diagnostics-v3.zip" \
  --extract-dir "$RUN_ROOT/artifacts/codex-pool-diagnostics-v3" \
  --timeout 180 --poll 1
cp "$RUN_ROOT/artifacts/codex-pool-diagnostics-v3.zip" \
  /workspace/verification/codex-pool-diagnostics-v3.zip
```

原样结果（exit 0）：

```text
status: ready
zip_test: ok
manifest_format: codex-pool-diagnostics-v3
zip_entries: 31
diagnostics_contains_access_token: false
diagnostics_contains_session_token: false
```

诊断分析同时确认两个相互独立的结果：

1. `account_auth_metadata.csv`：`credential_present=true`、`auth_method=access_token`、
   `credential_mode=chatgpt_auth_tokens`；
2. 模型测活与两次 Codex Responses 请求均为 200；
3. `account_rate_limits.csv` 另有 `quota_poll_error/error/http_error/401`，其来源是
   `quota_poll.go` 中固定的 `https://chatgpt.com/backend-api/wham/usage`，错误为合成 AT
   `unauthorized_unknown`；它不是 Responses 测活结果，也不是缺少 RT 的刷新错误。

文件：

```text
SHA256 53bf50b3aa4d6cf5ddf5235d8166b375764d73cc68a0c0720eeebc7c976d763a
/workspace/verification/codex-pool-diagnostics-v3.zip
```

## 9. 修改产物与自检

```text
SHA256 3bfc298ffca9b902402058d1db94b6d3a7483481405f84ad9c7521071e247488
/workspace/verification/codex-pool-server-session-token-e2e
```

命令与原样结果：

```bash
/workspace/verification/codex-pool-server-session-token-e2e --self-test
```

```text
codex-pool-server self-test ok
EXIT_STATUS=0
```

## 10. 回滚验证

命令：

```bash
WORKTREE=DISPOSABLE_COPY \
PATCH_FILE=/workspace/verification/chatgpt-session-token-import.patch \
  /workspace/verification/rollback-chatgpt-session-token-import.sh
```

原样结果（exit 0）：

```text
rollback applied: DISPOSABLE_COPY
ROLLBACK_DIFF=clean EXIT_STATUS=0
rollback already present: DISPOSABLE_COPY
ROLLBACK_REOPEN_EXECUTE=passed
```

反向应用后五个文件的 SHA-256 与第 2 节基线完全一致。

## 11. 失败修正记录

1. Go 镜像的 login shell 未解析 `go`：改用 `/usr/local/go/bin/go` 后测试通过。
2. 本机缺少 buildx：改用 `DOCKER_BUILDKIT=0 docker build` 后完整构建通过。
3. 首次启动时隔离目录为 0700，容器 UID 996 读取配置报错：仅对合成测试目录设置 0755、
   配置文件设置 0644，重建后 `/readyz` 返回 200。
4. 首轮存储断言读取了错误的 mock 字段名，并错误假设 AT 为明文：修正为
   `authorization_present`，随后验证 AT 与 session cookie 均为密文。
5. 仓库已有的 opt-in Codex Docker 测试仍使用旧 command-auth 假设；当前版本配置字段为
   `experimental_bearer_token`。因此采用当前 provider 配置完成了上面的官方 CLI 手工 E2E。
