# Emergency v3 诊断包分析与整改记录

## 样本与结论

- 样本：`example_zip/codex-pool-diagnostics-emergency-v3.zip`
- SHA-256：`2e540919acf719a3f6785ca356c33a6c41f5ac08eecae7e3f6e068fcef6ea859`
- 生成时间：2026-08-20 08:13:03 EDT
- 构建：`1150cbb4a399` 对应的 dirty 工作树
- 导出模式：`emergency_memory`，完整导出因 `deadline_exceeded` 在 30 秒内失败

诊断包证明了两条相互放大的故障链：完整诊断导出的无界/全表读取耗尽救援期限；后台维护在共享期限内发生写入背压，映射清理长期超时，导致过期 Goal 多阶段回收不能及时收敛。应急导出随后又把“未读取”误写成“0 行”，并遗漏了进程内 HTTP、路由和 provider 尝试环形缓冲区，因而丢失 503 风暴的精确归因证据。

## 全部记录项

| ID | 级别 | 诊断证据 | 判定 | 整改 |
|---|---|---|---|---|
| D-01 | 高 | 完整导出 `deadline_exceeded`，仅回退到 emergency archive | 完整导出在写 ZIP 前执行全表 `COUNT/MIN/MAX`、多表最近账户扫描以及无界 Codex mapping/snapshot 读取，生产历史增长后超过 30 秒救援期限 | 大表统计改成 `row_limit+1` 有界探针；mapping/snapshot 改成最近 20,001 行读取并导出最近 20,000 行；为所有新近排序路径增加索引 |
| D-02 | 高 | emergency manifest 仅声明 audit 500 行、Goal 101 行已读取，但其他表全部报告为 0 行 | “未读取”被伪装成“真实空表”，会误导事故判断 | emergency manifest 对未读取文件删除 `row_counts` 项，增加 `omitted_files`、逐文件 `read_status`、`database_snapshot`/`snapshot_consistency` gap，并将账户数置为 `null` 且声明 unavailable |
| D-03 | 高 | `http_requests.csv`、`route_attempts.csv`、`provider_attempts.csv` 为 0；supervisor 却记录 100 个 503 | 应急路径漏掉已存在的进程内环形缓冲区，历史 503 的具体选择/上游原因因此不可从该样本恢复 | emergency archive 直接快照并脱敏写入 HTTP、route、provider 环形缓冲；未知账户仍生成稳定、类型隔离的 HMAC alias；同时保留 sidecar adaptive 状态和真实 passive-health 快照 |
| M-01 | 高 | `database_backpressure_events=1262` | SQLite 单 writer 在维护与事件写入并发时产生显著背压 | 缩短维护事务的搜索路径，新增 expiry/updated/created 复合索引；把 mapping 与 route retention 移到 Goal 多阶段回收之前；各阶段使用独立子期限，仍受 10 秒总期限约束 |
| M-02 | 高 | `cleanup_failure_events=561`，最后操作 `mapping_cleanup`、错误 `timeout`；数量接近 48 小时内每五分钟一次 | mapping cleanup 被前序工作和缺失索引长期拖到共享期限末尾 | mapping cleanup 提前执行并获得独立 2.5 秒预算；新增 Codex alias expiry、binding/snapshot updated、upstream attempt created 索引，以及 affinity retention 索引 |
| M-03 | 中 | 101 个 Goal 中 7 个已过期；其中 6 个 awaiting-tool 共 188,180,836 bytes；另有 1 个 reclaiming Goal 已 0 bytes 但仍有 9 aliases | 每次维护只推进一个有界回收 phase，五分钟一次使大型 Goal 需要很久才能删除父记录/别名 | 暴露 `CleanupGoalContinuityStep` 的 bytes/goals/progress 结果；一次维护最多连续推进 16 个有界 phase，并把实际释放字节计入 disk-guard 指标 |
| D-04 | 中 | emergency `account_count=0`、`current_account_count=0`，但 `historical_reference_account_count=1` | 0 来自 nil 输入，不是经过数据库读取确认的账户数 | emergency 账户计数改为 `null`，新增 `account_count_status=unavailable_in_emergency_mode` |
| D-05 | 中 | Goal CSV 的 `active_run_count` 全为 0 | 查询使用不存在的运行态 `state='active'`，漏掉 `running`、`compacting`、`awaiting_tool_result` 以及租约有效性 | 按真实运行态集合及 `lease_expires_at > snapshot_now` 统计；新增 live-run 回归测试 |
| D-06 | 中 | full export 对截断表原先承诺精确总行数和 source-wide 时间范围 | 为得到精确值执行全表扫描，直接放大 D-01 | manifest 新增 `source_row_counts_exact`；有界探针达到上限时把数量声明为 lower bound；时间范围明确为 `exported_recent_window` |
| D-07 | 中 | emergency `passive_provider_health.json` 显示 legacy in-memory 的 0 series | 固定占位内容会覆盖真实运行态 | emergency 路径写入当前 passive health snapshot；未配置时明确标记 `runtime_unconfigured` |
| I-01 | 中 | incident callback 285 次投递，48 次 primary timeout；48 次 fallback 全成功，journal replay 48，pending/dropped/corrupt/replay failure 均为 0 | 是 M-01 的次生症状；durable fallback 工作正常，没有事件丢失 | 通过缩短 writer 占用路径降低 primary timeout；保留现有 fallback/journal 机制，不修改已验证的容灾语义 |
| H-01 | 中 | supervisor 100 个 HTTP 503：94 个 `v1.responses/http_status_5xx`，5 个 dashboard cancel，1 个 model-audit cancel | 2026-08-19 02:38:35–05:40:58 EDT 存在 responses 503 风暴；进程全部 recovered 且 response committed。旧应急包缺少 route/provider 证据，所以历史上游根因不可精确还原 | 修复 D-03，后续 emergency 包具备精确归因数据；未基于缺失证据更改模型、账户或重试语义 |
| H-02 | 信息 | supervisor 18 个模块 running、1 个 deferred-migration completed；panic/restart/unexpected exit 均为 0 | 不是进程崩溃或 supervisor 重启问题 | 无运行逻辑改动 |
| S-01 | 信息 | 磁盘 normal，37.4%/7.73 GB free；DB、journal、spool writable；无 admission block、spool reject 或 reservation reject | 不是磁盘容量故障 | 无容量策略改动 |
| G-01 | 信息 | 83 awaiting-tool、17 ready、1 reclaiming；总存储 479,673,380 bytes，低于 896 MiB maintenance target | 总量未越限，但过期对象因 M-02/M-03 滞留 | 只回收已过期/既有 budget 候选；保持 live lease 和 awaiting-tool 保护规则 |
| G-02 | 信息 | 101/101 `workspace_fingerprint_present=false`，其余稳定指纹/response 字段存在 | workspace fingerprint 是可选输入；样本没有证明上下文关联错误 | 不伪造 workspace 值；继续依赖精确 alias/downstream/initial/response 锚点 |
| A-01 | 信息 | 500 条 audit：167 history replaced、166 checkpoint、166 alias、1 条 `codex_no_ratelimit_headers` | ChatGPT backend API 缺少 `x-ratelimit-*` 已由审计文本声明为正常情况 | 不改变限流推断 |

