# 远端原生验证记录

## 环境

- 模式：远端原生进程，不使用 Docker。
- 服务：`127.0.0.1:34321`。
- 数据库：直接 SQLite 填充；填充前已保存在线备份。
- 最终二进制 SHA-256：`0ae17ee2b53ec494e2b54ea9a58b4a2820a9122b807e777a7f074d0816bfad86`。
- 修改源文件归档：`modified-source.tar.gz`，45 个文件，SHA-256
  `d5cf9a178c5add9088608cc29f63fb78ec172bd4e5aca72d439b53647bcc571b`。

## 数据填充结果

字面结果：

```json
{"counts":{"accounts":17,"sms_history":60,"sms_prices":8,"team_workflows":1,"team_workspaces":1},"seeded":true}
```

市场样例含 HeroSMS/SMSBower 的 BR、CO、PL、PH/ID；历史记录覆盖成功与失败，价格范围设为
`0.02–0.08 USD`。

## 基线与修复行为

命令（旧实现与修复实现使用同一测试、同一输入）：

```bash
go test ./internal/storage -run TestAccountReadersNormalizeLegacyNullableIdentityFields -count=1
```

- 基线：退出 `1`，SQL `NULL` 扫描到字符串失败。
- 修复：退出 `0`。
- 字面输出：`account-null-baseline-modified.txt`。

## API 结果

`runtime-api-verification.json` 的断言全部通过：

- `/admin/accounts`：HTTP 200，17 行，`admin.accounts.v1`，存在请求 ID。
- `/admin/register/sms-market`：HTTP 200，8 个候选，数据新鲜。
- 自动首选：`herosms / BR`，近 14 天 `11/12`，依据 `historical_success_rate`。
- 价格边界：最低 `0.02`、最高 `0.08`。
- Team 生命周期：1 个 workspace、1 个 workflow，接口均为 200。

## 测试和构建

所有命令在远端运行，完整字面输出和每步退出状态见 `remote-test-verification.txt`：

| 检查 | 结果 |
|---|---:|
| Go 可空账号回归 | exit 0 |
| Go 住宅代理解析 | exit 0 |
| Go SMS provider 全包 | exit 0 |
| Go 注册 pipeline | exit 0 |
| Go SMS market / Team API | exit 0 |
| Python 三个注册 worker `py_compile` | exit 0 |
| 前端契约测试 | 44/44，exit 0 |
| 前端 typecheck + production build | exit 0 |
| 9999 未排名哨兵回归 | exit 0 |

此前要求的旧分支 → 新版 `install.sh` 升级、数据保持、回滚与再部署证据仍位于：

- `artifacts/final-delivery-20260731/verification/install-upgrade.md`
- `artifacts/final-delivery-20260731/verification/install-upgrade-summary.json`

## UI 截图矩阵

`ui-verification.json`：

```text
total=18 passed=18 issues=0
desktop-light=6 desktop-dark=6 mobile-light=3 mobile-dark=3
```

自动检查包括：页面 ready、无本服务 4xx/5xx、无 console error、无可见错误条、无骨架残留、
无页面级横向溢出、主题与 localStorage 一致、账号池未出现旧通用错误、关键文案存在。

## 已验证行为

**基线：** 任一旧账号身份列为 SQL `NULL` 时，账号列表整体失败，前端只能显示通用不兼容提示。

**修改后：** 同一数据库行被规范化为空字符串，账号列表返回 200；协议引擎、接码市场、生命周期表单、
四格式代理解析在明暗和移动视图均正常呈现。

## 回滚演练

`rollback.sh` 已实际执行，随后重新应用 `source.patch`；两步均退出 0。恢复后的暂存源代码 diff
SHA-256 为 `dc74e5371a70b111edb5c1a6f8a424ad9db6ca16630600cb9791e8ca9f1fb422`，
与演练前一致。
