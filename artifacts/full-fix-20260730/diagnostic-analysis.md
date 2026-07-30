# 诊断包分析、根因与修复状态

分析日期：2026-07-30  
机器或账号标识：全部省略  
机器可读核验：[`diagnostic-archive-integrity.json`](./diagnostic-archive-integrity.json)  
对应源文件哈希：[`diagnostic-source-sha256.txt`](./diagnostic-source-sha256.txt)，SHA-256 `c3a50ec29fc50381e71553ef60c20a2e60b7e076bf22817795e8e18b5c531a23`

## 1. 结论

前端持续转圈的主因不是服务离线、内存耗尽、inode 耗尽或 ZIP 损坏，而是旧版诊断任务在繁忙的多 GiB SQLite 上执行**物理数据库复制**：

1. 诊断任务进入 `snapshotting`；
2. 并发写入令 `sqlite3_backup_step` 反复重启或长期无法收敛；
3. 未完成的物理快照增长到 1.10 GB，同时长读/复制过程令 WAL 增长到 2.28 GB；
4. 根文件系统当时仅剩 3.2 GB，任务一直没有进入可下载的 `ready` 状态；
5. 浏览器只能持续轮询，所以界面表现为一直转圈。

诊断包内还证明了第二个放大因素：旧版导出会把追加型大表完整写入 ZIP。500,000 条 `usage_records` 加 200,000 条 `audit_log` 的基线导出耗时 110.714 秒，其中 `validating` 阶段开始于 92.540 秒；这会显著延长浏览器等待，但不是本次 `snapshotting` 卡死的第一原因。

当前源代码已经将物理复制替换为一致性逻辑读快照，并加入大表上限、WAL/磁盘保护、旧快照回收、服务端截止时间和前端下载闭环。诊断包采集时运行的是旧发布 `20260729T180203Z-3b484ab1d114-54547`，因此包内现场本身不代表新代码已经部署。

## 2. 归档完整性

所有指定归档均通过完整读取、路径安全和 CRC/内部 SHA-256 核验。

| 归档 | SHA-256 | 核验结果 |
|---|---|---|
| `example_zip/codex-pool-vps-diagnostics.tar.gz` | `90b53b1ae13b4522f687418eca925fd81d1d17f7c935a121a6858a6dc1cecfea` | 37 个条目、32 个普通文件可完整读取；31 个内部 SHA-256 全部匹配 |
| `verification/codex-pool-diagnostics-v3-canary.zip` | `87fc25c5c3e8c24467fc48d058f3b636b09727681756a77fbeb0ed4e13a9a751` | 30 个条目，ZIP CRC 全部通过 |
| `artifacts/optimization-20260729/pre-fix-diagnostics.zip` | `504f2a41eca5b82333a875993b84f63d79984966bb0df36a7c55782b05a37691` | 30 个条目，ZIP CRC 全部通过 |
| `artifacts/optimization-20260729/pre-fix-large-diagnostics.zip` | `357e05e6c7b1871da27c979f40e856bbdd67804f7c553e2507c2db73d22b3f9e` | 30 个条目，102,591,296 字节解压数据，ZIP CRC 全部通过 |
| `remote-evidence/perf-pre-fix/pre-fix-diagnostics.zip` | `2c704ddf6f10340681c07b9d748c9f2dbf3a3e45a598c88b02213b2a6a1d1171` | 200,000/500,000 行基线，ZIP CRC 全部通过 |
| `remote-evidence/perf-final/final-diagnostics.zip` | `5807f7fbaa08ee32efc928ff6f8848ac5ab74995ad738371825510c8719d0110` | 每个大表 20,000 行，manifest 记录源行数与省略行数，ZIP CRC 全部通过 |

因此，已有 ZIP 的容器结构与 CRC 均正常；现场故障发生在归档生成状态机到达 `ready` 之前。

## 3. 现场证据

### 3.1 服务仍在线

- `/livez`、`/readyz`、`/standbyz` 均返回 HTTP 200。
- `readyz` 报告 `storage=true`、`ready=true`，采集瞬间有 9 个 inflight 请求。
- worker、handoff、sidecar 均为 active/running。

这排除了“服务整体离线导致转圈”这一分支。

### 3.2 磁盘与 SQLite

| 项目 | 现场值 |
|---|---:|
| 根文件系统 | 20 GB 总量，17 GB 已用，3.2 GB 可用，84% |
| inode | 12% 已用 |
| `pool.sqlite3` | 2,315,563,008 字节 |
| `pool.sqlite3-wal` | 2,284,853,152 字节 |
| `pool.sqlite3-shm` | 4,456,448 字节 |
| 残留 `.diagnostic-snapshot-*.sqlite3` | 1,102,053,376 字节 |
| 残留 snapshot journal | 1,024 字节 |
| `/var/lib/codex-pool` | 5.6 GB |
| SQLite `quick_check` | `ok` |
| `journal_mode` | `wal` |
| `auto_vacuum` | `0` |
| `freelist_count` | 1,067 页，约 4.17 MiB |

