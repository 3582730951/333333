# 用户分组流量兜底交付

本批次为用户分组增加按 GPT / Claude / Gemini 家族配置的跨分组流量兜底，
并支持多条“来源模型 → 目标用户分组 → 目标模型”转换规则。工作区没有
`docs/plan/1.md`，实现与验证依据实际存在的 `docs/plan/1.txt`，遵循其中的
Apple 风格、深浅主题、响应式、兼容、可回滚和风险驱动测试要求。

## 已实现

- 单一入口、两级 GPT / Claude / Gemini 选择器；每个家族可按顺序勾选多个兜底用户分组。
- 多条模型转换；来源和目标模型均接入 `/admin/models` 目录，同时保留手动输入。
- 精确匹配优先于末尾通配符，兜底分组按配置顺序尝试。
- 来源分组全部候选失败后，使用原始请求体改写目标模型并重新进入目标分组策略。
- 已提交的流、带服务端会话状态的请求不跨分组重放；最大深度为 8。
- 保存时校验目标存在、自引用、缺失映射、重复映射、通配符、长度和全图循环。
- 被引用的用户分组不可删除；迁移为旧记录补 `{}` / `[]` 默认值。
- Apple 风格高层级信息卡、长名称省略、键盘焦点、深浅主题及移动端单列布局。
- 修复了 Radix Portal 在模态框中可见但点击穿透的问题：浮层高于 modal 2001，且显式恢复 pointer events。

## 关键证据

- 完整验证：[`verification-record.md`](./verification-record.md)
- 最终截图报告：[`records/screenshot-report.json`](./records/screenshot-report.json)
- 数据填充清单：[`records/seed-manifest.json`](./records/seed-manifest.json)
- 旧库迁移前后：[`records/legacy-schema-before.json`](./records/legacy-schema-before.json)、
  [`records/legacy-schema-after.json`](./records/legacy-schema-after.json)
- 回滚与恢复：[`records/rollback-restore.status`](./records/rollback-restore.status)
- 修改补丁：[`traffic-fallback.patch`](./traffic-fallback.patch)
- 可执行回滚：[`rollback.sh`](./rollback.sh)

## 四个交付角色

1. **修改产物**：`traffic-fallback-modified-source.tar.gz`、`traffic-fallback-modified-console-dist.tar.gz`
2. **补丁**：`traffic-fallback.patch`
3. **验证记录**：`verification-record.md` 与 `records/`
4. **回滚**：`rollback.sh`、基线源码包和基线控制台包

回滚及恢复命令：

```bash
artifacts/traffic-fallback-20260731/rollback.sh rollback
artifacts/traffic-fallback-20260731/rollback.sh restore
```
