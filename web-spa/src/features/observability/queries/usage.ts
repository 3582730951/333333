import { useQuery } from '@tanstack/react-query';
import { fetchUsageDashboard, resetUsageCacheStats } from '../api/usage';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';
import type { UsageRange } from '../model/usage';

export const usageQueryKeys = {
  all: queryKeys.domain('usage-dashboard'),
  dashboard: (range: UsageRange) => queryKeys.list('usage-dashboard', { range }),
};

export function useUsageDashboardData(range: UsageRange) {
  return useQueryView(useQuery({
    queryKey: usageQueryKeys.dashboard(range),
    queryFn: ({ signal }) => fetchUsageDashboard(range, signal),
  }));
}

export function useResetUsageCacheMutation() {
  return useApiMutation({ mutationFn: resetUsageCacheStats, invalidate: [usageQueryKeys.all] });
}
