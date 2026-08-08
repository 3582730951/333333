// Detects visual collisions in the rendered SPA: text-vs-text overlap,
// content overflowing its container, and horizontal page overflow.
// Runs against the same Vite dev server + API fixtures as capture-ui-review.mjs.
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const outDir = path.join(workspaceRoot, '.run', 'overlap');
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;

const viewports = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '1280x720', width: 1280, height: 720 },
  { name: '820x1180', width: 820, height: 1180 },
  { name: '390x844', width: 390, height: 844, mobile: true },
  { name: '360x800', width: 360, height: 800, mobile: true },
];

const adminPages = [
  ['Dashboard', '/'],
  ['Accounts', '/accounts'],
  ['Usage', '/usage'],
  ['Quota', '/quota'],
  ['ModelQuality', '/model-quality'],
  ['System', '/system'],
  ['Keys', '/keys'],
  ['Users', '/users'],
  ['Audit', '/audit'],
  ['Groups', '/groups'],
  ['Providers', '/providers'],
  ['Settings', '/settings-v2'],
];
const userPages = [
  ['PortalDashboard', '/portal'],
  ['PortalKeys', '/portal/keys'],
  ['PortalModels', '/portal/models'],
  ['PortalProfile', '/portal/profile'],
];

function pick(list, envName) {
  const values = String(process.env[envName] || '').split(',').map((s) => s.trim()).filter(Boolean);
  return values.length ? list.filter(([name]) => values.includes(name)) : list;
}
// The default matrix is the widest desktop and the narrowest phone in both themes:
// those four combinations catch essentially every collision the full five-viewport
// sweep does, while staying fast enough to run in `npm run check`. Override with
// OVERLAP_VIEWPORTS / OVERLAP_THEMES for an exhaustive pass.
const DEFAULT_VIEWPORTS = ['1440x900', '360x800'];

function pickViewports() {
  const values = String(process.env.OVERLAP_VIEWPORTS || '').split(',').map((s) => s.trim()).filter(Boolean);
  const wanted = values.length ? values : DEFAULT_VIEWPORTS;
  return viewports.filter((v) => wanted.includes(v.name));
}
const activeThemes = String(process.env.OVERLAP_THEMES || 'light,dark').split(',').map((s) => s.trim()).filter(Boolean);

function canUsePort(port) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once('error', () => resolve(false));
    server.once('listening', () => server.close(() => resolve(true)));
    server.listen(port, '127.0.0.1');
  });
}
async function findPort(start) {
  for (let port = start; port < start + 40; port += 1) if (await canUsePort(port)) return port;
  throw new Error('no free port');
}
function startServer(port) {
  return spawn(process.execPath, [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], {
    cwd: root, stdio: ['ignore', 'pipe', 'pipe'],
  });
}
function waitForServer(child) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timeout = setTimeout(() => { if (!settled) { settled = true; reject(new Error('vite not ready')); } }, 60000);
    const onData = (chunk) => {
      if (settled) return;
      if (serverReadyPattern.test(String(chunk))) { settled = true; clearTimeout(timeout); resolve(); }
    };
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', (code) => { if (!settled) { settled = true; clearTimeout(timeout); reject(new Error(`vite exited ${code}`)); } });
  });
}
async function stopServer(child) {
  if (!child || child.exitCode !== null) return;
  await new Promise((resolve) => {
    const force = setTimeout(() => child.kill('SIGKILL'), 3000);
    child.once('exit', () => { clearTimeout(force); resolve(); });
    child.kill('SIGTERM');
  });
}

