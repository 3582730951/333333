/**
 * Measures how close rendered text sits to the boundary that encloses it.
 *
 * Every existing gate in this repo asks "does A overlap B" or "does content exceed its scroll
 * width". Text sitting 1px inside a table border answers no to both, so the whole class of
 * cramped-against-the-edge defects is invisible to them -- they report 0 and mean it.
 *
 * What is measured, and why it is not the element box: an element's getBoundingClientRect
 * includes its own padding, so a cell with `padding: 10px 14px` always looks 14px away from its
 * own border no matter where the glyphs actually are. The ink box comes from a Range over the
 * text nodes instead, which is where the reader's eye lands.
 *
 * The enclosing boundary is the nearest ancestor that actually draws one: a visible border side,
 * or a background that differs from its parent's. A wrapper with no border and no background of
 * its own draws nothing, so it is not an edge -- measuring to it would invent findings.
 *
 * Diagnostic by default: prints the distribution so a threshold can be chosen from real numbers
 * rather than guessed. Pass --max-findings=N --min-gap=N to make it fail.
 */
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const outDir = path.join(root, '.run', 'edge-proximity');

const ADMIN_PAGES = [
  ['Dashboard', '/'], ['Accounts', '/accounts'], ['Groups', '/groups'], ['Providers', '/providers'],
  ['Models', '/models'], ['Egress', '/egress'], ['UpstreamErrors', '/upstream-error-rules'],
  ['Registration', '/registration'], ['TeamLifecycle', '/team-lifecycle'], ['EmailPool', '/email-pool'],
  ['CloudflareMailbox', '/email-pool/cloudflare'], ['Usage', '/usage'], ['Quota', '/quota'],
  ['ModelQuality', '/model-quality'], ['System', '/system'], ['CFEvents', '/cf-events'],
  ['Audit', '/audit'], ['Keys', '/keys'], ['Users', '/users'], ['Settings', '/settings-v2'],
  ['PublicChat', '/public-chat'],
];
const USER_PAGES = [
  ['PortalDashboard', '/portal'], ['PortalKeys', '/portal/keys'],
  ['PortalModels', '/portal/models'], ['PortalProfile', '/portal/profile'],
];
const VIEWPORTS = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '1280x720', width: 1280, height: 720 },
  { name: '820x1180', width: 820, height: 1180 },
  { name: '390x844', width: 390, height: 844, mobile: true },
  { name: '360x800', width: 360, height: 800, mobile: true },
];

const argOf = (name, fallback) => {
  const hit = process.argv.find((a) => a.startsWith(`--${name}=`));
  return hit ? Number(hit.split('=')[1]) : fallback;
};
const MIN_GAP = argOf('min-gap', 0);
const MAX_FINDINGS = argOf('max-findings', Infinity);
const ONLY_PAGE = process.env.EDGE_PAGES || '';

function canUsePort(port) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once('error', () => resolve(false));
    server.once('listening', () => server.close(() => resolve(true)));
    server.listen(port, '127.0.0.1');
  });
}
async function findPort(start) {
  for (let port = start; port < start + 60; port += 1) if (await canUsePort(port)) return port;
  throw new Error('no free port');
}
function startServer(port) {
  return spawn('npx', ['vite', '--port', String(port), '--strictPort', '--host', '127.0.0.1'], {
    cwd: root, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, BROWSER: 'none' },
  });
}
function waitForServer(child) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('vite did not start')), 90000);
    const onData = (buf) => {
      if (/Local:|ready in/i.test(String(buf))) { clearTimeout(timer); resolve(); }
    };
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`vite exited ${code}`)); });
  });
}
async function stopServer(child) {
  if (!child || child.killed) return;
  child.kill('SIGTERM');
  await new Promise((resolve) => { child.once('exit', resolve); setTimeout(resolve, 4000); });
}

