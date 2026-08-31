import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src', 'app', 'routeDefinitions.ts'), 'utf8');
const appSource = fs.readFileSync(path.join(root, 'src', 'App.tsx'), 'utf8');
const aiSettingsSource = fs.readFileSync(path.join(root, 'src', 'pages', 'AISettings.tsx'), 'utf8');
const routePaths = [...source.matchAll(/\{ path: '([^']+)', role: '(?:admin|user)'/g)].map((match) => match[1]);
const redirects = [...source.matchAll(/\{ path: '([^']+)', to: '([^']+)' \}/g)].map((match) => ({ path: match[1], to: match[2] }));
const failures = [];

for (const pathValue of routePaths) {
  if (routePaths.filter((candidate) => candidate === pathValue).length > 1) failures.push(`duplicate route path: ${pathValue}`);
}
for (const redirect of redirects) {
  const target = redirect.to.split(/[?#]/, 1)[0];
  if (!routePaths.includes(target)) failures.push(`redirect target has no route: ${redirect.path} -> ${redirect.to}`);
}
if (routePaths.filter((pathValue) => pathValue.startsWith('/portal')).length !== 7) failures.push('portal route matrix must contain seven routes.');
if (routePaths.filter((pathValue) => !pathValue.startsWith('/portal')).length !== 28) failures.push('admin route metadata must contain twenty-eight canonical routes.');
const aiRoutes = routePaths.filter((pathValue) => pathValue.startsWith('/settings/ai/'));
if (aiRoutes.length !== 6) failures.push('AI settings must expose six canonical secondary routes.');
if (!aiSettingsSource.includes("dispatchBrowserEvent('pool-ai-settings-dirty', dirty)") || !appSource.includes("addWindowListener('pool-ai-settings-dirty'")) {
  failures.push('AI settings dirty state must be shared with the application shell.');
}
if (!/aiSettingsDirty\s*&&\s*!window\.confirm/.test(appSource) || !/navigateFromShell\(itemKey\)/.test(appSource)) {
  failures.push('Application sidebar navigation must confirm before leaving dirty AI settings.');
}
if (!source.includes('adminVisualRoutes') || !source.includes('portalVisualRoutes')) failures.push('visual route matrices must be generated from route metadata.');

if (failures.length) {
  console.error('SPA route consistency check failed:');
  failures.forEach((failure) => console.error(`- ${failure}`));
  process.exit(1);
}
console.log('SPA route consistency check passed.');
