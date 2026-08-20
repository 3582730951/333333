import { useQuery } from '@tanstack/react-query';
import { fetchAccountsPage } from '../api/accounts';
import type { AccountsPageParams } from '../model/types';
import { queryKeys, useQueryView } from '../../shared/queries';
import { useAccountGroupsData } from '../../groups/queries/groups';

export const accountQueryKeys = {
  all: queryKeys.domain('accounts'),
  list: (params: AccountsPageParams) => queryKeys.list('accounts', params),
};

export function useAccountsPage(params: AccountsPageParams) {
  const accounts = useQueryView(useQuery({
    queryKey: accountQueryKeys.list(params),
    queryFn: ({ signal }) => fetchAccountsPage(params, signal),
    placeholderData: (previous) => previous,
  }));
  const groups = useAccountGroupsData();
  return {
    ...accounts,
    // Group options enhance move/edit actions but do not belong to the account
    // table's critical path. The list paints as soon as /admin/accounts returns.
    data: accounts.data ? {
      ...accounts.data,
      groups: groups.data || [],
      error: groups.error || null,
    } : undefined,
  };
}
