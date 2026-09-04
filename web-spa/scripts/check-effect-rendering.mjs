// AURORA P7 acceptance gate: does each effect actually render in a real browser?
//
// docs/aurora/FINAL-ACCEPTANCE.md §A states the question this answers:
// "代码里写了特效 ≠ 浏览器里真的看得见特效". Unit tests and the GLSL gate check
// the first; only a real GPU and real pixels check the second. So every effect
// is loaded on its own into a real WebGL2 context, rendered at two distinct
// clock times, and its pixels are read back and counted.
//
// Three outcomes are recorded per effect, and the third is a failure, never a
// "designed degradation":
//   real-render   pixels are present AND they move (over time or under input)
//   dom-fallback  WebGL refused, and the effect's own applyDomFallback ran
//   absent        nothing was drawn and nothing fell back
//
// An effect that draws something static that no input can change is also a
// failure (`inert`): it is on screen but it is not an effect.
//
// Determinism: the frame clock is stopped before sampling, then driven by
// replay(t) + renderOnce(). `replay()` only seeks -- it does not render -- so
// sampling without the renderOnce() reads a stale buffer and every effect looks
// dead. The pair below is the whole reason this gate reports non-zero pixels.
//
// The IDLE_AFTER_MS trap in FINAL-ACCEPTANCE applies to src/lib/atmosphere.js's
// self-driven rAF loop. This gate never relies on that loop: it owns the clock.
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const evidenceDir = path.join(workspaceRoot, '.run', 'aurora-p7');
const port = Number(process.env.EFFECT_RENDER_PORT || 5473);
const harnessPath = '/console/__aurora_effect_harness__';
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');

const VIEWPORT = { width: 480, height: 300 };
// A fill:'element' effect covers a fraction of the viewport, so the floor is an
// absolute pixel count rather than a share of the frame. Every raw count is
// printed regardless, so the threshold is auditable rather than load-bearing.
const MIN_VISIBLE_PIXELS = 64;
const MIN_CHANGED_PIXELS = 32;
const SAMPLE_TIMES = [0, 1.7];

// The one approved palette, in the float form effect shaders consume. Reading it
// from the document would give zeros on a bare harness page and make every
// effect render black -- which reads exactly like "no effect".
const PALETTE = {
  effect: {
    void: [0.04, 0.05, 0.09],
    near: [0.20, 0.35, 0.75],
    far: [0.45, 0.20, 0.70],
    glow: [0.35, 0.85, 0.95],
  },
};

function startServer() {
  return spawn(
    process.execPath,
    [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'],
    { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] },
  );
}

function waitForServer(child) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (fn, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.stdout.off('data', onData);
      child.off('exit', onExit);
      fn(value);
    };
    const timer = setTimeout(() => finish(reject, new Error('Vite server did not become ready in time')), 60000);
    const onData = (chunk) => { if (serverReadyPattern.test(String(chunk))) finish(resolve); };
    const onExit = (code) => finish(reject, new Error(`Vite server exited before ready: ${code}`));
    child.stdout.on('data', onData);
    child.once('exit', onExit);
  });
}

async function stopServer(child) {
  if (!child || child.exitCode !== null || child.signalCode) return;
  await new Promise((resolve) => {
    const force = setTimeout(() => { if (child.exitCode === null) child.kill('SIGKILL'); }, 3000);
    child.once('exit', () => { clearTimeout(force); resolve(); });
    child.kill('SIGTERM');
  });
}

async function openHarness(browser, { webgl }) {
  const page = await browser.newPage();
  await page.setViewport(VIEWPORT);
  if (!webgl) {
    // R1: prove the refusal path, by making getContext('webgl2') return null
    // before any application script runs.
    await page.evaluateOnNewDocument(() => {
      const original = HTMLCanvasElement.prototype.getContext;
      HTMLCanvasElement.prototype.getContext = function patched(type, ...rest) {
        if (String(type).startsWith('webgl')) return null;
        return original.call(this, type, ...rest);
      };
      Object.defineProperty(window, 'WebGL2RenderingContext', { value: undefined, configurable: true });
    });
  }
  await page.setRequestInterception(true);
  page.on('request', (request) => {
    if (request.url().endsWith('__aurora_effect_harness__')) {
      request.respond({
        status: 200,
        contentType: 'text/html',
        body: '<!doctype html><html><head><meta charset="utf-8"><title>aurora harness</title></head>'
          + '<body style="margin:0;background:#05070d">'
          + '<div id="layer" style="position:fixed;inset:0"></div></body></html>',
      });
      return;
    }
    request.continue().catch(() => {});
  });
  await page.goto(`http://127.0.0.1:${port}${harnessPath}`, { waitUntil: 'domcontentloaded', timeout: 60000 });
  return page;
}

