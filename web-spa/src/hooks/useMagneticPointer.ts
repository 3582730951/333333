import { useEffect } from 'react';
import { addDocumentListener, cancelBrowserAnimationFrame, requestBrowserAnimationFrame } from '../lib/browserLifecycle.js';

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
 * The handler only records the pointer; all measurement and writing happens inside a
 * single animation frame, so moving the mouse quickly cannot queue more work than the
 * compositor can drain. Reads are batched ahead of writes to avoid forcing layout
 * once per button.
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
    const engaged = new Set<HTMLElement>();

    const release = (element: HTMLElement) => {
      element.style.transform = '';
      element.style.willChange = '';
    };

    const apply = () => {
      frame = null;
      const buttons = document.querySelectorAll<HTMLElement>(SELECTOR);
      const next = new Set<HTMLElement>();
      const writes: Array<[HTMLElement, number, number]> = [];
      for (const button of buttons) {
        const rect = button.getBoundingClientRect();
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

    // A pointer that leaves the window stops firing moves, so anything still pulled
    // would stay pulled.
    const removeLeave = addDocumentListener('pointerleave', () => {
      for (const button of engaged) release(button);
      engaged.clear();
    });

    return () => {
      removeMove();
      removeLeave();
      if (frame != null) cancelBrowserAnimationFrame(frame);
      for (const button of engaged) release(button);
      engaged.clear();
    };
  }, [enabled]);
}

export default useMagneticPointer;
