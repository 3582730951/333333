import { fetchAuditArchive } from '../api/exports';
import { useApiMutation } from '../../shared/queries';

export function useAuditArchiveMutation() {
  return useApiMutation({ mutationFn: fetchAuditArchive });
}