主库、WAL、未完成快照和 SHM 合计已经接近整个 5.6 GB 数据目录。物理复制若继续向 2.315 GB 主库大小靠近，将与仅 3.2 GB 的剩余空间直接冲突。

### 3.3 卡住的任务

- `diagnostic_jobs`：2 个 cancelled、4 个 ready、1 个 snapshotting。
- snapshotting 任务起始于 `2026-07-30T02:30:23Z`。
- VPS 包创建于 `2026-07-30T02:34:55Z`。
- 采集时该任务已停留在 snapshotting 至少 272 秒。
- 同时存在一个 0 字节 `.partial` ZIP 和一组 1.10 GB 的物理 SQLite 快照残留。

任务状态、文件时间窗口和残留快照三者互相印证。

### 3.4 数据库增长来源

行数最高的表包括：

- `codex_upstream_attempt`: 169,362
- `usage_events`: 50,918
- `user_group_target_bindings`: 46,127
- `audit_log`: 44,794
- `codex_session_alias`: 39,693
- `goal_payload_chunk`: 32,204
- `usage_records`: 10,497
- `billing_holds`: 10,697
- `goal_alias`: 9,706
- `affinity_aliases`: 7,957

`goal_payload_chunk` 共有 32,204 行，每块最大 64 KiB；它与约 2.3 GB 主库大小高度一致，是主库占用的强相关来源。VPS 包没有携带 `dbstat` 页面归属明细，因此这里是有证据支持的推断，不把它表述为精确页面归因。

### 3.5 资源并非当时的首要瓶颈

- 内存 7.8 GiB，总可用约 7.0 GiB；无 swap。
- worker 的 systemd `MemoryCurrent` 约 2.40 GiB。
- `limits.txt` 中采集命令环境的 open-files soft limit 为 1,024；包内没有单独给出 worker 进程的 `LimitNOFILE`。

内存余量表明本次卡死的第一原因仍是快照/WAL/磁盘路径。无 swap 是多下游高并发时的容量风险；worker 的实际文件描述符上限则需要从 systemd unit 单独确认。

## 4. 已落地修复

### 4.1 诊断生成

1. **逻辑一致性快照**  
   `internal/storage/diagnostic_snapshot.go:17-24,184-227` 使用专用只读连接和 SQLite WAL 读事务固定边界，不再复制整个数据库。

2. **旧物理快照安全回收**  
   `internal/storage/diagnostic_snapshot.go:67-181` 只处理精确私有前缀的普通文件族，检查同 UID 打开文件和 inode 身份后再删除，避免误删活动快照。

3. **WAL 与磁盘保护**  
   `internal/api/diagnostic_jobs.go:30-42,534-627` 每 500 ms 检查可用空间，保留文件系统安全余量，并把单次诊断期间 WAL 增量限制为最多 512 MiB 或剩余 headroom 的一半。

4. **WAL 收尾**  
   `internal/api/diagnostic_jobs.go:290-332` 对超过 256 MiB 的 WAL 做最多 2 秒的非阻塞 `TRUNCATE` checkpoint；繁忙时留待下一次维护循环重试。

5. **有界状态机**  
   服务端运行截止时间为 4 分 30 秒；前端运行等待为 5 分钟、队列等待为 10 分钟。服务端会先进入明确的 timeout/failed 状态，前端不再无限轮询。

6. **大表导出上限与可审计 manifest**  
   `internal/api/diagnostics_export.go:201-205,528-541,845-878` 每个追加型大表导出最近 20,000 行，同时记录 `source_row_counts`、`truncated_tables` 和 `large_table_row_limit`。支持信息仍可判定“源表有多少、导出了多少、按什么顺序取样”。

### 4.2 浏览器下载闭环

- `exports.ts:170-205` 校验下载 URL 同源、路径、ZIP Content-Type、最小长度和 `PK` 签名。
- `exports.ts:207-222` 直接接收 Blob，避免 ArrayBuffer + Blob 的双份峰值内存。
- `exports.ts:224-330` 区分 queued/running 超时、识别未知/终态，并在中止后用独立请求回收服务端任务。
- `browserDownload.js:3-54` 点击后保留 object URL 1 秒再撤销，消除 Firefox/Safari 中同步撤销导致“显示成功但没有文件”的竞态。

已有真实 Chrome 验证结果：3,087 ms 下载 9,415 字节，签名 `504b0304`，按钮恢复可点击，退出状态为 0。

### 4.3 性能结果

同一份 500,000 条 usage + 200,000 条 audit 数据：

| 指标 | 修复前 | 修复后 |
|---|---:|---:|
| 总耗时 | 110.714 s | 5.246 s |
| ZIP 大小 | 3,991,141 B | 239,466 B |
| usage 导出行 | 500,000 | 20,000 |
| audit 导出行 | 200,000 | 20,000 |
| manifest 源行数 | 未记录截断 | 完整记录 500,000 + 200,000 |

速度提升 21.10 倍，耗时下降 95.26%，归档大小下降 94.00%。

### 4.4 上下文与磁盘空间

