import { useQuery } from '@tanstack/react-query';
import {
  createAdminKey, createPortalKey, deleteAdminKey, deletePortalKey, fetchAdminKeyRoutingOptions, fetchAdminKeys,
  fetchPortalKeys, updateAdminKey, updatePortalKey,
} from '../api/keys';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const apiKeyQueryKeys = {
  admin: queryKeys.list('admin-api-keys'),
  adminAll: queryKeys.domain('admin-api-keys'),
  adminRouting: queryKeys.list('admin-api-key-routing'),
  portal: queryKeys.list('portal-api-keys'),
  portalAll: queryKeys.domain('portal-api-keys'),
};

export function useAdminKeysData() {
  return useQueryView(useQuery({ queryKey: apiKeyQueryKeys.admin, queryFn: ({ signal }) => fetchAdminKeys(signal) }));
}
export function useCreateAdminKeyMutation() {
  return useApiMutation({ mutationFn: createAdminKey, invalidate: [apiKeyQueryKeys.adminAll] });
}
export function useUpdateAdminKeyMutation() {
  return useApiMutation({ mutationFn: updateAdminKey, invalidate: [apiKeyQueryKeys.adminAll] });
}
export function useDeleteAdminKeyMutation() {
  return useApiMutation({ mutationFn: deleteAdminKey, invalidate: [apiKeyQueryKeys.adminAll] });
}
export function useAdminKeyRoutingOptionsData() {
  return useQueryView(useQuery({ queryKey: apiKeyQueryKeys.adminRouting, queryFn: ({ signal }) => fetchAdminKeyRoutingOptions(signal) }));
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
