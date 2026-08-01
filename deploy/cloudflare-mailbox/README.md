# Cloudflare 自建临时邮箱（仓库内适配器）

此 Worker 精确实现账号池当前适配器使用的三个接口：

- `POST /admin/new_address`（`x-admin-auth`）
- `POST /api/new_address`（默认关闭公开创建）
- `GET /api/mails?limit=20&offset=0`（`Authorization: Bearer <jwt>`）

邮件由 Cloudflare Email Routing 交给 Worker 的 `email()` handler，原始 MIME 保存到 D1；邮箱 JWT、邮箱记录和邮件均会过期。日志中不写 Token 或邮件正文。

## 一次复制部署

先在 Cloudflare 托管域名并启用 Email Routing，然后登录 Wrangler：

```bash
cd deploy/cloudflare-mailbox
npx wrangler login
MAIL_DOMAIN=mail.example.com \
API_HOST=mailbox-api.example.com \
./deploy.sh
```

不需要 API 自定义域名时删掉 `API_HOST=...`，使用部署输出里的 `workers.dev` URL。脚本会创建/复用 D1、应用迁移、生成并写入两个 Secret、部署 Worker，并在最后显示只出现一次的 Admin Token。

部署后按脚本输出，在 Cloudflare Dashboard 的 **Email Routing → Routing rules → Catch-all address** 中选择 **Send to a Worker** 并指定本 Worker。最后把 API URL、`MAIL_DOMAIN` 和 Admin Token 粘贴到本应用的“Cloudflare 自建邮箱”页面，点击“保存并测试”。

## 重复部署

首次运行生成的 `wrangler.jsonc` 保留了 D1 UUID；以后直接再次执行相同命令。若从全新目录接管已有数据库：

```bash
MAIL_DOMAIN=mail.example.com \
D1_DATABASE_ID=00000000-0000-0000-0000-000000000000 \
./deploy.sh
```

## 本地合约测试

```bash
cd deploy/cloudflare-mailbox
npm test
```

## 官方资料

- [Workers Wrangler 配置](https://developers.cloudflare.com/workers/wrangler/configuration/)
- [Email Worker handler](https://developers.cloudflare.com/email-service/api/route-emails/email-handler/)
- [Email Routing 规则与 Catch-all](https://developers.cloudflare.com/email-service/configuration/email-routing-addresses/)
- [D1 migrations](https://developers.cloudflare.com/d1/reference/migrations/)
- [Workers secrets](https://developers.cloudflare.com/workers/configuration/secrets/)