const PROBE = () => {
  const findings = [];
  const gaps = [];
  let inspected = 0;
  let edgesFound = 0;

  const isTransparent = (color) => !color || color === 'transparent' || /rgba\(\s*0,\s*0,\s*0,\s*0\s*\)/.test(color);

  // An ancestor is an "edge" only if it visibly draws one. Three ways that happens:
  //   1. a border side with non-zero width and a non-transparent colour
  //   2. a background of its own that differs from what is behind it
  //   3. an overflow clip, which cuts glyphs at the padding box whether or not it is painted
  // Anything else (a bare layout div) draws nothing, and measuring to it would manufacture
  // findings on elements the reader cannot see the boundary of.
  const edgeInfo = (el, parentBg) => {
    const s = getComputedStyle(el);
    const sides = {
      top: parseFloat(s.borderTopWidth) > 0 && !isTransparent(s.borderTopColor),
      right: parseFloat(s.borderRightWidth) > 0 && !isTransparent(s.borderRightColor),
      bottom: parseFloat(s.borderBottomWidth) > 0 && !isTransparent(s.borderBottomColor),
      left: parseFloat(s.borderLeftWidth) > 0 && !isTransparent(s.borderLeftColor),
    };
    const ownBg = !isTransparent(s.backgroundColor) && s.backgroundColor !== parentBg;
    const clips = /(hidden|clip|auto|scroll)/.test(s.overflowX) || /(hidden|clip|auto|scroll)/.test(s.overflowY);
    const any = sides.top || sides.right || sides.bottom || sides.left || ownBg || clips;
    return { any, sides, ownBg, clips, bg: s.backgroundColor, style: s };
  };

  // Ink box, not element box: a Range over the element's own text nodes reports where glyphs
  // actually are, so a cell's padding does not get counted as clearance it does not have.
  const inkRect = (el) => {
    const range = document.createRange();
    let started = false;
    for (const node of el.childNodes) {
      if (node.nodeType !== Node.TEXT_NODE) continue;
      if (!node.textContent || !node.textContent.trim()) continue;
      if (!started) { range.setStartBefore(node); started = true; }
      range.setEndAfter(node);
    }
    if (!started) return null;
    const rects = [...range.getClientRects()].filter((r) => r.width > 0.5 && r.height > 0.5);
    if (!rects.length) return null;
    return {
      left: Math.min(...rects.map((r) => r.left)),
      right: Math.max(...rects.map((r) => r.right)),
      top: Math.min(...rects.map((r) => r.top)),
      bottom: Math.max(...rects.map((r) => r.bottom)),
    };
  };

  const describe = (el) => {
    const cls = typeof el.className === 'string' ? el.className.trim().split(/\s+/).slice(0, 3).join('.') : '';
    return `${el.tagName.toLowerCase()}${cls ? `.${cls}` : ''}`;
  };

  for (const el of document.querySelectorAll('*')) {
    if (/^(SCRIPT|STYLE|SVG|PATH|HEAD|META|LINK|TITLE|BR|HR|IMG|INPUT|TEXTAREA)$/.test(el.tagName)) continue;
    if (el.closest('[aria-hidden="true"]')) continue;
    const s = getComputedStyle(el);
    if (s.display === 'none' || s.visibility === 'hidden' || Number(s.opacity) === 0) continue;

    const ink = inkRect(el);
    if (!ink) continue;
    // Sub-pixel slivers and collapsed lines carry no readable glyph.
    if (ink.right - ink.left < 2 || ink.bottom - ink.top < 4) continue;
    inspected += 1;

    // Nearest drawing ancestor, including the element itself: a bordered cell is its own edge.
    let node = el;
    let edge = null;
    let hops = 0;
    while (node && hops < 12) {
      const parentBg = node.parentElement ? getComputedStyle(node.parentElement).backgroundColor : '';
      const info = edgeInfo(node, parentBg);
      if (info.any) { edge = { el: node, ...info }; break; }
      node = node.parentElement;
      hops += 1;
    }
    if (!edge) continue;
    edgesFound += 1;

    // Measure to the padding box: the border is drawn just outside it, so that is the line the
    // reader sees. Content clipping also happens there.
    const box = edge.el.getBoundingClientRect();
    const st = edge.style;
    const inner = {
      left: box.left + parseFloat(st.borderLeftWidth || 0),
      right: box.right - parseFloat(st.borderRightWidth || 0),
      top: box.top + parseFloat(st.borderTopWidth || 0),
      bottom: box.bottom - parseFloat(st.borderBottomWidth || 0),
    };

    // Only sides that actually draw something are candidates. A cell with `border-bottom` alone
    // is not cramped horizontally by a border that does not exist.
    const measured = {};
    if (edge.sides.left || edge.ownBg || edge.clips) measured.left = ink.left - inner.left;
    if (edge.sides.right || edge.ownBg || edge.clips) measured.right = inner.right - ink.right;
    if (edge.sides.top || edge.ownBg || edge.clips) measured.top = ink.top - inner.top;
    if (edge.sides.bottom || edge.ownBg || edge.clips) measured.bottom = inner.bottom - ink.bottom;

    const entries = Object.entries(measured).filter(([, v]) => Number.isFinite(v));
    if (!entries.length) continue;

    // Negative means the glyph is already outside the boundary -- that is a clip, which the
    // other gates cover. This tool is about the small-positive band they cannot see.
    const positive = entries.filter(([, v]) => v >= 0);
    if (!positive.length) continue;
    const [side, gap] = positive.reduce((min, cur) => (cur[1] < min[1] ? cur : min));

    gaps.push(Math.round(gap * 10) / 10);
    if (gap <= 6) {
      findings.push({
        el: describe(el),
        edge: describe(edge.el),
        side,
        gap: Math.round(gap * 10) / 10,
        via: edge.sides[side] ? 'border' : edge.ownBg ? 'background' : 'clip',
        text: (el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 44),
      });
    }
  }

  const sorted = gaps.slice().sort((a, b) => a - b);
  const at = (q) => (sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * q))] : null);
  return {
    inspected,
    edgesFound,
    measured: gaps.length,
    p01: at(0.01), p05: at(0.05), p10: at(0.1), p25: at(0.25), p50: at(0.5),
    under2: gaps.filter((g) => g < 2).length,
    under4: gaps.filter((g) => g < 4).length,
    under6: gaps.filter((g) => g < 6).length,
    under8: gaps.filter((g) => g < 8).length,
    findings: findings.sort((a, b) => a.gap - b.gap).slice(0, 14),
  };
};

