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
  rollback?: (snapshot: TSnapshot, variables: TVariables, error: unknown) => void | Promise<void>;
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

    // Keep the deduplication promise alive through optimistic work, the request,
    // and the success/error callback. Callers must observe callback failures and
    // a second click must remain coalesced until every side effect has settled.
    const task = Promise.resolve().then(async () => {
      let snapshot: TSnapshot;
      let hasSnapshot = false;
      if (optimistic) {
        snapshot = optimistic(variables);
        hasSnapshot = true;
        setPhase('optimistic');
      }
      const context: MutationContext = idempotent ? { idempotencyKey: idempotencyKey() } : {};
      let result: TResult;
      try {
        result = await mutation.mutateAsync({ variables, context });
      } catch (reason) {
        // Roll back only when the request itself failed. A success callback
        // failure must not undo a successfully committed server mutation.
        if (hasSnapshot) await rollback?.(snapshot!, variables, reason);
        throw reason;
      }
      await onSuccess?.(result, variables);
      if (mounted.current) setPhase('settled');
      try {
        performance.measure('pool:mutation:accepted-to-settled', {
          start: acceptedAt,
          end: performance.now(),
        });
      } catch { /* optional telemetry */ }
      return result;
    }).catch((reason) => {
      const normalized = normalizeApiError(reason);
      if (mounted.current) {
        setError(normalized);
        setRequestId(normalized.requestId || '');
        setPhase('error');
      }
      throw reason;
    }).finally(() => {
      if (pending.current === task) pending.current = null;
    });
    pending.current = task;
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
