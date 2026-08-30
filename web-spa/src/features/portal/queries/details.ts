import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  fetchPortalQuota, fetchPortalSessions, fetchPortalUsageEvents, revokePortalSession, type PortalUsageFilters,
} from '../api/details';
import { queryKeys, useQueryView } from '../../shared/queries';

export const portalDetailQueryKeys = {
  usageDomain: queryKeys.domain('portal-usage-events'),
  usage: (filters: PortalUsageFilters) => queryKeys.list('portal-usage-events', filters),
  quota: queryKeys.list('portal-quota'),
  sessions: queryKeys.list('portal-sessions'),
};

export function usePortalUsageEventsData(filters: PortalUsageFilters) {
  return useQueryView(useQuery({
    queryKey: portalDetailQueryKeys.usage(filters),
    queryFn: ({ signal }) => fetchPortalUsageEvents(filters, signal),
    placeholderData: keepPreviousData,
  }));
}

export function usePortalQuotaData() {
  return useQueryView(useQuery({ queryKey: portalDetailQueryKeys.quota, queryFn: ({ signal }) => fetchPortalQuota(signal) }));
}

export function usePortalSessionsData() {
  return useQueryView(useQuery({ queryKey: portalDetailQueryKeys.sessions, queryFn: ({ signal }) => fetchPortalSessions(signal) }));
}

export function useRevokePortalSessionMutation() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: revokePortalSession,
    onSuccess: () => { void client.invalidateQueries({ queryKey: portalDetailQueryKeys.sessions }); },
  });
}