// The overlap probe runs inside the page.
const PROBE = `(() => {
  const EPS = 1.0;                 // sub-pixel tolerance
  const results = { textOverlaps: [], overflow: [], clipped: [], scrollable: [], pageOverflow: null };

  const intersectRect = (a, b) => {
    const left = Math.max(a.left, b.left);
    const top = Math.max(a.top, b.top);
    const right = Math.min(a.right, b.right);
    const bottom = Math.min(a.bottom, b.bottom);
    if (right <= left || bottom <= top) return null;
    return { left, top, right, bottom, width: right - left, height: bottom - top };
  };

  // An element is only really on screen where every clipping ancestor still shows it.
  // Without this, content scrolled out of a pane reports stale coordinates and looks
  // like a collision.
  const viewportRect = { left: 0, top: 0, right: innerWidth, bottom: innerHeight };
  const clipCache = new WeakMap();
  const clipRectFor = (el) => {
    if (!el || el === document.documentElement) return viewportRect;
    if (clipCache.has(el)) return clipCache.get(el);
    const parentClip = clipRectFor(el.parentElement);
    let clip = parentClip;
    if (parentClip) {
      const cs = getComputedStyle(el);
      const clips = cs.overflow !== 'visible' && cs.overflow !== '';
      if (clips) {
        const r = el.getBoundingClientRect();
        clip = intersectRect(parentClip, { left: r.left, top: r.top, right: r.right, bottom: r.bottom });
      }
    }
    clipCache.set(el, clip);
    return clip;
  };
  const visibleRectOf = (el) => {
    const r = el.getBoundingClientRect();
    const clip = clipRectFor(el.parentElement);
    if (!clip) return null;
    return intersectRect(clip, { left: r.left, top: r.top, right: r.right, bottom: r.bottom });
  };

  // Anything painted in a fixed/sticky layer (app bar, bottom tab bar, drawer,
  // toast, dialog) floats over normal flow by design. Comparing across layers
  // reports the pattern itself as a defect, so each element carries its layer.
  const layerCache = new WeakMap();
  const layerOf = (el) => {
    if (!el || el === document.body || el === document.documentElement) return 0;
    if (layerCache.has(el)) return layerCache.get(el);
    const cs = getComputedStyle(el);
    let layer;
    if (cs.position === 'fixed' || cs.position === 'sticky') layer = 1;
    else if (el.matches('[role="dialog"], [role="tooltip"], .pool-toast, .pool-modal-overlay, .pool-drawer-overlay, .pool-chart-tooltip, .pool-skip-link')) layer = 1;
    else layer = layerOf(el.parentElement);
    layerCache.set(el, layer);
    return layer;
  };

  const visible = (el, style) => {
    if (style.visibility === 'hidden' || style.display === 'none') return false;
    if (Number(style.opacity) < 0.05) return false;
    if (el.closest('[aria-hidden="true"]')) return false;
    if (el.closest('[hidden]')) return false;
    if (el.closest('[inert]')) return false;
    return true;
  };

  // Collect elements that directly render text (their own text nodes, not descendants').
  const textNodes = [];
  const all = document.querySelectorAll('body *');
  for (const el of all) {
    if (el.closest('svg')) continue;                     // chart internals handled by the library
    const tag = el.tagName.toLowerCase();
    if (tag === 'script' || tag === 'style' || tag === 'br') continue;
    let own = '';
    for (const node of el.childNodes) if (node.nodeType === 3) own += node.nodeValue;
    own = own.replace(/\\s+/g, ' ').trim();
    if (!own) continue;
    const style = getComputedStyle(el);
    if (!visible(el, style)) continue;
    const rect = visibleRectOf(el);
    if (!rect || rect.width <= 0.5 || rect.height <= 0.5) continue;
    textNodes.push({
      el, rect, text: own.slice(0, 60),
      layer: layerOf(el),
      path: (() => {
        const parts = [];
        let n = el;
        for (let i = 0; n && i < 4; i += 1) {
          parts.unshift(n.tagName.toLowerCase() + (n.className && typeof n.className === 'string' ? '.' + n.className.trim().split(/\\s+/).slice(0, 2).join('.') : ''));
          n = n.parentElement;
        }
        return parts.join('>');
      })(),
    });
  }

  const related = (a, b) => a.contains(b) || b.contains(a);

  for (let i = 0; i < textNodes.length; i += 1) {
    for (let j = i + 1; j < textNodes.length; j += 1) {
      const a = textNodes[i], b = textNodes[j];
      if (a.layer !== b.layer) continue;                 // overlay above content is intentional
      if (related(a.el, b.el)) continue;
      const hit = intersectRect(a.rect, b.rect);
      if (!hit) continue;
      if (hit.width < 1.5 || hit.height < 1.5) continue; // hairline touch, not a collision
      results.textOverlaps.push({
        a: a.text, b: b.text, aPath: a.path, bPath: b.path,
        overlap: { w: Math.round(hit.width), h: Math.round(hit.height) },
        aRect: { x: Math.round(a.rect.left), y: Math.round(a.rect.top), w: Math.round(a.rect.width), h: Math.round(a.rect.height) },
        bRect: { x: Math.round(b.rect.left), y: Math.round(b.rect.top), w: Math.round(b.rect.width), h: Math.round(b.rect.height) },
      });
    }
  }

  // Content spilling past its own card boundary (text escaping a panel border).
  // Measured against the card's real border box, not the viewport-clipped one.
  for (const el of all) {
    if (el.closest('svg')) continue;
    if (!el.classList.contains('pool-card') && !el.classList.contains('pool-panel') && !el.classList.contains('pool-chart-card')) continue;
    const style = getComputedStyle(el);
    if (style.overflow !== 'visible' && style.overflow !== '') continue;
    if (!visible(el, style)) continue;
    const rect = el.getBoundingClientRect();
    if (rect.width <= 0.5 || rect.height <= 0.5) continue;
    for (const child of el.children) {
      const cs = getComputedStyle(child);
      if (!visible(child, cs)) continue;
      if (cs.position === 'absolute' || cs.position === 'fixed') continue;
      const cr = child.getBoundingClientRect();
      if (cr.width <= 0.5 || cr.height <= 0.5) continue;
      const spillRight = cr.right - rect.right;
      const spillBottom = cr.bottom - rect.bottom;
      if (spillRight > 2 || spillBottom > 2) {
        results.overflow.push({
          container: el.className, child: child.className,
          spillRight: Math.round(spillRight), spillBottom: Math.round(spillBottom),
        });
      }
    }
  }

  const de = document.documentElement;
  if (de.scrollWidth - de.clientWidth > 2) {
    results.pageOverflow = { scrollWidth: de.scrollWidth, clientWidth: de.clientWidth };
  }

  // Data clipped at a container edge. A table that cannot fit its own columns on a
  // wide screen hides values behind the card border, which reads exactly like an
  // overlap to the operator even though the boxes never intersect.
  if (innerWidth >= 1100) {
    for (const el of document.querySelectorAll('.pool-table-wrapper, .pool-table, .pool-mobile-list')) {
      const style = getComputedStyle(el);
      if (!visible(el, style)) continue;
      if (el.scrollWidth - el.clientWidth <= 4) continue;
      // A pane the user can actually scroll is an affordance, not hidden data.
      const scrollable = style.overflowX === 'auto' || style.overflowX === 'scroll';
      (scrollable ? results.scrollable : results.clipped).push({
        kind: scrollable ? 'horizontal-scroll' : 'horizontal-clip',
        selector: el.className,
        scrollWidth: el.scrollWidth,
        clientWidth: el.clientWidth,
      });
    }
  }

  // Text hidden without an ellipsis affordance: the string is simply cut in half.
  for (const el of all) {
    if (el.closest('svg')) continue;
    if (!el.childNodes.length) continue;
    let own = '';
    for (const node of el.childNodes) if (node.nodeType === 3) own += node.nodeValue;
    if (!own.trim()) continue;
    const style = getComputedStyle(el);
    if (style.overflow === 'visible' || style.overflow === '') continue;
    if (style.textOverflow === 'ellipsis') continue;
    if (style.overflowX === 'auto' || style.overflowX === 'scroll') continue;
    if (!visible(el, style)) continue;
    // Visually-hidden helpers (skip links, live regions) are clipped on purpose.
    if (el.closest('.pool-sr-only') || style.clipPath === 'inset(50%)') continue;
    const box = el.getBoundingClientRect();
    if (box.width <= 2 || box.height <= 2) continue;
    if (el.scrollWidth - el.clientWidth > 4) {
      results.clipped.push({
        kind: 'text-cut',
        selector: el.className || el.tagName.toLowerCase(),
        text: own.replace(/\\s+/g, ' ').trim().slice(0, 50),
        scrollWidth: el.scrollWidth,
        clientWidth: el.clientWidth,
      });
    }
  }

  return results;
})()`;

