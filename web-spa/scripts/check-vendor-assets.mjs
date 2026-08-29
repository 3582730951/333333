import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const problems = [];

function read(relativePath) {
  return fs.readFileSync(path.join(workspaceRoot, relativePath), 'utf8');
}

function exists(relativePath) {
  return fs.existsSync(path.join(workspaceRoot, relativePath));
}

const vendorLogoPath = 'web-spa/src/components/VendorLogo.jsx';
const oauthModalPath = 'web-spa/src/components/OAuthLoginModal.jsx';
const policyPath = 'docs/architecture/console-asset-policy.md';
const requiredAssets = [
  'web-spa/src/assets/vendors/openai-blossom.svg',
  'web-spa/src/assets/vendors/anthropic.svg',
];

if (!exists(vendorLogoPath)) {
  problems.push('VendorLogo.jsx must exist.');
} else {
  const source = read(vendorLogoPath);
  for (const vendor of ['openai', 'chatgpt', 'codex', 'claude', 'anthropic', 'custom']) {
    if (!source.includes(`'${vendor}'`) && !source.includes(`"${vendor}"`)) {
      problems.push(`VendorLogo.jsx must support ${vendor}.`);
    }
  }
  for (const asset of ['openai-blossom.svg', 'anthropic.svg']) {
    if (!source.includes(asset)) {
      problems.push(`VendorLogo.jsx must import ${asset}.`);
    }
  }
}

for (const asset of requiredAssets) {
  if (!exists(asset)) problems.push(`${asset} must exist.`);
}

if (!exists(policyPath)) {
  problems.push('docs/architecture/console-asset-policy.md must document vendor logo provenance.');
} else {
  const policy = read(policyPath);
  for (const required of [
    'OpenAI',
    'https://openai.com/brand/',
    'Claude',
    'https://claude.com/',
    'Anthropic',
    'https://www.anthropic.com/',
    'Vendor Logo Register',
  ]) {
    if (!policy.includes(required)) problems.push(`Console asset policy must include ${required}.`);
  }
}

const oauthSource = read(oauthModalPath);
for (const disallowed of ['🤖', '🧠', '📋']) {
  if (oauthSource.includes(disallowed)) problems.push(`OAuthLoginModal.jsx must not include ${disallowed}.`);
}
if (!oauthSource.includes('VendorLogo')) {
  problems.push('OAuthLoginModal.jsx must render provider tabs/identity through VendorLogo.');
}

if (problems.length > 0) {
  console.error('Vendor asset check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Vendor asset check passed.');
