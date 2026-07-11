import { useQuery } from '@tanstack/react-query';
import { fetchAccountsBundle } from '../api/accounts';
import type { AccountsPageParams } from '../model/types';
import { queryKeys, useQueryView } from '../../shared/queries';

export const accountQueryKeys = {
  all: queryKeys.domain('accounts'),
  list: (params: AccountsPageParams) => queryKeys.list('accounts', params),
};

export function useAccountsPage(params: AccountsPageParams) {
  const query = useQuery({
    queryKey: accountQueryKeys.list(params),
    queryFn: ({ signal }) => fetchAccountsBundle(params, signal),
    placeholderData: (previous) => previous,
  });
  return useQueryView(query);
}
