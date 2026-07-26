import { useCallback } from 'react';
import { useMutation, useQuery, useQueryClient, type QueryClient, type QueryKey, type UseQueryOptions, type UseQueryResult } from '@tanstack/react-query';

export const queryKeys = {
  all: ['pool'] as const,
  domain: (domain: string) => ['pool', domain] as const,
  list: (domain: string, params: unknown = {}) => ['pool', domain, 'list', params] as const,
  detail: (domain: string, id: string) => ['pool', domain, 'detail', id] as const,
};

export async function invalidateQueryKeys(client: QueryClient, keys: ReadonlyArray<QueryKey>) {
  await Promise.all(keys.map((queryKey) => client.invalidateQueries({ queryKey })));
}

export function useApiQuery<T>(options: UseQueryOptions<T>) {
  return useQuery(options);
}

export function useQueryView<T>(query: UseQueryResult<T, Error>) {
  const refetch = query.refetch;
  const reload = useCallback(async () => {
    const result = await refetch({ cancelRefetch: true });
    return result.error ? undefined : result.data;
  }, [refetch]);
  return {
    data: query.data,
    loading: query.isFetching,
    error: query.error,
    lastRefresh: query.dataUpdatedAt ? new Date(query.dataUpdatedAt) : null,
    stale: Boolean(query.error && query.dataUpdatedAt),
    reload,
  };
}

interface ApiMutationOptions<TVariables, TResult> {
  mutationFn: (variables: TVariables) => Promise<TResult>;
  invalidate?: ReadonlyArray<QueryKey>;
  onSuccess?: (result: TResult, variables: TVariables) => void | Promise<void>;
}

export function useApiMutation<TVariables, TResult>({ mutationFn, invalidate = [], onSuccess }: ApiMutationOptions<TVariables, TResult>) {
  const client = useQueryClient();
  return useMutation({
    mutationFn,
    onSuccess: (result, variables) => {
      // Mutations complete when the write succeeds. List refreshes run in the
      // background so a slow read cannot leave dialogs and buttons locked.
      void invalidateQueryKeys(client, invalidate);
      return onSuccess?.(result, variables);
    },
  });
}
