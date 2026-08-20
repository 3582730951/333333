import { useQuery } from '@tanstack/react-query';
import { fetchModelAudit, fetchUsageCacheDiagnostic, fetchUsageDashboard, resetUsageCacheStats } from '../api/usage';
import type { UsageCacheDiagnosticField } from '../api/usage';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';
import type { UsageRange } from '../model/usage';

export const usageQueryKeys = {
  all: queryKeys.domain('usage-dashboard'),
  diagnosticsAll: queryKeys.domain('usage-cache-diagnostic'),
  modelAuditAll: queryKeys.domain('usage-model-audit'),
  dashboard: (range: UsageRange) => queryKeys.list('usage-dashboard', { range }),
  diagnostic: (range: UsageRange, field: UsageCacheDiagnosticField) => queryKeys.list('usage-cache-diagnostic', { range, field }),
  modelAudit: (range: UsageRange) => queryKeys.list('usage-model-audit', { range }),
};

export function useUsageDashboardData(range: UsageRange) {
  return useQueryView(useQuery({
    queryKey: usageQueryKeys.dashboard(range),
    queryFn: ({ signal }) => fetchUsageDashboard(range, signal),
    staleTime: 15_000,
  }));
}

export function useModelAuditData(range: UsageRange, enabled = true) {
  return useQueryView(useQuery({
    queryKey: usageQueryKeys.modelAudit(range),
    queryFn: ({ signal }) => fetchModelAudit(range, signal),
    enabled,
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
