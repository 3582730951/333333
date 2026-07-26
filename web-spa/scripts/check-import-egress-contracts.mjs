import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.join(root, 'src');
const read = (rel) => fs.readFileSync(path.join(src, rel), 'utf8');

const problems = [];

const api = read('api.js');
if (/oauthComplete\s*=\s*\([^)]*egressId/.test(api) || /egress_id:\s*egressId/.test(api)) {
  problems.push('api.oauthComplete must not copy account-pool group egress into an account import.');
}

const oauth = read('components/OAuthLoginModal.jsx');
if (/\/admin\/egress-profiles/.test(oauth) || /egressId|setEgressId|egress_id:\s*/.test(oauth)) {
  problems.push('OAuthLoginModal must inherit account-pool group egress instead of selecting or submitting an account egress.');
}
if (!/动态继承/.test(oauth) || !/不复制到账号记录/.test(oauth)) {
  problems.push('OAuthLoginModal must explain dynamic group egress inheritance.');
}

const providers = read('pages/Providers.jsx');
if (!/\/admin\/egress-profiles/.test(providers)) {
  problems.push('Providers page must load /admin/egress-profiles for ordered provider egress selection.');
}
if (!/egress_ids:\s*Array\.isArray\(values\.egress_ids\)/.test(providers) || !/OrderedEgressSelect/.test(providers)) {
  problems.push('Providers must submit and edit ordered egress_ids.');
}
if (/egress_id:\s*values\.egress_id/.test(providers) || /field="egress_id"/.test(providers)) {
  problems.push('Providers import-key must inherit its account-pool group egress instead of copying egress_id.');
}

const groups = read('pages/Groups.jsx');
if (!/\/admin\/egress-profiles/.test(groups)) {
  problems.push('Groups page must load /admin/egress-profiles.');
}
if (!/egress_ids/.test(groups) || !/OrderedEgressSelect/.test(groups)) {
  problems.push('Groups page must submit and edit ordered egress_ids.');
}
if (!/动态继承/.test(groups) || !/主出口/.test(groups)) {
  problems.push('Groups page must explain dynamic inheritance and primary/standby order.');
}

if (problems.length) {
  console.error('Import egress contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Import egress contract check passed.');
