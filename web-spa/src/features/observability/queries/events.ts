import { useQuery } from '@tanstack/react-query';
import { fetchAuditRows, fetchCFEvents, fetchQuota } from '../api/events';
import { queryKeys, useQueryView } from '../../shared/queries';

export const observabilityQueryKeys = {
  quota: [...queryKeys.domain('observability'), 'quota'] as const,
  cfEvents: [...queryKeys.domain('observability'), 'cf-events'] as const,
  audit: [...queryKeys.domain('observability'), 'audit'] as const,
};

export function useQuotaData() {
  return useQueryView(useQuery({ queryKey: observabilityQueryKeys.quota, queryFn: ({ signal }) => fetchQuota(signal) }));
}

export function useCFEventsData() {
  return useQueryView(useQuery({ queryKey: observabilityQueryKeys.cfEvents, queryFn: ({ signal }) => fetchCFEvents(signal) }));
}

export function useAuditData() {
  return useQueryView(useQuery({ queryKey: observabilityQueryKeys.audit, queryFn: ({ signal }) => fetchAuditRows(signal) }));
}