- `internal/storage/context_journal.go:156-293` 使用 `ctx2:r`/`ctx2:g` 长度封套；大于等于 1 KiB 时仅在 gzip 确实变小时采用压缩。
- 解压为流式有界读取，并核对声明长度；goal chunk 迁移还核对原始 `payload_bytes` 和 payload hash。
- `internal/storage/context_compression_migration.go` 每批最多 64 行或 8 MiB，逐批验证、更新、提交，避免一次性把 1M 级上下文装入内存。
- `cmd/pool-server/deferred_migrations.go` 在 active 角色后台串行重试，切换角色后可继续未完成迁移。
- `scripts/maintenance/reclaim-disk-space.sh` 默认为 dry-run；apply 前要求 SQLite quiescent，先做完整备份和不变量校验，再归档过期日志、清理缓存/诊断残留、checkpoint 和 VACUUM。账号、凭证、cookies、账号分组/出口绑定、goal/context/session、虚拟上下文账本、保留期内日志、API keys 与静态配置均列入保存与校验范围。

磁盘脚本的独立说明和远端验证位于：

- `scripts/maintenance/README-reclaim-disk-space.md`
- `artifacts/full-fix-20260730/disk-reclaim/remote-verification.txt`
- `artifacts/full-fix-20260730/disk-reclaim/verification.json`

### 4.5 1M 上下文、账号切换与压缩回归

远端低优先级回归把四项测试连续执行三轮：

- GPT-5.6：8 个并发下游各发送 1 MiB 模型可见上下文（约 250K–300K tokens，处于固定 372K 合同内），先绑定账号 A，再在 A 失败后并发切到 B；
- Claude Code：8 个并发 session 各发送 1,000,000 个 token-like 单元，再以只含 `tool_result` 的增量触发 A→B 切换；
- goal/context 与 virtual ledger 压缩后逐字节解压回放；
- legacy row 延迟迁移恰好执行一次，第二次运行不重写已压缩数据。

每一轮都验证了上下文 digest、下游之间零串线、tool call/result ID 一一对应且顺序正确、大整数参数不失真、切换后的 self-contained 重建，以及压缩存储小于逻辑数据的一半。三轮全部通过：

```text
internal/storage: PASS, 0.513s
internal/api:     PASS, 17.018s
EXIT_STATUS=0
```

日志：`artifacts/full-fix-20260730/remote-tests/context-stress-v7-count3.log`，SHA-256 `7e99ef23bbd858b9207cc7850b493c4602ed1245b7e2f74bffbe1ac28f807f44`。

## 5. 脱敏文件清单

VPS 包 manifest 声明并实际符合：

- `application_bundle_included=false`
- 配置内容、数据库、密钥内容、进程参数均未复制
- `dlp_scanned=true`
- `text_redacted=true`

包内仅包含：

- `system/`：内存、系统版本、进程摘要、uptime、mount、limits、监听端口、uname、磁盘与 inode；
- `service/`：worker/sidecar/handoff 日志、unit 定义与状态、active worker link、live/ready/standby 响应；
- `storage/`：指定目录的 stat、空间用量、文件布局、SQLite 元数据；
- `manifest.json` 与 `checksums.sha256`；
- 空的 `application/` 目录。

高熵文件名、主机和 IP 已替换为占位符。仍然保留的运维元数据包括目录结构、文件大小/权限/owner、服务版本、模型名称、时间戳、表行数、端口和服务拓扑；该包适合受控支持流转，不适合作为公开附件。

额外对 tar 与 5 个 ZIP 的全部普通文件执行 credential/identity 模式扫描，潜在敏感值命中为 0。扫描器另外识别并归类了两类非敏感形状：13 个 systemd template instance（`unit@instance.service`）被通用 email 正则命中，以及每个 ZIP 中 1 个 JSON schema key 被通用 key-prefix 正则命中；两者均位于标识符/key，不是账号或凭证值。机器可读分类保存在 `diagnostic-archive-integrity.json`。

## 6. 部署后重点确认

1. 新 worker release 已替换诊断包中的旧发布，并且 active worker link 指向新 release。
2. 首次维护循环已回收 `.diagnostic-snapshot-*` 文件族；WAL 在读事务释放后回落。
3. 运行 dry-run 检查备份盘容量，再在 quiescent 窗口执行磁盘回收；记录 apply 与 rollback 文件的 SHA-256。
4. 观察 `context_payload_compression_ctx2` 迁移完成标记、数据库/WAL 大小和可用磁盘。
5. 先读取 worker 的 systemd `LimitNOFILE`；若仍为 1,024，高并发环境把它设为经过容量测试的值（建议基线至少 65,536），同时监控实际 FD、连接池、inflight 与内存，而不是只提高单一上限。
6. 保留至少 10% 或 1 GiB（取较大值）的文件系统余量；诊断任务的 guard 已按相同原则执行。

## 7. 最终判断

诊断归档自身完整；故障链为“旧物理 SQLite 快照在并发写入下不收敛 → 快照与 WAL 同时膨胀 → 任务停在 snapshotting → 前端持续轮询”。当前逻辑快照、行数上限、磁盘/WAL guard、超时状态机和浏览器 Blob 下载修复分别切断了这条链上的生成、容量、状态和下载竞态。
