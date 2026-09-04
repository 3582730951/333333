#!/usr/bin/env node
/**
 * Aurora P0 rendered UI measurement.
 *
 * Routes are extracted from routeDefinitions.ts with Babel, then rendered through
 * the existing capture-ui-review fixture layer. Typography, numeric alignment,
 * CJK line length, sidebar geometry, resource timing, and overflow are measured
 * from the browser's layout engine rather than inferred from screenshots.
 *
 * Usage:
 *   node scripts/audit/measure-runtime-ui.mjs --out /tmp/aurora-p0-runtime.json
 */
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';
import puppeteer from 'puppeteer';

import { installMocks } from '../capture-ui-review.mjs';

const traverse = traverseModule.default;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');
const routeFile = path.join(root, 'src', 'app', 'routeDefinitions.ts');
const viewports = [
  { name: 'desktop-1440', width: 1440, height: 900, mobile: false },
  { name: 'mobile-390', width: 390, height: 844, mobile: true },
];

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

function stringValue(node) {
  return node?.type === 'StringLiteral' ? node.value : null;
}

function propertyByName(node, name) {
  if (node?.type !== 'ObjectExpression') return null;
  return node.properties.find((property) => {
    if (property.type !== 'ObjectProperty') return false;
    const key = property.key.type === 'Identifier' ? property.key.name : stringValue(property.key);
    return key === name;
  }) || null;
}

function extractRoutes() {
  const ast = parser.parse(fs.readFileSync(routeFile, 'utf8'), {
    sourceType: 'module',
    plugins: ['typescript'],
  });
  const routes = [];
  traverse(ast, {
    ObjectExpression(pathRef) {
      const pathProperty = propertyByName(pathRef.node, 'path');
      const roleProperty = propertyByName(pathRef.node, 'role');
      const loaderProperty = propertyByName(pathRef.node, 'lazyLoader');
      if (!pathProperty || !roleProperty || !loaderProperty) return;
      const route = { path: stringValue(pathProperty.value), role: stringValue(roleProperty.value), line: pathRef.node.loc.start.line };
      if (route.path && (route.role === 'admin' || route.role === 'user')) routes.push(route);
    },
  });
  return routes.sort((a, b) => a.role.localeCompare(b.role) || a.path.localeCompare(b.path));
}

function canUsePort(port) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once('error', () => resolve(false));
    server.once('listening', () => server.close(() => resolve(true)));
    server.listen(port, '127.0.0.1');
  });
}

async function findPort(start = 5410) {
  for (let port = start; port < start + 80; port += 1) if (await canUsePort(port)) return port;
  throw new Error('no free port for Aurora runtime audit');
}

function startServer(port) {
  return spawn(process.execPath, [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], {
    cwd: root,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

function waitForServer(child) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const done = (fn, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.stdout.off('data', onData);
      child.stderr.off('data', onData);
      child.off('exit', onExit);
      fn(value);
    };
    const onData = (chunk) => {
      if (/Local:\s+http:\/\/127\.0\.0\.1:/.test(String(chunk))) done(resolve);
    };
    const onExit = (code) => done(reject, new Error(`Vite exited before audit server was ready (${code})`));
    const timer = setTimeout(() => done(reject, new Error('Vite did not become ready within 60 seconds')), 60000);
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', onExit);
  });
}

async function stopServer(child) {
  if (!child || child.exitCode !== null || child.signalCode) return;
  await new Promise((resolve) => {
    const force = setTimeout(() => child.kill('SIGKILL'), 3000);
    child.once('exit', () => { clearTimeout(force); resolve(); });
    child.kill('SIGTERM');
  });
}

async function preparePage(browser, baseURL, role, viewport, theme) {
  const page = await browser.newPage();
  await installMocks(page);
  await page.setViewport({
    width: viewport.width,
    height: viewport.height,
    isMobile: viewport.mobile,
    hasTouch: viewport.mobile,
    deviceScaleFactor: 1,
  });
  await page.emulateMediaFeatures([
    { name: 'prefers-color-scheme', value: theme },
    { name: 'prefers-reduced-motion', value: 'no-preference' },
  ]);
  await page.evaluateOnNewDocument((nextTheme) => localStorage.setItem('pool_theme', nextTheme), theme);
  const expires = Math.floor(Date.now() / 1000) + 86400;
  await page.setCookie(
    { url: baseURL, name: 'cp_session', value: `${role}-fixture`, path: '/', expires },
    { url: baseURL, name: 'cp_csrf', value: `${role}-csrf`, path: '/', expires },
  );
  return page;
}

