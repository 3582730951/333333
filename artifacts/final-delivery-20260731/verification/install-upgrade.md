# 旧版安装 → 数据填充 → 新版 `install.sh` 升级复核

## 范围

- 旧分支：`cache-hit-optimization`
- 旧提交：`390edea477cb4d16b45133096bf1351d7d593db9`
- 环境：远端 Linux 原生进程，未使用 Docker，隔离端口 `34276`
- 旧、新顶层 `install.sh` SHA-256：
  `ad7071de1d2de4e4ea5e3d3f5586c0e4f52d578317f63049ee8e4390ce697fee`

## 旧版安装与运行

执行：

```bash
SOURCE_DIR=/root/autodl-tmp/legacy-install-upgrade-20260731/old-src \
PHASE=old RELEASE_ID=legacy-cache-hit-optimization \
bash remote-install-version.sh
```

安装参数为 `--minimal --no-systemd --no-start --no-tests
--without-go-install --no-open-firewall --no-migrate-user-groups`，配置、
数据、安装前缀均位于隔离目录。结果：

```text
INSTALL_PHASE=old
INSTALL_EXIT=0
SERVICE_RELEASE=legacy-cache-hit-optimization
READY=1
```

完整输出：`remote-evidence/logs/install-old.literal.log`。

## SQLite 直接填充

旧版首次启动建立模式后停止服务，执行：

```bash
python3 seed-direct-sqlite.py
```

直接写入：

- 16 个账号，最长标签 68 字符；
- 18 个邮箱，最长地址 81 字符；
- 16 条额度记录、32 条模型能力；
- 336 条初始用量、48 条审计；
- 6 个注册任务、18 条注册结果、8 个生命周期任务。

旧版运行时按既有保留策略将超出窗口的用量整理为 168 条；升级基线因此以
168 条为准。升级前 `PRAGMA quick_check` 与 `integrity_check` 均为 `ok`。

## 初次新版升级

远端先执行 `npm ci` 与 `npm run build`，后者同时执行 TypeScript 检查；
然后在同一配置和数据库目录执行：

```bash
SOURCE_DIR=/root/autodl-tmp/legacy-install-upgrade-20260731/new-src \
PHASE=new RELEASE_ID=apple-backend-optimized-20260731 \
bash remote-install-version.sh
```

结果：

```text
INSTALL_PHASE=new
INSTALL_EXIT=0
WARN: Keeping existing config
READY=1
```

`install.sh` 本身未出现安装故障。

## 截图发现与修复

初次新版 6 页浏览器截图的页面级检查通过；人工复核发现 81 字符邮箱会侵入
状态、分组和错误列。加入逐单元格几何边界检查后，基线复现为：

```text
SCREENSHOT_SUMMARY phase=new-overflow-baseline total=6 passed=5 issues=1
email-pool escapedCellContent=31
```

根因是表格内 `TextClamp` 仍为 inline 元素，`overflow:hidden` 对其没有形成
有效盒约束。修复：

- 邮箱地址和表格 `TextClamp` 改为块级、100% 宽、单行省略；
- 邮箱表格单元格建立硬裁剪边界；
- 增加对应 CSS 回归测试；
- 演示异常状态统一为后端实际枚举 `error`。

远端验证：

```text
tsc --noEmit                                      exit 0
vite build                                        exit 0
tests/email-pool-responsive.test.tsx: 2 passed    exit 0
SCREENSHOT_SUMMARY phase=final total=6 passed=6 issues=0
escapedCellContent=0
```

修复补丁：

- `email-overflow-fix.patch`
- SHA-256：
  `2214d2e9ed6f75bb0077baf191add882a45ff3d522a7a77bc303b64215924410`
- 已实际正向应用、与最终文件逐字节比较、反向应用并与基线逐字节比较。

## 最终 `install.sh`

修复后的最终源码再次从干净归档解压、安装依赖、构建并运行回归测试，然后执行：

```bash
SOURCE_DIR=/root/autodl-tmp/legacy-install-upgrade-20260731/new-src \
PHASE=new-v2-final RELEASE_ID=apple-backend-optimized-v2-20260731 \
bash remote-install-version.sh
```

结果：

```text
INSTALL_PHASE=new-v2-final
INSTALL_EXIT=0
SERVICE_RELEASE=apple-backend-optimized-v2-20260731
READY=1
```

最终可直接安装的已构建源码包：

- `new-source-built-final.tar.gz`
- SHA-256：
  `4a071ce44cd7acc293857a827f47b7cee56b4f68d6b278e55c858815e6b91fd4`

最终原生二进制：

- `codex-pool-server.final-install-upgrade`
- SHA-256：
  `429f98e8fb44b62b2fe4f71ca8745923dd6feac83629964e2ec82876e8cd9046`

## 配置与数据兼容

最终修复前后使用同一 SQLite 输入：

```text
logical fingerprint before = 9295ea6d34bdefe8e7152905920fef7b8c39e743fb2beb8806c2bc19071af869
logical fingerprint after  = 9295ea6d34bdefe8e7152905920fef7b8c39e743fb2beb8806c2bc19071af869
accounts                    = 16
email_pool                  = 18
usage_records               = 168
config SHA before/after     = 46b179c50b737b88daca98c5d4cf48f6190a414ec275550b948e70ebc8226b40
quick_check                 = ok
integrity_check             = ok
```

## 实际回滚与再部署

`remote-rollback-redeploy.sh` 实际完成：

1. 切换回 `legacy-cache-hit-optimization`；
2. 旧二进制读取升级后的同一数据库并通过 `/readyz`；
3. 恢复旧控制台资产；
4. 再切换到 `apple-backend-optimized-v2-20260731`；
5. 最终控制台哈希、数据指纹和配置哈希恢复并保持一致。

字面结果：

```text
ROLLBACK_BEHAVIOR=old-release-ready old-console-restored data-preserved
REDEPLOY_BEHAVIOR=final-release-ready final-console-restored data-preserved
ROLLBACK_REDEPLOY_OK=1
```

## 最终线上部署

同一最终二进制已部署到原有两个原生服务，并真实执行上一个发布回滚及最终版再部署：

```text
main release     = apple-email-overflow-final-main
frontend release = apple-email-overflow-final-frontend
binary SHA       = 429f98e8fb44b62b2fe4f71ca8745923dd6feac83629964e2ec82876e8cd9046
console SHA      = c56c968e9703f066dca5e601fb836c1665ff332843512d98ea769a308ed0037f
SQLite checks    = ok / ok
screenshots      = 6/6
cell escapes     = 0
rollback         = verified
redeploy         = verified
```

结构化记录：

- `../final-cloud-deploy-20260731/final-deployment-summary.json`
- `../final-cloud-deploy-20260731/final-cloud-deploy-evidence.tar.gz`

## 四个交付角色

1. 修改产物：`codex-pool-server.final-install-upgrade`
2. 补丁：`email-overflow-fix.patch`
3. 验证记录：本文件与 `final-summary.json`
4. 回滚：`remote-rollback-redeploy.sh`、`../final-cloud-deploy-20260731/service-control.sh`

隔离升级夹具已停止并从远端删除；正式服务保持最终发布。
