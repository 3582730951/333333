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
if (/\/admin\/egress-profiles/.test(providers) || /OrderedEgressSelect/.test(providers)) {
  problems.push('Providers must not select or load an egress; account/group routing owns the outlet.');
}
if (!/egress_ids:\s*\[\]/.test(providers) || !/提供商不再覆盖出口/.test(providers)) {
  problems.push('Providers must clear legacy egress_ids and explain that provider egress overrides are retired.');
}
if (/egress_id:\s*values\.egress_id/.test(providers) || /field="egress_id"/.test(providers)) {
  problems.push('Providers import-key must inherit its account-pool group egress instead of copying egress_id.');
}

const groups = read('pages/Groups.jsx');
const groupAPI = read('features/groups/api/groups.ts');
if (!/\/admin\/egress-profiles/.test(groups) && !/\/admin\/egress-profiles/.test(groupAPI)) {
  problems.push('Groups page must load /admin/egress-profiles.');
}
if (!/egress_ids:\s*selectedEgress\s*\?\s*\[selectedEgress\]\s*:\s*\[\]/.test(groups) || /OrderedEgressSelect/.test(groups)) {
  problems.push('Groups page must submit exactly zero or one inference egress.');
}
if (!/账号详情中单独指定的出口优先/.test(groups) || !/统一使用这一个出口/.test(groups)) {
  problems.push('Groups page must explain single-outlet inheritance and account override precedence.');
}

const accountDrawer = read('components/AccountDrawer.jsx');
if (!/inherit_group_egress:\s*true/.test(accountDrawer) || !/binding_scope/.test(accountDrawer)) {
  problems.push('Account drawer must expose explicit-vs-group routing and allow restoring group inheritance.');
}

const egress = read('pages/Egress.jsx');
if (!/del\(`\/admin\/egress-profiles\//.test(egress) || !/destructive:\s*true/.test(egress)) {
  problems.push('Egress page must expose a confirmed destructive delete action.');
}

if (problems.length) {
  console.error('Import egress contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Import egress contract check passed.');
