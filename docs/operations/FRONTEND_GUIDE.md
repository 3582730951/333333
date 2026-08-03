# Pool Console 前端开发与联动检查

## 当前入口与架构

- 管理端与用户端 SPA：`/console/`
- 前端源码：`web-spa/`（React、TypeScript/JavaScript、Vite、TanStack Query）
- 嵌入产物：`internal/console/dist/`，由 Go 服务直接提供
- 旧控制台兼容入口：`/legacy/`
- 页面清单的唯一来源：`web-spa/src/app/routeDefinitions.ts`
- HTTP 客户端统一入口：`web-spa/src/api.js` 与 `web-spa/src/features/*/api/`

生产环境不需要单独运行 Node 服务；发布前构建 SPA，再构建 Go 二进制即可：

```bash
npm --prefix web-spa ci
npm --prefix web-spa run build
go build -trimpath -o pool-server ./cmd/pool-server
./pool-server -config /path/to/config.json
```

浏览器访问 `http://HOST:PORT/console/`。配置了 `admin_token` 时使用管理员 Token 登录；未配置 Token 且尚无管理员用户时，控制台保留首次启动兼容模式。

## 修改后的标准门禁

```bash
# 静态 UI/路由/工作流契约、类型、单元测试和生产构建
npm --prefix web-spa run verify

# Playwright：全部管理/用户页面、桌面/移动、明暗主题、交互及 WCAG A/AA
npm --prefix web-spa run test:e2e

# Go API 与存储等全部后端包
go test ./...
go vet ./...
```

Vitest 默认限制为两个 jsdom worker，以便 1 核 1 GB 环境稳定运行；资源充足的 CI 可以设置 `VITEST_MAX_WORKERS`。

## 使用真实后端逐页联动检查

先启动当前构建的 Go 服务，再运行：

```bash
POOL_AUDIT_BASE_URL=http://127.0.0.1:8799 \
  npm --prefix web-spa run check:live-fullstack
```

若服务启用了管理员 Token：

```bash
POOL_AUDIT_BASE_URL=http://127.0.0.1:8799 \
POOL_AUDIT_ADMIN_TOKEN='TOKEN' \
  npm --prefix web-spa run check:live-fullstack
```

检查器从路由清单自动读取全部管理页和旧地址重定向，逐页确认：

- 页面进入 `data-page-ready="true"`；
- 没有 React 错误边界或浏览器运行时错误；
- 没有后端 HTTP 5xx；
- 页面没有横向溢出；
- 旧路由能落到当前兼容页面。
- 使用真实 `/admin/oauth/start` 生成授权链接，并在真实浏览器验证剪贴板内容一致。

默认报告写入 `web-spa/.run/live-fullstack-audit.json`（权限 `0600`）。可用 `POOL_AUDIT_OUTPUT` 和 `POOL_AUDIT_VIEWPORT=390x844` 修改报告位置与视口。
不希望创建短期 OAuth pending session 时可设置 `POOL_AUDIT_SKIP_OAUTH=1`。
如需同时验证真实用户端四个页面，可提供一个专用测试用户：

```bash
POOL_AUDIT_USER_EMAIL='audit-user@example.test' \
POOL_AUDIT_USER_PASSWORD='PASSWORD' \
  npm --prefix web-spa run check:live-fullstack
```

## API 响应契约

1. 后端列表接口即使为空也返回 `[]`，不要返回 `null`。
2. 前端在 `features/*/api` 中用 Zod 校验响应；兼容旧版本时在该边界集中适配，不在页面内分散猜测结构。
3. 所有变更请求由统一客户端添加 Session/CSRF 或管理员 Bearer 凭据。
4. 页面显示错误时保留服务端 `X-Request-ID` / `error.request_id`，诊断时用请求 ID 与诊断包关联。
5. 外部链接、下载、剪贴板和浏览器生命周期统一走 `src/lib/browser*.js`，避免页面各自实现能力检测。

## 常见故障定位

### 页面提示“接口返回了无法识别的数据”

这是前端契约校验拒绝了返回结构。依次检查：

1. 浏览器网络面板中的响应体与请求 ID；
2. 对应 `features/*/api` 的 Zod schema；
3. 空列表是否被 Go 编码为 `null`；
4. 新旧版本是否使用不同 envelope（如 `rows`、`items`、`users`）；
5. 运行真实后端逐页检查，避免 mock 数据掩盖前后端差异。

### 保存返回 401/403

- Session 登录请求必须携带 `cp_session` Cookie；变更请求还需要 `X-CP-CSRF`。
- 管理员 Token 请求使用 `Authorization: Bearer ...`，不需要 CSRF。
- 不要在页面组件内直接调用 `fetch` 绕过统一客户端。

### 生成/复制授权链接失败

- `/admin/oauth/start` 必须返回非空 `session_id`、绝对 HTTP(S) `auth_url` 和正数 `expires_in`。
- 复制优先使用 Clipboard API，受限浏览器自动退回 DOM copy；仍失败时会选中可见输入框供手动复制。
- 定向回归：`tests/oauth-copy.test.tsx`、`tests/browser-clipboard.test.ts` 和 Playwright OAuth 真实浏览器用例。
