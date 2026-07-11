// Shell and data-view breakpoints. CSS uses max-width: 767px for the same mobile
// boundary; this module is the single JavaScript source for responsive behavior.
export const BREAKPOINTS = Object.freeze({ mobile: 768, compactSidebar: 1024 });

export function responsiveState(width: number) {
  return {
    isMobile: width < BREAKPOINTS.mobile,
    collapsedByWidth: width < BREAKPOINTS.compactSidebar,
  };
}