// Flip the desktop sidebar between its collapsed and expanded rail. Returns false
// when the control is absent (portal shell, or a mobile viewport where the sidebar
// is a drawer rather than a persistent rail).
async function toggleSidebar(page) {
  const button = await page.$('.pool-sidebar-collapse button');
  if (!button) return false;
  const visible = await button.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  });
  if (!visible) return false;
  await button.click();
  // Let the width transition settle before re-measuring.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await new Promise((r) => setTimeout(r, 350));
  return true;
}

async function gotoApp(page, baseURL, route) {  await page.goto(`${baseURL}${route}`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await page.waitForFunction(() => {
    const content = document.querySelector('.pool-route-content[data-page-ready="true"]');
    return content && content.innerText.trim().length > 10;
  }, { timeout: 45000 }).catch(() => {});
  await page.waitForNetworkIdle({ idleTime: 300, timeout: 6000 }).catch(() => {});
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await new Promise((r) => setTimeout(r, 250));
}

async function main() {
  fs.rmSync(outDir, { recursive: true, force: true });
  fs.mkdirSync(outDir, { recursive: true });
  const port = await findPort(Number(process.env.OVERLAP_PORT || 5292));
  const baseURL = `http://127.0.0.1:${port}/console`;
  const server = startServer(port);
  const findings = [];
  try {
    await waitForServer(server);
    const { installMocks } = await import('./capture-ui-review.mjs');
    const browser = await puppeteer.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
    try {
      for (const theme of activeThemes) {
        for (const viewport of pickViewports()) {
          for (const [role, pages] of [['admin', pick(adminPages, 'OVERLAP_PAGES')], ['user', pick(userPages, 'OVERLAP_PAGES')]]) {
            if (!pages.length) continue;
            const page = await browser.newPage();
            await installMocks(page);
            await page.setViewport({ width: viewport.width, height: viewport.height, isMobile: !!viewport.mobile, hasTouch: !!viewport.mobile, deviceScaleFactor: 1 });
            await page.emulateMediaFeatures([
              { name: 'prefers-color-scheme', value: theme },
              { name: 'prefers-reduced-motion', value: 'reduce' },
            ]);
            await page.setCookie(
              { url: baseURL, name: 'cp_session', value: `${role}-fixture`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
              { url: baseURL, name: 'cp_csrf', value: `${role}-csrf`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
            );
            await page.evaluateOnNewDocument((next) => { localStorage.setItem('pool_theme', next); }, theme);
            for (const [name, route] of pages) {
              try {
                await gotoApp(page, baseURL, route);
                const res = await page.evaluate(PROBE);
                if (res.textOverlaps.length || res.overflow.length || res.clipped.length || res.scrollable.length || res.pageOverflow) {
                  findings.push({ role, theme, viewport: viewport.name, page: name, route, ...res });
                }
                // The sidebar auto-collapses below 1360px, so width alone only ever
                // exercises one state per viewport. Toggling it covers the other half:
                // a collapsed rail on a wide screen and an expanded one on a narrow screen,
                // which is where content has the least room and collides first.
                const toggled = await toggleSidebar(page);
                if (toggled) {
                  const res2 = await page.evaluate(PROBE);
                  if (res2.textOverlaps.length || res2.overflow.length || res2.clipped.length || res2.scrollable.length || res2.pageOverflow) {
                    findings.push({ role, theme, viewport: `${viewport.name}+toggled-sidebar`, page: name, route, ...res2 });
                  }
                  await toggleSidebar(page);
                }
              } catch (error) {
                findings.push({ role, theme, viewport: viewport.name, page: name, route, error: error.message });
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
  fs.writeFileSync(path.join(outDir, 'findings.json'), `${JSON.stringify(findings, null, 2)}\n`);
  const totalOverlaps = findings.reduce((s, f) => s + (f.textOverlaps?.length || 0), 0);
  const totalOverflow = findings.reduce((s, f) => s + (f.overflow?.length || 0), 0);
  const totalClipped = findings.reduce((s, f) => s + (f.clipped?.length || 0), 0);
  const totalScrollable = findings.reduce((s, f) => s + (f.scrollable?.length || 0), 0);
  const totalPageOverflow = findings.filter((f) => f.pageOverflow).length;
  const errors = findings.filter((f) => f.error);
  console.log(`text overlaps: ${totalOverlaps}`);
  console.log(`container overflow: ${totalOverflow}`);
  console.log(`clipped data: ${totalClipped}`);
  console.log(`horizontally scrollable panes (informational): ${totalScrollable}`);
  console.log(`page horizontal overflow: ${totalPageOverflow}`);
  console.log(`errors: ${errors.length}`);
  for (const f of findings.slice(0, 40)) {
    if (f.error) { console.log(`  ERROR ${f.role}/${f.theme}/${f.viewport}/${f.page}: ${f.error}`); continue; }
    const head = `  ${f.role}/${f.theme}/${f.viewport}/${f.page}`;
    if (f.pageOverflow) console.log(`${head}: page scrollWidth ${f.pageOverflow.scrollWidth} > ${f.pageOverflow.clientWidth}`);
    for (const o of (f.textOverlaps || []).slice(0, 6)) {
      console.log(`${head}: TEXT "${o.a}" ⨯ "${o.b}" (${o.overlap.w}×${o.overlap.h})`);
    }
    for (const o of (f.overflow || []).slice(0, 4)) {
      console.log(`${head}: SPILL ${o.container} → right ${o.spillRight} bottom ${o.spillBottom}`);
    }
    for (const o of (f.clipped || []).slice(0, 6)) {
      console.log(`${head}: CLIP [${o.kind}] ${o.selector}${o.text ? ` "${o.text}"` : ''} ${o.scrollWidth}>${o.clientWidth}`);
    }
  }
  console.log(`\nreport: ${path.relative(workspaceRoot, path.join(outDir, 'findings.json'))}`);
  if (totalOverlaps || totalOverflow || totalClipped || totalPageOverflow || errors.length) process.exit(1);
}

main().catch((error) => { console.error(error); process.exit(1); });
