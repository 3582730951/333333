import { useEffect, useState } from 'react';
import {
  addWindowListener,
  browserViewportWidth,
  cancelBrowserAnimationFrame,
  requestBrowserAnimationFrame,
} from '../lib/browserLifecycle.js';
import { responsiveState } from '../lib/breakpoints.ts';

function readViewport() {
  return responsiveState(browserViewportWidth());
}

export default function useResponsiveLayout() {
  const [state, setState] = useState(readViewport);

  useEffect(() => {
    let frame = 0;
    const sync = () => {
      frame = 0;
      setState((prev) => {
        const next = readViewport();
        return prev.collapsedByWidth === next.collapsedByWidth && prev.isMobile === next.isMobile ? prev : next;
      });
    };
    const onResize = () => {
      if (frame) return;
      frame = requestBrowserAnimationFrame(sync);
    };
    const removeResize = addWindowListener('resize', onResize);
    return () => {
      if (frame) cancelBrowserAnimationFrame(frame);
      removeResize();
    };
  }, []);

  return state;
}
