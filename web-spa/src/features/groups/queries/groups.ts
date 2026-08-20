import { useQuery } from '@tanstack/react-query';
import {
  fetchAccountGroups,
  fetchGroupEgresses,
  fetchGroupInstructions,
  fetchGroupModels,
  fetchGroupProviders,
  fetchGroupSuperSkills,
  fetchUserGroups,
} from '../api/groups';
import { queryKeys, useQueryView } from '../../shared/queries';

export const groupQueryKeys = {
  all: queryKeys.domain('groups'),
  accountGroups: [...queryKeys.domain('groups'), 'account-pool'] as const,
  userGroups: [...queryKeys.domain('groups'), 'user'] as const,
  instructions: [...queryKeys.domain('groups'), 'instructions'] as const,
  superSkills: [...queryKeys.domain('groups'), 'super-skills'] as const,
  egresses: [...queryKeys.domain('groups'), 'egresses'] as const,
  providers: [...queryKeys.domain('groups'), 'providers'] as const,
  models: [...queryKeys.domain('groups'), 'models'] as const,
};

function useGroupResource(queryKey: readonly unknown[], queryFn: (signal?: AbortSignal) => Promise<any[]>, enabled = true) {
  return useQueryView(useQuery({
    queryKey,
    queryFn: ({ signal }) => queryFn(signal),
    enabled,
    staleTime: 60_000,
    placeholderData: (previous) => previous,
  }));
}

export const useAccountGroupsData = () => useGroupResource(groupQueryKeys.accountGroups, fetchAccountGroups);
export const useUserGroupsData = () => useGroupResource(groupQueryKeys.userGroups, fetchUserGroups);
export const useGroupInstructionsData = (enabled = true) => useGroupResource(groupQueryKeys.instructions, fetchGroupInstructions, enabled);
export const useGroupSuperSkillsData = (enabled = true) => useGroupResource(groupQueryKeys.superSkills, fetchGroupSuperSkills, enabled);
export const useGroupEgressesData = (enabled = true) => useGroupResource(groupQueryKeys.egresses, fetchGroupEgresses, enabled);
export const useGroupProvidersData = (enabled = true) => useGroupResource(groupQueryKeys.providers, fetchGroupProviders, enabled);
export const useGroupModelsData = (enabled = true) => useGroupResource(groupQueryKeys.models, fetchGroupModels, enabled);
