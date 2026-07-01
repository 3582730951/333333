import { useCallback, useEffect, useRef, useState } from 'react';

export default function useAsyncAction(action, options = {}) {
  const { dropConcurrent = true } = options;
  const actionRef = useRef(action);
  const mountedRef = useRef(true);
  const runningRef = useRef(false);
  const [running, setRunning] = useState(false);

  actionRef.current = action;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const run = useCallback(async (...args) => {
    if (dropConcurrent && runningRef.current) return undefined;
    runningRef.current = true;
    if (mountedRef.current) setRunning(true);
    try {
      return await actionRef.current(...args);
    } finally {
      runningRef.current = false;
      if (mountedRef.current) setRunning(false);
    }
  }, [dropConcurrent]);

  return { run, running };
}
