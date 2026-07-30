import { fetchAuditArchive } from '../api/exports';
import type { AuditExportKind, DiagnosticExportOptions } from '../api/exports';
import { useApiMutation } from '../../shared/queries';

export interface AuditArchiveMutationInput {
  kind: AuditExportKind;
  diagnosticOptions?: DiagnosticExportOptions;
}

export function useAuditArchiveMutation() {
  return useApiMutation({
    mutationFn: ({ kind, diagnosticOptions = {} }: AuditArchiveMutationInput) => (
      fetchAuditArchive(kind, diagnosticOptions)
    ),
  });
}
