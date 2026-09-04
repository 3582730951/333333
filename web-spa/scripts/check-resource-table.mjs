import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { assertCanonicalRouteCoverage } from './lib/route-coverage.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const problems = [];

// This is deliberately a complete route-to-surface manifest, not a list of pages that happened
// to use ResourceTable when this gate was first written. Every non-table entry is an explicit
// design decision with a reason, so a new route cannot become an unreviewed omission by default.
const routeContracts = [
  { role: 'admin', path: '/', source: 'src/pages/Dashboard.tsx', reason: 'KPI dashboard cards and charts, not a row collection.' },
  { role: 'admin', path: '/accounts', source: 'src/pages/Accounts.jsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/groups', source: 'src/pages/Groups.jsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/providers', source: 'src/pages/Providers.jsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/models', source: 'src/pages/Models.tsx', reason: 'ModelNameList is a searchable capability directory rather than a table.' },
  { role: 'admin', path: '/public-chat', source: 'src/pages/PublicChat.jsx', reason: 'Link cards and an edit form are the primary interaction surface.' },
  { role: 'admin', path: '/egress', source: 'src/pages/Egress.jsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/upstream-error-rules', source: 'src/pages/UpstreamErrorRules.jsx', reason: 'Rule cards and their inline editor are not a tabular list.' },
  { role: 'admin', path: '/registration', source: 'src/pages/Registration.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/team-lifecycle', source: 'src/pages/TeamLifecycle.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/email-pool', source: 'src/pages/EmailPool.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/email-pool/cloudflare', source: 'src/pages/CloudflareMailbox.tsx', reason: 'Mailbox provisioning cards and configuration form, not a row list.' },
  { role: 'admin', path: '/usage', source: 'src/pages/Usage.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/quota', source: 'src/pages/Quota.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/model-quality', source: 'src/pages/ModelQuality.jsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/system', source: 'src/pages/System.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/cf-events', source: 'src/pages/CFEvents.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/audit', source: 'src/pages/Audit.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/codex-threads', source: 'src/pages/CodexThreads.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/keys', source: 'src/pages/Keys.tsx', tableSource: 'src/components/ApiKeysTable.tsx', resourceTable: './ResourceTable.jsx' },
  { role: 'admin', path: '/users', source: 'src/pages/Users.tsx', resourceTable: '../components/ResourceTable.jsx' },
  { role: 'admin', path: '/settings-v2', source: 'src/pages/SettingsV2.tsx', reason: 'Settings form sections are not a tabular list.' },
  { role: 'admin', path: '/settings/ai/chatgpt', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'admin', path: '/settings/ai/claude', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'admin', path: '/settings/ai/kiro', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'admin', path: '/settings/ai/antigravity', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'admin', path: '/settings/ai/codex', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'admin', path: '/settings/ai/claude-code', source: 'src/pages/AISettings.tsx', reason: 'Provider configuration form, not a tabular list.' },
  { role: 'portal', path: '/portal', source: 'src/pages/portal/PortalDashboard.tsx', resourceTable: '../../components/ResourceTable.jsx' },
  { role: 'portal', path: '/portal/keys', source: 'src/pages/portal/PortalKeys.tsx', tableSource: 'src/components/ApiKeysTable.tsx', resourceTable: './ResourceTable.jsx' },
  { role: 'portal', path: '/portal/usage', source: 'src/pages/portal/PortalUsage.tsx', resourceTable: '../../components/ResourceTable.jsx' },
  { role: 'portal', path: '/portal/quota', source: 'src/pages/portal/PortalQuota.tsx', reason: 'Quota summary cards and a definition list are not a table.' },
  { role: 'portal', path: '/portal/models', source: 'src/pages/portal/PortalModels.tsx', reason: 'ModelNameList is a searchable capability directory rather than a table.' },
  { role: 'portal', path: '/portal/profile', source: 'src/pages/portal/PortalProfile.jsx', reason: 'Profile details and account controls are not a row list.' },
  { role: 'portal', path: '/portal/sessions', source: 'src/pages/portal/PortalSessions.tsx', reason: 'Session cards expose revoke actions without table-only affordances.' },
];

const routeCoverage = assertCanonicalRouteCoverage({
  root,
  gate: 'Resource table boundary',
  admin: routeContracts.filter((contract) => contract.role === 'admin'),
  portal: routeContracts.filter((contract) => contract.role === 'portal'),
});
if (!routeCoverage.ok) process.exit(1);

function read(relative) {
  return fs.readFileSync(path.join(root, relative), 'utf8');
}

function checkResourceTableUsage(name, source, importPath) {
  if (!source.includes(`import ResourceTable from '${importPath}';`)) {
    problems.push(`${name} must render list state through ResourceTable.`);
  }
  if (/import\s+\{[^}]*\bTable\b[^}]*\}\s+from\s+['"]@douyinfe\/semi-ui['"]/.test(source)) {
    problems.push(`${name} must not import Semi Table directly; use ResourceTable.`);
  }
  const rendersImportedTable = /<ResourceTable\b/.test(source)
    || (/\b(?:const|let)\s+DataTable\s*=\s*ResourceTable\b/.test(source) && /<DataTable\b/.test(source));
  if (!rendersImportedTable) {
    problems.push(`${name} must include a ResourceTable element.`);
  }
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

