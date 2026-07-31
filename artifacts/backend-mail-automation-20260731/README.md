# Backend Mail Automation Final Delivery

完成范围：

- Apple 风格前端、数据可视化、长账号/邮箱、清晰额度和明暗模式。
- ChatGPT Free 协议/浏览器注册的统一 pipeline。
- 多邮箱 Provider、Cloudflare 自有域邮箱配置/健康/默认路由。
- PAT 优先、Codex OAuth 回退、条件 `add_phone` 与 SMS 的 Team 子账号循环。
- 1% 额度触发移除和替补注册。
- 两份最新诊断包根因分析与后端磁盘/审计优化。
- OAuth Worker 纳入原生 `install.sh`、健康门和回滚。
- 旧分支安装升级、数据兼容、生产部署、真实回滚与重部署。

## 先看

1. 架构与审计：[`AUDIT.md`](AUDIT.md)
2. 联网项目对比：[`RESEARCH.md`](RESEARCH.md)
3. 精确命令和结果：[`verification-record.md`](verification-record.md)
4. 截图报告：
   [`remote-final/screenshots/final/final-ui-visual-report.json`](remote-final/screenshots/final/final-ui-visual-report.json)

## 四个交付角色

| 角色 | 路径 |
| --- | --- |
| 修改产物 | `modified/codex-pool-server-linux-amd64` |
| 补丁 | `backend-mail-automation.patch` |
| 验证记录 | `verification-record.md` |
| 回滚 | `production-control.sh rollback` |

## 当前远端发布

```text
main      backend-mail-automation-final-main
frontend  backend-mail-automation-final-frontend
worker    http://127.0.0.1:8802/healthz
binary    b2c0d6d718f0b880a3901768e2ce7007bda19cd2349b779d3693ec958e2b5543
```

主服务、展示服务、注册 readiness、Team lifecycle、Cloudflare mailbox 和 OAuth
Worker 均已最终复核。生产数据库在实际回滚中恢复到部署前语义指纹，之后重新迁移并
保持相同业务计数。

## 截图

`remote-final/screenshots/final/` 包含 33 张远端 Chromium 截图：

- 8 个路由；
- desktop/mobile；
- light/dark；
- 额外 1 张深色 Team 事件明细。

最终自动结果：`32/32 passed, 0 issues`。

## 校验

```bash
bash artifacts/backend-mail-automation-20260731/verify-delivery.sh
```

完整目录哈希见 `SHA256SUMS`。
