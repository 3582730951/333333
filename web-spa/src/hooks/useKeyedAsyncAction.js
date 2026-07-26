import { useCallback, useEffect, useRef, useState } from 'react';

export default function useKeyedAsyncAction(action, options = {}) {
  const mountedRef = useRef(false);
  const actionRef = useRef(action);
  const pendingRef = useRef(new Map());
  const [activeKeys, setActiveKeys] = useState(() => new Set());
  actionRef.current = action;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      pendingRef.current.clear();
    };
  }, []);

  const run = useCallback((key, ...args) => {
    if (options.dropConcurrent !== false && pendingRef.current.has(key)) {
      return pendingRef.current.get(key);
    }
    if (mountedRef.current) {
      setActiveKeys((current) => new Set(current).add(key));
    }
    const pending = Promise.resolve().then(() => actionRef.current(key, ...args));
    pendingRef.current.set(key, pending);
    const cleanup = () => {
      if (pendingRef.current.get(key) === pending) pendingRef.current.delete(key);
      if (mountedRef.current) {
        setActiveKeys((current) => {
          const next = new Set(current);
          next.delete(key);
          return next;
        });
      }
    };
    void pending.then(cleanup, cleanup);
    return pending;
  }, [options.dropConcurrent]);

  const reset = useCallback((key) => {
    if (key === undefined) pendingRef.current.clear();
    else pendingRef.current.delete(key);
    if (mountedRef.current) {
      setActiveKeys((current) => {
        if (key === undefined) return new Set();
        const next = new Set(current);
        next.delete(key);
        return next;
      });
    }
  }, []);

  const isRunning = useCallback((key) => activeKeys.has(key), [activeKeys]);
  const isBlocked = useCallback(() => false, []);
  const activeKey = activeKeys.values().next().value ?? null;

  return {
    run,
    reset,
    running: activeKeys.size > 0,
    activeKey,
    activeKeys,
    isRunning,
    isBlocked,
  };
}
