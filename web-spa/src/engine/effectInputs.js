/*
 * Real input → effect uniforms.
 *
 * The compositor will happily admit an interaction effect that nothing ever
 * feeds; it then renders its default uniforms forever, which on screen is a
 * halo frozen at the centre of the viewport. Admission is not response, so this
 * module is what makes the interaction slot mean anything.
 *
 * Uniform names are per-effect contracts (see each manifest), so the mapping is
 * explicit rather than guessed from a shared convention that does not exist.
 */

const NOOP = () => {};

// Continuous signals: driven straight from the event, no timer needed.
// `pointer` arrives in 0..1 UV space with y already flipped for GL.
const CONTINUOUS = Object.freeze({
  'cursor-glow': (signals, write) => write('cursor-glow', 'uPointer', signals.pointer),
  'magnetic-button': (signals, write) => write('magnetic-button', 'uPointer', signals.pointer),
  'hover-parallax-tilt': (signals, write) => write('hover-parallax-tilt', 'uTilt', signals.tilt),
  'inertial-scroll': (signals, write) => {
    write('inertial-scroll', 'uVelocity', signals.scrollVelocity);
    write('inertial-scroll', 'uOverscroll', signals.overscroll);
  },
});

// Impulse signals: a pointerdown starts a bounded ramp that drives one uniform
// from 0 to 1 and then stops. It is deliberately not a persistent rAF loop --
// an always-on loop is what stops Chrome ever reaching networkIdle.
const IMPULSE = Object.freeze({
  'press-elastic': Object.freeze({ origin: 'uOrigin', progress: 'uPress', durationMs: 420, release: 0 }),
  'click-ripple': Object.freeze({ origin: 'uOrigin', progress: 'uProgress', durationMs: 620, release: 0 }),
  'success-particles': Object.freeze({ origin: 'uOrigin', progress: 'uProgress', durationMs: 900, release: 0 }),
});

export function interactionEffectIds() {
  return [...Object.keys(CONTINUOUS), ...Object.keys(IMPULSE)];
}

/**
 * Binds a live engine session to a set of loaded effect ids. Returns a driver
 * whose methods are safe to call for effects that are not loaded: the host
 * rejects unknown ids, so a route that never loads `click-ripple` costs nothing.
 */
export function createEffectInputDriver({ session, requestFrame, cancelFrame, now }) {
  if (!session || typeof session.setEffectParameters !== 'function') return null;
  const readTime = typeof now === 'function' ? now : () => Date.now();
  const schedule = typeof requestFrame === 'function' ? requestFrame : NOOP;
  const unschedule = typeof cancelFrame === 'function' ? cancelFrame : NOOP;

  // One reusable payload and one reusable vector: copyEffectParameters() slices
  // arrays and copies scalars, so nothing here can be aliased by an effect after
  // the call returns, and no event allocates.
  const payload = {};
  const vectorScratch = [0, 0];
  const signals = { pointer: [0.5, 0.5], tilt: [0, 0], scrollVelocity: 0, overscroll: 0 };
  const impulses = new Map();
  let rampHandle = null;
  let loaded = [];

  function write(id, uniform, value) {
    if (!loaded.includes(id)) return false;
    if (Array.isArray(value)) {
      vectorScratch[0] = value[0];
      vectorScratch[1] = value[1];
      payload[uniform] = vectorScratch;
    } else {
      payload[uniform] = value;
    }
    const accepted = session.setEffectParameters(id, payload);
    delete payload[uniform];
    return accepted;
  }

  function applyContinuous() {
    for (let index = 0; index < loaded.length; index += 1) {
      const apply = CONTINUOUS[loaded[index]];
      if (apply) apply(signals, write);
    }
  }

  function step() {
    rampHandle = null;
    const time = readTime();
    let stillRunning = false;
    for (const [id, state] of impulses) {
      const spec = IMPULSE[id];
      const elapsed = time - state.startedAt;
      if (elapsed >= spec.durationMs) {
        write(id, spec.progress, spec.release);
        impulses.delete(id);
        continue;
      }
      write(id, spec.progress, elapsed / spec.durationMs);
      stillRunning = true;
    }
    // The ramp exists only while an impulse is in flight; when the last one
    // finishes the page goes quiet again.
    if (stillRunning) rampHandle = schedule(step);
  }

  return {
    setLoadedEffects(ids) {
      loaded = Array.isArray(ids) ? ids : [];
      applyContinuous();
    },
    /** Pointer in 0..1 UV space, origin bottom-left to match GL. */
    setPointer(x, y) {
      signals.pointer[0] = Math.min(1, Math.max(0, x));
      signals.pointer[1] = Math.min(1, Math.max(0, y));
      signals.tilt[0] = signals.pointer[0] * 2 - 1;
      signals.tilt[1] = signals.pointer[1] * 2 - 1;
      applyContinuous();
    },
    setScroll(velocity, overscroll = 0) {
      signals.scrollVelocity = Number.isFinite(velocity) ? velocity : 0;
      signals.overscroll = Number.isFinite(overscroll) ? overscroll : 0;
      applyContinuous();
    },
    /** Starts the bounded ramp for every impulse effect currently loaded. */
    press(x, y) {
      let started = false;
      for (const id of Object.keys(IMPULSE)) {
        if (!loaded.includes(id)) continue;
        const spec = IMPULSE[id];
        write(id, spec.origin, [x, y]);
        write(id, spec.progress, 0);
        impulses.set(id, { startedAt: readTime() });
        started = true;
      }
      if (started && rampHandle === null) rampHandle = schedule(step);
      return started;
    },
    dispose() {
      if (rampHandle !== null) unschedule(rampHandle);
      rampHandle = null;
      impulses.clear();
      loaded = [];
    },
  };
}

export { CONTINUOUS, IMPULSE };
