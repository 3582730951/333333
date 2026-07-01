import { useCallback, useEffect, useRef, useState } from 'react';
import { isAbortError } from '../api.js';
import { abortController, abortSignal, createAbortController } from '../lib/browserAbort.js';

export default function useAsyncResource(loader, deps = [], options = {}) {
  const {
    initialData = null,
    auto = true,
    keepDataOnError = true,
    resetDataOnReload = false,
    onError,
  } = options;
  const loaderRef = useRef(loader);
  const onErrorRef = useRef(onError);
  const initialDataRef = useRef(initialData);
  const keepDataOnErrorRef = useRef(keepDataOnError);
  const resetDataOnReloadRef = useRef(resetDataOnReload);
  const autoRef = useRef(auto);
  const abortRef = useRef(null);
  const requestRef = useRef(0);
  const mountedRef = useRef(true);
  const [state, setState] = useState({
    data: initialData,
    error: null,
    loading: Boolean(auto),
    lastRefresh: null,
  });

  loaderRef.current = loader;
  onErrorRef.current = onError;
  initialDataRef.current = initialData;
  keepDataOnErrorRef.current = keepDataOnError;
  resetDataOnReloadRef.current = resetDataOnReload;
  autoRef.current = auto;

  const cancel = useCallback(() => {
    abortController(abortRef.current);
    abortRef.current = null;
  }, []);

  const reload = useCallback(async () => {
    const requestID = requestRef.current + 1;
    requestRef.current = requestID;
    cancel();
    const controller = createAbortController();
    const signal = abortSignal(controller);
    abortRef.current = controller;

    setState((prev) => ({
      ...prev,
      data: resetDataOnReloadRef.current ? initialDataRef.current : prev.data,
      loading: true,
      error: null,
    }));
    try {
      const data = await loaderRef.current({ signal });
      if (!mountedRef.current || requestRef.current !== requestID || signal?.aborted) {
        return undefined;
      }
      setState({ data, error: null, loading: false, lastRefresh: new Date() });
      return data;
    } catch (error) {
      if (!mountedRef.current || requestRef.current !== requestID || signal?.aborted || isAbortError(error)) {
        return undefined;
      }
      setState((prev) => ({
        data: keepDataOnErrorRef.current ? prev.data : initialDataRef.current,
        error,
        loading: false,
        lastRefresh: prev.lastRefresh,
      }));
      onErrorRef.current?.(error);
      return undefined;
    }
  }, [cancel]);

  useEffect(() => {
    mountedRef.current = true;
    if (autoRef.current) reload();
    return () => {
      mountedRef.current = false;
      cancel();
    };
  }, deps);

  return { ...state, reload, cancel };
}
