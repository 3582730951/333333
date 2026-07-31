import { describe, expect, it } from 'vitest';
import { parseProxyEndpoint } from '../src/lib/proxyEndpoint';

describe('residential proxy input parser', () => {
  it.each([
    ['proxy.example:1080:alice:secret', 'socks5h://alice:secret@proxy.example:1080'],
    ['alice:secret@proxy.example:1080', 'socks5h://alice:secret@proxy.example:1080'],
    ['proxy.example:1080@alice:secret', 'socks5h://alice:secret@proxy.example:1080'],
    ['socks5://alice:secret@proxy.example:1080', 'socks5://alice:secret@proxy.example:1080'],
  ])('normalizes %s', (input, endpoint) => {
    expect(parseProxyEndpoint(input, 'socks5h_proxy').endpoint).toBe(endpoint);
  });

  it('masks the password and infers an explicit SOCKS5 type', () => {
    const result = parseProxyEndpoint('socks5://alice:p%40ss@proxy.example:1080', 'http_proxy');
    expect(result).toMatchObject({
      endpoint: 'socks5://alice:p%40ss@proxy.example:1080',
      egressType: 'socks5_proxy',
      masked: 'socks5://alice:••••@proxy.example:1080',
    });
    expect(result.masked).not.toContain('p@ss');
  });
});
