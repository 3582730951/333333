import { del, get, patch, post } from '../../../api.js';

function rowsOf(value: any): any[] {
  if (Array.isArray(value)) return value;
  if (value && Array.isArray(value.connections)) return value.connections;
  return [];
}

export async function fetchSub2APIHubConnections(signal?: AbortSignal) {
  const response = await get('/admin/sub2api-hub/connections', undefined, { signal });
  return {
    connections: rowsOf(response),
    global_enabled: Boolean(response?.global_enabled),
  };
}

export async function createSub2APIHubConnection(input: Record<string, unknown>) {
  return post('/admin/sub2api-hub/connections', input);
}

export async function updateSub2APIHubConnection(id: string, input: Record<string, unknown>) {
  return patch(`/admin/sub2api-hub/connections/${encodeURIComponent(id)}`, input);
}

export async function rotateSub2APIHubKey(id: string) {
  return post(`/admin/sub2api-hub/connections/${encodeURIComponent(id)}/rotate-key`, {});
}

export async function testSub2APIHubConnection(id: string) {
  return post(`/admin/sub2api-hub/connections/${encodeURIComponent(id)}/test`, {});
}

export async function revokeSub2APIHubConnection(id: string) {
  return del(`/admin/sub2api-hub/connections/${encodeURIComponent(id)}`);
}

export async function setSub2APIHubEnabled(enabled: boolean) {
  return post('/admin/settings-center', [{ section: 'config', values: { sub2api_hub_compat_v1: Boolean(enabled) } }]);
}
