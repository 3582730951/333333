import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.join(root, 'src');
const read = (rel) => fs.readFileSync(path.join(src, rel), 'utf8');

const problems = [];

const api = read('api.js');
if (!/oauthComplete\s*=\s*\([^)]*egressId/.test(api) || !/egress_id:\s*egressId/.test(api)) {
  problems.push('api.oauthComplete must accept egressId and submit egress_id to /admin/oauth/complete.');
}

const oauth = read('components/OAuthLoginModal.jsx');
if (!/\/admin\/egress-profiles/.test(oauth)) {
  problems.push('OAuthLoginModal must load /admin/egress-profiles for import egress selection.');
}
if (!/egressId/.test(oauth) || !/setEgressId/.test(oauth)) {
  problems.push('OAuthLoginModal must keep selected egressId state.');
}
if (!/oauthComplete\([^)]*egressId/.test(oauth)) {
  problems.push('OAuthLoginModal OAuth completion must pass selected egressId.');
}
if (!/egress_id:\s*egressId/.test(oauth)) {
  problems.push('OAuthLoginModal manual auth.json import must submit egress_id.');
}
if (!/账号默认出口/.test(oauth)) {
  problems.push('OAuthLoginModal must label the selector as 账号默认出口.');
}

const providers = read('pages/Providers.jsx');
if (!/\/admin\/egress-profiles/.test(providers)) {
  problems.push('Providers page must load /admin/egress-profiles for import-key egress selection.');
}
if (!/egress_id:\s*values\.egress_id/.test(providers)) {
  problems.push('Providers import-key payload must submit egress_id.');
}
if (!/field="egress_id"/.test(providers) || !/账号默认出口/.test(providers)) {
  problems.push('Providers import-key form must render an 账号默认出口 select field.');
}

const groups = read('pages/Groups.jsx');
if (!/\/admin\/egress-profiles/.test(groups)) {
  problems.push('Groups page must load /admin/egress-profiles.');
}
if (!/default_egress_id:\s*String\(values\.default_egress_id/.test(groups)) {
  problems.push('Groups cleanGroupValues must include default_egress_id.');
}
if (!/default_egress_id/.test(groups) || !/默认出口/.test(groups)) {
  problems.push('Groups page must display and edit default_egress_id.');
}

if (problems.length) {
  console.error('Import egress contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Import egress contract check passed.');
