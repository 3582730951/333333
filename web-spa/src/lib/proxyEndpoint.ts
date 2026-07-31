export interface ParsedProxyEndpoint {
  endpoint: string;
  egressType: 'http_proxy' | 'https_proxy' | 'socks5_proxy' | 'socks5h_proxy';
  masked: string;
}

const TYPE_SCHEME: Record<string, string> = {
  http_proxy: 'http',
  https_proxy: 'https',
  socks5_proxy: 'socks5',
  socks5h_proxy: 'socks5h',
};

const SCHEME_TYPE: Record<string, ParsedProxyEndpoint['egressType']> = {
  http: 'http_proxy',
  https: 'https_proxy',
  socks5: 'socks5_proxy',
  socks5h: 'socks5h_proxy',
};

function validPort(value: string): boolean {
  const port = Number(value);
  return /^\d+$/.test(value) && port >= 1 && port <= 65535;
}

function hostPort(value: string): { host: string; port: string } | null {
  const input = value.trim();
  const ipv6 = input.match(/^\[([^\]]+)]:(\d+)$/);
  if (ipv6 && validPort(ipv6[2])) return { host: ipv6[1], port: ipv6[2] };
  const index = input.lastIndexOf(':');
  if (index < 1) return null;
  const host = input.slice(0, index).trim();
  const port = input.slice(index + 1).trim();
  return host && validPort(port) ? { host, port } : null;
}

function credentials(value: string): { username: string; password: string } | null {
  const index = value.indexOf(':');
  if (index < 1) return null;
  return { username: value.slice(0, index).trim(), password: value.slice(index + 1) };
}

function renderHost(host: string): string {
  return host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
}

function build(
  scheme: string,
  host: string,
  port: string,
  username = '',
  password = '',
): ParsedProxyEndpoint {
  const egressType = SCHEME_TYPE[scheme];
  if (!egressType) throw new Error('仅支持 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理');
  if (!host.trim() || !validPort(port)) throw new Error('代理地址需要有效的主机名和 1–65535 端口');
  const authority = username
    ? `${encodeURIComponent(username)}:${encodeURIComponent(password)}@`
    : '';
  const endpoint = `${scheme}://${authority}${renderHost(host.trim())}:${port}`;
  const masked = `${scheme}://${username ? `${username}:••••@` : ''}${renderHost(host.trim())}:${port}`;
  return { endpoint, egressType, masked };
}

/**
 * Parses provider exports without retaining a separate plaintext credential copy.
 * Supported inputs:
 * host:port:user:pass, scheme://user:pass@host:port,
 * user:pass@host:port, and host:port@user:pass.
 */
export function parseProxyEndpoint(value: unknown, fallbackType = 'socks5h_proxy'): ParsedProxyEndpoint {
  const input = String(value || '').trim();
  if (!input) throw new Error('请粘贴代理地址');
  if (input.includes('://')) {
    let parsed: URL;
    try { parsed = new URL(input); } catch { throw new Error('代理 URL 格式不完整'); }
    const scheme = parsed.protocol.replace(':', '').toLowerCase();
    if (!parsed.hostname || !parsed.port || !['', '/'].includes(parsed.pathname) || parsed.search || parsed.hash) {
      throw new Error('代理 URL 只能包含协议、账号、主机和端口');
    }
    return build(
      scheme,
      parsed.hostname.replace(/^\[|]$/g, ''),
      parsed.port,
      decodeURIComponent(parsed.username),
      decodeURIComponent(parsed.password),
    );
  }

  const at = input.lastIndexOf('@');
  if (at >= 0) {
    const left = input.slice(0, at);
    const right = input.slice(at + 1);
    const standardHost = hostPort(right);
    if (standardHost) {
      const auth = credentials(left);
      if (!auth) throw new Error('代理账号需要 username:password');
      return build(TYPE_SCHEME[fallbackType] || 'socks5h', standardHost.host, standardHost.port, auth.username, auth.password);
    }
    const reversedHost = hostPort(left);
    const auth = credentials(right);
    if (!reversedHost || !auth) throw new Error('请检查代理主机、端口、账号和密码的顺序');
    return build(TYPE_SCHEME[fallbackType] || 'socks5h', reversedHost.host, reversedHost.port, auth.username, auth.password);
  }

  const parts = input.split(':');
  if (parts.length < 2) throw new Error('请使用 host:port:user:pass 格式');
  const host = parts.shift() || '';
  const port = parts.shift() || '';
  const username = parts.shift() || '';
  const password = parts.join(':');
  return build(TYPE_SCHEME[fallbackType] || 'socks5h', host, port, username, password);
}
