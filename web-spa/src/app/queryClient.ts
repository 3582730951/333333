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
      // Keep the last successful control-plane snapshot around for an operator's
      // whole working session. Queries still revalidate after 30 seconds, but a
      // return navigation paints from memory instead of falling back to a blank
      // table while the network request is in flight.
      staleTime: 30_000,
      gcTime: 30 * 60_000,
      retry: shouldRetry,
      refetchOnWindowFocus: false,
    },
    mutations: { retry: false },
  },
});