async function gotoApp(page, baseURL, route) {
  await page.goto(`${baseURL}${route}`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await page.waitForFunction(() => {
    const root = document.getElementById('root');
    return root && root.children.length > 0;
  }, { timeout: 30000 });
  // Charts and meters settle a frame or two after mount; measuring before that reports the
  // pre-layout position of everything inside them.
  await new Promise((r) => setTimeout(r, 700));
}

async function main() {
  fs.rmSync(outDir, { recursive: true, force: true });
  fs.mkdirSync(outDir, { recursive: true });
  const port = await findPort(Number(process.env.EDGE_PORT || 5392));
  const baseURL = `http://127.0.0.1:${port}/console`;
  const server = startServer(port);
  const rows = [];
  let visits = 0;

  try {
    await waitForServer(server);
    const { installMocks } = await import('./capture-ui-review.mjs');
    const browser = await puppeteer.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
    try {
      for (const theme of ['light']) {
        for (const viewport of VIEWPORTS) {
          for (const [role, pages] of [['admin', ADMIN_PAGES], ['user', USER_PAGES]]) {
            const filtered = ONLY_PAGE ? pages.filter(([n]) => ONLY_PAGE.split(',').includes(n)) : pages;
            if (!filtered.length) continue;
            const page = await browser.newPage();
            await installMocks(page);
            await page.setViewport({
              width: viewport.width, height: viewport.height,
              isMobile: !!viewport.mobile, hasTouch: !!viewport.mobile, deviceScaleFactor: 1,
            });
            await page.emulateMediaFeatures([
              { name: 'prefers-color-scheme', value: theme },
              { name: 'prefers-reduced-motion', value: 'reduce' },
            ]);
            await page.setCookie(
              { url: baseURL, name: 'cp_session', value: `${role}-fixture`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
              { url: baseURL, name: 'cp_csrf', value: `${role}-csrf`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
            );
            await page.evaluateOnNewDocument((next) => { localStorage.setItem('pool_theme', next); }, theme);
            for (const [name, route] of filtered) {
              try {
                await gotoApp(page, baseURL, route);
                visits += 1;
                const res = await page.evaluate(PROBE);
                rows.push({ role, theme, viewport: viewport.name, page: name, route, ...res });
              } catch (err) {
                rows.push({ role, theme, viewport: viewport.name, page: name, route, error: String(err.message || err) });
              }
            }
            await page.close();
          }
        }
      }
    } finally {
      await browser.close();
    }
  } finally {
    await stopServer(server);
  }

  fs.writeFileSync(path.join(outDir, 'proximity.json'), JSON.stringify(rows, null, 2));

  const errors = rows.filter((r) => r.error);
  const ok = rows.filter((r) => !r.error);
  const sum = (key) => ok.reduce((s, r) => s + (r[key] || 0), 0);

  // A zero here has to be falsifiable the same way the overlap gate's is: if nothing was
  // measured, say so instead of printing a clean summary.
  console.log(`visited: ${visits} route renders, ${ok.length} measured, ${errors.length} errored`);
  console.log(`text runs inspected: ${sum('inspected')}, with a drawn edge: ${sum('edgesFound')}, gaps recorded: ${sum('measured')}`);
  if (!sum('measured')) {
    console.log('FAIL: no gap was measured at all -- the probe found no text inside any drawn boundary.');
    process.exit(1);
  }
  console.log(`gaps under 2px: ${sum('under2')}   under 4px: ${sum('under4')}   under 6px: ${sum('under6')}   under 8px: ${sum('under8')}`);

  const all = [];
  for (const r of ok) for (const f of r.findings || []) all.push({ ...f, at: `${r.role}/${r.viewport}/${r.page}` });
  all.sort((a, b) => a.gap - b.gap);

  // Group by the element+edge pair: one CSS rule usually explains every instance, so the
  // per-instance list is noise next to the count of distinct causes.
  const byCause = new Map();
  for (const f of all) {
    const key = `${f.el} inside ${f.edge} (${f.side}, ${f.via})`;
    const cur = byCause.get(key);
    if (cur) { cur.count += 1; cur.min = Math.min(cur.min, f.gap); cur.pages.add(f.at.split('/')[2]); }
    else byCause.set(key, { count: 1, min: f.gap, pages: new Set([f.at.split('/')[2]]), sample: f.text });
  }

  console.log(`\ndistinct causes at <=6px: ${byCause.size}`);
  const ranked = [...byCause.entries()].sort((a, b) => a[1].min - b[1].min || b[1].count - a[1].count);
  for (const [key, v] of ranked.slice(0, 24)) {
    console.log(`  ${String(v.min).padStart(5)}px x${String(v.count).padEnd(4)} ${key}`);
    console.log(`         pages: ${[...v.pages].slice(0, 6).join(', ')}${v.pages.size > 6 ? ` +${v.pages.size - 6}` : ''}  text: "${v.sample}"`);
  }
  if (ranked.length > 24) console.log(`  ... +${ranked.length - 24} more causes`);

  if (errors.length) {
    console.log('\nerrored renders:');
    for (const e of errors.slice(0, 8)) console.log(`  ${e.role}/${e.viewport}/${e.page}: ${e.error}`);
  }
  console.log(`\nreport: ${path.relative(root, path.join(outDir, 'proximity.json'))}`);

  const violations = all.filter((f) => f.gap < MIN_GAP);
  if (errors.length) { console.log(`FAIL: ${errors.length} render(s) errored`); process.exit(1); }
  if (violations.length > MAX_FINDINGS) {
    console.log(`FAIL: ${violations.length} text run(s) under ${MIN_GAP}px (allowed ${MAX_FINDINGS})`);
    process.exit(1);
  }
  console.log(MIN_GAP ? `edge proximity ok at >=${MIN_GAP}px` : 'diagnostic run (no threshold enforced)');
}

main().catch((err) => { console.error(err); process.exit(1); });