// Everything below runs inside the page. It is one function so a single
// page.evaluate owns the whole lifecycle of one effect, including disposal:
// leaking a context past ~16 effects makes every later effect look "absent".
async function probeEffectInPage(effectId, options) {
  const { sampleTimes, palette, minChanged } = options;
  const layer = document.getElementById('layer');
  layer.innerHTML = '';
  const diagnostics = [];
  const report = {
    id: effectId,
    engineActive: false,
    admitted: false,
    visiblePixels: 0,
    animatedPixels: 0,
    interactivePixels: 0,
    drivenVisiblePixels: 0,
    inputChannel: 'none',
    domFallbackNodes: 0,
    totalPixels: 0,
    frames: [],
    diagnostics,
  };

  const [{ createEngineHost }, { createEffectInputDriver, interactionEffectIds }, registry] = await Promise.all([
    import('/console/src/engine/host.js'),
    import('/console/src/engine/effectInputs.js'),
    import('/console/src/engine/effects/registry.generated.js'),
  ]);

  let session = null;
  try {
    session = await createEngineHost({
      effectLayer: layer,
      fallbackRoot: layer,
      quality: 'high',
      capabilities: { quality: 'high' },
      atmosphere: { mode: 'none' },
      adaptiveQuality: false,
      palette,
      onDiagnostic: (message) => diagnostics.push(String(message)),
    });
  } catch (error) {
    diagnostics.push(`createEngineHost threw: ${String(error)}`);
    return report;
  }

  report.engineActive = Boolean(session.active);
  if (!session.active) {
    report.reason = session.reason;
    // R1: the refusal must still reach the effect's own DOM fallback.
    try {
      const module = await registry.effectLoaders[effectId]();
      module.applyDomFallback(layer, { state: 'fallback', reason: session.reason });
      report.domFallbackNodes = layer.querySelectorAll('*').length;
    } catch (error) {
      diagnostics.push(`applyDomFallback threw: ${String(error)}`);
    }
    session.dispose();
    return report;
  }

  report.admitted = await session.loadEffect(effectId, {});
  const canvas = session.effectCanvas;
  const probe = document.createElement('canvas');
  probe.width = canvas.width;
  probe.height = canvas.height;
  const context = probe.getContext('2d', { willReadFrequently: true });
  report.totalPixels = probe.width * probe.height;
  // Measurements come straight off the GL context. Compositing the WebGL canvas
  // into a 2D canvas premultiplies and re-rounds at 8 bits, which erases a low
  // alpha entirely: `ripple` peaks at alpha 6-16 and read back as literally zero
  // through drawImage while readPixels saw 5,984 lit pixels. The 2D canvas stays,
  // but only to produce the PNG evidence.
  const gl = canvas.getContext('webgl2');
  const pixelBuffer = new Uint8Array(canvas.width * canvas.height * 4);

  // The clock must be stopped or rAF keeps advancing time between the two reads
  // and "did it change" stops being a question about the effect.
  session.stop();

  const grab = (time) => {
    session.replay(time);
    session.renderOnce();
    gl.readPixels(0, 0, canvas.width, canvas.height, gl.RGBA, gl.UNSIGNED_BYTE, pixelBuffer);
    return pixelBuffer.slice();
  };
  // Evidence only, and only valid in the same task as the grab above, while the
  // drawing buffer still holds the frame.
  const snapshot = () => {
    context.clearRect(0, 0, probe.width, probe.height);
    context.drawImage(canvas, 0, 0);
    return probe.toDataURL('image/png');
  };
  const countVisible = (pixels) => {
    let visible = 0;
    for (let index = 3; index < pixels.length; index += 4) if (pixels[index] > 4) visible += 1;
    return visible;
  };
  const countChanged = (left, right) => {
    let changed = 0;
    for (let index = 0; index < left.length; index += 4) {
      if (left[index] !== right[index] || left[index + 1] !== right[index + 1]
        || left[index + 2] !== right[index + 2] || left[index + 3] !== right[index + 3]) changed += 1;
    }
    return changed;
  };

  // Runs the real tick loop briefly so simulate() advances, then restores the
  // stopped, deterministic state the samples are taken in.
  const settle = async () => {
    session.start();
    await new Promise((resolve) => window.setTimeout(resolve, 260));
    session.stop();
  };

  const first = grab(sampleTimes[0]);
  const firstPng = snapshot();
  const second = grab(sampleTimes[1]);
  const secondPng = snapshot();
  report.visiblePixels = Math.max(countVisible(first), countVisible(second));
  report.animatedPixels = countChanged(first, second);
  report.frames.push({ label: `t${sampleTimes[0]}`, png: firstPng }, { label: `t${sampleTimes[1]}`, png: secondPng });

  // An effect that does not move on the clock alone may still be correct: the
  // interaction and dataviz families are driven by the host. Give them the input
  // the application gives them, and only then decide.
  if (report.animatedPixels < minChanged && report.admitted) {
    const driverIds = interactionEffectIds();
    if (driverIds.includes(effectId)) {
      // The real P6 plumbing, not a private shortcut: if this path cannot move
      // the effect, the application cannot either.
      const driver = createEffectInputDriver({
        session,
        requestFrame: (callback) => window.setTimeout(() => callback(), 0),
        cancelFrame: (handle) => window.clearTimeout(handle),
        now: () => performance.now(),
      });
      driver.setLoadedEffects([effectId]);
      driver.setPointer(0.22, 0.78);
      driver.setScroll(0.85, 0.2);
      driver.press(0.22, 0.78);
      // Effects ease toward a new target inside simulate(), and simulate() only
      // runs on a real tick -- replay() seeks, renderOnce() renders, neither
      // simulates. So the clock has to actually run before the second read.
      await settle();
      report.inputChannel = 'input-driver';
      const driven = grab(sampleTimes[1]);
      report.interactivePixels = countChanged(second, driven);
      report.drivenVisiblePixels = countVisible(driven);
      report.frames.push({ label: 'input', png: snapshot() });
      driver.dispose();
    } else {
      // Host-driven uniforms (progress, rate, roll...): move each one well away
      // from its declared default and require the frame to follow.
      const module = await registry.effectLoaders[effectId]();
      // Only uniforms that declare a range are driven. Writing an invented value
      // into an unranged one is how this probe first "proved" that `ripple` does
      // not render: uTint is a vec3, and it was being handed a scalar.
      //
      // One point in the range is not enough. A transition is a sweep: at 79% of
      // every range at once, `ripple`'s ring has already travelled off the
      // viewport and the frame is legitimately empty. Several points are tried
      // and the strongest response is the verdict.
      const componentCount = { vec2: 2, vec3: 3, vec4: 4 };
      const buildParameters = (fraction) => {
        const parameters = {};
        for (const [name, definition] of Object.entries(module.manifest.uniforms)) {
          if (!Number.isFinite(definition.min) || !Number.isFinite(definition.max)) continue;
          const span = definition.max - definition.min;
          const components = componentCount[definition.type];
          if (components) {
            const value = [];
            for (let index = 0; index < components; index += 1) {
              value.push(definition.min + span * (index % 2 === 0 ? fraction : 1 - fraction));
            }
            parameters[name] = value;
          } else {
            parameters[name] = definition.min + span * fraction;
          }
        }
        return parameters;
      };
      report.inputChannel = 'host-parameters';
      let bestPng = null;
      for (const fraction of [0.22, 0.5, 0.79]) {
        session.setEffectParameters(effectId, buildParameters(fraction));
        await settle();
        const driven = grab(sampleTimes[1]);
        const changed = countChanged(second, driven);
        const visible = countVisible(driven);
        if (changed > report.interactivePixels || visible > report.drivenVisiblePixels) {
          bestPng = snapshot();
          report.drivenFraction = fraction;
        }
        report.interactivePixels = Math.max(report.interactivePixels, changed);
        report.drivenVisiblePixels = Math.max(report.drivenVisiblePixels, visible);
        if (report.interactivePixels >= minChanged && report.drivenVisiblePixels >= 64) break;
      }
      if (bestPng) report.frames.push({ label: 'input', png: bestPng });
    }
  }

  session.dispose();
  // Release the GL context eagerly; Chrome keeps only ~16 alive and a leak makes
  // every effect after the sixteenth report as absent.
  try { gl?.getExtension('WEBGL_lose_context')?.loseContext(); } catch { /* best effort */ }
  return report;
}

