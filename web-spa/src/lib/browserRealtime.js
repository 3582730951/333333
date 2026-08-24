const fallbackState = {
  connecting: 0,
  open: 1,
  closed: 3,
};

function websocketState(name, fallback) {
  try {
    if (typeof WebSocket === 'undefined') return fallback;
    return WebSocket[name] ?? fallback;
  } catch {
    return fallback;
  }
}

export function createWebSocket(url) {
  try {
    if (typeof WebSocket === 'undefined') {
      return { socket: null, error: new Error('当前浏览器不支持实时日志连接') };
    }
    return { socket: new WebSocket(url), error: null };
  } catch (error) {
    return { socket: null, error };
  }
}

export function isWebSocketConnectingOrOpen(socket) {
  if (!socket) return false;
  const connecting = websocketState('CONNECTING', fallbackState.connecting);
  const open = websocketState('OPEN', fallbackState.open);
  return socket.readyState === connecting || socket.readyState === open;
}

export function isWebSocketClosed(socket) {
  if (!socket) return true;
  return socket.readyState === websocketState('CLOSED', fallbackState.closed);
}

export function closeWebSocket(socket) {
  try {
    if (!isWebSocketClosed(socket)) socket.close();
  } catch {
    // Socket teardown should never break React cleanup paths.
  }
}

// --- Server-Sent Events -----------------------------------------------------------------
//
// SSE rather than a WebSocket wherever the data only flows server -> client. It is a plain
// HTTP/1.1 response, so it needs no upgrade handshake, crosses proxies that block upgrades,
// and the browser reconnects on its own. `withCredentials` is what carries the cp_session
// cookie; note that EventSource cannot send an Authorization header at all, so a console
// authenticated only by the legacy admin Bearer token will be refused here and the caller is
// expected to fall back to polling rather than treat that as an outage.

export function createEventSource(url, { withCredentials = true } = {}) {
  try {
    if (typeof EventSource === 'undefined') {
      return { source: null, error: new Error('EventSource unsupported') };
    }
    return { source: new EventSource(url, { withCredentials }), error: null };
  } catch (error) {
    return { source: null, error };
  }
}

export function closeEventSource(source) {
  try {
    source?.close?.();
  } catch {
    // Teardown must never break a React cleanup path.
  }
}
