import { useEffect, useRef, useState } from 'react';
import {
  addDocumentListener,
  clearBrowserInterval,
  isDocumentVisible,
  setBrowserInterval,
  setBrowserTimeout,
} from '../lib/browserLifecycle.js';

export function usePageVisible() {
  const [visible, setVisible] = useState(isDocumentVisible);

  useEffect(() => {
    const sync = () => setVisible(isDocumentVisible());
    return addDocumentListener('visibilitychange', sync);
  }, []);

  return visible;
}

export default function useVisibleInterval(callback, delay, options = {}) {
  const { enabled = true, fireOnVisible = true, dropWhileRunning = true } = options;
  const callbackRef = useRef(callback);
  const runningRef = useRef(false);
  callbackRef.current = callback;

  useEffect(() => {
    if (!enabled || !delay || delay < 0) return undefined;

    let timer = 0;
    const runCallback = () => {
      if (dropWhileRunning && runningRef.current) return;
      let result;
      try {
        result = callbackRef.current();
      } catch (error) {
        runningRef.current = false;
        throw error;
      }
      if (!result || typeof result.then !== 'function') return;
      runningRef.current = true;
      Promise.resolve(result)
        .catch((error) => {
          setBrowserTimeout(() => { throw error; }, 0);
        })
        .finally(() => {
          runningRef.current = false;
        });
    };
    const clearTimer = () => {
      if (!timer) return;
      clearBrowserInterval(timer);
      timer = 0;
    };
    const startTimer = () => {
      clearTimer();
      if (isDocumentVisible()) {
        timer = setBrowserInterval(runCallback, delay);
      }
    };
    const onVisibilityChange = () => {
      if (isDocumentVisible()) {
        startTimer();
        if (fireOnVisible) runCallback();
      } else {
        clearTimer();
      }
    };

    startTimer();
    const removeVisibility = addDocumentListener('visibilitychange', onVisibilityChange);
    return () => {
      clearTimer();
      removeVisibility();
    };
  }, [delay, enabled, fireOnVisible, dropWhileRunning]);
}
