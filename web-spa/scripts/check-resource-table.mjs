import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const managedPages = ['Groups.jsx', 'Users.tsx', 'Egress.jsx', 'Providers.jsx', 'Audit.tsx', 'CFEvents.tsx', 'Quota.tsx', 'Gopay.jsx', 'Accounts.jsx'];
const tableOnlyPages = ['Usage.tsx', 'System.tsx', 'Lifecycle.tsx', 'Registration.tsx'];
const portalTableOnlyPages = ['PortalDashboard.tsx'];
const managedComponents = ['DataPage.jsx', 'ApiKeysTable.tsx'];
const highValueMobilePages = ['Accounts.jsx', 'Providers.jsx', 'Users.tsx'];
const problems = [];

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

function readPage(page) {
  const file = path.join(root, 'src', 'pages', page);
  return fs.readFileSync(file, 'utf8');
}

for (const page of managedPages) {
  const source = readPage(page);
  checkResourceTableUsage(page, source, '../components/ResourceTable.jsx');
  const hasFirstLoadFailureBoundary = /if\s*\(\s*error\s*&&\s*!lastRefresh/.test(source);
  if ((/LoadErrorBanner/.test(source) && !hasFirstLoadFailureBoundary) || /TableSkeleton/.test(source)) {
    problems.push(`${page} must not duplicate list error or first-load skeleton UI.`);
  }
}

for (const page of highValueMobilePages) {
  const source = readPage(page);
  for (const required of ['MobileResourceCell', 'mobileColumns={mobileColumns}', 'mobileScroll={false}', 'pool-mobile-table']) {
    if (!source.includes(required)) {
      problems.push(`${page} must keep high-value mobile ResourceTable support via ${required}.`);
    }
  }
}

for (const page of tableOnlyPages) {
  const source = readPage(page);
  checkResourceTableUsage(page, source, '../components/ResourceTable.jsx');
  if (/TableSkeleton/.test(source)) {
    problems.push(`${page} must not duplicate first-load skeleton UI.`);
  }
}

for (const page of portalTableOnlyPages) {
  const source = fs.readFileSync(path.join(root, 'src', 'pages', 'portal', page), 'utf8');
  checkResourceTableUsage(page, source, '../../components/ResourceTable.jsx');
  if (/TableSkeleton/.test(source)) {
    problems.push(`${page} must not duplicate first-load skeleton UI.`);
  }
}

for (const component of managedComponents) {
  const source = fs.readFileSync(path.join(root, 'src', 'components', component), 'utf8');
  checkResourceTableUsage(component, source, './ResourceTable.jsx');
  if (/LoadErrorBanner|TableSkeleton|EmptyState/.test(source)) {
    problems.push(`${component} must centralize list error, skeleton, and empty states through ResourceTable.`);
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

const apiKeysTableSource = fs.readFileSync(path.join(root, 'src', 'components', 'ApiKeysTable.tsx'), 'utf8');
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

console.log('Resource table boundary check passed.');
