import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.join(root, 'src');

function read(rel) {
  return fs.readFileSync(path.join(src, rel), 'utf8');
}

const problems = [];
const workflowFile = path.join(src, 'components', 'WorkflowPrimitives.jsx');
if (!fs.existsSync(workflowFile)) {
  problems.push('missing shared workflow component file src/components/WorkflowPrimitives.jsx');
} else {
  const source = fs.readFileSync(workflowFile, 'utf8');
  for (const name of ['TaskProgress', 'ReadinessPanel', 'ServiceHealthStrip', 'TaskDetailDrawer', 'LogStream']) {
    if (!new RegExp(`export\\s+function\\s+${name}\\b`).test(source)) {
      problems.push(`WorkflowPrimitives.jsx must export ${name}`);
    }
  }
}

const registration = read('pages/Registration.jsx');
const lifecycle = read('pages/Lifecycle.jsx');
for (const [label, source] of [['Registration.jsx', registration], ['Lifecycle.jsx', lifecycle]]) {
  if (!source.includes('TaskDetailDrawer')) problems.push(`${label} must use shared TaskDetailDrawer`);
  if (!source.includes('TaskProgress')) problems.push(`${label} must use shared TaskProgress`);
}
if (!registration.includes('ReadinessPanel')) problems.push('Registration.jsx must use shared ReadinessPanel');
if (!lifecycle.includes('ServiceHealthStrip')) problems.push('Lifecycle.jsx must use shared ServiceHealthStrip');
if (!lifecycle.includes('LogStream')) problems.push('Lifecycle.jsx must use shared LogStream');

const gopay = read('pages/Gopay.jsx');
if (!/subscriptions/.test(gopay)) problems.push('Gopay.jsx must handle subscriptions from /admin/gopay');
if (!/serviceStatus|service_status|statusPanel|serviceHealth/.test(gopay)) problems.push('Gopay.jsx must render service status when no financial rows exist');
if (!/settings/.test(gopay) || !/logs/.test(gopay)) problems.push('Gopay.jsx must render settings/logs from existing /admin/gopay payload when present');

const form = read('components/pool/Form.jsx');
if (!/\bsetValue\b/.test(form)) problems.push('Pool Form API must expose setValue for existing portal profile reset behavior');

if (problems.length > 0) {
  console.error('Workflow contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Workflow contract check passed.');
