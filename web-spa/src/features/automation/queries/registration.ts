import { useQuery } from '@tanstack/react-query';
import {
  fetchRegistrationCountries, fetchRegistrationDashboard, fetchRegistrationOptions,
  fetchRegistrationStrategy, saveRegistrationStrategy, startRegistrationJob,
} from '../api/registration';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const REGISTRATION_REFETCH_INTERVAL = 5_000;
export const registrationQueryKeys = {
  dashboard: queryKeys.list('registration-dashboard'),
  options: queryKeys.list('registration-options'),
  countries: queryKeys.list('registration-countries'),
  strategy: queryKeys.list('registration-strategy'),
};

export function useRegistrationDashboardData() {
  return useQueryView(useQuery({
    queryKey: registrationQueryKeys.dashboard,
    queryFn: ({ signal }) => fetchRegistrationDashboard(signal),
    refetchInterval: REGISTRATION_REFETCH_INTERVAL,
    refetchIntervalInBackground: false,
  }));
}

export function useRegistrationOptionsData() {
  return useQueryView(useQuery({
    queryKey: registrationQueryKeys.options,
    queryFn: ({ signal }) => fetchRegistrationOptions(signal),
    staleTime: 5 * 60_000,
  }));
}

export function useRegistrationCountriesData() {
  return useQueryView(useQuery({
    queryKey: registrationQueryKeys.countries,
    queryFn: ({ signal }) => fetchRegistrationCountries(signal),
    staleTime: 30 * 60_000,
  }));
}

export function useRegistrationStrategyData() {
  return useQueryView(useQuery({
    queryKey: registrationQueryKeys.strategy,
    queryFn: ({ signal }) => fetchRegistrationStrategy(signal),
    staleTime: 5 * 60_000,
  }));
}

export function useSaveRegistrationStrategyMutation() {
  return useApiMutation({ mutationFn: saveRegistrationStrategy, invalidate: [registrationQueryKeys.strategy] });
}

export function useStartRegistrationJobMutation() {
  return useApiMutation({ mutationFn: startRegistrationJob, invalidate: [registrationQueryKeys.dashboard] });
}
