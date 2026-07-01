import { useCallback, useEffect, useRef, useState } from 'react';
import { clearBrowserTimeout, setBrowserTimeout } from '../lib/browserLifecycle.js';

export default function useDelayedReveal(delay = 260) {
  const [value, setValue] = useState('');
  const timerRef = useRef(null);

  const cancelTimer = useCallback(() => {
    clearBrowserTimeout(timerRef.current);
    timerRef.current = null;
  }, []);

  const clear = useCallback(() => {
    cancelTimer();
    setValue('');
  }, [cancelTimer]);

  const reveal = useCallback((nextValue) => {
    cancelTimer();
    timerRef.current = setBrowserTimeout(() => {
      timerRef.current = null;
      setValue(nextValue || '');
    }, delay);
  }, [cancelTimer, delay]);

  useEffect(() => cancelTimer, [cancelTimer]);

  return { value, reveal, clear };
}
