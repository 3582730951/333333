import { describe, expect, it } from 'vitest';
import { providerFormValues, providerRoutesPayload } from '../src/pages/Providers.jsx';

describe('provider invocation routes', () => {
  it('adapts stored routes into an isolated editable copy', () => {
    const source = {
      id: 'relay', base_url: 'https://default.example/v1',
      routes: [{ id: 'codex', downstream_path: '/v1/responses', base_url: 'https://responses.example/v1' }],
    };
    const values = providerFormValues(source);
    expect(values.routes).toEqual(source.routes);
    values.routes[0].base_url = 'https://changed.example/v1';
    expect(source.routes[0].base_url).toBe('https://responses.example/v1');
  });

  it('writes compact typed route payloads and drops blank rows', () => {
    expect(providerRoutesPayload([
      {
        id: ' codex-edge ', downstream_path: ' /v1/responses ',
        base_url: ' https://relay.example/v1 ', upstream_protocol: ' responses ',
        transport_profile: ' codex_cli ',
      },
      { id: '', downstream_path: '  ', base_url: '' },
    ])).toEqual([{
      id: 'codex-edge', downstream_path: '/v1/responses',
      base_url: 'https://relay.example/v1', upstream_protocol: 'responses',
      transport_profile: 'codex_cli',
    }]);
  });
});
