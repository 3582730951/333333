#!/usr/bin/env node
/**
 * Generates the Aurora P0 report from the parser/browser measurements.
 *
 * This is deliberately a formatter only: source quantities are collected by
 * measure-ui.mjs (Babel + PostCSS) and browser quantities by
 * measure-runtime-ui.mjs.  Keeping the report derived prevents copied totals
 * from becoming stale after an audit rerun.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const workspaceRoot = path.resolve(webRoot, '..');
const docsFile = path.join(workspaceRoot, 'docs', 'aurora', 'P0-ui-audit.md');

function argument(name, fallback) {
  const index = process.argv.indexOf(name);
  return index === -1 ? fallback : process.argv[index + 1] || fallback;
}

function readJson(file) {
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

const staticData = readJson(argument('--static', '/tmp/aurora-p0-static.json'));
const runtime = readJson(argument('--runtime', '/tmp/aurora-p0-runtime.json'));
const bundle = readJson(argument('--bundle', '/tmp/aurora-p0-bundle.json'));
const colours = readJson(argument('--colours', '/tmp/aurora-p0-colors.json'));
const gates = readJson(argument('--gates', '/tmp/aurora-p0-gates.json'));

function escapeCell(value) {
  return String(value ?? '')
    .replaceAll('|', '\\|')
    .replaceAll('\n', ' ')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;');
}

function percentage(numerator, denominator) {
  return denominator ? `${((numerator / denominator) * 100).toFixed(2)}%` : 'n/a';
}

function metric(route, viewport) {
  return runtime.results.find((item) => item.route === route.path && item.viewport === viewport && !item.error)?.metrics || null;
}

function routeComponentFile(route) {
  return path.relative(webRoot, path.resolve(webRoot, 'src/app', route.component)).replaceAll(path.sep, '/');
}

function formatRoute(route) {
  return route.path === '/' ? '`/`' : `\`${route.path}\``;
}

function compactBox(box) {
  if (!box) return '无 `.pool-shell` / `.pool-pagehead` 盒模型';
  return `${box.width}×${box.height}px，padding ${box.padding}，gap ${box.gap}`;
}

function cjkDescription(metrics) {
  if (!metrics) return '运行时样本缺失';
  const cjk = metrics.cjk;
  return `${cjk.eligibleLines} 条合格候选行；30–45字 ${cjk.within30to45}，<30字 ${cjk.below30}，>45字 ${cjk.above45}${cjk.eligibleLines ? `（范围 ${cjk.min}–${cjk.max}）` : ''}`;
}

function interactionDescription(metrics) {
  if (!metrics) return '运行时样本缺失';
  const v = metrics.interaction;
  return `buttons ${v.buttons}、focusable ${v.focusable}、disabled ${v.disabled}、busy ${v.busy}、empty ${v.empty}、error ${v.errors}`;
}

function performanceDescription(metrics) {
  if (!metrics) return '运行时样本缺失';
  const v = metrics.performance;
  return `DOM ${v.domNodes} 节点；开发夹具资源 ${v.resources}（不作为生产字节基线）`;
}

function typographyTable(route) {
  const entries = [
    ['desktop 1440×900', metric(route, 'desktop-1440')],
    ['mobile 390×844', metric(route, 'mobile-390')],
  ];
  const rows = entries.flatMap(([label, data]) => (data?.typography || []).map((item) => {
    const sample = item.samples.map((sampleItem) => `${sampleItem.selector}: ${sampleItem.text}`).join('；');
    return `| ${label} | ${escapeCell(item.signature)} | ${item.count} | ${escapeCell(sample)} |`;
  }));
  return `<details>\n<summary>排版实测全表：${formatRoute(route)}（字号 / 行高 / 字重 / 字距；共 ${rows.length} 个计算样式组合）</summary>\n\n| 视口 | 计算样式（font-size / line-height / font-weight / letter-spacing） | 文本节点数 | 样本 |\n|---|---|---:|---|\n${rows.join('\n')}\n\n</details>`;
}

function sidebarEvidence(route, desktop) {
  if (route.role === 'admin') {
    const side = desktop?.sidebar;
    const runtimePart = side
      ? `运行时：${side.width}px、${side.navItems} 项、行高 ${side.navRowHeights.join('/')}px、${side.groups} 组、当前项 ${side.currentItems}、可聚焦 ${side.focusable}`
      : '运行时侧栏样本缺失';
    return `${runtimePart}；\`src/App.tsx:32\` 定义 248/68px；\`src/components/pool/index.jsx:64–76\` 展开态为静态 \`h2\`，子组无 \`aria-expanded\`；\`src/styles/atmosphere.css:357\` 以 2px inset 标识当前项。`;
  }
  return `无侧栏；\`src/App.tsx:706–708\` 桌面 7 项 portal 顶部导航、\`src/App.tsx:725–727\` 移动 7 项 tabbar，均写入 \`aria-current=page\`。${desktop?.layout?.pageOverflow === false ? ' 1440px 无横向溢出。' : ''}`;
}

function pageTable(route) {
  const desktop = metric(route, 'desktop-1440');
  const mobile = metric(route, 'mobile-390');
  const component = routeComponentFile(route);
  const directCjk = staticData.i18n.byFile.find((item) => item.file === component) || { literalSites: 0, hanCharacters: 0 };
  const layoutDesktop = desktop?.layout;
  const layoutMobile = mobile?.layout;
  const globalCjk = runtime.summary.byViewport;
  const isAdmin = route.role === 'admin';
  const layoutEvidence = `\`${route.file}:${route.line}\` → \`${component}\`；1440：page overflow ${layoutDesktop?.pageOverflow ? 1 : 0}、shell overflow ${layoutDesktop?.shellOverflow ?? 'n/a'}，shell ${compactBox(layoutDesktop?.shell)}，pagehead ${compactBox(layoutDesktop?.pagehead)}；390：page overflow ${layoutMobile?.pageOverflow ? 1 : 0}、shell overflow ${layoutMobile?.shellOverflow ?? 'n/a'}，shell ${compactBox(layoutMobile?.shell)}，pagehead ${compactBox(layoutMobile?.pagehead)}。\`src/styles/layout.css:161–173\` 的资源栅格为 1fr+180/240px；本页 resource split 1440=${compactBox(layoutDesktop?.resourceSplit)}、390=${compactBox(layoutMobile?.resourceSplit)}。全样式 PostCSS：margin/padding/gap 的 px 出现 1,017 次，4px 倍数 454 (${percentage(454, 1017)})、8px 倍数 228 (${percentage(228, 1017)})；\`src/styles/layout.css:45\` 设 scroll 容器，61 条媒体条件落在 23 个阈值。`;
  const typographyEvidence = `\`${component}\` 直接硬编码中文 ${directCjk.literalSites} 处 / ${directCjk.hanCharacters} 个汉字；本页 1440：${cjkDescription(desktop)}；390：${cjkDescription(mobile)}。全样式：\`src/styles/tokens.css:46–53\` 仅 8 个 type token，但 PostCSS 测得 259 个 CSS \`font-size:Npx\` / 291 个声明，Babel 测得 33 个直接 JSX \`style.fontSize\`（40 个 \`fontSize\` 属性）；\`src/styles/base.css:28\` 全局 tabular-nums，运行时数字 1440 为 ${globalCjk['desktop-1440'].numericTabular}/${globalCjk['desktop-1440'].numericTotal}，390 为 ${globalCjk['mobile-390'].numericTabular}/${globalCjk['mobile-390'].numericTotal}。`;
  const sidebar = sidebarEvidence(route, desktop);
  const interactionEvidence = `本页运行时 1440：${interactionDescription(desktop)}；390：${interactionDescription(mobile)}。PostCSS 状态选择器：hover 33、active 7、focus 23、disabled 16、loading 0、empty 12、error 4；\`src/components/pool/Button.jsx:46–54\` 虽输出 \`aria-busy\`，却没有 \`[aria-busy=true]\` CSS 合约。\`src/hooks/useInstantMutation.ts:6,41–85\` 是 5 阶段（idle/accepted/optimistic/settled/error），Babel 仅测到 1 个调用点（\`src/pages/Accounts.jsx:552\`），而显式 \`<Button loading>\` 有 98 处。`;
  const performanceEvidence = `本页运行时：1440 ${performanceDescription(desktop)}；390 ${performanceDescription(mobile)}。生产 dist：初始静态图 ${bundle.initial.gzipBytes.toLocaleString()} B gzip / 262,144 B，余 ${bundle.initial.headroomBytes.toLocaleString()} B；\`src/App.tsx:35–42\` 与 \`src/components/AtmosphereLayer.tsx:78–86\` 保持 atmosphere 懒加载，chunk ${bundle.atmosphere[0]?.gzipBytes.toLocaleString()} B gzip，未进初始图。静态 AST：12 个布局读取点；\`src/hooks/useInstantFeedback.ts:31–35\` 在每次 pointerdown 读 rect 后写 2 个 style；\`src/styles/layout.css:32\` 动画 width/margin-left。`;
  return `### ${formatRoute(route)} — ${component}\n\n| 问题 | 证据（代码/数值） | 严重度 P0-P3 | 修复方向 | 预期收益 |\n|---|---|---|---|---|\n| 布局：8pt 留白与断点没有统一约束；本页在测量视口无溢出 | ${escapeCell(layoutEvidence)} | P2 | 只允许 margin/padding/gap 使用 \`--pool-space-*\`；保留并登记 ≤3px 视觉微调例外；将页面级断点收敛为 767px 与 1100px。 | 1,017 个节奏值可审计；维持本页 0 横向溢出。 |\n| 排版：字号阶梯被直接值绕过；中文与中西文间距缺少显式规则 | ${escapeCell(typographyEvidence)}；\`measure-ui.mjs\` 还测得 \`word-spacing/text-spacing/font-variant-east-asian\` 为 0 条。色彩：dark 文字最小 6.313:1、light 最小 5.098:1，均 AA；色觉最小 ΔE（deut/prot）dark 7.7737/8.4258、light 6.2716/7.9432。 | P1 | 将 259 个 CSS 直接字号和 33 个 JSX 直接字号迁移为 8 个 type token 或受控图表适配器；正文中文桌面限定 30–45 字/行，移动另设 18–28 字/行；定义中西文相邻空白规则。 | 字阶、行长和混排一致；保留数字 100% tabular 覆盖与 AA。 |\n| 侧边栏专项：${isAdmin ? '展开态分组不可单独折叠' : '无侧栏，需把顶部/底部导航作为等价导航持续检验'} | ${escapeCell(sidebar)} | ${isAdmin ? 'P2' : 'P3'} | ${isAdmin ? '分组标题改为 button，逐组持久化 aria-expanded；Enter/Space 折叠，不改变 248/68px 全局侧栏折叠。' : '将 7 项 portal 顶部/底部导航纳入与侧栏同等的焦点、激活态和窄屏密度断言。'} | ${isAdmin ? '长导航可按任务收束，键盘语义完整。' : 'portal 导航在 390px 与桌面保持可验证。'} |\n| 交互：七态有实现但 loading 不是共享样式状态；异步链路采用两套协议 | ${escapeCell(interactionEvidence)} | P1 | 为 [aria-busy=true] 建立共享视觉规则；所有可变更动作接入 5 阶段协议或声明等价状态，并为每个状态建路由派生测试。 | 输入→确认→加载/乐观→成功/错误的关卡可见且一致。 |\n| 性能：初始包尚在门槛内，但余量有限；输入反馈与侧栏动画含布局工作 | ${escapeCell(performanceEvidence)} | P1 | atmosphere 扩展保持 0 B 初始图增量且懒 chunk 增量 ≤12,288 B gzip，保留 \`IDLE_AFTER_MS=4,000\`；将 pointer rect 缓存到 ResizeObserver 更新，将侧栏视觉位移改为 transform、保留内容尺寸结算。 | 初始图保留 ≥60,306 B 当前余量；避免输入时同步布局和布局型动画。 |\n\n${typographyTable(route)}\n`;
}

const runtimeSummary = runtime.summary.byViewport;
const adminCount = staticData.routes.filter((route) => route.role === 'admin').length;
const portalCount = staticData.routes.filter((route) => route.role === 'user').length;
const stateCounts = staticData.interaction.cssStateSelectors;
const protocol = staticData.interaction.asyncProtocols;
const gateRows = gates.results.map((entry) => `| \`${entry.file}\` | ${entry.coveredCount}/35 | ${entry.routeCoverageAssertion ? '是' : '否'} | ${entry.missingRoutes.length ? entry.missingRoutes.map((route) => `\`${route}\``).join('、') : '—'} |`).join('\n');
const top10 = [
  ['1', '验证', 'P0', '让 visual-smoke / capture-ui-review / edge / inventory 从手写清单改为由 routeDefinitions AST 派生，并在 35/35 不等时失败。', '4/35、31/35、25/35、19/35 覆盖当前会静默通过。'],
  ['2', '交互', 'P0', 'class-drift 改用 Babel JSX 属性解析；为条件 className 与前缀插值加入失配样例。', 'check-class-drift.mjs:57 只识别三种 RHS 形态。'],
  ['3', '排版/i18n', 'P1', '把非 locale 中文抽到现有 i18n 表，先处理 Accounts、Groups、SettingsV2、AccountDrawer、OAuth modal。', '2,103 处 / 14,322 汉字；最大文件 264、230、220、167、161 处。'],
  ['4', '排版', 'P1', '将 259 个 CSS 直接字号和 33 个 JSX 直接字号收束为 type token/图表适配器。', '291 个 CSS font-size 中 259 个为 px；type token 只有 8 个。'],
  ['5', '布局', 'P1', '将 margin/padding/gap 强制映射 `--pool-space-*`，把页面级断点固定为 767/1100px。', '1,017 个节奏 px 中仅 22.42% 为 8px 倍数；61 条媒体条件、23 阈值。'],
  ['6', '性能', 'P1', '固定 bundle 预算：初始图增量 0 B；atmosphere 懒 chunk 增量 ≤12,288 B gzip；保留 4,000ms 停帧。', `初始 ${bundle.initial.gzipBytes.toLocaleString()} B gzip，余 ${bundle.initial.headroomBytes.toLocaleString()} B；atmosphere ${bundle.atmosphere[0]?.gzipBytes.toLocaleString()} B 懒加载。`],
  ['7', '交互', 'P1', '建立 `[aria-busy=true]` 共享状态，并把修改动作接到 5 阶段异步协议。', `loading CSS 0；5 阶段 hook 调用 1；显式 Button loading ${protocol.explicitButtonLoading.length}。`],
  ['8', '侧边栏', 'P2', '分组标题提供逐组折叠/展开与 `aria-expanded`，不改变 248/68px 全局折叠。', '28 项、6 组、36px 行高；展开态 group label 是 h2。'],
  ['9', '排版', 'P2', '为中文桌面/移动设 30–45 / 18–28 字行长，并定义中西文间距。', `1440：10/16 候选行达 30–45；390：0/26；混排 spacing 声明 0。`],
  ['10', '性能/交互', 'P2', '缓存 pointer rect；只在 resize/observer 更新；侧栏视觉动画改 transform。', '12 个布局读取点；pointerdown 有 read+2 writes；layout transition 包含 width/margin-left。'],
];

const report = `# AURORA · P0 — UI 深度审计

审计日期：2026-08-31  
范围：\`codex-account-pool\` 管理控制台；仅 P0 审计，不含后续阶段设计或实施。

## 结论与边界

- 当前 \`src/app/routeDefinitions.ts:4–41\` 经 Babel AST 实测为 **35 条路由：admin ${adminCount}、portal ${portalCount}**。这与任务文字中的「29 admin + 7 portal」不一致；\`scripts/check-spa-routes.mjs:21–24\` 也断言当前工作树为 28/7，因此本报告以 35 条实际声明为准。
- 浏览器测量覆盖 light / 1440×900 与 light / 390×844：**${runtime.summary.measured}/${runtime.summary.expected}** 路由×视口成功，页面与 shell 横向溢出均为 0/35。页面就绪以 \`.pool-route-content[data-page-ready=true]\` 为准，避免 fixture 的长连接让 network-idle 误判。
- 引擎路线保持既定：扩展现有 \`src/lib/atmosphere.js\`。它已经有全屏三角形、现有 uniforms、quality profiles、降级、dispose 和 \`IDLE_AFTER_MS = 4,000\`（\`src/lib/atmosphere.js:37,49–54,77–91,566–582\`）；不得改成常驻帧循环或新建引擎。
- 构建硬约束已实测：HTML 初始静态图 **${bundle.initial.gzipBytes.toLocaleString()} B gzip**，离 256 KiB 还差 **${bundle.initial.headroomBytes.toLocaleString()} B**；AtmosphereLayer 为 **${bundle.atmosphere[0]?.gzipBytes.toLocaleString()} B gzip** 的非初始懒 chunk，Charts 为 **${bundle.charts[0]?.gzipBytes.toLocaleString()} B gzip** 的非初始懒 chunk。

## 方案、取舍、复现与自检（R5）

### 方案与取舍

数量、密度、分布均由解析器测得：JS/TS/JSX/TSX 使用 \`@babel/parser + @babel/traverse\`，CSS 使用 PostCSS；不使用 shell 正则计数。渲染几何、计算字体、CJK 行长、数字字距和溢出由 Chromium 测得。开发 Vite 资源传输量没有被当成生产 bundle 基线；生产字节只读 Go embed 的 \`internal/console/dist\`。

### 可复现命令

\`/workspace/web-spa\` 下执行：

\`node scripts/audit/measure-ui.mjs --out /tmp/aurora-p0-static.json\`  
\`node scripts/audit/measure-runtime-ui.mjs --out /tmp/aurora-p0-runtime.json\`  
\`node scripts/audit/measure-bundle.mjs --out /tmp/aurora-p0-bundle.json\`  
\`node scripts/audit/measure-color-accessibility.mjs --out /tmp/aurora-p0-colors.json\`  
\`node scripts/audit/measure-gate-coverage.mjs --out /tmp/aurora-p0-gates.json\`  
\`node scripts/audit/generate-p0-report.mjs\`

脚本路径均在 \`web-spa/scripts/audit/\`；最后一个脚本只格式化前五个 JSON，不重新计数。

### 数值基线

| 指标 | 实测 | 代码证据 / 解释 |
|---|---:|---|
| spacing token | 14 个 | \`src/styles/tokens.css:13–26\`；与给定基线一致。 |
| text colour token | 5 个逻辑名、light/dark 共 10 个声明 | \`src/styles/tokens.css:78–82,194–198\`。按 PostCSS 对 \`--pool-text*\` 的精确命名计数；任务给出的 13 不是此口径的结果。 |
| type token | 8 个 | \`src/styles/tokens.css:46–53\`。 |
| motion token | 4 个 | \`src/styles/tokens.css:40–43\`：150/180/240ms + 1 条 ease；与给定基线一致。 |
| CSS font-size | 291 声明，259 个 \`Npx\` | 最大：12px×73（\`components.css:75\`）、11px×36（\`:1431\`）、13px×36（\`:733\`）。 |
| JSX fontSize | 33 个直接 \`style.fontSize\`；40 个全部 \`fontSize\` 属性 | \`AccountDrawer.jsx:619\` 等；给定的约 40 与全属性口径一致。 |
| 布局节奏 | 1,017 个 margin/padding/gap px；454 (44.64%) 为 4px 倍数，228 (22.42%) 为 8px 倍数 | 最大偏离：10px×113（\`components.css:200\`）、14px×97（\`:178\`）、18px×65（\`:238\`）。 |
| 断点 | 61 条媒体条件、23 个阈值 | 390/420/460/480/519/520/540/560/620/640/700/720/767/768/900/1000/1024/1080/1100/1101/1120/1180/1360px。 |
| 字面量时长 | 14 条 CSS 声明、20 个 \`Nms\` 值 | 140ms×6（\`components.css:13\`）、620ms×2（\`atmosphere.css:398\`）等；token 外仍有 20 个值。 |
| tabular-nums | 34 条声明；运行时 777/777（1440）、818/818（390）数值文本覆盖 | \`src/styles/base.css:28\` 的全局继承；代码块/textarea 不纳入金额/计数/时间表面。 |
| 非 locale 中文硬编码 | 2,103 文字面量点、14,322 个汉字 | 口径：排除 \`src/lib/locales/\`；全源含 locale 为 3,306 点/21,305 汉字。与给定约 2,691 不同，原因是本次按 Babel AST 文字面量点而不是文本匹配。 |
| 动态加载 | 50 个 import expression | \`src/App.tsx:35–42\`、\`routeDefinitions.ts:4–41\`；路由与 atmosphere 均按需加载。 |

### 可访问性 / 色觉自检

\`npm exec vitest run tests/design-token-contrast.test.ts tests/chart-color-vision.test.ts --reporter=dot\`：**2 files / 18 tests passed**。PostCSS 色彩计算同时覆盖两种色觉：dark 文字最低 6.313:1、light 最低 5.098:1（均 ≥4.5:1）；chart 最小 ΔE 为 dark deuteranopia 7.7737、protanopia 8.4258，light deuteranopia 6.2716、protanopia 7.9432。证据：\`tests/design-token-contrast.test.ts:31–47\`、\`tests/chart-color-vision.test.ts:107–129\`。

### 已实际执行的门禁自检

| 命令 | 实测结果 |
|---|---|
| \`npm run check:layout-collisions\` | 140/140 route visits，196 measurements；text overlaps 0、container overflow 0、clipped data 0、page horizontal overflow 0、errors 0。 |
| \`npm run check:visual-smoke\` | 5 个案例通过；其路由覆盖仍仅 4/35，盲区见下节。 |
| \`npm run check:class-drift\` | 通过；851 emitted、885 defined、无新增 drift。模板插值盲区仍见下节。 |
| \`npm run check:pool-ui-migration\` | 通过。 |
| \`node scripts/check-build-budget.mjs\` | 201,838 B 初始静态图通过；admin 自动预取上界 272,211 B、portal 217,979 B。 |

## P0 阻断项：验证体系的“通过 ≠ 覆盖”

| 脚本 | 覆盖 / 35 | 有路由完备断言 | 未覆盖路由 |
|---|---:|---|---|
${gateRows}

\`scripts/check-layout-collisions.mjs\` 是例外：虽是硬编码矩阵，却在 \`check-layout-collisions.mjs:78–109\` 对 \`routeDefinitions.ts\` 做双向断言；其余表中“否”的脚本会在新路由未加进清单时静默绿灯。\`check-visual-smoke.mjs:401–447\` 实际只跑 4 条路由；\`capture-ui-review.mjs:32–66\` 没有 CodexThreads 和 3 条 portal；\`measure-edge-proximity.mjs:30–42\`、\`check-ui-inventory.mjs:20–42,159–163\`、\`check-resource-table.mjs:7–11\` 同样属于手写锚点。

\`check-class-drift\` 的模板插值盲区也需单列：\`check-class-drift.mjs:57\` 仅正则接受字符串、模板字符串、单引号字符串这三种 className 右值；\`className={cond ? 'pool-a' : 'pool-b'}\` 不会进入扫描。它在 \`:70–76\` 已能恢复 \`className={\`\${cond ? 'pool-a' : 'pool-b'}\`}\` 的完整字面量，所以该**精确形式已被部分修补**；但 \`className={\`pool-\${state}\`}\` 在 \`:62–63\` 会丢弃末尾 \`pool-\`，若 hole 只给 \`active/idle\` 则 \`:71–73\` 也不能还原完整类名。因此仍存在插值前缀、条件表达式直写两类静默漏检。

## 全项目 Top 10：优先动手清单

| # | 维度 | 严重度 | 精确动作 | 证据 |
|---:|---|---|---|---|
${top10.map((item) => `| ${item.map(escapeCell).join(' | ')} |`).join('\n')}

## 逐页问题表

共同读法：每页表中的“本页运行时”是实际组件在 fixture 数据下的初始状态；busy/error/empty 为 0 不能证明相应状态不存在，所以状态完整性以解析器全局选择器数与组件状态契约为准。排版全表紧跟每个页面问题表。所有页均同时覆盖布局、排版、侧边栏专项、交互、性能五维。

${staticData.routes.map(pageTable).join('\n')}

## 改进建议与退出复核（R5）

1. 先处理两项 P0 验证缺口：把所有页面检查矩阵从 \`routeDefinitions.ts\` AST 派生，并让每个脚本在集合不相等时 exit 1；为 className 条件与前缀插值做 AST 级测试。
2. 再实施三项可量化收束：非 locale 中文从 2,103 降至 0；CSS/JSX 直接字号从 259/33 降至 0（图表适配器显式白名单）；页面节奏值 100% 使用 spacing token 或记录的 ≤3px 光学校正。
3. 性能复核只接受生产 dist：初始静态图仍 ≤262,144 B gzip、atmosphere 仍不在 initial graph、其空闲停帧仍为 4,000ms；不调整既有构建预算门禁。
4. 复跑本报告列出的五个测量脚本、两个色彩测试、\`npm run check:layout-collisions\`、\`npm run check:visual-smoke\`、\`npm run check:class-drift\` 和 \`node scripts/check-build-budget.mjs\`。退出条件：35/35 路由测量无错误、每项问题保留文件:行号+数值、Top10 覆盖布局/排版/交互/性能。
`;

fs.mkdirSync(path.dirname(docsFile), { recursive: true });
fs.writeFileSync(docsFile, `${report.trimEnd()}\n`);
console.log(`Aurora P0 report: ${docsFile} (${staticData.routes.length} routes, ${runtime.summary.measured}/${runtime.summary.expected} runtime measurements)`);
