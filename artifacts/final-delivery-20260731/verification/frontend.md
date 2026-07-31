# 前端 Apple 风格优化：云端验证记录

- 日期：2026-07-30（America/New_York）
- 执行位置：远端原生进程（未使用 Docker）
- 源码：`/root/autodl-tmp/frontend-ui-shot-20260731/optimized-src`
- 服务：`http://127.0.0.1:34274`
- 最终 release：`frontend-apple-final`

## 1. 完整前端门禁

命令：

```bash
PATH=/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin:$PATH \
npm --prefix /root/autodl-tmp/frontend-ui-shot-20260731/optimized-src/web-spa run verify
```

输入：最终 21 个变更/新增前端文件。

字面结果：

```text
Visual smoke check passed.
Test Files  15 passed (15)
Tests       73 passed (73)
✓ built in 463ms
FRONTEND_VERIFY_EXIT=0
```

退出状态：`0`

完整输出：`frontend-verify.literal.log`

## 2. 真实浏览器截图矩阵

命令：

```bash
PATH=/root/miniconda3/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
NODE_PATH=/root/autodl-tmp/frontend-ui-shot-20260731/browser-runner/node_modules \
/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin/node \
/root/autodl-tmp/frontend-ui-shot-20260731/browser-runner/capture-matrix-apple-final.mjs
```

输入：

- 35 个路由（30 个管理路由、登录页、4 个用户门户路由）
- 桌面浅色、桌面深色、移动浅色
- 1440×1000 与 390×844
- `prefers-reduced-motion: reduce`
- 远端 SQLite 演示数据（含超长邮箱、超长账号名称、90% 配额）

字面结果：

```text
SUMMARY total=105 passed=105 issues=0
```

退出状态：`0`

完整输出：`ui-matrix.literal.log`  
结构化报告：`ui-matrix-apple-final-report.json`  
截图归档：`ui-matrix-apple-final-screenshots.tar.gz`

## 3. 补丁验证

基线源码上的正向检查：

```bash
git -C /root/autodl-tmp/frontend-ui-shot-20260731/optimized-src \
apply --check \
/root/autodl-tmp/frontend-ui-shot-20260731/artifacts/frontend-apple-final/change.patch
```

字面结果：`PATCH_APPLY_CHECK_OK`  
退出状态：`0`

最终源码上的反向检查：

```bash
git -C /root/autodl-tmp/frontend-ui-shot-20260731/optimized-src \
apply --check --reverse \
/root/autodl-tmp/frontend-ui-shot-20260731/artifacts/frontend-apple-final/change.patch
```

退出状态：`0`  
完整输出：`patch-reverse-check.literal.log`

## 4. 可执行回滚

命令：

```bash
bash /root/autodl-tmp/frontend-ui-shot-20260731/artifacts/frontend-apple-final/rollback.sh
```

字面结果：

```text
ROLLBACK_OK pid=468392
```

退出状态：`0`

基线行为：

```text
settings: height=9938, categories=0
system: height=7300, compactRecords=0, visibleTables=2
accounts: mobileLists=0, quota90=false
common=true, behavior=true, ok=true
```

完整输出：`rollback-behavior.literal.log`  
健康记录：`rollback-health.json`

## 5. 最终重部署

命令：

```bash
bash /root/autodl-tmp/frontend-ui-shot-20260731/artifacts/frontend-apple-final/redeploy.sh
```

字面结果：

```text
REDEPLOY_OK pid=470990
```

退出状态：`0`

最终行为：

```text
settings: height=1261, categories=5, expandedCategories=1
system: height=4682, compactRecords=20, visibleTables=0, mobileLists=2
accounts: height=3232, mobileLists=1, quota90=true
common=true, behavior=true, ok=true
```

完整输出：`redeploy-behavior.literal.log`  
健康记录：`redeploy-health.json`

## 6. 数据验证

查询：

```sql
SELECT email,status,group_name
FROM email_pool
ORDER BY created_at
LIMIT 3;

SELECT a.id,a.label,a.email,r.used_percent,r.remaining_tokens,r.status
FROM accounts a
JOIN account_rate_limits r ON r.account_id=a.id
WHERE r.used_percent >= 90
ORDER BY r.used_percent DESC
LIMIT 3;
```

已确认：

- 超长邮箱完整保存在数据库中；
- `used_percent=90.0`；
- 剩余 token 分别为 `200000`、`500000`；
- 邮箱池同时含 ready/error 等状态。

完整字面输出：`database-verification.literal.log`

## 7. 角色与哈希

```text
c86ca322cd55a994b6af707677eac1b8586f7d9363a3f6aa115475161307265e  codex-pool-server.optimized
9922a7737f842609529766efba3b717b3fac3658ebd27813c2a810fe05d5a66c  pool.optimized.sqlite3
07862a5ff14392ed3fef01b6209cbfccb88e820fc1a66398891372ee4608ad1b  change.patch
6e59922a5925879cb06938c07ef5d44113d21f0a5821fa6144983e50589f0aee  rollback.sh
f0a4f8a1efb29317ea3eee5693714a8ea321ceafcbb4bd89e4f4f2e40f7fa8eb  redeploy.sh
e1e73614c51da979e4adc141ae0c85c5bcef75477772ce70f87f03c8a4f1a217  ui-matrix-apple-final-screenshots.tar.gz
```

原始文件：16 个。  
最终文件：21 个。  
最终源码副本与远端运行源码比对：`source_mismatches=[]`。
