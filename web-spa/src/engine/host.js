import { createCommandRingBuffer } from './commandBuffer.js';
import { copyEffectParameters, normalizeQuality, validateEffectModule } from './contracts.js';
import { createEffectCompositor } from './effectCompositor.js';
import { createFallbackManager } from './fallbackManager.js';
import { createFrameClock } from './frameClock.js';
import { createEffectContext } from './gl/effectContext.js';
import { createRectangleRenderer } from './rectangleRenderer.js';
import { effectLoaders } from './effects/registry.generated.js';
import { addWindowListener } from '../lib/browserLifecycle.js';
import { createFrameBudgetMonitor } from './frameBudgetMonitor.js';
import { createAdaptiveQualityMonitor, engineQuality } from './adaptiveQualityMonitor.js';

const NOOP = () => {};
const QUALITY_FACTORS = Object.freeze({ high: 1, medium: 0.62, low: 0.3 });
const QUALITY_DPR_CAPS = Object.freeze({ high: 1.5, medium: 1.25, low: 1 });
// The adaptive controller may only lower quality below what the caller asked
// for; sampling heap size every frame is a Chrome-only getter, so it is read on
// a slow cadence and treated as absent everywhere else.
const MEMORY_SAMPLE_INTERVAL_FRAMES = 30;

function performanceNow() {
  try {
    return typeof performance !== 'undefined' && typeof performance.now === 'function'
      ? performance.now()
      : Date.now();
  } catch {
    return Date.now();
  }
}

function readUsedHeapBytes() {
  try {
    return Number(performance.memory?.usedJSHeapSize) || 0;
  } catch {
    return 0;
  }
}

function copyColor(target, source) {
  if (!source || source.length < 3) return;
  target[0] = Number(source[0]) || 0;
  target[1] = Number(source[1]) || 0;
  target[2] = Number(source[2]) || 0;
}

function createPalette() {
  return {
    void: new Float32Array(3),
    near: new Float32Array(3),
    far: new Float32Array(3),
    glow: new Float32Array(3),
  };
}

function createEffectCanvas(effectLayer) {
  if (!effectLayer || typeof document === 'undefined') throw new Error('effectLayer is required to create a canvas');
  const canvas = document.createElement('canvas');
  canvas.setAttribute('aria-hidden', 'true');
  canvas.style.position = 'absolute';
  canvas.style.inset = '0';
  canvas.style.width = '100%';
  canvas.style.height = '100%';
  canvas.style.display = 'block';
  canvas.style.pointerEvents = 'none';
  canvas.style.zIndex = '1';
  effectLayer.append(canvas);
  return canvas;
}

function getWebGL2Context(canvas, quality) {
  try {
    return canvas.getContext('webgl2', {
      alpha: true,
      antialias: false,
      depth: false,
      stencil: false,
      preserveDrawingBuffer: false,
      powerPreference: quality === 'high' ? 'high-performance' : 'low-power',
    });
  } catch {
    return null;
  }
}

function browserPixelRatio() {
  try {
    return Math.max(1, Number(window.devicePixelRatio) || 1);
  } catch {
    return 1;
  }
}

function resolveQuality(requested, capabilities) {
  const requestedQuality = normalizeQuality(requested || capabilities?.quality);
  // A capability probe may lower the ceiling, but callers can always request a
  // less expensive profile for accessibility, power, or page-specific reasons.
  if (capabilities?.quality === 'low') return 'low';
  return requestedQuality;
}

function inactiveSession(fallback, reason) {
  return {
    active: false,
    reason,
    dispose() { fallback.dispose(); },
    start: NOOP,
    stop: NOOP,
    invalidate: NOOP,
    pause: NOOP,
    setTimeScale: NOOP,
    replay: NOOP,
    renderOnce: NOOP,
    loadEffect: async () => false,
    unloadEffect: () => false,
    setEffectParameters: () => false,
  };
}

/**
 * Lazy renderer host. It is deliberately framework-neutral: a React component
 * can own the DOM nodes, while this object owns only its two WebGL canvases and
 * never writes into the semantic content subtree.
 */
