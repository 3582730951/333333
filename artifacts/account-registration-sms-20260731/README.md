# 账号契约、注册体验与接码市场交付

本目录是 2026-07-31 远端原生环境的可复核交付包。运行环境未使用 Docker，数据库由
SQLite 直接填充，截图来自同一远端进程。

## 已完成

- 修复旧数据库可空账号身份字段导致整个账号池读取失败的问题。
- 账号接口声明 `admin.accounts.v1`，前端保留请求 ID，并区分 HTML 回退、错误对象与具体字段类型错误。
- 住宅代理支持四种导出格式并在失焦时规范化、掩码预览。
- 注册页解释协议、浏览器与 Node 引擎；以 Apple 风格卡片展示接码价格、库存、历史样本和决策依据。
- 管理员可设置最低/最高接码单价及首选国家；每小时扫描全国家市场并持久化。
- 自动选择以近 14 天成功率为主，结合价格、平台排名和冷启动社区顺序；购买时再次执行价格边界。
- Team 生命周期表单改为从账号池选择母号/子号，原生连接器自动解析 workspace ID，并展示完整循环。
- 浏览器注册与 Codex OAuth `add_phone` 使用同一个已选择国家，避免代理地区与手机号地区漂移。

## 核心证据

- `diagnostic-analysis.md`：诊断包及账号池根因。
- `account-null-baseline-modified.txt`：同一回归测试的旧实现失败/修复后成功对照。
- `runtime-api-verification.json`：远端 API 契约、账号数、接码决策及生命周期数据。
- `remote-test-verification.txt`：Go、Python、前端测试和构建的命令、输出、退出状态。
- `ui-verification.json`：18 张明暗/桌面/移动截图的自动检查，18/18 通过。
- `screenshots/`：真实页面截图。
- `modified-source.tar.gz`：45 个最终修改源文件的可复核归档。
- `source.patch`：本批源代码补丁。
- `rollback.sh`：补丁级回滚。

## 代表截图

- `screenshots/desktop-light-registration-engines-sms-market.png`
- `screenshots/desktop-dark-registration-engines-sms-market.png`
- `screenshots/desktop-light-team-workspace-guided-form.png`
- `screenshots/desktop-dark-team-lifecycle-overview.png`
- `screenshots/desktop-light-egress-four-format-parser.png`
- `screenshots/mobile-dark-accounts-long-identities.png`
