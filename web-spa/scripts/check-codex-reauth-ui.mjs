import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src', 'components', 'AccountDrawer.jsx'), 'utf8');
const problems = [];
for (const endpoint of [
  'codex-reauth-status',
  'codex-reauth-config',
  'codex-reauth/run',
  'codex-reauth/oauth/start',
  'codex-reauth/oauth/complete',
]) {
  if (!source.includes(endpoint)) problems.push(`AccountDrawer must use /admin/accounts/:id/${endpoint}`);
}
for (const marker of ['password_configured', 'otp_url_configured', 'target_workspace_id', 'auto_enabled']) {
  if (!source.includes(marker)) problems.push(`AccountDrawer must surface ${marker}`);
}
if (!/password:\s*reauthForm\.password/.test(source)) problems.push('AccountDrawer must only send password from the local edit form');
if (!/otp_url:\s*reauthForm\.otp_url/.test(source)) problems.push('AccountDrawer must only send otp_url from the local edit form');
if (problems.length) {
  console.error('Codex reauth UI contract failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}
console.log('Codex reauth UI contract passed.');
