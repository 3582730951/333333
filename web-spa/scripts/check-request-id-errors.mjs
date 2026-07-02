import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');
const problems = [];

function read(rel) {
  return fs.readFileSync(path.join(root, rel), 'utf8');
}

function walk(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walk(full));
    else if (/\.(jsx|js)$/.test(entry.name)) out.push(full);
  }
  return out;
}

const apiSource = read('src/api.js');
if (!apiSource.includes('errRequestID') || !apiSource.includes('request_id: errRequestID(e)')) {
  problems.push('api.js must preserve backend request_id in batch operation failures.');
}

const requestIDLine = read('src/components/RequestIDLine.jsx');
for (const required of ['writeClipboard', 'IconCopy', '复制请求 ID']) {
  if (!requestIDLine.includes(required)) {
    problems.push(`RequestIDLine.jsx must support request ID copy via ${required}.`);
  }
}

const errorToast = read('src/components/ErrorToast.jsx');
for (const required of ['errRequestID', 'RequestIDLine', 'Toast.error({']) {
  if (!errorToast.includes(required)) {
    problems.push(`ErrorToast.jsx must show backend request IDs through ${required}.`);
  }
}

const errorBoundary = read('src/components/AppErrorBoundary.jsx');
for (const required of ['errRequestID(error)', 'request_id: requestID', 'RequestIDLine']) {
  if (!errorBoundary.includes(required)) {
    problems.push(`AppErrorBoundary.jsx must carry request_id through ${required}.`);
  }
}

for (const file of walk(srcRoot)) {
  const rel = path.relative(root, file);
  const source = fs.readFileSync(file, 'utf8');
  if (/Toast\.error\([^;]*errMsg\s*\(/s.test(source)) {
    problems.push(`${rel} must use showErrorToast for backend error toasts.`);
  }
  if (/Toast\.error\([^;]*\+\s*errMsg\s*\(/s.test(source)) {
    problems.push(`${rel} must not concatenate errMsg into Toast.error; use showErrorToast prefix.`);
  }
}

if (problems.length > 0) {
  console.error('Request ID error handling check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Request ID error handling check passed.');