function classify(report) {
  if (!report.engineActive) {
    return report.domFallbackNodes > 0
      ? { verdict: 'dom-fallback', ok: true, why: `engine refused (${report.reason}); applyDomFallback produced ${report.domFallbackNodes} node(s)` }
      : { verdict: 'absent', ok: false, why: `engine refused (${report.reason}) and applyDomFallback produced no DOM` };
  }
  if (!report.admitted) {
    return { verdict: 'absent', ok: false, why: 'effect was not admitted by the compositor' };
  }
  // Visibility has to be judged across the driven frame too. A route transition
  // sitting at uProgress=0 draws nothing and is *supposed* to: calling that
  // "absent" before the input phase has run condemns a correct effect.
  const visible = Math.max(report.visiblePixels, report.drivenVisiblePixels || 0);
  const moved = Math.max(report.animatedPixels, report.interactivePixels);
  if (visible < MIN_VISIBLE_PIXELS && moved < MIN_CHANGED_PIXELS) {
    return {
      verdict: 'absent',
      ok: false,
      why: `${visible} non-transparent pixel(s) at rest or under input (floor ${MIN_VISIBLE_PIXELS}), `
        + `and nothing moved (${moved} px via ${report.inputChannel})`,
    };
  }
  if (moved < MIN_CHANGED_PIXELS) {
    return {
      verdict: 'inert',
      ok: false,
      why: `drawn (${visible} px) but unchanged by clock (${report.animatedPixels} px) `
        + `or input (${report.interactivePixels} px via ${report.inputChannel})`,
    };
  }
  const source = report.animatedPixels >= MIN_CHANGED_PIXELS ? 'clock' : `input:${report.inputChannel}`;
  return {
    verdict: 'real-render',
    ok: true,
    why: `${visible} px drawn, ${moved} px changed by ${source}`,
  };
}

