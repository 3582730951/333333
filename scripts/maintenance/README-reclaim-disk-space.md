# 一次性磁盘空间释放

脚本：`scripts/maintenance/reclaim-disk-space.sh`

## 保留边界

脚本把数据分成三类不可破坏集合，并在执行前后记录行数与 SHA-256：

1. 账号：`accounts`、认证令牌、Kiro/Antigravity 凭据、Cookie、重新认证配置、账号出口绑定、分组成员关系、生命周期/限额状态。
2. 当前上下文：`context_journal`、`virtual_context_ledger`、全部 Goal checkpoint/segment/chunk/run，以及 Codex session binding/alias/instruction snapshot。
3. 日志：保留期内的审计、用量、注册、生命周期、代理、质量和团队日志。超过保留期的行会先逐表导出为 `codex-pool-maintenance-jsonl-v1`，gzip 压缩、完整读取校验并记录行数/双 SHA-256，随后才从在线数据库删除。

`journal/` 下的 durable usage journal、`keys/`、用户/API Key、分组、Provider、出口和静态设置始终保留。脚本也不会操作 journald、`/var/log` 或任何主机级缓存。

直接释放的对象仅包括：

- 诊断 ZIP、诊断任务/事件/租约；
- 已过期的模型发现、Kiro/Antigravity、route affinity 缓存；
- 已过期的用户 session、maintenance lease 和旧的终态 reauth job；
- 超过指定年龄的请求 spool、浏览器临时目录和遗留诊断快照；
- SQLite WAL/free pages，或 PostgreSQL 大日志表中的死元组。

## 推荐执行顺序

先 dry-run，不会创建备份或修改任何文件：

```bash
sudo ./scripts/maintenance/reclaim-disk-space.sh \
  --config /etc/codex-pool/config.json \
  --service codex-pool.service \
  --backup-dir /mnt/backup/codex-pool
```

确认输出的候选行数、文件数和备份位置，再执行：

```bash
sudo ./scripts/maintenance/reclaim-disk-space.sh \
  --apply \
  --config /etc/codex-pool/config.json \
  --service codex-pool.service \
  --stop-service \
  --backup-dir /mnt/backup/codex-pool \
  --optimize-config
```

也可以直接指定 SQLite：

```bash
sudo ./scripts/maintenance/reclaim-disk-space.sh \
  --apply \
  --db /var/lib/codex-pool/pool.sqlite3 \
  --data-dir /var/lib/codex-pool/data \
  --assume-quiesced \
  --backup-dir /mnt/backup/codex-pool
```

PostgreSQL 使用 `--postgres-dsn`；DSN 不会出现在报告中。未由脚本停止服务时，PostgreSQL 的 apply 必须显式加 `--assume-quiesced`。

## 空间配置优化

`--optimize-config` 会先备份 JSON 配置，再原子替换。它：

- 保留至少 7 天 Goal/Codex session；
- 根据当前实际数据量设置带 20% 余量的 context/Goal 上限，绝不把上限降到当前占用以下；
- 使用 8 MiB usage journal segment；
- 保留至少 512 MiB body spool 紧急空间。

只有同时满足以下条件时，SQLite 模式才会关闭未来的 legacy 整段快照双写：

1. 配置明确启用 `goal_continuity_enabled`；
2. 每个未过期 `context_journal.response_id` 都能唯一映射到未过期、未 reclaiming 的 Goal；
3. 每个映射 Goal 都存在 checkpoint payload，且未压缩进 checkpoint 的所有 segment 都有 payload。

证明成立后，脚本同时写入运行时 setting
`goal_legacy_journal_dual_write=false` 和配置文件。证明不完整时保持原值，并把原因和缺失 alias 的不可逆哈希前缀写入报告。已有 `context_journal` 行不删除，继续作为读取回退并自然按 TTL 到期。PostgreSQL 离线模式不尝试这项证明，因此保持双写设置不变。

## 回滚与验证

SQLite apply 会在修改前建立并 `quick_check` 完整数据库备份。所有数据库删除在一个事务内完成；事务提交后依次执行 WAL truncate、`VACUUM`、`PRAGMA optimize`、再次 `quick_check` 和三类 SHA-256 校验。后续步骤失败时，脚本会在服务重启前自动恢复原库。

成功报告位于：

```text
BACKUP_DIR/reclaim-YYYYMMDDTHHMMSSZ/verification.json
```

完整回滚备份为 `*.before.sqlite3.gz`。手动回滚示例：

```bash
sudo ./scripts/maintenance/reclaim-disk-space.sh \
  --apply \
  --db /var/lib/codex-pool/pool.sqlite3 \
  --data-dir /var/lib/codex-pool/data \
  --rollback /mnt/backup/codex-pool/reclaim-TIMESTAMP/pool.sqlite3.before.sqlite3.gz \
  --service codex-pool.service \
  --stop-service
```

PostgreSQL 生成 custom-format `postgres-before.dump`，SQL 出错会自动回滚事务；显式 `--rollback FILE --apply` 使用 `pg_restore --clean --if-exists --single-transaction` 恢复。备份目录应优先放在另一块磁盘；SQLite 完整回滚镜像在压缩前仍需要接近原数据库大小的临时可用空间。
