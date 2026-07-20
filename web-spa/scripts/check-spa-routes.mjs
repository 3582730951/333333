import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src', 'app', 'routeDefinitions.ts'), 'utf8');
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
if (routePaths.filter((pathValue) => pathValue.startsWith('/portal')).length !== 4) failures.push('portal route matrix must contain four routes.');
if (routePaths.filter((pathValue) => !pathValue.startsWith('/portal')).length !== 19) failures.push('admin route metadata must contain nineteen canonical routes.');
if (!source.includes('adminVisualRoutes') || !source.includes('portalVisualRoutes')) failures.push('visual route matrices must be generated from route metadata.');

if (failures.length) {
  console.error('SPA route consistency check failed:');
  failures.forEach((failure) => console.error(`- ${failure}`));
  process.exit(1);
}
console.log('SPA route consistency check passed.');
