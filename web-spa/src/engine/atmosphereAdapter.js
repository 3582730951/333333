/**
 * The only path by which the new engine may own the existing atmosphere. It
 * dynamically reuses src/lib/atmosphere.js rather than implementing a second
 * background field with a drifting palette or lifecycle.
 */

function call(controller, name, ...argumentsList) {
  if (typeof controller?.[name] === 'function') controller[name](...argumentsList);
}

export function createExternalAtmosphereAdapter(controller) {
  if (!controller || typeof controller !== 'object') return null;
  return {
    resize(width, height, pixelRatio) { call(controller, 'resize', width, height, pixelRatio); },
    setPalette(palette) { call(controller, 'setPalette', palette); },
    setEnergy(value) { call(controller, 'setEnergy', value); },
    setPointer(x, y) { call(controller, 'setPointer', x, y); },
    setFocus(x, y) { call(controller, 'setFocus', x, y); },
    setScroll(position, velocity) { call(controller, 'setScroll', position, velocity); },
    setActivity(value) { call(controller, 'setActivity', value); },
    setQuality(value) { call(controller, 'setQuality', value); },
    start() { call(controller, 'start'); },
    stop() { call(controller, 'stop'); },
    dispose() {},
  };
}

export async function createAtmosphereAdapter({ canvas, palette, quality, onDiagnostic }) {
  const { createAtmosphere } = await import('../lib/atmosphere.js');
  const controller = createAtmosphere(canvas, { quality, onDiagnostic });
  if (!controller) return null;
  if (palette) controller.setPalette(palette);
  return {
    resize(width, height, pixelRatio) { controller.resize(width, height, pixelRatio); },
    setPalette(nextPalette) { controller.setPalette(nextPalette); },
    setEnergy(value) { controller.setEnergy(value); },
    setPointer(x, y) { controller.setPointer(x, y); },
    setFocus(x, y) { controller.setFocus(x, y); },
    setScroll(position, velocity) { controller.setScroll(position, velocity); },
    setActivity(value) { controller.setActivity(value); },
    setQuality(value) { controller.setQuality(value); },
    start() { controller.start(); },
    stop() { controller.stop(); },
    dispose() { controller.dispose(); },
  };
}

/**
 * Reads the one approved atmosphere palette and returns both the string form
 * consumed by the legacy renderer and float triplets consumed by effect shaders.
 * It imports the existing parser instead of accepting a second colour grammar.
 */
export async function readAtmosphereTokenPalette(element = document.documentElement) {
  const { parseColorChannels } = await import('../lib/atmosphere.js');
  const style = getComputedStyle(element);
  const atmosphere = {
    uVoid: style.getPropertyValue('--pool-atmo-void').trim(),
    uNear: style.getPropertyValue('--pool-atmo-near').trim(),
    uFar: style.getPropertyValue('--pool-atmo-far').trim(),
    uGlow: style.getPropertyValue('--pool-atmo-glow').trim(),
    alpha: Number(style.getPropertyValue('--pool-atmo-alpha')) || 1,
    grain: Number(style.getPropertyValue('--pool-grain-alpha')) || 0,
  };
  return {
    atmosphere,
    effect: {
      void: parseColorChannels(atmosphere.uVoid),
      near: parseColorChannels(atmosphere.uNear),
      far: parseColorChannels(atmosphere.uFar),
      glow: parseColorChannels(atmosphere.uGlow),
    },
  };
}
