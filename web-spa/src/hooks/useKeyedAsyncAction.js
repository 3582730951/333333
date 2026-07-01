import { useCallback, useEffect, useRef, useState } from 'react';
import useAsyncAction from './useAsyncAction.js';

export default function useKeyedAsyncAction(action, options = {}) {
  const mountedRef = useRef(false);
  const [activeKey, setActiveKey] = useState(null);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const { run: runAction, running } = useAsyncAction(async (key, ...args) => {
    if (mountedRef.current) setActiveKey(key);
    try {
      return await action(key, ...args);
    } finally {
      if (mountedRef.current) setActiveKey(null);
    }
  }, options);

  const run = useCallback((key, ...args) => runAction(key, ...args), [runAction]);
  const isRunning = useCallback((key) => running && activeKey === key, [activeKey, running]);
  const isBlocked = useCallback((key) => running && activeKey !== key, [activeKey, running]);

  return {
    run,
    running,
    activeKey,
    isRunning,
    isBlocked,
  };
}
