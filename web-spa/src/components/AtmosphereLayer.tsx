import { useEffect, useMemo, useRef, useState } from 'react';
import {
  createAtmosphere, normalizeVisualQuality,
} from '../lib/atmosphere.js';
import { reportClientError } from './AppErrorBoundary.jsx';
import { readDocumentElementCustomProperties } from '../lib/browserDocument.js';
import {
  addDocumentListener, addWindowListener, cancelBrowserAnimationFrame,
  isDocumentVisible, requestBrowserAnimationFrame,
} from '../lib/browserLifecycle.js';
import { consoleBootstrap } from '../app/bootstrap';
import { startAuroraEnhancement } from '../engine/bootstrap.js';
import { effectsForRoute } from '../engine/routeEffects.js';
import { createEffectInputDriver } from '../engine/effectInputs.js';

type AmbientVisualSample = {
  energy?: number;
  rpm_activity?: number;
  ui_experience_v2?: boolean;
};
type Subscribe = (listener: (sample: AmbientVisualSample) => void) => () => void;
type VisualQuality = 'auto' | 'high' | 'balanced' | 'low' | 'still' | 'off';

const VISUAL_QUALITY_KEY = 'pool.visual_quality';

// The two vocabularies are not the same set: the atmosphere has 'balanced' and
// two non-rendering states, the engine has three rendering levels. 'still' and
// 'off' map to no engine at all rather than to a slow one.
const ENGINE_QUALITY: Partial<Record<Exclude<VisualQuality, 'auto'>, 'high' | 'medium' | 'low'>> = {
  high: 'high',
  balanced: 'medium',
  low: 'low',
};

// `startAuroraEnhancement` returns one of two shapes. A refused capability probe
// or a failed dynamic import yields the small inactive session, which has only
// `dispose`; treating the two as one type is how a "just call stop()" cleanup
// turns into a TypeError on exactly the low-end machines the fallback exists for.
type EngineSession = {
  active: boolean;
  reason?: string;
  dispose: () => void;
  start?: () => void;
  stop?: () => void;
  loadEffect?: (id: string, parameters?: Record<string, unknown>) => Promise<boolean>;
  unloadEffect?: (id: string) => boolean;
  syncPaletteFromDocument?: (element: Element) => Promise<boolean>;
  syncSize?: () => void;
};
type ActiveEngineSession = EngineSession & Required<Pick<
  EngineSession, 'start' | 'stop' | 'loadEffect' | 'unloadEffect' | 'syncPaletteFromDocument' | 'syncSize'
