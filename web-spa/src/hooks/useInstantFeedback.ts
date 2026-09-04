import { useEffect } from 'react';
import { addDocumentListener, addWindowListener } from '../lib/browserLifecycle.js';

const ACTIONABLE = 'button:not([disabled]), [role="button"]:not([aria-disabled="true"]), a[href], input[type="submit"]';

function actionableTarget(target: EventTarget | null): HTMLElement | null {
  return target instanceof Element ? target.closest<HTMLElement>(ACTIONABLE) : null;
}

/**
 * Paints acknowledgement in the same frame as pointer/keyboard intent.
 *
 * React state is intentionally not involved: a delegated capture listener writes
 * one data attribute and two CSS variables before the click handler or network
 * request runs. Mutation hooks then own accepted/optimistic/settled state; this
 * hook owns only the physical "I felt your input" acknowledgement.
 */
export default function useInstantFeedback(enabled = true) {
  useEffect(() => {
    if (!enabled) return undefined;
    let active: HTMLElement | null = null;
    // P0 Top10 #10: this used to call getBoundingClientRect() on every
    // pointerdown and then write two custom properties -- a read wedged between
    // the user's input and the paint that acknowledges it, which is exactly the
    // shape that forces synchronous layout. The rect is now cached per element
    // and only dropped when something that can move it happens. A stale rect
    // costs at most a few pixels of ripple origin; it cannot affect behaviour.
    let rects = new WeakMap<HTMLElement, DOMRect>();
    const invalidate = () => { rects = new WeakMap(); };
    const rectFor = (target: HTMLElement) => {
      const cached = rects.get(target);
      if (cached) return cached;
      const measured = target.getBoundingClientRect();
      rects.set(target, measured);
      return measured;
    };

    const release = () => {
      active?.removeAttribute('data-pool-pressed');
      active = null;
    };
    const press = (target: HTMLElement, x?: number, y?: number) => {
      release();
      active = target;
      if (Number.isFinite(x) && Number.isFinite(y)) {
        const rect = rectFor(target);
        target.style.setProperty('--pool-press-x', `${Number(x) - rect.left}px`);
        target.style.setProperty('--pool-press-y', `${Number(y) - rect.top}px`);
      }
      target.setAttribute('data-pool-pressed', 'true');
      try { performance.mark('pool:interaction:intent'); } catch { /* optional telemetry */ }
    };

    const removePointerDown = addDocumentListener('pointerdown', (event: PointerEvent) => {
      const target = actionableTarget(event.target);
      if (target) press(target, event.clientX, event.clientY);
    }, { capture: true, passive: true });
    const removePointerUp = addDocumentListener('pointerup', release, { capture: true, passive: true });
    const removePointerCancel = addDocumentListener('pointercancel', release, { capture: true, passive: true });
    const removeKeyDown = addDocumentListener('keydown', (event: KeyboardEvent) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      const target = actionableTarget(event.target);
      if (target) press(target);
    }, { capture: true });
    const removeKeyUp = addDocumentListener('keyup', release, { capture: true });
    // Anything that can move an element invalidates every cached rect. Scroll is
    // captured because a scrolling container changes viewport coordinates without
    // ever reaching window's own scroll event.
    const removeResize = addWindowListener('resize', invalidate, { passive: true });
    const removeScroll = addDocumentListener('scroll', invalidate, { capture: true, passive: true });

    return () => {
      release();
      removePointerDown();
      removePointerUp();
      removePointerCancel();
      removeKeyDown();
      removeKeyUp();
      removeResize();
      removeScroll();
    };
  }, [enabled]);
}
