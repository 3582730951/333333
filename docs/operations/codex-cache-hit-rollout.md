# Codex prompt-cache 无损优化发布手册

## 运行时开关

| 设置 | 默认值 | 范围/模式 | 回滚值 |
| --- | --- | --- | --- |
| `codex_prompt_cache_key_shards` | `4` | `1..16` | `1`（旧 key 行为） |
| `codex_cache_singleflight_enabled` | `true` | boolean | `false`（停止协调） |
| `codex_gpt56_explicit_cache_mode` | `observe` | `observe/auto/off` | `off` |

`observe` 仅在原生 API-key Responses transport 的双请求探针状态为
`supported` 时透传客户端断点。`auto` 还要求第二次缓存读取 token 大于第一次
写入 token 的 `1.25x`；否则保持 observe 行为。ChatGPT Codex transport 始终清除
其不支持的 `prompt_cache_options` 和嵌套 breakpoint。

## 计量与诊断

- `usage.input_tokens_details.cache_write_tokens` 映射为
  `cache_creation_tokens`；Chat-compatible 的 `prompt_tokens_details` 同样支持。
- `cache_creation_present` 按字段存在性记录，显式 `0` 仍表示上游报告了指标。
- `usage_records` 记录按部署密钥、账号和模型域隔离的截断 HMAC `prompt_cache_key_hash`、分片号、分钟 RPM、并发峰值、
  协调前缀来源、singleflight 等待原因和释放原因。
- `codex_cache_capabilities` 持久化 API-key + 模型级双请求探针结果；进程重启后按需
  载入内存缓存。
- cache-hits 与 diagnostics ZIP manifest 均声明 cache-write 字段映射。

比较发布前后数据时使用完整且等长窗口，并等待 usage journal 清空。真实 token 命中率：

```sql
SELECT
  1.0 * SUM(cache_read_tokens) /
  NULLIF(SUM(CASE WHEN cache_total_input_tokens > 0
                  THEN cache_total_input_tokens ELSE prompt_tokens END), 0)
FROM usage_records
WHERE estimated = 0 AND usage_source = 'upstream'
  AND created_at >= ? AND created_at < ?;
```

热 key 检查：

```sql
SELECT prompt_cache_key_hash, prompt_cache_key_shard,
       MAX(prompt_cache_key_minute_rpm) AS max_rpm,
       MAX(prompt_cache_key_concurrency_peak) AS peak_concurrency,
       SUM(singleflight_waited_requests) AS waits
FROM usage_records
WHERE created_at >= ? AND created_at < ?
GROUP BY prompt_cache_key_hash, prompt_cache_key_shard
ORDER BY max_rpm DESC;
```

## 发布与门禁

1. 全量发布计量字段映射和诊断列。
2. key 分片 + 精确前缀 singleflight：`5% → 25% → 100%`。
3. 原生 API-key 显式断点：`observe` 完成双请求探针；满足收益门槛后
   `auto` 按 `5% → 25% → 100%` 灰度。
4. 每阶段比较全局/热路由命中率、首 token p95、模型/effort/context/tool 历史差异。

门禁：全局真实 token 命中率不低于 88%，热扇出路由不低于 80%，p95 首 token
不高于基线 5%，按 `1.25x` 写入成本计算净收益为正；请求结构、模型、reasoning
effort、工具 schema/order、`previous_response_id`、output/reasoning/tool items 差异为零。

任一门禁触发时，仅回滚对应设置：先将显式模式设为 `off`，再关闭 singleflight，
最后将分片数设为 `1`。计量字段保持启用，以便确认回滚效果。
