import { useEffect, useMemo, useRef, useState } from 'react';
import {
  createAtmosphere, normalizeVisualQuality,
} from '../lib/atmosphere.js';
import { reportClientError } from './AppErrorBoundary.jsx';
import { readDocumentElementCustomProperties } from '../lib/browserDocument.js';
import {
  addDocumentListener, addWindowListener, isDocumentVisible,
} from '../lib/browserLifecycle.js';
import { consoleBootstrap } from '../app/bootstrap';

type AmbientVisualSample = {
  energy?: number;
  rpm_activity?: number;
  ui_experience_v2?: boolean;
};
type Subscribe = (listener: (sample: AmbientVisualSample) => void) => () => void;
type VisualQuality = 'auto' | 'high' | 'balanced' | 'low' | 'still' | 'off';

const VISUAL_QUALITY_KEY = 'pool.visual_quality';

const PALETTE_TOKENS = [
  '--pool-atmo-void', '--pool-atmo-near', '--pool-atmo-far', '--pool-atmo-glow',
  '--pool-atmo-alpha', '--pool-grain-alpha',
] as const;

function readPalette() {
  const tokens = readDocumentElementCustomProperties([...PALETTE_TOKENS]);
  return {
    uVoid: tokens['--pool-atmo-void'],
    uNear: tokens['--pool-atmo-near'],
    uFar: tokens['--pool-atmo-far'],
    uGlow: tokens['--pool-atmo-glow'],
    alpha: Number(tokens['--pool-atmo-alpha']) || 1,
    grain: Number(tokens['--pool-grain-alpha']) || 0,
  };
}

function prefersReducedMotion() {
  try {
    return Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches);
  } catch {
    return false;
  }
}

function saveDataEnabled() {
  try {
    return Boolean((navigator as Navigator & { connection?: { saveData?: boolean } }).connection?.saveData);
  } catch {
    return false;
  }
}

function readVisualQuality(): VisualQuality {
  try {
    return normalizeVisualQuality(localStorage.getItem(VISUAL_QUALITY_KEY)) as VisualQuality;
  } catch {
    return 'auto';
  }
}

export function resolveVisualQuality(preference: VisualQuality): Exclude<VisualQuality, 'auto'> {
  if (preference === 'off' || saveDataEnabled()) return 'off';
  if (preference === 'still' || prefersReducedMotion()) return 'still';
  if (preference !== 'auto') return preference;
  const hardware = navigator as Navigator & { deviceMemory?: number };
  const cores = Number(hardware.hardwareConcurrency) || 4;
  const memory = Number(hardware.deviceMemory) || 4;
  if (cores <= 4 || memory <= 4) return 'low';
  if (cores >= 8 && memory >= 8 && window.matchMedia?.('(min-width: 1024px) and (pointer: fine)').matches) return 'high';
  return 'balanced';
}

/**
 * The console's ambient depth field.
 *
 * Mounted once by the shell, behind everything, and lazily: this module and the
 * WebGL program it pulls in must never reach the initial dependency graph that
 * `scripts/check-build-budget.mjs` measures.
 *
 * Everything here is progressive enhancement over the CSS gradient painted by
 * `.pool-atmosphere` in atmosphere.css. If WebGL2 is missing, the context is
 * refused, the program fails to link, or the visitor asked for reduced motion,
 * the canvas either never appears or renders a single still frame, and the CSS
 * layer underneath carries the design on its own.
 */
