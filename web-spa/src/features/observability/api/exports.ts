import api from '../../../api.js';

export type AuditExportKind = 'diagnostics' | 'cache-hits';

export interface AuditArchive {
  blob: Blob;
  filename: string;
}

const archiveDefinitions: Record<AuditExportKind, { endpoint: string; fallbackName: string }> = {
  diagnostics: { endpoint: '/admin/export/logs', fallbackName: 'codex-pool-diagnostics.zip' },
  'cache-hits': { endpoint: '/admin/export/cache-hits', fallbackName: 'codex-pool-cache-hits.zip' },
};

export function filenameFromDisposition(value: unknown): string {
  const raw = String(value || '');
  const utf8 = raw.match(/filename\*=UTF-8''([^;]+)/i);
  if (utf8?.[1]) {
    const encoded = utf8[1].trim().replace(/^"|"$/g, '');
    try {
      return decodeURIComponent(encoded);
    } catch {
      return encoded;
    }
  }
  const plain = raw.match(/filename=([^;]+)/i);
  return plain?.[1]?.trim().replace(/^"|"$/g, '') || '';
}

export async function fetchAuditArchive(kind: AuditExportKind): Promise<AuditArchive> {
  const definition = archiveDefinitions[kind];
  const response = await api.get(definition.endpoint, { responseType: 'blob' });
  const filename = filenameFromDisposition(response.headers?.['content-disposition']) || definition.fallbackName;
  const blob = response.data instanceof Blob
    ? response.data
    : new Blob([response.data], { type: 'application/zip' });
  return { blob, filename };
}
