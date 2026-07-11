import { useQuery } from '@tanstack/react-query';
import { fetchSystemMetrics } from '../api/system';
import { queryKeys, useQueryView } from '../../shared/queries';

export const SYSTEM_REFETCH_INTERVAL = 3_000;
export const systemQueryKeys = {
  metrics: queryKeys.list('system-metrics'),
};

export function useSystemMetricsData() {
  return useQueryView(useQuery({
    queryKey: systemQueryKeys.metrics,
    queryFn: ({ signal }) => fetchSystemMetrics(signal),
    refetchInterval: SYSTEM_REFETCH_INTERVAL,
    refetchIntervalInBackground: false,
  }));
}
