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
