import { useCallback, useEffect, useRef } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

let loaderSequence = 0;
const loaderIDs = new WeakMap();

function loaderID(loader) {
  if (!loaderIDs.has(loader)) loaderIDs.set(loader, ++loaderSequence);
  return loaderIDs.get(loader);
}

// Compatibility façade for legacy pages. Server state is owned by TanStack Query;
// page components keep the familiar {data, loading, error, reload} API while they
// are migrated feature-by-feature.
export default function useAsyncResource(loader, deps = [], options = {}) {
  const {
    initialData = null,
    auto = true,
    keepDataOnError = true,
    onError,
  } = options;
  const loaderRef = useRef(loader);
  const onErrorRef = useRef(onError);
  const queryClient = useQueryClient();
  loaderRef.current = loader;
  onErrorRef.current = onError;

  const queryKey = ['legacy-resource', loaderID(loader), ...deps];
  const query = useQuery({
    queryKey,
    enabled: Boolean(auto),
    queryFn: ({ signal }) => loaderRef.current({ signal }),
    placeholderData: initialData,
  });
  const refetch = query.refetch;

  useEffect(() => {
    if (query.error) onErrorRef.current?.(query.error);
  }, [query.error]);

  const reload = useCallback(async () => {
    const result = await refetch({ cancelRefetch: true });
    return result.error ? undefined : result.data;
  }, [refetch]);

  const currentLoaderID = loaderID(loader);
  const cancel = useCallback(() => queryClient.cancelQueries({ queryKey: ['legacy-resource', currentLoaderID] }), [queryClient, currentLoaderID]);
  const hasFreshData = query.dataUpdatedAt > 0;
  const data = query.error && !keepDataOnError ? initialData : query.data;

  return {
    data,
    error: query.error,
    loading: query.isFetching,
    lastRefresh: hasFreshData ? new Date(query.dataUpdatedAt) : null,
    reload,
    cancel,
    stale: Boolean(query.error && hasFreshData),
  };
}
