import { useQuery } from '@tanstack/react-query';
import { createUser, deleteUser, fetchUsers, updateUser } from '../api/users';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const userQueryKeys = {
  all: queryKeys.domain('users'),
  list: queryKeys.list('users'),
};

export function useUsersData() {
  return useQueryView(useQuery({ queryKey: userQueryKeys.list, queryFn: ({ signal }) => fetchUsers(signal) }));
}

export function useCreateUserMutation() {
  return useApiMutation({ mutationFn: createUser, invalidate: [userQueryKeys.all] });
}

export function useUpdateUserMutation() {
  return useApiMutation({ mutationFn: updateUser, invalidate: [userQueryKeys.all] });
}

export function useDeleteUserMutation() {
  return useApiMutation({ mutationFn: deleteUser, invalidate: [userQueryKeys.all] });
}
