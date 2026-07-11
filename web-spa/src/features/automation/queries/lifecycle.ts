import { useQuery } from '@tanstack/react-query';
import {
  cancelLifecycleTask, createLifecycleTask, fetchLifecycleDashboard, fetchLifecycleOptions,
} from '../api/lifecycle';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const lifecycleQueryKeys = {
  all: queryKeys.domain('lifecycle'),
  dashboard: queryKeys.list('lifecycle-dashboard'),
  options: queryKeys.list('lifecycle-options'),
};
export const LIFECYCLE_REFETCH_INTERVAL = 5_000;

export function useLifecycleDashboardData() {
  return useQueryView(useQuery({
    queryKey: lifecycleQueryKeys.dashboard,
    queryFn: ({ signal }) => fetchLifecycleDashboard(signal),
    refetchInterval: LIFECYCLE_REFETCH_INTERVAL,
    refetchIntervalInBackground: false,
  }));
}

export function useLifecycleOptionsData() {
  return useQueryView(useQuery({
    queryKey: lifecycleQueryKeys.options,
    queryFn: ({ signal }) => fetchLifecycleOptions(signal),
    staleTime: 5 * 60_000,
  }));
}

export function useCreateLifecycleTaskMutation() {
  return useApiMutation({ mutationFn: createLifecycleTask, invalidate: [lifecycleQueryKeys.dashboard] });
}

export function useCancelLifecycleTaskMutation() {
  return useApiMutation({ mutationFn: cancelLifecycleTask, invalidate: [lifecycleQueryKeys.dashboard] });
}
