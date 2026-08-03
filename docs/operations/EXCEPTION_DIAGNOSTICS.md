# 通用异常诊断框架

生产入口在 `internal/supervisor`，持久化适配在 `internal/incident`。事件依次进入：

```text
Report / RecoverEvent / LogPanicEvent
  -> 100 条进程内事件环
  -> 有超时和 panic 隔离的主回调
     -> diagnostic_events
     -> 失败时 fsync 到 data/journal/exception-events
        -> 启动及后台重放 -> diagnostic_events
           -> diagnostic_events.csv
```

服务进程在私有数据目录完成预检后就注册回调；数据库初始化前的启动错误会直接写入兜底日志，数据库可用后再切换主回调并重放。因此存储初始化、密钥加载、迁移以及正常运行期使用同一事件链路。

## 新代码接入

已处理错误：

```go
supervisor.ReportError("module-name", "operation_name", err)
```

需要隔离的 goroutine 或回调：

```go
defer supervisor.RecoverEvent(supervisor.Event{
    Module: "module-name",
    Operation: "operation_name",
})
```

HTTP 层已经统一记录 panic 和 5xx，不应在业务 handler 重复上报同一错误。需要关联请求时只传服务端生成的 `REQ-` ID、稳定路由名、状态码和错误分类；不要传请求体、提示词、凭证或第三方响应正文。

## 稳定性约束

- 主回调和兜底回调各有独立超时；回调 panic 不会递归触发自身。
- 落盘文件采用 `0600`、原子替换、文件与目录 fsync，事件 ID 保证数据库重放幂等。
- 日志上限为 4096 条或 16 MiB；淘汰或损坏会生成 `exception_journal_gap`，不会静默丢失。
- 数据库仅保存分类、状态、稳定路由、请求 ID 和单向指纹；指纹只由分类字段生成，不使用原始错误文本。原始错误只留在本机 supervisor 日志/状态视图。
- `/admin/system` 和诊断包 `runtime_storage.json` 提供回调、超时、兜底、待重放及缺口计数。

新增异常边界至少应测试：主回调成功、主回调失败、回调 panic/超时、兜底回调 panic、并发落盘、进程重启重放、诊断 ZIP 关联和原始敏感文本不落盘。