## 代码变更

- `internal/api/diagnostics_emergency.go`
  - 完整声明 emergency 的 included/omitted/unavailable 状态。
  - 写入进程内 HTTP、路由、provider、sidecar 和 passive-health 证据。
  - 消除 synthetic zero account/table counts。
- `internal/api/diagnostics_export.go`
  - 大表统计改成有界最近窗口，并在 manifest 中表达 exact/lower-bound 语义。
  - Codex mapping 与 instruction snapshot 导出有界化。
  - 修正 Goal live-run 统计。
- `internal/api/disk_guard.go`
  - 调整维护顺序并设置独立阶段期限。
  - 一次周期推进多个 expired/budget Goal reclaim phase，累计真实释放字节。
- `internal/storage/goal_continuity.go`
  - 新增有进度语义的单步回收 API；保留旧 count-only API 兼容性。
- `internal/storage/codex_session_mapping.go`
  - 新增最近窗口诊断读取 API，保留原无界兼容 API。
- `internal/storage/storage.go`
  - 通过 additive migration 增加 retention/diagnostics 索引，避免修改 PostgreSQL checksum-pinned base schema。

## 回归覆盖与验收

新增或更新的测试覆盖：

- 截断表的 lower-bound/exact manifest 语义及 newest-row 选择。
- emergency runtime HTTP/route/provider 证据、账户脱敏、omitted table 声明及 synthetic-zero 消除。
- `running` live lease 的 Goal active-run 计数。
- 一次 disk-guard 维护完成 expired Goal 的多阶段回收并记录 bytes。
- recent Codex mapping/snapshot 的 limit、顺序与零 limit。
- SQLite additive indexes 存在，Codex immutable PostgreSQL base schema 不含新增索引。