async function gotoApp(page, baseURL, route) {
  await page.goto(`${baseURL}${route}`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await page.waitForFunction(() => {
    const content = document.querySelector('.pool-route-content[data-page-ready="true"]');
    return content && content.innerText.trim().length > 10;
  }, { timeout: 15000 });
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  // The fixture deliberately exercises EventSource reconnect behavior. Waiting on
  // transport idleness here turns one valid route render into a 6 s timeout; the
  // audited readiness contract is the DOM's data-page-ready marker instead.
  await new Promise((resolve) => setTimeout(resolve, 120));
}

const BROWSER_MEASURE = `(() => {
  const root = document.querySelector('.pool-route-content') || document.body;
  const hanChar = /\\p{Script=Han}/u;
  const hanGlobal = /\\p{Script=Han}/gu;
  const hasDigit = /[0-9]/;
  const isVisible = (element) => {
    if (!element || element.closest('[aria-hidden="true"], [hidden], [inert]')) return false;
    const style = getComputedStyle(element);
    if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) < 0.05) return false;
    const rect = element.getBoundingClientRect();
    return rect.width > 0.5 && rect.height > 0.5;
  };
  const directText = (element) => [...element.childNodes]
    .filter((node) => node.nodeType === Node.TEXT_NODE)
    .map((node) => node.nodeValue || '')
    .join(' ')
    .replace(/\\s+/g, ' ')
    .trim();
  const selectorHint = (element) => {
    const classes = typeof element.className === 'string' ? element.className.trim().split(/\\s+/).slice(0, 2).join('.') : '';
    return element.tagName.toLowerCase() + (classes ? '.' + classes : '');
  };
  const signatures = new Map();
  const numeric = { total: 0, tabular: 0, misses: [], money: { total: 0, tabular: 0 }, count: { total: 0, tabular: 0 }, time: { total: 0, tabular: 0 } };
  const cjkLines = [];
  const textWalker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const textNodes = [];
  while (textWalker.nextNode()) textNodes.push(textWalker.currentNode);
  for (const textNode of textNodes) {
    const element = textNode.parentElement;
    const value = (textNode.nodeValue || '').replace(/\\s+/g, ' ').trim();
    if (!value || !isVisible(element)) continue;
    const style = getComputedStyle(element);
    const signature = [style.fontSize, style.lineHeight, style.fontWeight, style.letterSpacing].join(' / ');
    const entry = signatures.get(signature) || { signature, count: 0, samples: [] };
    entry.count += 1;
    if (entry.samples.length < 3) entry.samples.push({ text: value.slice(0, 72), selector: selectorHint(element) });
    signatures.set(signature, entry);
    // Command snippets are rendered documentation, not the operational
    // amount/count/time surfaces whose column alignment is being audited.
    if (hasDigit.test(value) && !element.closest('textarea, pre, code, [data-code]')) {
      const tabular = style.fontVariantNumeric.includes('tabular-nums');
      const type = /(?:[$¥€]\s?\d|USD\s?\d|CNY\s?\d|金额|费用|积分|额度)/i.test(value) ? 'money'
        : /(?:\\d{1,2}:\\d{2}|分钟|小时|天|周|月|年|ms\\b|秒)/i.test(value) ? 'time' : 'count';
      numeric.total += 1;
      if (tabular) numeric.tabular += 1;
      else if (numeric.misses.length < 12) numeric.misses.push({ text: value.slice(0, 96), selector: selectorHint(element), type, fontVariantNumeric: style.fontVariantNumeric });
      numeric[type].total += 1;
      if (tabular) numeric[type].tabular += 1;
    }
    const hanCount = (value.match(hanGlobal) || []).length;
    const bodyLike = hanCount >= 30 && Number.parseFloat(style.fontSize) >= 12 && Number.parseFloat(style.fontSize) <= 18
      && Number.parseFloat(style.lineHeight) / Number.parseFloat(style.fontSize) >= 1.35;
    if (!bodyLike) continue;
    const lineCounts = new Map();
    const raw = textNode.nodeValue || '';
    for (let index = 0; index < raw.length; index += 1) {
      if (!hanChar.test(raw[index])) continue;
      const range = document.createRange();
      range.setStart(textNode, index);
      range.setEnd(textNode, index + 1);
      const rect = range.getBoundingClientRect();
      if (rect.width <= 0 || rect.height <= 0) continue;
      const key = Math.round(rect.top);
      lineCounts.set(key, (lineCounts.get(key) || 0) + 1);
    }
    for (const [top, count] of lineCounts.entries()) {
      cjkLines.push({ count, top, selector: selectorHint(element), text: value.slice(0, 96) });
    }
  }
  const box = (selector) => {
    const element = document.querySelector(selector);
    if (!element || !isVisible(element)) return null;
    const rect = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    return {
      selector, x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height),
      padding: [style.paddingTop, style.paddingRight, style.paddingBottom, style.paddingLeft].join(' '),
      gap: style.gap, display: style.display, gridTemplateColumns: style.gridTemplateColumns,
    };
  };
  const sider = document.querySelector('.pool-shell-sider');
  const siderStyle = sider ? getComputedStyle(sider) : null;
  const navRows = sider ? [...sider.querySelectorAll('.pool-nav-item')]
    .filter(isVisible)
    .map((item) => Math.round(item.getBoundingClientRect().height)) : [];
  const unique = (values) => [...new Set(values)].sort((a, b) => a - b);
  const resourceEntries = performance.getEntriesByType('resource')
    .map((entry) => ({ name: entry.name.split('/').slice(-1)[0], duration: Math.round(entry.duration), transfer: entry.transferSize || 0, decoded: entry.decodedBodySize || 0 }))
    .sort((a, b) => b.transfer - a.transfer || b.duration - a.duration)
    .slice(0, 8);
  const navigation = performance.getEntriesByType('navigation')[0];
  const pagehead = document.querySelector('.pool-pagehead');
  const pageheadStyle = pagehead ? getComputedStyle(pagehead) : null;
  const main = document.querySelector('.pool-content');
  const shell = document.querySelector('.pool-shell');
  const cjk = {
    eligibleLines: cjkLines.length,
    within30to45: cjkLines.filter((line) => line.count >= 30 && line.count <= 45).length,
    below30: cjkLines.filter((line) => line.count < 30).length,
    above45: cjkLines.filter((line) => line.count > 45).length,
    min: cjkLines.length ? Math.min(...cjkLines.map((line) => line.count)) : null,
    max: cjkLines.length ? Math.max(...cjkLines.map((line) => line.count)) : null,
    samples: cjkLines.slice(0, 6),
  };
  return {
    ready: Boolean(document.querySelector('.pool-route-content[data-page-ready="true"]')),
    typography: [...signatures.values()].sort((a, b) => b.count - a.count || a.signature.localeCompare(b.signature)),
    numeric,
    cjk,
    layout: {
      documentWidth: document.documentElement.scrollWidth,
      viewportWidth: innerWidth,
      pageOverflow: document.documentElement.scrollWidth > innerWidth + 1,
      shellOverflow: main ? main.scrollWidth - main.clientWidth : null,
      scrollHost: main ? {
        clientHeight: main.clientHeight,
        scrollHeight: main.scrollHeight,
        scrollTop: main.scrollTop,
        scrollable: main.scrollHeight > main.clientHeight + 1,
      } : null,
      windowScrollHeight: document.documentElement.scrollHeight,
      shell: box('.pool-shell'),
      pagehead: box('.pool-pagehead'),
      resourceSplit: box('.pool-resource-split'),
      pageheadGap: pageheadStyle?.gap || null,
      pageheadMarginBottom: pageheadStyle?.marginBottom || null,
    },
    sidebar: sider ? {
      width: Math.round(sider.getBoundingClientRect().width), height: Math.round(sider.getBoundingClientRect().height),
      position: siderStyle.position, transform: siderStyle.transform, ariaHidden: sider.getAttribute('aria-hidden'),
      navItems: navRows.length, navRowHeights: unique(navRows), groups: sider.querySelectorAll('.pool-nav-section').length,
      currentItems: sider.querySelectorAll('[aria-current="page"]').length,
      focusable: sider.querySelectorAll('button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])').length,
    } : null,
    interaction: {
      buttons: root.querySelectorAll('button').length,
      disabled: root.querySelectorAll('button:disabled, input:disabled, select:disabled, textarea:disabled, [aria-disabled="true"]').length,
      busy: root.querySelectorAll('[aria-busy="true"]').length,
      errors: root.querySelectorAll('[role="alert"], .pool-field__error, .pool-error-state').length,
      empty: root.querySelectorAll('.pool-empty-state, .pool-empty, [data-empty="true"]').length,
      focusable: root.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])').length,
    },
    performance: {
      domNodes: document.getElementsByTagName('*').length,
      resources: performance.getEntriesByType('resource').length,
      domContentLoadedMS: navigation ? Math.round(navigation.domContentLoadedEventEnd) : null,
      loadMS: navigation ? Math.round(navigation.loadEventEnd) : null,
      largestResources: resourceEntries,
    },
  };
})()`;

function summarize(results) {
  const successes = results.filter((result) => !result.error);
  const byViewport = Object.fromEntries(viewports.map((viewport) => {
    const items = successes.filter((result) => result.viewport === viewport.name);
    const numericTotal = items.reduce((sum, item) => sum + item.metrics.numeric.total, 0);
    const numericTabular = items.reduce((sum, item) => sum + item.metrics.numeric.tabular, 0);
    const cjk = items.reduce((sum, item) => {
      sum.eligible += item.metrics.cjk.eligibleLines;
      sum.within += item.metrics.cjk.within30to45;
      sum.below += item.metrics.cjk.below30;
      sum.above += item.metrics.cjk.above45;
      return sum;
    }, { eligible: 0, within: 0, below: 0, above: 0 });
    return [viewport.name, {
      measured: items.length,
      pageOverflow: items.filter((item) => item.metrics.layout.pageOverflow).length,
      shellOverflow: items.filter((item) => (item.metrics.layout.shellOverflow || 0) > 1).length,
      numericTotal, numericTabular, numericCoverage: numericTotal ? numericTabular / numericTotal : null,
      cjk,
      avgDomNodes: items.length ? Math.round(items.reduce((sum, item) => sum + item.metrics.performance.domNodes, 0) / items.length) : 0,
      maxDomNodes: items.length ? Math.max(...items.map((item) => item.metrics.performance.domNodes)) : 0,
    }];
  }));
  return {
    expected: results.length,
    measured: successes.length,
    errors: results.filter((result) => result.error),
    byViewport,
  };
}

async function main() {
  const onlyRoute = readArg('--route');
  const routes = extractRoutes().filter((route) => !onlyRoute || route.path === onlyRoute);
  if (!routes.length) throw new Error(`no declared route matches ${onlyRoute || '(empty selection)'}`);
  const port = await findPort();
  const baseURL = `http://127.0.0.1:${port}/console`;
  const server = startServer(port);
  const results = [];
  try {
    await waitForServer(server);
    const browser = await puppeteer.launch({
      headless: 'new',
      args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-background-timer-throttling'],
    });
    try {
      // Typography and geometry are theme-invariant. Both token themes are measured
      // by the dedicated contrast run; avoiding a duplicate 70-page paint sweep
      // keeps this source-layout probe bounded.
      for (const theme of ['light']) {
        for (const viewport of viewports) {
          for (const role of ['admin', 'user']) {
            const page = await preparePage(browser, baseURL, role, viewport, theme);
            try {
              for (const route of routes.filter((item) => item.role === role)) {
                try {
                  console.error(`Aurora P0 runtime progress: ${theme}/${viewport.name}/${role}${route.path}`);
                  await gotoApp(page, baseURL, route.path);
                  const browserMetrics = await page.evaluate(BROWSER_MEASURE);
                  results.push({ route: route.path, role, line: route.line, theme, viewport: viewport.name, metrics: browserMetrics });
                } catch (error) {
                  results.push({ route: route.path, role, line: route.line, theme, viewport: viewport.name, error: String(error?.message || error) });
                }
              }
            } finally {
              await page.close();
            }
          }
        }
      }
    } finally {
      await browser.close();
    }
  } finally {
    await stopServer(server);
  }
  const payload = { generatedAt: new Date().toISOString(), routes, viewports, results, summary: summarize(results) };
  const out = readArg('--out');
  const text = `${JSON.stringify(payload, null, 2)}\n`;
  if (out) {
    fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
    fs.writeFileSync(out, text);
  } else process.stdout.write(text);
  console.log(`Aurora P0 runtime: ${payload.summary.measured}/${payload.summary.expected} route/viewport/theme measurements; output ${out || 'stdout'}`);
  if (payload.summary.errors.length) process.exitCode = 1;
}

main().catch((error) => { console.error(error); process.exit(1); });
