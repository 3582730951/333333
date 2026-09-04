import { cancelBrowserAnimationFrame, requestBrowserAnimationFrame } from '../lib/browserLifecycle.js';

const NOOP = () => {};

function finitePositive(value, fallback) {
  const numeric = Number(value);
  return Number.isFinite(numeric) && numeric > 0 ? numeric : fallback;
}

// The repository routes every browser lifecycle primitive through
// lib/browserLifecycle.js so a single module owns the missing-API and
// throwing-host cases; the engine is not exempt from that. That helper already
// degrades to a timer on its own, and pairing it with the matching cancel is
// what keeps a timer id from being handed to cancelAnimationFrame or back.
const defaultRequestFrame = requestBrowserAnimationFrame;
const defaultCancelFrame = cancelBrowserAnimationFrame;

/**
 * Fixed-step simulation clock with a mutable, caller-owned frame object.  The
 * same frame object is passed to every simulation and render callback, avoiding
 * per-frame object creation while keeping shader time deterministic.
 */
export function createFrameClock({
  fixedStep = 1 / 60,
  maxDelta = 0.25,
  maxSteps = 5,
  simulate = NOOP,
  render = NOOP,
  requestFrame = defaultRequestFrame,
  cancelFrame = defaultCancelFrame,
} = {}) {
  const step = finitePositive(fixedStep, 1 / 60);
  const maximumDelta = finitePositive(maxDelta, 0.25);
  const maximumSteps = Math.max(1, Math.floor(finitePositive(maxSteps, 5)));
  const frame = {
    time: 0,
    deltaTime: 0,
    renderDelta: 0,
    interpolation: 0,
    frameIndex: 0,
    simulationStep: 0,
    timeScale: 1,
    paused: false,
  };
  let accumulator = 0;
  let lastTimestamp = 0;
  let frameHandle = null;
  let running = false;
  let disposed = false;
  let renderCount = 0;

  const onFrame = (rawTimestamp) => {
    // A rAF host passes a DOMHighResTimeStamp; the timer fallback inside
    // requestBrowserAnimationFrame passes nothing, and `undefined - last` is
    // NaN, which silently freezes time instead of failing loudly.
    const timestamp = typeof rawTimestamp === 'number' ? rawTimestamp : Date.now();
    if (!running || disposed) return;
    const seconds = lastTimestamp === 0 ? 0 : Math.min(maximumDelta, Math.max(0, (timestamp - lastTimestamp) / 1000));
    lastTimestamp = timestamp;
    const scaledSeconds = frame.paused ? 0 : seconds * frame.timeScale;
    accumulator += scaledSeconds;
    frame.renderDelta = scaledSeconds;
    let steps = 0;
    while (accumulator >= step && steps < maximumSteps) {
      frame.deltaTime = step;
      frame.time += step;
      frame.simulationStep += 1;
      simulate(step, frame);
      accumulator -= step;
      steps += 1;
    }
    // Drop an unrecoverable backlog rather than executing a spiral of old work.
    if (steps === maximumSteps && accumulator >= step) accumulator = 0;
    frame.interpolation = accumulator / step;
    frame.frameIndex += 1;
    render(frame.interpolation, frame);
    renderCount += 1;
    frameHandle = requestFrame(onFrame);
  };

  function start() {
    if (disposed || running) return;
    running = true;
    lastTimestamp = 0;
    frameHandle = requestFrame(onFrame);
  }

  function stop() {
    running = false;
    if (frameHandle != null) cancelFrame(frameHandle);
    frameHandle = null;
  }

  function invalidate() {
    if (frame.paused) renderOnce();
    else start();
  }

  function pause(value = true) {
    frame.paused = Boolean(value);
    if (frame.paused) {
      stop();
      renderOnce();
    } else {
      start();
    }
  }

  function setTimeScale(value) {
    const numeric = Number(value);
    frame.timeScale = Number.isFinite(numeric) ? Math.min(8, Math.max(0, numeric)) : 1;
  }

  function seek(seconds = 0) {
    frame.time = Math.max(0, Number(seconds) || 0);
    accumulator = 0;
  }

  function replay(seconds = 0) {
    seek(seconds);
    frame.simulationStep = 0;
    frame.frameIndex = 0;
  }

  function renderOnce() {
    if (disposed) return;
    frame.interpolation = accumulator / step;
    frame.frameIndex += 1;
    render(frame.interpolation, frame);
    renderCount += 1;
  }

  function copyMetrics(target) {
    target.fixedStep = step;
    target.renderCount = renderCount;
    target.storageAllocations = 1;
    target.running = running;
    return target;
  }

  function dispose() {
    stop();
    disposed = true;
  }

  return {
    frame,
    start,
    stop,
    invalidate,
    pause,
    setTimeScale,
    seek,
    replay,
    renderOnce,
    copyMetrics,
    dispose,
    get storageAllocations() { return 1; },
    get running() { return running; },
  };
}