验证环境为 Go 1.25.12。核心包结果：

```text
go test ./internal/api ./internal/storage -count=1
ok  codex-account-pool/internal/api      290.769s
ok  codex-account-pool/internal/storage   34.431s
```

历史样本本身保持不变；修复面向下一次运行、维护周期和诊断导出。旧样本中缺失的 route/provider 事件没有可逆来源，因此报告仅记录已证明的 503 时间窗和分类，不推测具体账户或上游根因。

## 运行期后续复现与整改

在上述诊断修复完成后，对管理端复测、版本刷新和页面交互继续做了端到端复现：

| ID | 级别 | 复现结果 | 整改 |
|---|---|---|---|
| R-01 | 高 | 普通 402/429/额度错误会调用 `BenchBindingForRecheck`；账号凭据仍 alive，却在首个下游额度响应后显示“待复测” | 将容量错误改成只写权威 reset/cooldown；额度包装成 5xx 时也不累计服务故障 streak；只有连续未分类 5xx、CF 故障或显式 `cooldown_recheck` 规则进入复测 |
| R-02 | 高 | 手工模型测活返回 `ready=true` 时只清隔离，不清既有 `recheck_pending`；API-key 的付费推理测活同样遗漏 | 成功模型/推理探针原子清除 cooldown 与 recheck gate，并立即通知调度器；认证-only 结果不清推理门禁 |
| V-01 | 高 | Codex 官方版本只请求未认证 GitHub Releases API，HTTP 403 直接使整次兼容清单刷新失败 | 以实际 CLI 分发包 `@openai/codex/latest` 的 npm 元数据为主源；仅在主源失败/畸形时单次查询 GitHub fallback |
| V-02 | 中 | 并发刷新和进程重启可重复访问发布源；失败退避没有使用服务端 reset 时间 | 同配置刷新 singleflight；重启复用持久化 last-success 的下次刷新期限；失败采用节点抖动指数退避，并遵守最长 24 小时的 `Retry-After`/`X-RateLimit-Reset` |
| V-03 | 中 | 远端已发布版本可能领先本构建已验证的请求指纹，直接套用会形成混合 wire profile | 自动生效仅接受 `CodexCLIFingerprintForVersion` 精确命中的版本；未知未来版本可被发现，但保持当前已验证指纹，等待代码库适配 |
| U-01 | 高 | 账号列表每页依次执行 provider、capability、egress、group、usage、token、quota、reauth、Kiro 九组读取 | 在现有 SQLite WAL 多连接只读池上并发执行独立聚合，任一失败取消其余读取；空页零查询返回 |
| U-02 | 中 | React Query 后台刷新仍把 `loading=isFetching` 传给整张表，缓存数据被 skeleton 替换；路由就绪还人为等待 180 ms | 初始无数据才显示 loading；有缓存时保持交互并单独暴露 refreshing；后台 revalidate 不再撤销 page-ready，并移除固定 180 ms 延迟 |

版本拉取的回归使用内存 HTTP transport，不访问外部发布源：验证 npm 成功时 GitHub 调用为 0、npm 失败时 fallback 各调用 1 次、8 个并发刷新合并为各源 1 次，以及服务端最长退避时间被保留。账号状态回归覆盖取消请求后的容量冷却、5xx 包装额度、成功测活清门禁及显式复测规则；账户聚合通过 race 检查。

后续验证使用 Go 1.25.12：`internal/api` 全套 281.130 秒通过，`internal/compatmanifest`、`internal/supervisor` 及账户聚合 race 回归通过，`go vet ./...` 通过。前端既有 39 个测试文件/224 项测试全绿，新增后台刷新交互测试单独通过；静态 UI 合约、124 条路由布局探测、TypeScript 检查、生产构建和嵌入式 console release 校验均通过。
