import { useCallback, useRef } from 'react';
import { useMutation } from '@tanstack/react-query';

// Compatibility façade backed by Query mutations. `dropConcurrent` preserves the
// previous click-deduplication contract for destructive and batch actions.
export default function useAsyncAction(action, options = {}) {
  const { dropConcurrent = true } = options;
  const actionRef = useRef(action);
  const runningRef = useRef(false);
  actionRef.current = action;

  const mutation = useMutation({ mutationFn: (args) => actionRef.current(...args) });
  const mutateAsync = mutation.mutateAsync;
  const run = useCallback(async (...args) => {
    if (dropConcurrent && runningRef.current) return undefined;
    runningRef.current = true;
    try {
      return await mutateAsync(args);
    } finally {
      runningRef.current = false;
    }
  }, [dropConcurrent, mutateAsync]);

  return { run, running: mutation.isPending, error: mutation.error, reset: mutation.reset };
}
