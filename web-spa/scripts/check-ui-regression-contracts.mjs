import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.join(root, 'src');
const problems = [];

function read(rel) {
  return fs.readFileSync(path.join(src, rel), 'utf8');
}

const button = read('components/pool/Button.jsx');
if (!/forwardRef/.test(button) || !/ref=/.test(button)) {
  problems.push('Pool Button must use React.forwardRef and pass ref to the native button for Radix asChild triggers.');
}

const feedback = read('components/pool/Feedback.jsx');
if (!/function Space\([^)]*wrap/.test(feedback) || /<span[^>]*\{\.\.\.props\}[^>]*>/.test(feedback) && !/const domProps/.test(feedback)) {
  problems.push('Space must consume wrap/spacing props instead of forwarding non-DOM props to <span>.');
}

const dataTable = read('components/pool/DataTable.jsx');
if (!/rowKey\(row,\s*index\)\s*\?\?/.test(dataTable) && !/rowKey\(row,\s*index\)[\s\S]{0,80}index/.test(dataTable)) {
  problems.push('DataTable must fall back to the row index when a function rowKey returns nullish.');
}
if (!/data-label=/.test(dataTable)) {
  problems.push('DataTable cells must expose data-label for mobile card layout.');
}
if (!/pool-table--cards/.test(dataTable)) {
  problems.push('DataTable must mark tables with a mobile card-layout class.');
}

const componentsCss = read('styles/components.css');
if (!/\.pool-table--cards/.test(componentsCss) || !/data-label/.test(componentsCss)) {
  problems.push('components.css must implement the DataTable mobile card layout.');
}

const charts = read('components/Charts.jsx');
if (!/function GroupedBar\([^)]*showValues\s*=\s*false/.test(charts)) {
  problems.push('GroupedBar must default showValues to false so dense bar charts do not overlap labels.');
}
if (!/function DonutChart\([^)]*valueFormatter/.test(charts) || !/formatValue\(total\)/.test(charts)) {
  problems.push('DonutChart must support a local valueFormatter for token-only donut units.');
}
for (const label of ['Token', '请求数', '请求命中率', '真实 Token 命中', '可缓存命中', '写缓存占比']) {
  if (!charts.includes(label)) problems.push(`ModelMetricsTooltip must render metric label ${label}.`);
}

const usage = read('pages/Usage.tsx');
const usageApi = read('features/observability/api/usage.ts');
const usageSurface = `${usage}\n${usageApi}`;
if (!/series_dimension:\s*'provider_model'/.test(usageSurface) || !/series_limit:\s*8/.test(usageSurface) || !/dimension:\s*'provider_model'/.test(usageSurface)) {
  problems.push('Usage Token trend and summary must request Provider + Model dimensions by default.');
}
if (!/PRIMARY_CACHE_FIELDS\s*=\s*'summary,by_model,by_provider,by_provider_model'/.test(usageSurface)
  || !/fields:\s*PRIMARY_CACHE_FIELDS/.test(usageSurface)
  || !/fetchUsageCacheDiagnostic/.test(usageSurface)
  || !/fields:\s*field/.test(usageSurface)) {
  problems.push('Usage must load primary cache chart fields up front and defer heavy diagnostic dimensions until selected.');
}
if (!/UsageModelAreaChart/.test(usage) || !/cacheCompositionSegments/.test(usage) || !/selectedCacheModels/.test(usage)) {
  problems.push('Usage must include model hover metrics, cache composition model hover segments, and model cache trend selection.');
}

const dashboard = read('pages/Dashboard.tsx');
const dashboardApi = read('features/observability/api/dashboard.ts');
const dashboardSurface = `${dashboard}\n${dashboardApi}`;
if (!/\/admin\/usage\/(?:dashboard|cache)/.test(dashboardSurface) || !/fields:\s*'summary,by_account,by_provider,by_provider_model'/.test(dashboardSurface)) {
  problems.push('Dashboard aggregate must request Provider + Model, account, and completeness fields.');
}
if (!/series_dimension:\s*'provider_model'/.test(dashboardSurface) || !/dimension:\s*'provider_model'/.test(dashboardSurface)) {
  problems.push('Dashboard trends and summaries must use Provider + Model dimensions.');
}
if (!/modelTokenFormatter/.test(dashboard) || !/(?:DonutChart|Donut)[^>]*valueFormatter=\{modelTokenFormatter\}/.test(dashboard)) {
  problems.push('Dashboard model token donut must use a local token formatter instead of changing fmtTokens globally.');
}

