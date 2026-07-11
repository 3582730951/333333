import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
const read = (p) => fs.readFileSync(path.join(root, p), 'utf8');
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };

const app = read('src/app/routeDefinitions.ts');
assert(app.includes('UpstreamErrorRules'), 'routeDefinitions.ts must lazy-load UpstreamErrorRules page');
assert(app.includes("/upstream-error-rules"), 'admin route /upstream-error-rules must be registered');
assert(app.includes('nav.upstream_error_rules'), 'navigation label key must be present');

const pagePath = path.join(root, 'src/pages/UpstreamErrorRules.jsx');
assert(fs.existsSync(pagePath), 'src/pages/UpstreamErrorRules.jsx must exist');
const page = fs.readFileSync(pagePath, 'utf8');
for (const endpoint of [
  '/admin/upstream-error-rules',
  '/admin/upstream-error-rules/test',
  '/admin/upstream-error-rules/model-options',
]) {
  assert(page.includes(endpoint), `page must call ${endpoint}`);
}
for (const term of ['上游错误规则', '新建规则', '测试匹配', '流式心跳空转', '全部模型', '手动 pattern']) {
  assert(page.includes(term), `page must render ${term}`);
}
for (const symbol of ['selectedProvider', 'selectedFamily', 'model_options', 'body_keywords', 'status_codes']) {
  assert(page.includes(symbol), `page must include cascade/form state ${symbol}`);
}

const css = read('src/styles/components.css');
for (const klass of ['upstream-rules-page', 'upstream-rule-card', 'upstream-rule-sheet', 'upstream-rule-summary']) {
  assert(css.includes(klass), `components.css must style ${klass}`);
}
console.log('upstream error rules UI contract ok');
