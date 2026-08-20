import type { QueryClient } from '@tanstack/react-query';

type Cancelled = () => boolean;

// Warm only the data that determines the first useful paint of the operator's
// highest-frequency routes. Tasks run one at a time, stop on logout/unmount, and
// are deduplicated by QueryClient, so this cannot turn login into a request burst.
export async function warmAdminData(client: QueryClient, cancelled: Cancelled = () => false) {
  const tasks: Array<() => Promise<void>> = [
    async () => {
      const [{ dashboardQueryKeys }, { fetchDashboardCore }] = await Promise.all([
        import('../features/observability/queries/dashboard'),
        import('../features/observability/api/dashboard'),
      ]);
      await client.prefetchQuery({
        queryKey: dashboardQueryKeys.core,
        queryFn: ({ signal }) => fetchDashboardCore(signal),
        staleTime: 30_000,
        retry: false,
      });
    },
    async () => {
      const [{ accountQueryKeys }, { fetchAccountsPage }] = await Promise.all([
        import('../features/accounts/queries/accounts'),
        import('../features/accounts/api/accounts'),
      ]);
      const params = { page: 1, pageSize: 50, search: '', authType: 'all' as const };
      await client.prefetchQuery({
        queryKey: accountQueryKeys.list(params),
        queryFn: ({ signal }) => fetchAccountsPage(params, signal),
        staleTime: 30_000,
        retry: false,
      });
    },
    async () => {
      const [{ groupQueryKeys }, { fetchAccountGroups, fetchUserGroups }] = await Promise.all([
        import('../features/groups/queries/groups'),
        import('../features/groups/api/groups'),
      ]);
      await client.prefetchQuery({ queryKey: groupQueryKeys.accountGroups, queryFn: ({ signal }) => fetchAccountGroups(signal), staleTime: 60_000, retry: false });
      if (cancelled()) return;
      await client.prefetchQuery({ queryKey: groupQueryKeys.userGroups, queryFn: ({ signal }) => fetchUserGroups(signal), staleTime: 60_000, retry: false });
    },
    async () => {
      const [{ observabilityQueryKeys }, { fetchQuota }] = await Promise.all([
        import('../features/observability/queries/events'),
        import('../features/observability/api/events'),
      ]);
      await client.prefetchQuery({ queryKey: observabilityQueryKeys.quota, queryFn: ({ signal }) => fetchQuota(signal), staleTime: 30_000, retry: false });
    },
    async () => {
      const [{ usageQueryKeys }, { fetchUsageDashboard }] = await Promise.all([
        import('../features/observability/queries/usage'),
        import('../features/observability/api/usage'),
      ]);
      await client.prefetchQuery({
        queryKey: usageQueryKeys.dashboard('today'),
        queryFn: ({ signal }) => fetchUsageDashboard('today', signal),
        staleTime: 30_000,
        retry: false,
      });
    },
  ];

  for (const task of tasks) {
    if (cancelled()) return;
    try {
      await task();
    } catch {
      // Warm-up is best effort. The destination route retains its normal error
      // handling and retry controls if an idle request fails.
    }
  }
}