const accounts = read('pages/Accounts.jsx');
if (!/quota_summary/.test(accounts)) {
  problems.push('Accounts page must render quota_summary rather than ad hoc quota fields.');
}
const drawer = read('components/AccountDrawer.jsx');
for (const [action, label] of [['clear-quarantine', '解除隔离'], ['clear-cooldown', '解除冷却']]) {
  if (!accounts.includes(`'${action}'`) || !accounts.includes(label) || !drawer.includes(`'${action}'`) || !drawer.includes(label)) {
    problems.push(`Accounts page and drawer must expose the ${action} (${label}) administrator action.`);
  }
}
if (!/clearedCooldownPatch\s*=\s*\{[\s\S]{0,160}cooldown_until:\s*0[\s\S]{0,80}recheck_pending:\s*false/.test(accounts)
  || !/act\s*===\s*'clear-cooldown'[\s\S]{0,100}clearedCooldownPatch/.test(accounts)) {
  problems.push('Clear-cooldown must immediately clear the cached binding cooldown and pending recheck state.');
}
if (!/bulkAction\('clear-cooldown',\s*'解除冷却'\)/.test(accounts)) {
  problems.push('Accounts bulk toolbar must expose the clear-cooldown administrator action.');
}
if (!/saveGroup/.test(drawer) || !/groupPolicy/.test(drawer) || !/\/admin\/accounts\/.*\/group/.test(drawer)) {
  problems.push('AccountDrawer must support changing group and displaying inherited group policy.');
}
const identityIndex = drawer.indexOf('title="身份"');
const quotaIndex = drawer.indexOf('title="账号额度"');
if (quotaIndex <= identityIndex || identityIndex < 0) {
  problems.push('AccountDrawer must render an 账号额度 section immediately after identity.');
}
if (!/quota_summary\?\.reset_credits/.test(drawer) || !/formatResetCredits/.test(drawer)) {
  problems.push('AccountDrawer must render quota_summary.reset_credits through a reset-credit formatter.');
}
if (!/available_count[\s\S]{0,160}`\$\{credits\.available_count\} 次`/.test(drawer)) {
  problems.push('AccountDrawer reset-credit formatter must preserve known 0 as "0 次" instead of unknown.');
}

const providers = read('pages/Providers.jsx');
if (!/CLAUDE_MODEL_TABLE\s*=\s*\[/.test(providers)
  || !/anthropic_messages[\s\S]{0,300}Claude 候选模型表探测/.test(providers)) {
  problems.push('Providers must expose the maintained Claude candidate table when Anthropic auto-discovery is selected.');
}
if (!/model_mappings:\s*splitModelMappings/.test(providers)
  || !/下游 → 上游模型映射/.test(providers)
  || !/source => target/.test(providers)) {
  problems.push('Providers must persist and explain downstream-to-upstream model mappings.');
}
if (!/\/admin\/providers\/\$\{encodeURIComponent\(tester\.id\)\}\/test/.test(providers)
  || !/requested_model/.test(providers)
  || !/target_model/.test(providers)) {
  problems.push('Providers must expose the administrator model reachability test and render requested/target models.');
}
const importKeyCall = providers.match(/post\('\/admin\/accounts\/import-key',\s*\{([\s\S]{0,360}?)\}\)/);
if (!importKeyCall || /group_name/.test(importKeyCall[1])) {
  problems.push('Custom provider key import must require only provider, API key, and optional label—not a group field.');
}

if (problems.length > 0) {
  console.error('UI regression contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('UI regression contract check passed.');