// A page can have several independent resources: an options form may need its own retry banner
// while the list below it correctly delegates its own error state to ResourceTable. Only reject a
// banner that repeats the exact error expression already passed to that table.
function hasDuplicateListErrorBoundary(source) {
  const tableErrors = [...source.matchAll(/<(?:ResourceTable|DataTable)\b[\s\S]*?\berror=\{([^}]+)\}/g)]
    .map((match) => match[1].trim())
    .filter(Boolean);
  return tableErrors.some((error) => {
    const exclusiveFirstLoad = new RegExp(
      `if\\s*\\(\\s*${escapeRegExp(error)}\\s*&&\\s*!lastRefresh\\s*&&\\s*!loading\\s*\\)`,
    ).test(source);
    const repeatsTableError = new RegExp(
      `<(?:ErrorBanner|LoadErrorBanner)\\b[^>]*\\berror=\\{\\s*${escapeRegExp(error)}\\s*\\}`,
    ).test(source);
    return repeatsTableError && !exclusiveFirstLoad;
  });
}

function canonicalSource(route) {
  return path.relative(root, path.resolve(root, 'src', 'app', route.lazyLoader)).replaceAll(path.sep, '/');
}

const canonicalByPath = new Map(routeCoverage.canonical.all.map((route) => [route.path, route]));
for (const contract of routeContracts) {
  const canonical = canonicalByPath.get(contract.path);
  if (!canonical) continue;
  const expected = canonicalSource(canonical);
  if (contract.source !== expected) {
    problems.push(`${contract.path} manifest maps to ${contract.source}, but routeDefinitions.ts loads ${expected}.`);
  }
  if (!fs.existsSync(path.join(root, contract.source))) {
    problems.push(`${contract.path} manifest source does not exist: ${contract.source}.`);
  }
  if (!contract.resourceTable && !contract.reason) {
    problems.push(`${contract.path} is not a ResourceTable route but has no explicit reason.`);
  }
}

const tableTargets = new Map();
for (const contract of routeContracts.filter((item) => item.resourceTable)) {
  const source = contract.tableSource || contract.source;
  const key = `${source}:${contract.resourceTable}`;
  const target = tableTargets.get(key) || { source, importPath: contract.resourceTable, routes: [] };
  target.routes.push(contract.path);
  tableTargets.set(key, target);
}
for (const target of tableTargets.values()) {
  if (!fs.existsSync(path.join(root, target.source))) {
    problems.push(`${target.routes.join(', ')} ResourceTable source does not exist: ${target.source}.`);
    continue;
  }
  const source = read(target.source);
  const label = `${target.source} (${target.routes.join(', ')})`;
  checkResourceTableUsage(label, source, target.importPath);
  if (hasDuplicateListErrorBoundary(source) || /TableSkeleton/.test(source)) {
    problems.push(`${label} must not duplicate list error or first-load skeleton UI.`);
  }
}

for (const route of ['/accounts', '/providers', '/users']) {
  const contract = routeContracts.find((item) => item.path === route);
  const source = contract ? read(contract.source) : '';
  for (const required of ['MobileResourceCell', 'mobileColumns={mobileColumns}', 'mobileScroll={false}', 'pool-mobile-table']) {
    if (!source.includes(required)) {
      problems.push(`${contract?.source || route} must keep high-value mobile ResourceTable support via ${required}.`);
    }
  }
}

for (const component of [
  { source: 'src/components/DataPage.jsx', importPath: './ResourceTable.jsx' },
  { source: 'src/components/ApiKeysTable.tsx', importPath: './ResourceTable.jsx' },
]) {
  const source = read(component.source);
  checkResourceTableUsage(component.source, source, component.importPath);
  if (/LoadErrorBanner|TableSkeleton|EmptyState/.test(source)) {
    problems.push(`${component.source} must centralize list error, skeleton, and empty states through ResourceTable.`);
  }
}

const componentFile = path.join(root, 'src', 'components', 'ResourceTable.jsx');
const componentSource = fs.readFileSync(componentFile, 'utf8');
for (const required of ['LoadErrorBanner', 'TableSkeleton', 'EmptyState']) {
  if (!componentSource.includes(required)) {
    problems.push(`ResourceTable.jsx must centralize ${required}.`);
  }
}
for (const required of ['defaultTableScrollX', 'resolvedScroll', 'scroll={resolvedScroll}']) {
  if (!componentSource.includes(required)) {
    problems.push(`ResourceTable.jsx must centralize responsive table scroll via ${required}.`);
  }
}
for (const required of ['mobileColumns', 'mobileScroll', 'activeColumns']) {
  if (!componentSource.includes(required)) {
    problems.push(`ResourceTable.jsx must expose mobile table adaptation via ${required}.`);
  }
}

const apiKeysTableSource = read('src/components/ApiKeysTable.tsx');
for (const required of ['mobileColumns={mobileColumns}', 'mobileScroll={false}', 'pool-key-table']) {
  if (!apiKeysTableSource.includes(required)) {
    problems.push(`ApiKeysTable.tsx must use ResourceTable mobile table support via ${required}.`);
  }
}

if (problems.length > 0) {
  console.error('Resource table boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

const tableRouteCount = routeContracts.filter((contract) => contract.resourceTable).length;
console.log(`Resource table boundary check passed (${tableRouteCount} ResourceTable routes; ${routeContracts.length - tableRouteCount} explicit non-table routes).`);
