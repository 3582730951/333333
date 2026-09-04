import { useEffect } from 'react';
import {
  addDocumentListener,
  addWindowListener,
  cancelBrowserAnimationFrame,
  requestBrowserAnimationFrame,
} from '../lib/browserLifecycle.js';

// Only the primary action. A dense operations console puts a dozen buttons in a
// toolbar, and a toolbar where every control drifts is a toolbar you have to aim at
// twice. One magnetic element per view reads as craft; twelve reads as a bug.
const SELECTOR = '.pool-button--primary';
// Pull begins this far outside the button's own box, in CSS pixels.
const FIELD_PX = 46;
// Maximum displacement. Small on purpose: the control moves *toward* the cursor, so
// it shortens the travel rather than dodging, and past a few pixels that stops being
// true and starts being a moving target.
const MAX_SHIFT_PX = 4;

type CachedRect = {
  left: number;
  top: number;
  right: number;
  bottom: number;
  width: number;
  height: number;
};

function prefersReducedMotion() {
  try {
    return Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches);
  } catch {
    return false;
  }
}

/**
 * Magnetic pull on the primary action.
 *
 * One delegated pointermove listener for the whole document rather than a listener
 * per button: the set of primary buttons changes on every route and every modal, and
 * per-element listeners would have to be attached and torn down alongside them.
 *
 * Geometry is cached and refreshed only after resize/scroll/DOM changes. Pointer frames
 * read that cache and write transforms, so a 1000Hz mouse cannot turn into a 1000Hz
 * `getBoundingClientRect()` loop. When a refresh is needed, all reads still happen
 * before any transform write.
 */
export function useMagneticPointer(enabled: boolean) {
  useEffect(() => {
    if (!enabled) return undefined;
    if (prefersReducedMotion()) return undefined;
    // Coarse pointers have no hover to be magnetic about, and running this on touch
    // would move the control out from under the finger already committed to it.
    try {
      if (!window.matchMedia?.('(hover: hover) and (pointer: fine)').matches) return undefined;
    } catch {
      return undefined;
    }

    let frame: ReturnType<typeof requestBrowserAnimationFrame> = null;
    let pointerX = 0;
    let pointerY = 0;
    let geometryDirty = true;
    const engaged = new Set<HTMLElement>();
    const geometry = new Map<HTMLElement, CachedRect>();
    const shifts = new Map<HTMLElement, readonly [number, number]>();

    const resizeObserver = typeof ResizeObserver === 'function'
      ? new ResizeObserver(() => {
        geometryDirty = true;
        schedule();
      })
      : null;

    const mutationObserver = typeof MutationObserver === 'function'
      ? new MutationObserver(() => {
        geometryDirty = true;
        schedule();
      })
      : null;

    const release = (element: HTMLElement) => {
      element.style.transform = '';
      element.style.willChange = '';
      shifts.delete(element);
    };

    const refreshGeometry = () => {
      const buttons = new Set(document.querySelectorAll<HTMLElement>(SELECTOR));
      for (const button of geometry.keys()) {
        if (buttons.has(button)) continue;
        release(button);
        engaged.delete(button);
        geometry.delete(button);
        resizeObserver?.unobserve(button);
      }
      for (const button of buttons) {
        if (!geometry.has(button)) resizeObserver?.observe(button);
        const rendered = button.getBoundingClientRect();
        const [shiftX, shiftY] = shifts.get(button) || [0, 0];
        // getBoundingClientRect includes our compositor transform. Store the
        // unshifted layout box so repeated invalidations cannot make it drift.
        const left = rendered.left - shiftX;
        const top = rendered.top - shiftY;
        geometry.set(button, {
          left,
          top,
          right: left + rendered.width,
          bottom: top + rendered.height,
          width: rendered.width,
          height: rendered.height,
        });
      }
      geometryDirty = false;
    };

    const apply = () => {
      frame = null;
      if (geometryDirty) refreshGeometry();
      const next = new Set<HTMLElement>();
      const writes: Array<[HTMLElement, number, number]> = [];
      for (const [button, rect] of geometry) {
        if (rect.width <= 0 || rect.height <= 0) continue;
        const centreX = rect.left + rect.width / 2;
        const centreY = rect.top + rect.height / 2;
        // Distance to the button's box, not to its centre: a wide button should pull
        // along its whole edge rather than only near the middle.
        const dx = Math.max(rect.left - pointerX, 0, pointerX - rect.right);
        const dy = Math.max(rect.top - pointerY, 0, pointerY - rect.bottom);
        const distance = Math.hypot(dx, dy);
        if (distance > FIELD_PX) continue;
        const strength = (1 - distance / FIELD_PX) ** 2;
        const shiftX = ((pointerX - centreX) / (rect.width / 2 + FIELD_PX)) * MAX_SHIFT_PX * strength;
        const shiftY = ((pointerY - centreY) / (rect.height / 2 + FIELD_PX)) * MAX_SHIFT_PX * strength;
        writes.push([button, shiftX, shiftY]);
        next.add(button);
      }
      for (const button of engaged) if (!next.has(button)) release(button);
      engaged.clear();
      for (const [button, shiftX, shiftY] of writes) {
        button.style.willChange = 'transform';
        button.style.transform = `translate3d(${shiftX.toFixed(2)}px, ${shiftY.toFixed(2)}px, 0)`;
        shifts.set(button, [shiftX, shiftY]);
        engaged.add(button);
      }
    };

    const schedule = () => {
      if (frame != null) return;
      frame = requestBrowserAnimationFrame(apply);
    };

    const removeMove = addDocumentListener('pointermove', (event: PointerEvent) => {
      if (event.pointerType !== 'mouse') return;
      pointerX = event.clientX;
      pointerY = event.clientY;
      schedule();
    }, { passive: true });

    const invalidateGeometry = () => {
      geometryDirty = true;
      schedule();
    };
    const removeResize = addWindowListener('resize', invalidateGeometry, { passive: true });
    // Capture catches scrollable panels as well as the window. Scrolling changes
    // viewport-relative positions without changing element sizes, so ResizeObserver
    // alone cannot keep this cache correct.
    const removeScroll = addDocumentListener('scroll', invalidateGeometry, { capture: true, passive: true });
    mutationObserver?.observe(document.body, { childList: true, subtree: true });
    schedule();

    // A pointer that leaves the window stops firing moves, so anything still pulled
    // would stay pulled.
    const removeLeave = addDocumentListener('pointerleave', () => {
      for (const button of engaged) release(button);
      engaged.clear();
    });

    return () => {
      removeMove();
      removeLeave();
      removeResize();
      removeScroll();
      resizeObserver?.disconnect();
      mutationObserver?.disconnect();
      if (frame != null) cancelBrowserAnimationFrame(frame);
      for (const button of engaged) release(button);
      engaged.clear();
      geometry.clear();
      shifts.clear();
    };
  }, [enabled]);
}

export default useMagneticPointer;
