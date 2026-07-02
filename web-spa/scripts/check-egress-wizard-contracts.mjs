import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

const failures = [];
const formPath = path.join(root, 'src/components/EgressProfileForm.jsx');

if (!fs.existsSync(formPath)) {
  failures.push('src/components/EgressProfileForm.jsx must exist and own the new profile wizard.');
} else {
  const source = fs.readFileSync(formPath, 'utf8');
  const required = [
    ['EGRESS_TEMPLATES', 'wizard templates'],
    ['proxy_url', 'ordinary proxy URL template'],
    ["id: 'socks5'", 'dedicated SOCKS5 template'],
    ['cliproxy_api', 'CLIPProxy API template'],
    ['sidecar', 'sidecar template'],
    ['showAdvanced', 'advanced field toggle'],
    ['testConnection', 'connection test action'],
    ['endpointPlaceholder', 'type-specific endpoint placeholder'],
    ['socks5://user:pass@host:port', 'SOCKS placeholder'],
    ['dynamic_config_json', 'advanced JSON field'],
  ];
  for (const [needle, label] of required) {
    if (!source.includes(needle)) failures.push(`EgressProfileForm missing ${label} (${needle}).`);
  }
  if (/dynamic_config_json[\s\S]{0,120}showAdvanced/.test(source) === false && /showAdvanced[\s\S]{0,500}dynamic_config_json/.test(source) === false) {
    failures.push('dynamic_config_json must be gated by the advanced toggle.');
  }
}

const egress = read('src/pages/Egress.jsx');
if (!/EgressProfileForm/.test(egress)) failures.push('Egress.jsx must render EgressProfileForm.');
if (!/joinSavedProfileToRegistrationPool/.test(egress)) failures.push('Egress.jsx must offer the save-success registration-pool next step.');
if (!/pool_registration_default/.test(egress)) failures.push('Egress.jsx must create/use pool_registration_default when no default registration pool exists.');

if (failures.length) {
  console.error('Egress wizard contract check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Egress wizard contract check passed.');
