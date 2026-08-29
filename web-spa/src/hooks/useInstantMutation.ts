import { useCallback, useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { normalizeApiError } from '../api/errors';

export type InstantMutationPhase = 'idle' | 'accepted' | 'optimistic' | 'settled' | 'error';

type MutationContext = {
  idempotencyKey?: string;
};

type InstantMutationOptions<TVariables, TResult, TSnapshot = unknown> = {
  mutationFn: (variables: TVariables, context: MutationContext) => Promise<TResult>;
  idempotent?: boolean;
  optimistic?: (variables: TVariables) => TSnapshot;
  rollback?: (snapshot: TSnapshot, variables: TVariables, error: unknown) => void;
  onSuccess?: (result: TResult, variables: TVariables) => void | Promise<void>;
};

function idempotencyKey() {
  try {
    return crypto.randomUUID();
  } catch {
    return `pool-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  }
}

/** Four-phase mutation protocol used by latency-sensitive controls. */
export function useInstantMutation<TVariables, TResult, TSnapshot = unknown>({
  mutationFn, idempotent = false, optimistic, rollback, onSuccess,
}: InstantMutationOptions<TVariables, TResult, TSnapshot>) {
  const mounted = useRef(true);
  const pending = useRef<Promise<TResult> | null>(null);
  const [phase, setPhase] = useState<InstantMutationPhase>('idle');
  const [requestId, setRequestId] = useState('');
  const [error, setError] = useState<Error | null>(null);
  const mutationRef = useRef(mutationFn);
  mutationRef.current = mutationFn;

  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const mutation = useMutation<TResult, unknown, { variables: TVariables; context: MutationContext }>({
    mutationFn: ({ variables, context }) => mutationRef.current(variables, context),
  });

  const run = useCallback((variables: TVariables) => {
    if (pending.current) return pending.current;
    const acceptedAt = performance.now();
    setError(null);
    setRequestId('');
    setPhase('accepted');
    try {
      performance.mark('pool:mutation:accepted');
      performance.measure('pool:mutation:intent-to-accepted', 'pool:interaction:intent', 'pool:mutation:accepted');
    } catch { /* keyboard/programmatic actions may not have a pointer intent mark */ }

    let snapshot: TSnapshot | undefined;
    if (optimistic) {
      snapshot = optimistic(variables);
      setPhase('optimistic');
    }
    const context: MutationContext = idempotent ? { idempotencyKey: idempotencyKey() } : {};
    const task = mutation.mutateAsync({ variables, context });
    pending.current = task;
    void task.then(async (result) => {
      await onSuccess?.(result, variables);
      if (mounted.current) setPhase('settled');
      try {
        performance.measure('pool:mutation:accepted-to-settled', {
          start: acceptedAt,
          end: performance.now(),
        });
      } catch { /* optional telemetry */ }
    }, (reason) => {
      if (snapshot !== undefined) rollback?.(snapshot, variables, reason);
      const normalized = normalizeApiError(reason);
      if (mounted.current) {
        setError(normalized);
        setRequestId(normalized.requestId || '');
        setPhase('error');
      }
    }).finally(() => {
      if (pending.current === task) pending.current = null;
    });
    return task;
  }, [idempotent, mutation, onSuccess, optimistic, rollback]);

  const reset = useCallback(() => {
    setPhase('idle');
    setRequestId('');
    setError(null);
    mutation.reset();
  }, [mutation]);

  return {
    run,
    reset,
    phase,
    accepted: phase !== 'idle',
    pending: phase === 'accepted' || phase === 'optimistic',
    error,
    requestId,
  };
}

export default useInstantMutation;
