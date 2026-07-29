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

const registration = read('pages/Registration.tsx');
if (!registration.includes('TaskDetailDrawer')) problems.push('Registration.tsx must use shared TaskDetailDrawer');
if (!registration.includes('TaskProgress')) problems.push('Registration.tsx must use shared TaskProgress');
if (!registration.includes('ReadinessPanel')) problems.push('Registration.tsx must use shared ReadinessPanel');

const form = read('components/pool/Form.jsx');
if (!/\bsetValue\b/.test(form)) problems.push('Pool Form API must expose setValue for existing portal profile reset behavior');

if (problems.length > 0) {
  console.error('Workflow contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Workflow contract check passed.');
