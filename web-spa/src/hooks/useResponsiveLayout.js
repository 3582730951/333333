import { useEffect, useState } from 'react';
import {
  addWindowListener,
  browserViewportWidth,
  cancelBrowserAnimationFrame,
  requestBrowserAnimationFrame,
} from '../lib/browserLifecycle.js';

function readViewport() {
  const width = browserViewportWidth();
  return {
    collapsedByWidth: width < 920,
    isMobile: width < 768,
  };
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
