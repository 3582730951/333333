import { useCallback, useEffect, useState } from 'react';
import { get, isAbortError } from '../api.js';
import { abortController, abortSignal, createAbortController } from '../lib/browserAbort.js';
import { addDocumentListener, clearBrowserTimeout, isDocumentVisible, setBrowserTimeout } from '../lib/browserLifecycle.js';
import { sameOriginWebSocketURL } from '../lib/browserNavigation.js';
import { closeWebSocket, createWebSocket, isWebSocketClosed, isWebSocketConnectingOrOpen } from '../lib/browserRealtime.js';

const reconnectDelays = [1000, 2000, 5000];
const maxLogRows = 1000;

function logKey(entry) {
  if (entry.id) return `id:${entry.id}`;
  return [entry.timestamp || 0, entry.account_index ?? '', entry.level || '', entry.message || ''].join('|');
}

function mergeLogs(prev, next) {
  const seen = new Set(prev.map(logKey));
  const merged = [...prev];
  (next || []).forEach((entry) => {
    const key = logKey(entry);
    if (seen.has(key)) return;
    seen.add(key);
    merged.push(entry);
  });
  merged.sort((a, b) => ((a.timestamp || 0) - (b.timestamp || 0)) || ((a.id || 0) - (b.id || 0)));
  return merged.length > maxLogRows ? merged.slice(-maxLogRows) : merged;
}

export function lifecycleLogKey(entry, index = 0) {
  return entry.id ? logKey(entry) : `${logKey(entry)}|${index}`;
}

function newestLogID(entries) {
  return (entries || []).reduce((maxID, entry) => Math.max(maxID, Number(entry?.id || 0)), 0);
}

function lifecycleLogStreamURL(taskID, sinceID = 0) {
  const query = sinceID > 0 ? `?since_id=${encodeURIComponent(String(sinceID))}` : '';
  return sameOriginWebSocketURL(`/admin/lifecycle/tasks/${encodeURIComponent(taskID)}/stream`, query);
}

function lifecycleLogHTTPPath(taskID, sinceID = 0) {
  const query = sinceID > 0 ? `?since_id=${encodeURIComponent(String(sinceID))}` : '';
  return `/admin/lifecycle/tasks/${encodeURIComponent(taskID)}/logs${query}`;
}

export default function useLifecycleTaskLogs(taskID) {
  const [reloadKey, setReloadKey] = useState(0);
  const [logs, setLogs] = useState([]);
  const [error, setError] = useState(null);
  const [streaming, setStreaming] = useState(false);

  const reload = useCallback(() => {
    setReloadKey((key) => key + 1);
  }, []);

  useEffect(() => {
    if (!taskID) {
      setLogs([]);
      setError(null);
      setStreaming(false);
      return undefined;
    }

    let stopped = false;
    let socket = null;
    let suppressReconnectSocket = null;
    let reconnectTimer = 0;
    let fallbackTimer = 0;
    let reconnectAttempt = 0;
    let lastLogID = 0;
    const controller = createAbortController();

    setLogs([]);
    setError(null);
    setStreaming(false);

    const loadLogs = async (sinceID = 0) => {
      const data = await get(lifecycleLogHTTPPath(taskID, sinceID), undefined, { signal: abortSignal(controller) });
      if (!stopped) {
        const entries = Array.isArray(data) ? data : data?.logs || [];
        lastLogID = Math.max(lastLogID, newestLogID(entries));
        setLogs((prev) => mergeLogs(prev, entries));
      }
      return lastLogID;
    };

    const loadInitial = async () => {
      try {
        return await loadLogs(0);
      } catch (loadError) {
        if (!stopped && !isAbortError(loadError)) setError(loadError);
      }
      return lastLogID;
    };

    function scheduleFallbackPoll(delay = 0) {
      if (stopped || fallbackTimer || !isDocumentVisible()) return;
      fallbackTimer = setBrowserTimeout(async () => {
        fallbackTimer = 0;
        try {
          await loadLogs(lastLogID);
        } catch (fallbackError) {
          if (!stopped && !isAbortError(fallbackError)) setError(fallbackError);
        }
      }, delay);
    }

    function scheduleReconnect() {
      if (stopped || reconnectTimer || !isDocumentVisible()) return;
      const delay = reconnectDelays[Math.min(reconnectAttempt, reconnectDelays.length - 1)];
      reconnectAttempt += 1;
      setError(new Error(`日志流连接已断开，${Math.round(delay / 1000)} 秒后重试`));
      scheduleFallbackPoll(0);
      reconnectTimer = setBrowserTimeout(() => {
        reconnectTimer = 0;
        connect(lastLogID);
      }, delay);
    }

    function connect(sinceID = lastLogID) {
      if (stopped || !isDocumentVisible()) return;
      if (isWebSocketConnectingOrOpen(socket)) return;
      try {
        const { socket: nextSocket, error: connectError } = createWebSocket(lifecycleLogStreamURL(taskID, sinceID));
        if (!nextSocket) throw connectError || new Error('日志流连接不可用');
        socket = nextSocket;
        nextSocket.onopen = () => {
          if (!stopped) {
            reconnectAttempt = 0;
            setStreaming(true);
            setError(null);
          }
        };
        nextSocket.onmessage = (event) => {
          if (stopped) return;
          try {
            const payload = JSON.parse(event.data);
            if (payload?.error) {
              setError(new Error(payload.error.message || '日志流读取失败'));
              return;
            }
            const entries = Array.isArray(payload) ? payload : payload?.logs;
            if (Array.isArray(entries)) {
              lastLogID = Math.max(lastLogID, newestLogID(entries));
              setLogs((prev) => mergeLogs(prev, entries));
            }
          } catch (parseError) {
            setError(parseError);
          }
        };
        nextSocket.onerror = () => {
          if (!stopped) setError(new Error('日志流连接失败'));
        };
        nextSocket.onclose = () => {
          if (stopped) return;
          if (socket === nextSocket) socket = null;
          setStreaming(false);
          if (suppressReconnectSocket === nextSocket) {
            suppressReconnectSocket = null;
            return;
          }
          scheduleReconnect();
        };
      } catch (connectError) {
        setError(connectError);
        scheduleReconnect();
      }
    }

    function closeSocketForHidden() {
      if (reconnectTimer) {
        clearBrowserTimeout(reconnectTimer);
        reconnectTimer = 0;
      }
      if (fallbackTimer) {
        clearBrowserTimeout(fallbackTimer);
        fallbackTimer = 0;
      }
      if (socket && !isWebSocketClosed(socket)) {
        suppressReconnectSocket = socket;
        closeWebSocket(socket);
      }
      setStreaming(false);
    }

    function onVisibilityChange() {
      if (isDocumentVisible()) {
        connect(lastLogID);
      } else {
        closeSocketForHidden();
      }
    }

    loadInitial().then((sinceID) => {
      if (!stopped) connect(sinceID || 0);
    });
    const removeVisibility = addDocumentListener('visibilitychange', onVisibilityChange);

    return () => {
      stopped = true;
      abortController(controller);
      removeVisibility();
      if (reconnectTimer) clearBrowserTimeout(reconnectTimer);
      if (fallbackTimer) clearBrowserTimeout(fallbackTimer);
      closeWebSocket(socket);
    };
  }, [taskID, reloadKey]);

  return { logs, error, streaming, reload };
}
