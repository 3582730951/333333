import { useQuery } from '@tanstack/react-query';
import { fetchPortalUsageDashboard } from '../api/usage';
import { queryKeys, useQueryView } from '../../shared/queries';

export const portalUsageQueryKeys = {
  dashboard: queryKeys.list('portal-usage'),
};

export function usePortalUsageDashboardData() {
  return useQueryView(useQuery({
    queryKey: portalUsageQueryKeys.dashboard,
    queryFn: ({ signal }) => fetchPortalUsageDashboard(signal),
  }));
}
