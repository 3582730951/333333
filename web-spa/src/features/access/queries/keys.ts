import { useQuery } from '@tanstack/react-query';
import {
  createAdminKey, createPortalKey, deleteAdminKey, deletePortalKey, fetchAdminKeys, fetchPortalKeys, updatePortalKey,
} from '../api/keys';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const apiKeyQueryKeys = {
  admin: queryKeys.list('admin-api-keys'),
  adminAll: queryKeys.domain('admin-api-keys'),
  portal: queryKeys.list('portal-api-keys'),
  portalAll: queryKeys.domain('portal-api-keys'),
};

export function useAdminKeysData() {
  return useQueryView(useQuery({ queryKey: apiKeyQueryKeys.admin, queryFn: ({ signal }) => fetchAdminKeys(signal) }));
}
export function useCreateAdminKeyMutation() {
  return useApiMutation({ mutationFn: createAdminKey, invalidate: [apiKeyQueryKeys.adminAll] });
}
export function useDeleteAdminKeyMutation() {
  return useApiMutation({ mutationFn: deleteAdminKey, invalidate: [apiKeyQueryKeys.adminAll] });
}
export function usePortalKeysData() {
  return useQueryView(useQuery({ queryKey: apiKeyQueryKeys.portal, queryFn: ({ signal }) => fetchPortalKeys(signal) }));
}
export function useCreatePortalKeyMutation() {
  return useApiMutation({ mutationFn: createPortalKey, invalidate: [apiKeyQueryKeys.portalAll] });
}
export function useUpdatePortalKeyMutation() {
  return useApiMutation({ mutationFn: updatePortalKey, invalidate: [apiKeyQueryKeys.portalAll] });
}
export function useDeletePortalKeyMutation() {
  return useApiMutation({ mutationFn: deletePortalKey, invalidate: [apiKeyQueryKeys.portalAll] });
}
