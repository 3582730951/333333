import { useEffect, useRef, useState } from 'react';
import { createAtmosphere } from '../lib/atmosphere.js';
import { reportClientError } from './AppErrorBoundary.jsx';
import { readDocumentElementCustomProperties } from '../lib/browserDocument.js';
import {
  addDocumentListener, addWindowListener, isDocumentVisible,
} from '../lib/browserLifecycle.js';

type Subscribe = (listener: (sample: { energy?: number }) => void) => () => void;

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

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return undefined;

    // A machine without WebGL2 is a capability, not a fault, and stays quiet. A
    // context that exists but cannot build the program is a defect, and it used to
    // be indistinguishable from the first case -- a missing `in vec2 vUv;` failed
    // every compile for an entire development cycle while looking exactly like
    // "this browser has no WebGL".
    const atmosphere = createAtmosphere(canvas, {
      onDiagnostic: (message: string) => reportClientError(new Error(message), {
        source: 'atmosphere.webgl',
        componentStack: 'AtmosphereLayer',
      }),
    });
    // No WebGL2, or the program did not link. The CSS field stays visible and the
    // canvas is left transparent rather than painted an unintended flat colour.
    if (!atmosphere) return undefined;

    const reduced = prefersReducedMotion();
    let disposed = false;
    setLive(true);

    const syncSize = () => {
      if (disposed) return;
      const rect = canvas.getBoundingClientRect();
      atmosphere.resize(rect.width, rect.height, window.devicePixelRatio || 1);
      if (reduced) atmosphere.renderStill();
    };

    atmosphere.setPalette(readPalette());
    syncSize();

    // Theme changes rewrite the palette tokens, so the field has to re-read them.
    // Watching the attribute rather than threading the resolved theme down as a
    // prop keeps this correct for any future theme, including one set outside React.
    const themeObserver = new MutationObserver(() => {
      if (disposed) return;
      atmosphere.setPalette(readPalette());
      if (reduced) atmosphere.renderStill();
    });
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

    const resizeObserver = typeof ResizeObserver === 'function' ? new ResizeObserver(syncSize) : null;
    resizeObserver?.observe(canvas);
    const removeResize = resizeObserver ? () => {} : addWindowListener('resize', syncSize);

    let removePointer = () => {};
    let removeVisibility = () => {};
    let unsubscribe = () => {};

    if (reduced) {
      atmosphere.renderStill();
    } else {
      // Pointer parallax. The handler only stores a target; the render loop eases
      // toward it, so this never schedules work of its own however fast the mouse
      // moves. Passive because it never calls preventDefault.
      removePointer = addWindowListener('pointermove', (event: PointerEvent) => {
        const w = window.innerWidth || 1;
        const h = window.innerHeight || 1;
        atmosphere.setPointer((event.clientX / w) * 2 - 1, (event.clientY / h) * 2 - 1);
      }, { passive: true });

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
        if (reduced) atmosphere.renderStill();
      });
    }

    return () => {
      disposed = true;
      setLive(false);
      unsubscribe();
      removePointer();
      removeVisibility();
      removeResize();
      resizeObserver?.disconnect();
      themeObserver.disconnect();
      // Releases the GL program and the drawing buffer. Without this a hot reload
      // that remounts the shell repeatedly walks into the live-context ceiling and
      // every later mount silently gets no context at all.
      atmosphere.dispose();
    };
  }, [subscribe]);

  return (
    <div className="pool-atmosphere" data-webgl={live ? 'true' : 'false'} aria-hidden="true">
      <canvas ref={canvasRef} className="pool-atmosphere__canvas" />
      <div className="pool-atmosphere__grain" />
    </div>
  );
}
