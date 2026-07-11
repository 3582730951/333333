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
if (!/series_dimension:\s*'model'/.test(usageSurface) || !/series_limit:\s*6/.test(usageSurface)) {
  problems.push('Usage Token trend must request model series by default.');
}
if (!/FULL_CACHE_FIELDS\s*=\s*'summary,by_account,by_model,by_api_key,by_account_model,by_route,by_route_account_model,by_time_bucket'/.test(usageSurface) || !/fields:\s*FULL_CACHE_FIELDS/.test(usageSurface)) {
  problems.push('Usage cache diagnostics must request the full reset-aware cache field set explicitly.');
}
if (!/UsageModelAreaChart/.test(usage) || !/cacheCompositionSegments/.test(usage) || !/selectedCacheModels/.test(usage)) {
  problems.push('Usage must include model hover metrics, cache composition model hover segments, and model cache trend selection.');
}

const dashboard = read('pages/Dashboard.tsx');
const dashboardApi = read('features/observability/api/dashboard.ts');
const dashboardSurface = `${dashboard}\n${dashboardApi}`;
if (!/\/admin\/usage\/cache/.test(dashboardSurface) || !/fields:\s*'summary,by_model'/.test(dashboardSurface)) {
  problems.push('Dashboard cache modules must use reset-aware /admin/usage/cache with lightweight fields.');
}
if (!/modelTokenFormatter/.test(dashboard) || !/(?:DonutChart|Donut)[^>]*valueFormatter=\{modelTokenFormatter\}/.test(dashboard)) {
  problems.push('Dashboard model token donut must use a local token formatter instead of changing fmtTokens globally.');
}

const accounts = read('pages/Accounts.jsx');
if (!/quota_summary/.test(accounts)) {
  problems.push('Accounts page must render quota_summary rather than ad hoc quota fields.');
}
const drawer = read('components/AccountDrawer.jsx');
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

if (problems.length > 0) {
  console.error('UI regression contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('UI regression contract check passed.');
