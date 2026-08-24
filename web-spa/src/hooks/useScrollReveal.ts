import { useEffect } from 'react';

// Past this the stagger stops accumulating. At 60ms a step, an unbounded index on a
// long page would put the last section more than a second behind the first, which
// stops reading as choreography and starts reading as the page being slow.
const MAX_STAGGER_STEPS = 8;
// Reveal slightly before the element's top edge arrives, so the transition is
// finishing as it enters rather than starting once it is already being read.
const ROOT_MARGIN = '0px 0px -12% 0px';

function prefersReducedMotion() {
  try {
    return Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches);
  } catch {
    return false;
  }
}

/**
 * Scroll-triggered narrative reveal for everything inside `containerRef`.
 *
 * Opt-in per element via `data-reveal`. The hook stamps `data-revealed` when the
 * element first intersects and assigns its stagger position; atmosphere.css owns
 * what those states look like.
 *
 * Two safety properties that matter more than the effect itself:
 *
 *   - The hiding styles are scoped to `.pool-narrative`, and this hook is the only
 *     thing that adds that class. If IntersectionObserver is missing, or the
 *     visitor asked for reduced motion, or this effect simply never runs, the class
 *     is absent and every section renders visible. A reveal animation must never be
 *     able to leave a page blank.
 *   - Elements are unobserved as soon as they have been revealed, so a long page
 *     does not keep an observer entry per section for the life of the route.
 *
 * `routeKey` re-arms the whole thing on navigation, and a scoped MutationObserver
 * catches sections that mount later when their data resolves.
 */
/*
 * Takes the element itself rather than a ref object.
 *
 * A ref was the first shape and it never armed: the shell renders a boot screen
 * while auth resolves, so on the render that runs this effect the container does
 * not exist yet, `ref.current` is null, and the effect bails. The ref object is
 * stable and `routeKey` has not changed by the time the real container mounts, so
 * the effect never re-runs and the narrative stays off for the whole session. A
 * state-backed callback ref in the caller makes the element itself the dependency,
 * which is what actually changes.
 */
export function useScrollReveal(container: HTMLElement | null, routeKey: string) {
  useEffect(() => {
    if (!container) return undefined;
    if (typeof IntersectionObserver !== 'function') return undefined;
    if (prefersReducedMotion()) return undefined;

    container.classList.add('pool-narrative');

    let counter = 0;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        const element = entry.target as HTMLElement;
        element.dataset.revealed = 'true';
        observer.unobserve(element);
      }
    }, { rootMargin: ROOT_MARGIN, threshold: 0.01 });

    const arm = () => {
      const targets = container.querySelectorAll<HTMLElement>('[data-reveal]:not([data-revealed])');
      for (const element of targets) {
        // Marked immediately so a second arm pass triggered by the mutation
        // observer cannot enqueue the same element twice and restart its stagger.
        element.dataset.revealed = 'false';
        element.style.setProperty('--pool-reveal-index', String(Math.min(counter, MAX_STAGGER_STEPS)));
        counter += 1;
        observer.observe(element);
      }
    };

    arm();

    // Pages fetch before they render their sections, so most `[data-reveal]` nodes
    // do not exist on the first pass. Scoped to this container and to childList, so
    // it is not watching the whole document or any attribute traffic.
    const mutations = new MutationObserver(arm);
    mutations.observe(container, { childList: true, subtree: true });

    return () => {
      mutations.disconnect();
      observer.disconnect();
      container.classList.remove('pool-narrative');
      // Leave no element stranded mid-reveal: the next route owns these nodes and
      // must not inherit a half-applied state from this one.
      for (const element of container.querySelectorAll<HTMLElement>('[data-reveal]')) {
        delete element.dataset.revealed;
        element.style.removeProperty('--pool-reveal-index');
      }
    };
  }, [container, routeKey]);
}

export default useScrollReveal;