function writeFrames(report) {
  const written = [];
  for (const frame of report.frames) {
    const base64 = String(frame.png).split(',')[1] || '';
    if (!base64) continue;
    const file = path.join(evidenceDir, `${report.id}-${frame.label}.png`);
    fs.writeFileSync(file, Buffer.from(base64, 'base64'));
    written.push(path.relative(workspaceRoot, file));
  }
  return written;
}

async function main() {
  fs.mkdirSync(evidenceDir, { recursive: true });
  const registryFile = path.join(root, 'src', 'engine', 'effects', 'registry.generated.js');
  const registrySource = fs.readFileSync(registryFile, 'utf8');
  const effectIds = [...registrySource.matchAll(/^\s*'([^']+)':\s*\(\)\s*=>\s*import\(/gm)].map((match) => match[1]);
  if (!effectIds.length) {
    console.error('Effect rendering check failed: registry.generated.js exposed no effects');
    process.exit(1);
  }
  // The denominator is printed before any work, so a crash midway can never be
  // mistaken for a smaller, fully-passing run.
  console.log(`Effect rendering acceptance: ${effectIds.length} effect(s) to verify in a real browser`);

  const server = startServer();
  const results = [];
  let fallbackSummary = null;
  try {
    await waitForServer(server);
    const browser = await puppeteer.launch({
      headless: 'new',
      args: [
        '--no-sandbox', '--disable-dev-shm-usage',
        '--enable-unsafe-swiftshader', '--use-gl=angle', '--use-angle=swiftshader',
      ],
    });
    try {
      const page = await openHarness(browser, { webgl: true });
      page.on('pageerror', (error) => console.log(`  page error: ${error.message}`));
      for (const effectId of effectIds) {
        let report;
        try {
          report = await page.evaluate(probeEffectInPage, effectId, {
            sampleTimes: SAMPLE_TIMES, palette: PALETTE, minChanged: MIN_CHANGED_PIXELS,
          });
        } catch (error) {
          report = {
            id: effectId, engineActive: false, admitted: false, visiblePixels: 0, animatedPixels: 0,
            interactivePixels: 0, inputChannel: 'none', domFallbackNodes: 0, totalPixels: 0,
            frames: [], diagnostics: [`probe threw: ${String(error && error.message ? error.message : error)}`],
            reason: 'probe-threw',
          };
        }
        const verdict = classify(report);
        const evidence = writeFrames(report);
        results.push({ ...report, frames: undefined, evidence, ...verdict });
        console.log(`  ${verdict.ok ? 'OK  ' : 'FAIL'} ${effectId.padEnd(24)} ${verdict.verdict.padEnd(13)} ${verdict.why}`);
        for (const message of report.diagnostics) console.log(`         diagnostic: ${message}`);
      }
      await page.close();

      // R1 in its own browsing context: WebGL2 removed before any script runs.
      const fallbackPage = await openHarness(browser, { webgl: false });
      const sample = effectIds[0];
      const fallbackReport = await fallbackPage.evaluate(probeEffectInPage, sample, {
        sampleTimes: SAMPLE_TIMES, palette: PALETTE, minChanged: MIN_CHANGED_PIXELS,
      });
      const fallbackVerdict = classify(fallbackReport);
      fallbackSummary = { id: sample, ...fallbackVerdict, engineActive: fallbackReport.engineActive, reason: fallbackReport.reason };
      console.log(`  ${fallbackVerdict.ok ? 'OK  ' : 'FAIL'} ${`${sample} (no-webgl)`.padEnd(24)} `
        + `${fallbackVerdict.verdict.padEnd(13)} ${fallbackVerdict.why}`);
      await fallbackPage.close();
    } finally {
      await browser.close();
    }
  } finally {
    await stopServer(server);
  }

  const byVerdict = results.reduce((counts, entry) => {
    counts[entry.verdict] = (counts[entry.verdict] || 0) + 1;
    return counts;
  }, {});
  const failures = results.filter((entry) => !entry.ok);
  const findings = {
    generated_at: new Date().toISOString(),
    denominator: effectIds.length,
    verified: results.length,
    by_verdict: byVerdict,
    webgl_unavailable_probe: fallbackSummary,
    results,
  };
  fs.writeFileSync(path.join(evidenceDir, 'findings.json'), `${JSON.stringify(findings, null, 2)}\n`);

  console.log(`Effect rendering acceptance: ${results.length}/${effectIds.length} effect(s) probed; `
    + `${Object.entries(byVerdict).map(([name, count]) => `${name}=${count}`).join(' ')}`);
  console.log(`Evidence: ${path.relative(workspaceRoot, evidenceDir)} (2+ frames per effect, findings.json)`);

  if (results.length !== effectIds.length) {
    console.error(`Effect rendering check failed: probed ${results.length} of ${effectIds.length} effects`);
    process.exit(1);
  }
  if (!fallbackSummary || !fallbackSummary.ok) {
    console.error('Effect rendering check failed: the WebGL-unavailable fallback path did not produce DOM');
    process.exit(1);
  }
  if (failures.length) {
    console.error(`Effect rendering check failed: ${failures.length} effect(s) are not really rendering`);
    for (const failure of failures) console.error(`  ${failure.id}: ${failure.verdict} -- ${failure.why}`);
    process.exit(1);
  }
  console.log('Effect rendering check passed: every effect renders in a real browser and responds to clock or input');
}

main().catch((error) => {
  console.error(`Effect rendering check crashed: ${error && error.stack ? error.stack : error}`);
  process.exit(1);
});