export default function AtmosphereLayer({ subscribe }: { subscribe?: Subscribe }) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // Drives `data-webgl`, which tells the stylesheet whether the CSS field is doing
  // the work or merely sitting under an opaque canvas.
  const [live, setLive] = useState(false);
  const [experienceEnabled, setExperienceEnabled] = useState(() => consoleBootstrap()?.ui_experience_v2 !== false);
  const [qualityPreference, setQualityPreference] = useState<VisualQuality>(readVisualQuality);
  const [effectiveQuality, setEffectiveQuality] = useState<Exclude<VisualQuality, 'auto'>>(() => resolveVisualQuality(readVisualQuality()));
  const [contextGeneration, setContextGeneration] = useState(0);
  const resolvedQuality = useMemo(() => resolveVisualQuality(qualityPreference), [qualityPreference]);

  useEffect(() => {
    const onPreference = (event?: Event) => {
      const detail = event instanceof CustomEvent ? event.detail : undefined;
      const next = normalizeVisualQuality(detail ?? readVisualQuality()) as VisualQuality;
      try { localStorage.setItem(VISUAL_QUALITY_KEY, next); } catch { /* storage is optional */ }
      setQualityPreference(next);
    };
    const removeCustom = addWindowListener('pool-visual-quality-change', onPreference);
    const removeStorage = addWindowListener('storage', (event: StorageEvent) => {
      if (event.key === VISUAL_QUALITY_KEY) onPreference();
    });
    return () => { removeCustom(); removeStorage(); };
  }, []);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return undefined;

    setEffectiveQuality(resolvedQuality);
    if (resolvedQuality === 'off') {
      setLive(false);
      return undefined;
    }

    // A machine without WebGL2 is a capability, not a fault, and stays quiet. A
    // context that exists but cannot build the program is a defect, and it used to
    // be indistinguishable from the first case -- a missing `in vec2 vUv;` failed
    // every compile for an entire development cycle while looking exactly like
    // "this browser has no WebGL".
    const atmosphere = createAtmosphere(canvas, {
      quality: resolvedQuality,
      onQualityChange: (next: string) => {
        if (next === 'high' || next === 'balanced' || next === 'low' || next === 'still') {
          setEffectiveQuality(next);
        }
      },
      onDiagnostic: (message: string) => reportClientError(new Error(message), {
        source: 'atmosphere.webgl',
        componentStack: 'AtmosphereLayer',
      }),
    });
    // No WebGL2, or the program did not link. The CSS field stays visible and the
    // canvas is left transparent rather than painted an unintended flat colour.
    if (!atmosphere) return undefined;

    const still = resolvedQuality === 'still';
    let disposed = false;
    atmosphere.setEnabled(experienceEnabled);
    setLive(experienceEnabled);

    const syncSize = () => {
      if (disposed) return;
      const rect = canvas.getBoundingClientRect();
      atmosphere.resize(rect.width, rect.height, window.devicePixelRatio || 1);
      if (still) atmosphere.renderStill();
    };

    atmosphere.setPalette(readPalette());
    syncSize();

    // Theme changes rewrite the palette tokens, so the field has to re-read them.
    // Watching the attribute rather than threading the resolved theme down as a
    // prop keeps this correct for any future theme, including one set outside React.
    const themeObserver = new MutationObserver(() => {
      if (disposed) return;
      atmosphere.setPalette(readPalette());
      if (still) atmosphere.renderStill();
    });
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

    const resizeObserver = typeof ResizeObserver === 'function' ? new ResizeObserver(syncSize) : null;
    resizeObserver?.observe(canvas);
    const removeResize = resizeObserver ? () => {} : addWindowListener('resize', syncSize);

    let removePointer = () => {};
    let removeScroll = () => {};
    let removeFocus = () => {};
    let removeActivity = () => {};
    let removePointerActivity = () => {};
    let removeVisibility = () => {};
    let unsubscribe = () => {};

    if (still) {
      atmosphere.renderStill();
    } else {
      // Pointer parallax. The handler only stores a target; the render loop eases
      // toward it, so this never schedules work of its own however fast the mouse
      // moves. Passive because it never calls preventDefault.
      let previousPointer = { x: window.innerWidth / 2, y: window.innerHeight / 2, at: performance.now() };
      removePointer = addWindowListener('pointermove', (event: PointerEvent) => {
        const w = window.innerWidth || 1;
        const h = window.innerHeight || 1;
        atmosphere.setPointer((event.clientX / w) * 2 - 1, (event.clientY / h) * 2 - 1);
        const now = performance.now();
        const elapsed = Math.max(16, now - previousPointer.at);
        const distance = Math.hypot(event.clientX - previousPointer.x, event.clientY - previousPointer.y);
        atmosphere.setActivity(Math.min(1, distance / elapsed / 1.4));
        previousPointer = { x: event.clientX, y: event.clientY, at: now };
      }, { passive: true });

      let previousScroll = { y: window.scrollY, at: performance.now() };
      removeScroll = addWindowListener('scroll', () => {
        const max = Math.max(1, document.documentElement.scrollHeight - window.innerHeight);
        const now = performance.now();
        const elapsed = Math.max(16, now - previousScroll.at);
        const velocity = Math.min(1, Math.abs(window.scrollY - previousScroll.y) / elapsed / 1.2);
        atmosphere.setScroll((window.scrollY / max) * 2 - 1, velocity);
        previousScroll = { y: window.scrollY, at: now };
      }, { passive: true });

      removeFocus = addDocumentListener('focusin', (event: FocusEvent) => {
        if (!(event.target instanceof HTMLElement)) return;
        const rect = event.target.getBoundingClientRect();
        const x = ((rect.left + rect.width / 2) / Math.max(1, window.innerWidth)) * 2 - 1;
        const y = ((rect.top + rect.height / 2) / Math.max(1, window.innerHeight)) * 2 - 1;
        atmosphere.setFocus(x, y);
        atmosphere.setActivity(0.42);
      });
      removePointerActivity = addDocumentListener('pointerdown', () => atmosphere.setActivity(0.72), { passive: true });
      removeActivity = addWindowListener('pool-rpm-activity', (event: CustomEvent<number>) => {
        atmosphere.setActivity(Number(event.detail) || 0);
      });

      // A background tab still runs rAF in some browsers and always costs power in
      // none. Stop the loop outright rather than relying on the browser to throttle.
      removeVisibility = addDocumentListener('visibilitychange', () => {
        if (disposed) return;
        if (isDocumentVisible()) atmosphere.start();
        else atmosphere.stop();
      });

      if (isDocumentVisible()) atmosphere.start();
    }

    if (subscribe) {
      unsubscribe = subscribe((sample) => {
        if (disposed) return;
        atmosphere.setEnergy(Number(sample?.energy) || 0);
        atmosphere.setActivity(Number(sample?.rpm_activity) || 0);
        if (typeof sample?.ui_experience_v2 === 'boolean') {
          setExperienceEnabled(sample.ui_experience_v2);
          atmosphere.setEnabled(sample.ui_experience_v2);
          if (sample.ui_experience_v2) {
            setLive(true);
          } else {
            setLive(false);
          }
        }
        if (still && sample?.ui_experience_v2 !== false) atmosphere.renderStill();
      });
    }

    const onContextLost = (event: Event) => {
      event.preventDefault();
      atmosphere.stop();
      setLive(false);
    };
    const onContextRestored = () => setContextGeneration((value) => value + 1);
    canvas.addEventListener('webglcontextlost', onContextLost);
    canvas.addEventListener('webglcontextrestored', onContextRestored);

    return () => {
      disposed = true;
      setLive(false);
      unsubscribe();
      removePointer();
      removeScroll();
      removeFocus();
      removeActivity();
      removePointerActivity();
      removeVisibility();
      removeResize();
      resizeObserver?.disconnect();
      themeObserver.disconnect();
      canvas.removeEventListener('webglcontextlost', onContextLost);
      canvas.removeEventListener('webglcontextrestored', onContextRestored);
      // Releases the GL program and the drawing buffer. Without this a hot reload
      // that remounts the shell repeatedly walks into the live-context ceiling and
      // every later mount silently gets no context at all.
      atmosphere.dispose();
    };
  }, [contextGeneration, resolvedQuality, subscribe]);

  return (
    <div
      className="pool-atmosphere"
      data-webgl={live && experienceEnabled ? 'true' : 'false'}
      data-quality={effectiveQuality}
      data-experience={experienceEnabled ? 'enhanced' : 'base'}
      aria-hidden="true"
    >
      <canvas ref={canvasRef} className="pool-atmosphere__canvas" />
      <div className="pool-atmosphere__grain" />
    </div>
  );
}