>>;

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
export default function AtmosphereLayer({ subscribe, pathname }: { subscribe?: Subscribe; pathname?: string }) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const layerRef = useRef<HTMLDivElement | null>(null);
  // The engine starts asynchronously and outlives individual route renders, so
  // it is held in a ref rather than state: a re-render must not restart it.
  const sessionRef = useRef<EngineSession | null>(null);
  const activeEffectsRef = useRef<string[]>([]);
  const routeRef = useRef<string>(pathname || '/');
  const inputDriverRef = useRef<ReturnType<typeof createEffectInputDriver>>(null);
  const experienceEnabledRef = useRef(true);
  // Single narrowing point: callers get the full surface or nothing at all.
  const activeSession = (): ActiveEngineSession | null => {
    const session = sessionRef.current;
    // `active` alone is the host's claim; the typeof check is the one that
    // actually protects the call sites, so it is what the narrowing rests on.
    if (!session || !session.active || typeof session.start !== 'function') return null;
    return session as ActiveEngineSession;
  };
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
      activeSession()?.syncSize();
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
      // P3 §7.3: every effect reads --pool-atmo-*, so a theme swap has to reach
      // the engine too or the effects keep painting the previous theme's colours.
      void activeSession()?.syncPaletteFromDocument(document.documentElement);
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
      // Pointer and scroll input are sampled once per paint. High-Hz devices can
      // deliver hundreds of events between frames; keeping only the newest sample
      // preserves one-frame feedback while avoiding duplicate math and wakeups.
      let previousPointer = { x: window.innerWidth / 2, y: window.innerHeight / 2, at: performance.now() };
      let pendingPointer: { x: number; y: number } | null = null;
      let scrollPending = false;
      let inputFrame: ReturnType<typeof requestBrowserAnimationFrame> = null;
      let previousScroll = { y: window.scrollY, at: performance.now() };
      const flushInputs = () => {
        inputFrame = null;
        if (pendingPointer) {
          const { x, y } = pendingPointer;
          pendingPointer = null;
          const w = window.innerWidth || 1;
          const h = window.innerHeight || 1;
          atmosphere.setPointer((x / w) * 2 - 1, (y / h) * 2 - 1);
          inputDriverRef.current?.setPointer(x / w, 1 - y / h);
          const now = performance.now();
          const elapsed = Math.max(16, now - previousPointer.at);
          const distance = Math.hypot(x - previousPointer.x, y - previousPointer.y);
          atmosphere.setActivity(Math.min(1, distance / elapsed / 1.4));
          previousPointer = { x, y, at: now };
        }
        if (scrollPending) {
          scrollPending = false;
          const max = Math.max(1, document.documentElement.scrollHeight - window.innerHeight);
          const now = performance.now();
          const y = window.scrollY;
          const elapsed = Math.max(16, now - previousScroll.at);
          const velocity = Math.min(1, Math.abs(y - previousScroll.y) / elapsed / 1.2);
          atmosphere.setScroll((y / max) * 2 - 1, velocity);
          inputDriverRef.current?.setScroll(velocity, y < 0 ? -y / max : 0);
          previousScroll = { y, at: now };
        }
      };
      const scheduleInputs = () => {
        if (inputFrame == null) inputFrame = requestBrowserAnimationFrame(flushInputs);
      };
      removePointer = addWindowListener('pointermove', (event: PointerEvent) => {
        const w = window.innerWidth || 1;
        const h = window.innerHeight || 1;
        pendingPointer = {
          x: Math.min(w, Math.max(0, event.clientX)),
          y: Math.min(h, Math.max(0, event.clientY)),
        };
        scheduleInputs();
      }, { passive: true });

      removeScroll = addWindowListener('scroll', () => {
        scrollPending = true;
        scheduleInputs();
      }, { passive: true });

      removeFocus = addDocumentListener('focusin', (event: FocusEvent) => {
        if (!(event.target instanceof HTMLElement)) return;
        const rect = event.target.getBoundingClientRect();
        const x = ((rect.left + rect.width / 2) / Math.max(1, window.innerWidth)) * 2 - 1;
        const y = ((rect.top + rect.height / 2) / Math.max(1, window.innerHeight)) * 2 - 1;
        atmosphere.setFocus(x, y);
        atmosphere.setActivity(0.42);
      });
      removePointerActivity = addDocumentListener('pointerdown', (event: PointerEvent) => {
        atmosphere.setActivity(0.72);
        inputDriverRef.current?.press(
          event.clientX / Math.max(1, window.innerWidth),
          1 - event.clientY / Math.max(1, window.innerHeight),
        );
      }, { passive: true });
      removeActivity = addWindowListener('pool-rpm-activity', (event: CustomEvent<number>) => {
        atmosphere.setActivity(Number(event.detail) || 0);
      });

      // A background tab still runs rAF in some browsers and always costs power in
      // none. Stop the loop outright rather than relying on the browser to throttle.
      removeVisibility = addDocumentListener('visibilitychange', () => {
        if (disposed) return;
        if (isDocumentVisible()) {
          atmosphere.start();
          activeSession()?.start();
        } else {
          atmosphere.stop();
          activeSession()?.stop();
        }
      });

      if (isDocumentVisible()) atmosphere.start();

      const cancelInputFrame = () => {
        if (inputFrame != null) cancelBrowserAnimationFrame(inputFrame);
        inputFrame = null;
      };
      const previousRemovePointer = removePointer;
      const previousRemoveScroll = removeScroll;
      removePointer = () => { previousRemovePointer(); cancelInputFrame(); };
      removeScroll = () => { previousRemoveScroll(); cancelInputFrame(); };
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

    // ── P6 engine wiring ───────────────────────────────────────────────────
    // This component stays the ONE owner of `createAtmosphere` (P3 §7.1): the
    // host is handed the existing controller as an `external` adapter and never
    // builds a second background field. `startAuroraEnhancement` runs its own
    // capability probe and returns an inactive session on refusal, so the CSS
    // gradient plus this canvas remain the whole picture on machines that
    // cannot take more.
    const engineQualityLevel = ENGINE_QUALITY[resolvedQuality];
    if (engineQualityLevel && layerRef.current) {
      void startAuroraEnhancement({
        effectLayer: layerRef.current,
        fallbackRoot: layerRef.current,
        quality: engineQualityLevel,
        atmosphere: { mode: 'external', controller: atmosphere },
        effects: effectsForRoute(routeRef.current),
        onDiagnostic: (message: string) => reportClientError(new Error(message), {
          source: 'aurora.engine',
          componentStack: 'AtmosphereLayer',
        }),
      }).then((session: EngineSession) => {
        // The effect may have been torn down while the dynamic import was in
        // flight; disposing here rather than storing it prevents a leaked GL
        // context that no cleanup path can still reach.
        if (disposed) { session.dispose(); return; }
        sessionRef.current = session;
        // An inactive session is still stored above so the cleanup below can
        // dispose it, but it exposes nothing else to drive.
        const live = activeSession();
        if (!live) return;
        activeEffectsRef.current = effectsForRoute(routeRef.current).map((entry) => entry.id);
        // Without this the interaction slot renders its default uniforms forever:
        // admitted, drawn, and completely unresponsive to the pointer.
        inputDriverRef.current = createEffectInputDriver({
          session: live,
          requestFrame: requestBrowserAnimationFrame,
          cancelFrame: cancelBrowserAnimationFrame,
          now: () => performance.now(),
        });
        inputDriverRef.current?.setLoadedEffects(activeEffectsRef.current);
        void live.syncPaletteFromDocument(document.documentElement);
        if (still || !experienceEnabledRef.current || !isDocumentVisible()) live.stop();
      }).catch((error: unknown) => {
        reportClientError(error instanceof Error ? error : new Error(String(error)), {
          source: 'aurora.engine.start',
          componentStack: 'AtmosphereLayer',
        });
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
      // every later mount silently gets no context at all. The engine owns a
      // second context on its own canvas and has exactly the same problem.
      inputDriverRef.current?.dispose();
      inputDriverRef.current = null;
      sessionRef.current?.dispose();
      sessionRef.current = null;
      activeEffectsRef.current = [];
      atmosphere.dispose();
    };
  }, [contextGeneration, resolvedQuality, subscribe]);

  // `ui_experience_v2` arrives on the live ambient sample and can flip at any
  // time. Toggling it through the effect's dependency list would tear down and
  // rebuild two GL contexts on every flip; the atmosphere has always handled it
  // imperatively via setEnabled(), and the engine is handled the same way here.
  useEffect(() => {
    experienceEnabledRef.current = experienceEnabled;
    const session = activeSession();
    if (!session) return;
    if (experienceEnabled && isDocumentVisible()) session.start();
    else session.stop();
  }, [experienceEnabled]);

  // Route changes swap the effect set in place. Restarting the host instead
  // would rebuild the GL context on every navigation, which is both the slow
  // path and the one that eventually exhausts the browser's context budget.
  useEffect(() => {
    const nextRoute = pathname || '/';
    routeRef.current = nextRoute;
    const session = activeSession();
    if (!session) return;
    const next = effectsForRoute(nextRoute);
    const nextIds = next.map((entry) => entry.id);
    const previousIds = activeEffectsRef.current;
    if (previousIds.length === nextIds.length && previousIds.every((id, index) => id === nextIds[index])) return;
    for (const id of previousIds) {
      if (!nextIds.includes(id)) session.unloadEffect(id);
    }
    activeEffectsRef.current = nextIds;
    inputDriverRef.current?.setLoadedEffects(nextIds);
    for (const entry of next) {
      if (previousIds.includes(entry.id)) continue;
      void session.loadEffect(entry.id, entry.parameters);
    }
  }, [pathname]);

  return (
    <div
      ref={layerRef}
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
