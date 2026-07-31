# 账号池诊断分析

## 输入清点

当前工作区 `example_zip/` 实际可见一份压缩包：

- `codex-pool-diagnostics-v3-diagjob_e79ca0f91f2b4be1a62e5474d49e6cf4.zip`
- SHA-256：`ad9541db76eed065ae97b721669b537ecaf28ead040510a8d37695a5aa1a9700`
- 格式：`codex-pool-diagnostics-v3`
- 构建修订：`96155fab1aa474f9c9844eacccaf58f77776f947`
- 当前账号：15；导出条目：30

用户先前提到两份包，但本次文件系统复核只有上述一份；该包内所有条目均已检查。

## 包内证据边界

1. `accounts_snapshot.csv` 有 15 行，但为脱敏快照，不包含
   `upstream_account_id`、`chatgpt_user_id`、`email` 的原始空值形态。
2. 包内没有浏览器收到的 `/admin/accounts` 原始响应、响应 Content-Type、
   `X-Response-Contract` 或当次 `X-Request-ID`，因此只凭该包不能定位是哪一个 JSON 字段触发前端解析失败。
3. `account_rate_limits.csv` 有 52 行，且多个账号处于 cooldown/recheck；这是额度与调度状态，
   不会把合法账号 JSON 变成“无法识别的数据”，属于并行问题而非本错误根因。

## 已确认根因

远端原生升级夹具保留了旧数据库的合法可空形态，并由新增回归测试精确复现：

```text
BASELINE EXIT_STATUS=1
sql: Scan error on column index 3, name "upstream_account_id": converting NULL to string is unsupported

MODIFIED EXIT_STATUS=0
EXPECTED_BASELINE_FAILURE_AND_MODIFIED_SUCCESS=true
```

完整字面输出位于 `account-null-baseline-modified.txt`。

根因链如下：

1. 初始 `accounts` 表将 `upstream_account_id`、`chatgpt_user_id`、`email`、`plan_type`
   定义为可空列；旧导入器或管理员直填数据库会留下 SQL `NULL`。
2. `scanAccount` 过去把这些列直接扫描到 Go `string`，任意一行出现 `NULL` 都会终止整个列表查询。
3. 旧前端对部分可空/数字化字段过严，并在响应契约失败时只显示统一文案；旧请求封装还丢失响应头，
   因而用户看不到真实字段和请求 ID。

## 修复闭环

- 存储层用 `sql.NullString` 接收四个历史可空字段，并统一映射为空字符串；列表、分页和单账号读取均覆盖。
- `/admin/accounts` 增加 `X-Response-Contract: admin.accounts.v1`；分页体增加 `contract_version`。
- 前端兼容 nullable 字符串、0/1 布尔、数字 ID、nullable capabilities/usage。
- 对象响应必须实际含账号数组，错误对象不再静默变成空账号池。
- 前端保留 `X-Request-ID`，分别提示反向代理 HTML 回退、文本、错误对象或具体字段路径。
- 远端数据库保留含 SQL `NULL` 的演示账号后，接口返回 200、17 个账号、契约头正确且请求 ID 存在。

## 诊断包后续建议

后续诊断导出应额外记录：账号响应契约版本、Content-Type、状态码、请求 ID、顶层 JSON 形状，
以及可空身份字段的“空值计数”（不导出原值）。这样再次发生版本错配时可直接从包内定界。

