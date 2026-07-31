# 后端优化交付

## 入口

- 两包诊断结论：`comparison.md`
- 全量机器数据：`comparison.json`
- 修改、测试、部署与回滚实录：`verification-record.md`
- 最终补丁：`backend-fix.patch`
- 修改源码：`modified-source/`
- 原始源码：`originals/source/`
- 修改后二进制：`codex-pool-server.optimized`
- 可执行回滚：`rollback.sh`（依赖同目录 `service-control.sh`）
- 可执行重部署：`redeploy.sh`
- 最终截图：`final-dashboard-after-backend.png`
- 一体化交付包：`backend-latest-fix-delivery.tar.gz`

两份原始诊断包保存在 `originals/`，完整解包内容按生成时间保存在
`packages/`。远端测试、构建、探针、部署与截图的原始日志位于
`remote-evidence/`。

配置和 SQLite 部署备份只保留在远端部署备份目录，没有复制进交付包。