export async function createEngineHost(options = {}) {
  const effectLayer = options.effectLayer;
  const fallbackRoot = options.fallbackRoot || effectLayer || null;
  let quality = resolveQuality(options.quality, options.capabilities);
  const onDiagnostic = typeof options.onDiagnostic === 'function' ? options.onDiagnostic : NOOP;
  const requestedEffects = new Map();
  const commandBuffer = createCommandRingBuffer({
    capacity: options.commandCapacity || 256,
    overflow: 'drop-newest',
  });
  const framePalette = createPalette();
  const effectCanvas = options.effectCanvas || createEffectCanvas(effectLayer);
  let ownsEffectCanvas = !options.effectCanvas;
  let gl = null;
  let resources = null;
  let atmosphere = null;
  let clock = null;
  let disposed = false;
  let cssWidth = 0;
  let cssHeight = 0;
  let pixelRatio = 1;
  let fallbackCleanups = [];
  let resizeObserver = null;
  let removeWindowResize = NOOP;
  let simulateElapsedMs = 0;
  let memorySampleCountdown = 0;
  let lastHeapBytes = 0;
  let pendingQuality = null;
  let qualityCeiling = quality;

  function clearEffectFallbacks() {
    for (let index = 0; index < fallbackCleanups.length; index += 1) fallbackCleanups[index]();
    fallbackCleanups = [];
  }

  function activateDomFallback(detail) {
    clearEffectFallbacks();
    const cleanup = typeof options.onDomFallback === 'function' ? options.onDomFallback(detail) : null;
    if (typeof cleanup === 'function') fallbackCleanups.push(cleanup);
    for (const record of requestedEffects.values()) {
      try {
        const effectCleanup = record.module.applyDomFallback(fallbackRoot, detail);
        if (typeof effectCleanup === 'function') fallbackCleanups.push(effectCleanup);
      } catch (error) {
        onDiagnostic(`DOM fallback failed for ${record.id}: ${String(error)}`);
      }
    }
    return clearEffectFallbacks;
  }

  function destroyResources() {
    if (!resources) return;
    resources.compositor.dispose();
    resources.rectangleRenderer.dispose();
    resources.effectContext.dispose();
    resources = null;
  }

  function instantiateRecord(record, targetResources) {
    try {
      const instance = record.module.createEffect(targetResources.effectContext, record.parameters);
      if (!instance || typeof instance !== 'object') throw new Error('createEffect did not return an instance');
      const lifecycleMethods = ['setParameters', 'setQuality', 'resize', 'simulate', 'render', 'dispose'];
      for (let index = 0; index < lifecycleMethods.length; index += 1) {
        const method = lifecycleMethods[index];
        if (typeof instance[method] !== 'function') throw new Error(`effect instance lacks ${method}()`);
      }
      const admitted = targetResources.compositor.add(record.manifest, instance);
      if (!admitted) onDiagnostic(`effect not admitted at ${quality} quality: ${record.id}`);
      return true;
    } catch (error) {
      onDiagnostic(`effect initialization failed for ${record.id}: ${String(error)}`);
      return false;
    }
  }

  function buildGpuResources() {
    const nextGl = getWebGL2Context(effectCanvas, quality);
    if (!nextGl || nextGl.isContextLost()) return false;
    destroyResources();
    gl = nextGl;
    let effectContext = null;
    let compositor = null;
    let rectangleRenderer = null;
    try {
      effectContext = createEffectContext(gl, { onDiagnostic });
      compositor = createEffectCompositor({
        gl,
        quality,
        baseCostUnits: Number(options.atmosphere?.costBudgetUnits?.[quality]) || 0,
      });
      rectangleRenderer = createRectangleRenderer(gl, { capacity: commandBuffer.capacity });
      resources = { effectContext, compositor, rectangleRenderer };
      gl.disable(gl.DEPTH_TEST);
      gl.disable(gl.CULL_FACE);
      gl.enable(gl.BLEND);
      for (const record of requestedEffects.values()) instantiateRecord(record, resources);
      return true;
    } catch (error) {
      onDiagnostic(`WebGL initialization failed: ${String(error)}`);
      if (resources) destroyResources();
      else {
        rectangleRenderer?.dispose();
        compositor?.dispose();
        effectContext?.dispose();
      }
      return false;
    }
  }

  function syncSize() {
    if (disposed || !resources) return;
    const rect = effectCanvas.getBoundingClientRect();
    const nextCssWidth = Math.max(1, Math.round(rect.width || effectCanvas.clientWidth || 1));
    const nextCssHeight = Math.max(1, Math.round(rect.height || effectCanvas.clientHeight || 1));
    const nextPixelRatio = Math.min(browserPixelRatio(), QUALITY_DPR_CAPS[quality]);
    const bufferWidth = Math.max(1, Math.round(nextCssWidth * nextPixelRatio));
    const bufferHeight = Math.max(1, Math.round(nextCssHeight * nextPixelRatio));
    const changed = bufferWidth !== effectCanvas.width || bufferHeight !== effectCanvas.height;
    cssWidth = nextCssWidth;
    cssHeight = nextCssHeight;
    pixelRatio = nextPixelRatio;
    if (changed) {
      effectCanvas.width = bufferWidth;
      effectCanvas.height = bufferHeight;
      gl.viewport(0, 0, bufferWidth, bufferHeight);
      resources.compositor.resize(bufferWidth, bufferHeight, pixelRatio);
    }
    if (clock) {
      clock.frame.resolution[0] = bufferWidth;
      clock.frame.resolution[1] = bufferHeight;
      clock.frame.pixelRatio = pixelRatio;
    }
    if (atmosphere) atmosphere.resize(cssWidth, cssHeight, browserPixelRatio());
  }

  const fallback = createFallbackManager({
    root: fallbackRoot,
    canvas: effectCanvas,
    onStateChange(detail) {
      if (detail.state === 'fallback' || detail.state === 'restoring') clock?.stop();
      if (typeof options.onStateChange === 'function') options.onStateChange(detail);
    },
    onDomFallback: activateDomFallback,
    onDomRestore(detail) {
      if (typeof options.onDomRestore === 'function') options.onDomRestore(detail);
    },
    async onRestore() {
      if (disposed || !buildGpuResources()) return false;
      syncSize();
      clock?.start();
      return true;
    },
  });

  if (!buildGpuResources()) {
    fallback.fallback('webgl2-context-unavailable');
    return inactiveSession(fallback, 'webgl2-context-unavailable');
  }

  // P5 frame ledger. Both monitors preallocate their windows, and the hot path
  // below only ever hands them numbers, so enabling them does not break the R3
  // steady-state zero-allocation claim.
  const budgetMonitor = options.performanceMonitoring === false ? null : createFrameBudgetMonitor({
    windowSize: options.frameWindowSize || 120,
    onViolation: typeof options.onBudgetViolation === 'function' ? options.onBudgetViolation : NOOP,
  });
  const qualityMonitor = options.adaptiveQuality === false ? null : createAdaptiveQualityMonitor({
    initialQuality: quality === 'high' ? 'high' : quality === 'medium' ? 'medium' : 'power-saving',
    windowSize: options.qualityWindowSize || 60,
    memoryBudgetBytes: options.memoryBudgetBytes,
    onQualityChange(detail) {
      // Applied at the next frame boundary: setQuality() rebuilds sizes and
      // invalidates the clock, which must not happen inside a render callback.
      const next = detail.engineQuality || engineQuality(detail.quality);
      pendingQuality = QUALITY_FACTORS[next] === undefined ? null : clampToCeiling(next);
      if (typeof options.onQualityChange === 'function') options.onQualityChange(detail);
    },
  });

  clock = createFrameClock({
    fixedStep: options.fixedStep || 1 / 60,
    maxSteps: options.maxSteps || 5,
    simulate(deltaTime, frame) {
      if (!budgetMonitor) {
        resources?.compositor.simulate(deltaTime, frame);
        return;
      }
      const started = performanceNow();
      resources?.compositor.simulate(deltaTime, frame);
      simulateElapsedMs += performanceNow() - started;
    },
    render(interpolation, frame) {
      if (pendingQuality) {
        const next = pendingQuality;
        pendingQuality = null;
        applyQuality(next);
      }
      if (!resources || !gl || gl.isContextLost()) {
        simulateElapsedMs = 0;
        return;
      }
      const renderStart = performanceNow();
      frame.interpolation = interpolation;
      gl.viewport(0, 0, effectCanvas.width, effectCanvas.height);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND);
      resources.compositor.render(frame);
      resources.rectangleRenderer.render(commandBuffer, frame);
      if (!budgetMonitor) return;
      const renderEnd = performanceNow();
      const instructionsMs = simulateElapsedMs;
      const gpuMs = renderEnd - renderStart;
      simulateElapsedMs = 0;
      budgetMonitor.startFrame(renderStart - instructionsMs);
      budgetMonitor.recordPhase('instructions', instructionsMs);
      budgetMonitor.recordPhase('gpu', gpuMs);
      budgetMonitor.recordFrame(null, instructionsMs + gpuMs);
      if (!qualityMonitor) return;
      memorySampleCountdown -= 1;
      if (memorySampleCountdown <= 0) {
        memorySampleCountdown = MEMORY_SAMPLE_INTERVAL_FRAMES;
        lastHeapBytes = readUsedHeapBytes();
      }
      qualityMonitor.observe(instructionsMs + gpuMs, lastHeapBytes);
    },
  });
  clock.frame.resolution = new Float32Array(2);
  clock.frame.palette = framePalette;
  clock.frame.pixelRatio = 1;
  clock.frame.qualityFactor = QUALITY_FACTORS[quality];

  async function setupAtmosphere() {
    const configuration = options.atmosphere;
    if (!configuration || configuration.mode === 'none') return;
    try {
      const adapter = await import('./atmosphereAdapter.js');
      if (configuration.mode === 'external') {
        atmosphere = adapter.createExternalAtmosphereAdapter(configuration.controller);
      } else if (configuration.mode === 'reuse' && configuration.canvas) {
        atmosphere = await adapter.createAtmosphereAdapter({
          canvas: configuration.canvas,
          palette: configuration.palette,
          quality: configuration.quality || (quality === 'medium' ? 'balanced' : quality),
          onDiagnostic,
        });
      }
      if (atmosphere) syncSize();
    } catch (error) {
      // The existing CSS field remains in place if the optional base renderer
      // refuses a context; effects and semantic DOM still have their own paths.
      onDiagnostic(`atmosphere adapter unavailable: ${String(error)}`);
    }
  }

  function setPalette(palette) {
    const effectPalette = palette?.effect || palette;
    copyColor(framePalette.void, effectPalette?.void);
    copyColor(framePalette.near, effectPalette?.near);
    copyColor(framePalette.far, effectPalette?.far);
    copyColor(framePalette.glow, effectPalette?.glow);
    if (atmosphere && palette?.atmosphere) atmosphere.setPalette(palette.atmosphere);
    clock.renderOnce();
  }

  async function syncPaletteFromDocument(element) {
    try {
      const { readAtmosphereTokenPalette } = await import('./atmosphereAdapter.js');
      const palette = await readAtmosphereTokenPalette(element);
      setPalette(palette);
      return true;
    } catch (error) {
      onDiagnostic(`atmosphere palette read failed: ${String(error)}`);
      return false;
    }
  }

  function setAtmosphereInputs(inputs = {}) {
    if (!atmosphere) return;
    if (Number.isFinite(inputs.energy)) atmosphere.setEnergy(inputs.energy);
    if (Array.isArray(inputs.pointer)) atmosphere.setPointer(inputs.pointer[0], inputs.pointer[1]);
    if (Array.isArray(inputs.focus)) atmosphere.setFocus(inputs.focus[0], inputs.focus[1]);
    if (Array.isArray(inputs.scroll)) atmosphere.setScroll(inputs.scroll[0], inputs.scroll[1]);
    if (Number.isFinite(inputs.activity)) atmosphere.setActivity(inputs.activity);
  }

  async function loadEffect(id, parameters = {}) {
    if (disposed || !effectLoaders[id]) return false;
    if (requestedEffects.has(id)) unloadEffect(id);
    let module;
    try {
      module = await effectLoaders[id]();
    } catch (error) {
      onDiagnostic(`effect module failed to load for ${id}: ${String(error)}`);
      return false;
    }
    const validation = validateEffectModule(module);
    if (!validation.valid || module.manifest.id !== id) {
      onDiagnostic(`invalid effect module ${id}: ${validation.errors.join('; ')}`);
      return false;
    }
    const record = {
      id,
      module,
      manifest: module.manifest,
      parameters: copyEffectParameters(parameters),
    };
    requestedEffects.set(id, record);
    if (!resources) return false;
    const initialized = instantiateRecord(record, resources);
    if (!initialized) requestedEffects.delete(id);
    syncSize();
    clock.invalidate();
    return initialized;
  }

  function unloadEffect(id) {
    const existed = requestedEffects.delete(id);
    if (resources) resources.compositor.remove(id);
    return existed;
  }

  function setEffectParameters(id, nextParameters) {
    const record = requestedEffects.get(id);
    if (!record || !nextParameters || typeof nextParameters !== 'object') return false;
    const copy = copyEffectParameters(nextParameters);
    const names = Object.keys(copy);
    for (let index = 0; index < names.length; index += 1) record.parameters[names[index]] = copy[names[index]];
    // Effect instances receive changes at an event boundary, never in a frame
    // callback. A future context restoration reuses the saved record parameters.
    if (resources) resources.compositor.setParameters(id, copy);
    clock.invalidate();
    return true;
  }

  function queuePaletteRectangle(paletteName, x, y, width, height, alpha = 1) {
    const color = framePalette[paletteName] || framePalette.glow;
    const accepted = commandBuffer.pushRectangle(x, y, width, height, color[0], color[1], color[2], alpha);
    if (accepted) clock.invalidate();
    return accepted;
  }

  function start() {
    if (disposed) return;
    atmosphere?.start();
    clock.start();
  }

  function stop() {
    atmosphere?.stop();
    clock.stop();
  }

  // Applies a already-resolved level. The adaptive controller reaches the engine
  // only through here, so it can lower quality but never lift it above the
  // ceiling the caller and the capability probe agreed on.
  function applyQuality(resolved) {
    if (resolved === quality) return false;
    quality = resolved;
    clock.frame.qualityFactor = QUALITY_FACTORS[quality];
    resources?.compositor.setQuality(quality);
    resources?.compositor.setBaseCostUnits(Number(options.atmosphere?.costBudgetUnits?.[quality]) || 0);
    atmosphere?.setQuality(quality === 'medium' ? 'balanced' : quality);
    syncSize();
    clock.invalidate();
    return true;
  }

  function setQuality(nextQuality) {
    const resolved = resolveQuality(nextQuality, options.capabilities);
    qualityCeiling = resolved;
    applyQuality(resolved);
    return true;
  }

  function clampToCeiling(requested) {
    const order = ['low', 'medium', 'high'];
    return order.indexOf(requested) > order.indexOf(qualityCeiling) ? qualityCeiling : requested;
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    qualityMonitor?.dispose?.();
    stop();
    clearEffectFallbacks();
    resizeObserver?.disconnect();
    removeWindowResize();
    fallback.dispose();
    destroyResources();
    atmosphere?.dispose();
    if (ownsEffectCanvas) effectCanvas.remove();
    ownsEffectCanvas = false;
  }

  if (typeof ResizeObserver === 'function') {
    resizeObserver = new ResizeObserver(syncSize);
    resizeObserver.observe(effectLayer || effectCanvas);
  } else {
    // Same lifecycle rule as frameClock: the helper owns the no-window and
    // throwing-host cases and hands back its own remover.
    removeWindowResize = addWindowListener('resize', syncSize, { passive: true });
  }

  await setupAtmosphere();
  setPalette(options.palette);
  syncSize();
  fallback.activate('webgl2-ready');

  const initialEffects = Array.isArray(options.effects) ? options.effects : [];
  for (let index = 0; index < initialEffects.length; index += 1) {
    const requested = initialEffects[index];
    await loadEffect(requested.id, requested.parameters);
  }
  start();

  return {
    active: true,
    effectCanvas,
    commandBuffer,
    frame: clock.frame,
    start,
    stop,
    invalidate: clock.invalidate,
    pause: clock.pause,
    setTimeScale: clock.setTimeScale,
    replay: clock.replay,
    renderOnce: clock.renderOnce,
    loadEffect,
    unloadEffect,
    setEffectParameters,
    setPalette,
    syncPaletteFromDocument,
    setAtmosphereInputs,
    queuePaletteRectangle,
    syncSize,
    setQuality,
    dispose,
    copyMetrics(target) {
      commandBuffer.copyMetrics(target.commandBuffer || (target.commandBuffer = {}));
      clock.copyMetrics(target.clock || (target.clock = {}));
      resources?.compositor.copyMetrics(target.compositor || (target.compositor = {}));
      resources?.rectangleRenderer.copyMetrics(target.rectangles || (target.rectangles = {}));
      budgetMonitor?.copyMetrics(target.frameBudget || (target.frameBudget = {}));
      qualityMonitor?.copyMetrics(target.adaptiveQuality || (target.adaptiveQuality = {}));
      return target;
    },
    // Diagnostic boundary: allocates, so it is never called from a frame callback.
    performanceSnapshot() {
      return {
        quality,
        qualityCeiling,
        frameBudget: budgetMonitor ? budgetMonitor.snapshot() : null,
        adaptiveQuality: qualityMonitor ? qualityMonitor.snapshot() : null,
      };
    },
  };
}
