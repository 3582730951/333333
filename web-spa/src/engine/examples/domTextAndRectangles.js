import { startAuroraEnhancement } from '../bootstrap.js';
import { createDomTextBatch } from '../dom/textBatch.js';

/**
 * Complete hybrid-layer example.
 *
 * `effectLayer` is an aria-hidden WebGL-only sibling below `domTextLayer`.
 * `domTextLayer` stays in the document's content plane; its Chinese or other
 * localized text is selectable and exposed to assistive technology normally.
 */
export async function mountDomTextAndRectanglesDemo({
  effectLayer,
  domTextLayer,
  atmosphereCanvas,
  fallbackRoot = effectLayer,
} = {}) {
  const text = createDomTextBatch(domTextLayer, { capacity: 3 });
  const engine = await startAuroraEnhancement({
    effectLayer,
    fallbackRoot,
    atmosphere: atmosphereCanvas ? {
      mode: 'reuse',
      canvas: atmosphereCanvas,
      costBudgetUnits: { high: 3, medium: 2, low: 1 },
    } : { mode: 'none' },
    effects: [{
      id: 'aurora-pulse',
      parameters: { uIntensity: 0.16, uSpeed: 0.6, uAmplitude: 0.12 },
    }],
    // The surrounding application must have rendered equivalent ordinary DOM
    // before this point. This callback only marks a visual fallback state.
    onDomFallback(detail) {
      if (fallbackRoot?.setAttribute) fallbackRoot.setAttribute('data-demo-fallback', detail.reason);
      return () => fallbackRoot?.removeAttribute?.('data-demo-fallback');
    },
  });
  if (engine.active) await engine.syncPaletteFromDocument();

  function render(data = {}) {
    text.set(0, { text: data.title || '实时用量', x: 24, y: 20 });
    text.set(1, { text: data.value || '72%', x: 24, y: 52, ariaLabel: data.valueLabel || '当前用量 72%' });
    text.set(2, { text: data.caption || 'DOM 文本；可选择、可朗读', x: 24, y: 84 });
    if (!engine.active) return;
    // Rectangle colours are copied from the atmo token palette supplied to the
    // engine, not hard-coded in the effect. A caller should call setPalette with
    // values read from --pool-atmo-* whenever the theme changes.
    engine.queuePaletteRectangle('near', 20, 116, 280, 8, 0.32);
    engine.queuePaletteRectangle('glow', 20, 116, Math.max(0, Math.min(280, Number(data.width) || 202)), 8, 0.78);
    engine.queuePaletteRectangle('far', 20, 138, 280, 1, 0.45);
    engine.invalidate();
  }

  render();
  return {
    engine,
    render,
    dispose() {
      engine.dispose();
      text.dispose();
    },
    unload() {
      if (engine.active) engine.unloadEffect('aurora-pulse');
    },
  };
}
