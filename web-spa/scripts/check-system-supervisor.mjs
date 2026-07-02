import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const systemSource = fs.readFileSync(path.join(root, 'src', 'pages', 'System.jsx'), 'utf8');
const summarySource = fs.readFileSync(path.join(root, 'src', 'components', 'SystemHealthSummary.jsx'), 'utf8');
const problems = [];

function requireSnippet(source, snippet, message) {
  if (!source.includes(snippet)) problems.push(message);
}

requireSnippet(systemSource, "failed: 'red'", 'System.jsx must render failed supervisor modules/events as red.');
requireSnippet(systemSource, 'unexpected_exit_count', 'System.jsx must expose supervisor unexpected_exit_count in the module table.');
requireSnippet(systemSource, 'last_uptime_millis', 'System.jsx must expose supervisor last_uptime_millis in the module table.');
requireSnippet(systemSource, 'restart_backoff_millis', 'System.jsx must expose supervisor restart_backoff_millis in the module table.');
requireSnippet(systemSource, 'supervisor_events', 'System.jsx must read supervisor_events from /admin/system.');
requireSnippet(systemSource, 'supervisor_modules', 'System.jsx must read supervisor_modules from /admin/system.');
requireSnippet(summarySource, "failed: 'red'", 'SystemHealthSummary.jsx must render failed supervisor state as red.');
requireSnippet(summarySource, 'last_message', 'SystemHealthSummary.jsx must include failed/restarting module messages in the health summary.');
requireSnippet(summarySource, 'problemStatuses.has(recent.status)', 'SystemHealthSummary.jsx must only promote module messages for problematic states.');
requireSnippet(summarySource, 'moduleTimingText(recent)', 'SystemHealthSummary.jsx must include module uptime/backoff diagnostics.');
requireSnippet(summarySource, 'restart_backoff_millis', 'SystemHealthSummary.jsx must include restart backoff diagnostics.');

if (problems.length > 0) {
  console.error('System supervisor UI check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('System supervisor UI check passed.');
