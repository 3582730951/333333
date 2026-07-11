import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query';
import { normalizeApiError } from '../api/errors';

function shouldRetry(failureCount: number, error: unknown): boolean {
  return failureCount < 2 && normalizeApiError(error).retryable;
}

export const queryClient = new QueryClient({
  queryCache: new QueryCache(),
  mutationCache: new MutationCache(),
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      gcTime: 5 * 60_000,
      retry: shouldRetry,
      refetchOnWindowFocus: false,
    },
    mutations: { retry: false },
  },
});
