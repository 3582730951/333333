# `example_zip` 诊断包分析报告（2026-08-02）

## 1. 输入与完整性

- 文件：`example_zip/codex-pool-diagnostics-v3-diagjob_7a186b3c8da94b0cbb2e5dcfe86754c3.zip`
- SHA-256：`9ec0567b992ef2420d4eb388b984b95e6598e289fa0d2b5426f4fd086b1a4b6e`
- 格式：`codex-pool-diagnostics-v3`
- 内容：30 个文件，解压后 5,207,712 bytes；manifest 声明所有大表均未截断。
- 构建：基线提交 `788ad1c2aac5` 的 dirty build，生成时间早于本轮修复。

分析过程只读取别名化标识、计数、状态和错误分类；未输出 `settings.csv` 的值或任何凭据字段。

## 2. 可直接确认的事实

| 证据 | 观察 | 判定 |
|---|---|---|
| `manifest.json` | 14 个当前账户、18 个历史引用账户；30 个导出文件；无截断表 | 数据量足以审计导出范围 |
| `diagnostic_events.csv` | 52/52 全是 `storage_pressure`；没有 HTTP error、panic、timeout、settings save 或 decrypt 事件 | 旧诊断模型没有记录用户报告的触发错误 |
| `provider_attempts.csv` | 0 行 | 无法从旧包还原接码/邮箱/求解器保存失败的 Provider 级原因 |
| 导出文件清单 | 不含 `http_requests.csv`，也不含 email-pool 行级快照 | 请求 ID `REQ-89C6735FD8ABC561` 在包内不存在，无法跨表定位当次响应 |
| `settings.csv` | 42 个键，包含 `node_registrar_config`，但不提供一次保存事务的阶段/失败原因 | 能证明存在注册器设置，不足以证明哪一字段写入失败 |
| `route_attempts.csv` | 4,557 行：success 3,273；upstream 5xx 700；429 339；no_account 224；permanent 4xx 19；`local_response_storage` 2 | 两次本地响应存储失败与用户的流中断症状相关，但旧包没有错误正文，不能把它们直接等同于某一条报错 |
| `diagnostic_summary.json` | `goal_persistence_degraded=376`、`history_replaced=1614`、`compaction_completed=4`、`resume_recovered=204` | 上下文连续性有大量降级/替换，旧压缩完成次数明显不足，支持优先修复压缩和中途接力链路 |
| billing summary | failed_streaming 6、stream_interrupted_compensated 60、failed_before_response 14、failed_upstream 3 | 确有流式失败及补偿，但没有保存 decrypt 错误分类 |
| `runtime_storage.json` | disk level=normal，空闲 58.3%/约 12.0 GB；DB/journal/spool 均可写；0 次容量拒绝 | 当时问题不是磁盘不足、数据库只读或 spool 容量拒绝导致 |
| `audit_log.csv` | `goal_persistence_degraded` 376 次且均为 retryable/codex_terminal；`routing_unavailable` 155 次；compaction completed 4 次 | 连续性与路由问题真实存在，并非单纯前端提示文案 |

## 3. 对用户报告问题的结论

### 3.1 设置保存失败

旧包证明 `node_registrar_config` 已存在，但没有保存事务事件或 Provider 失败记录，因此不能从该 ZIP 精确还原失败字段。本轮通过前后端代码路径和回归夹具确认了更直接的问题：

1. 前端把 Provider 与 registrar 分成两个请求，前一个成功、后一个失败时会出现部分提交。
2. 旧批量保存响应只有数值 `saved`，前端按 settings diff 数组解析时会误判。
3. replace 模式会把留空的 write-only 密钥当删除，读取接口又可能回传敏感值。
4. Provider reload 失败会把已提交保存误报成整体失败。

现已改为同一数据库事务保存 Provider/defaults/registrar；密钥读取只返回 `*_configured`，留空表示保留；Provider 级失败返回安全的 type/key/request ID；提交成功但 reload 失败返回 `reload_ok=false` 和明确 warning。未知的新版本/插件 Provider 字段也会保留。

### 3.2 邮箱池 `REQ-89C6735FD8ABC561`

该 request ID 不在旧包中，原因是 v3 包没有导出 HTTP request 表。真实嵌入式服务复现确认：空库时 Go nil slice 编码为 `null`，而前端 schema 要求数组，最终显示“接口返回了无法识别的数据”。同时遗漏 status 或 `status=all` 曾被错误归一为 idle，导致旧客户端结果被静默缩窄。

修复后：

- storage 固定初始化空 slice；后端输出 `"accounts":[]`。
- 前端兼容旧服务的 `null -> []`。
- omitted/all/any/* 维持“不筛选”语义。
- 真实 Go 服务上的邮箱池页面和契约测试均通过。

### 3.3 `context_length_exceeded`

ZIP 不含报错正文，但 `goal_persistence_degraded=376`、`history_replaced=1614`、仅 4 次 compaction 是直接的异常信号。当前实现已形成以下顺序：客户端原生 Codex/Claude Code compact 优先；失败后使用 Kiro 有界 map-reduce 摘要；上下文错误保持已输出内容并请求压缩；中途流量切至 Kiro 后保持 sticky；Kiro context error 时进入 Codex native compact，下一轮返回 Kiro。

本地目标测试与 race 通过；既有云端目标记录也验证了 Codex→Kiro 中途切换、sticky，以及 context error→Codex compact→Kiro 回归。

### 3.4 `Encrypted function output ... could not be decrypted`

旧包仅有 2 次 `local_response_storage` 和若干流式失败，未保存 decrypt 错误分类或正文，因此这里只能判定“症状相符，不能逐请求归因”。当前代码已识别专用 `ResponsesContextErrorEncryptedFunctionOutput`，在同步和已提交流式失败两条路径上退休旧 epoch、保留可见部分输出、去除失效 encrypted content、修复 tool exchange 并在下一轮重建 session mapping；相应同步、流式晚失败和降级重放测试通过。

## 4. 旧诊断包暴露出的框架缺口与修复

| 旧包缺口 | 当前通用框架 |
|---|---|
| request ID 不可关联 | 所有 5xx/panic 进入同一结构化 incident，并与 `http_requests.csv` 共用服务端 `REQ-*` |
| Provider/save 失败没有事件 | middleware、业务 handler、goroutine、timeout 统一进入 supervisor callback |
| callback 自身失败会丢记录 | primary error/panic/timeout 后原子写入 fallback journal，健康启动时重放 |
| 只看到 storage pressure | 事件包含 component、operation、event_type、delivery、safe detail 和 fingerprint |
| 无法验证导出稳定性 | 诊断 canary 实测 31 文件；panic/500 可关联；ZIP 完整性、0600 权限和敏感夹具缺失均自动验证 |

新的诊断 canary 位于最终交付物的
`final-product-stability-20260802-final/verification/incident-diagnostic-canary.zip`，对应验证记录为 `incident-diagnostic-content.log` 和 `startup-fallback-e2e.log`。

## 5. 验证映射

- 设置/Provider 原子保存、write-only 密钥、reload warning：Go API 测试与前端 122 项全量测试通过。
- 邮箱池空响应/旧查询兼容：后端契约、前端 schema、真实浏览器页面均通过。
- 上下文与 encrypted output：目标测试、race、完整 Go 回归通过。
- 新诊断闭环：错误、panic、timeout、callback 失败、64 并发 fallback/replay、启动失败后重放、诊断 ZIP 均通过。
- 完整结果与命令：`artifacts/final-product-stability-20260802-final/verification-record.md`。
