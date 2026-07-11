import { useQuery } from '@tanstack/react-query';
import { fetchDashboardCore, fetchDashboardSecondary } from '../api/dashboard';
import { queryKeys, useQueryView } from '../../shared/queries';

export const DASHBOARD_REFRESH_MS = 15_000;

export const dashboardQueryKeys = {
  all: queryKeys.domain('dashboard'),
  core: queryKeys.list('dashboard', { layer: 'core' }),
  secondary: queryKeys.list('dashboard', { layer: 'secondary' }),
};

export function useDashboardCoreData() {
  return useQueryView(useQuery({
    queryKey: dashboardQueryKeys.core,
    queryFn: ({ signal }) => fetchDashboardCore(signal),
    refetchInterval: DASHBOARD_REFRESH_MS,
    refetchIntervalInBackground: false,
  }));
}

export function useDashboardSecondaryData() {
  return useQueryView(useQuery({
    queryKey: dashboardQueryKeys.secondary,
    queryFn: ({ signal }) => fetchDashboardSecondary(signal),
    refetchInterval: DASHBOARD_REFRESH_MS,
    refetchIntervalInBackground: false,
  }));
}
