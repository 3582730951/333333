import { useQuery } from '@tanstack/react-query';
import { fetchUsageCacheDiagnostic, fetchUsageDashboard, resetUsageCacheStats } from '../api/usage';
import type { UsageCacheDiagnosticField } from '../api/usage';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';
import type { UsageRange } from '../model/usage';

export const usageQueryKeys = {
  all: queryKeys.domain('usage-dashboard'),
  diagnosticsAll: queryKeys.domain('usage-cache-diagnostic'),
  dashboard: (range: UsageRange) => queryKeys.list('usage-dashboard', { range }),
  diagnostic: (range: UsageRange, field: UsageCacheDiagnosticField) => queryKeys.list('usage-cache-diagnostic', { range, field }),
};

export function useUsageDashboardData(range: UsageRange) {
  return useQueryView(useQuery({
    queryKey: usageQueryKeys.dashboard(range),
    queryFn: ({ signal }) => fetchUsageDashboard(range, signal),
    staleTime: 15_000,
  }));
}

export function useUsageCacheDiagnosticData(range: UsageRange, field: UsageCacheDiagnosticField | null) {
  return useQueryView(useQuery({
    queryKey: field ? usageQueryKeys.diagnostic(range, field) : queryKeys.list('usage-cache-diagnostic', { range, field: 'none' }),
    queryFn: ({ signal }) => fetchUsageCacheDiagnostic(range, field!, signal),
    enabled: Boolean(field),
    staleTime: 30_000,
  }));
}

export function useResetUsageCacheMutation() {
  return useApiMutation({ mutationFn: resetUsageCacheStats, invalidate: [usageQueryKeys.all, usageQueryKeys.diagnosticsAll] });
}
