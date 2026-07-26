import axios from 'axios';
import { getCookie } from './lib/browserCookies.js';
import { dispatchBrowserEvent } from './lib/browserEvents.js';
import { clearBrowserTimeout, setBrowserTimeout } from './lib/browserLifecycle.js';
import { getLocalItem, removeLocalItem, setLocalItem } from './lib/browserStorage.js';
import { normalizeApiError } from './api/errors.ts';

// Admin token (legacy Bearer) lives in safe browser storage; user sessions use the cp_session
// cookie (same-origin, sent automatically). Mutations on a session need the double-submit
// CSRF header (X-CP-CSRF = cp_csrf cookie). Admin-token requests are CSRF-exempt server-side.
const TOKEN_KEY = 'pool_admin_token';

export const getToken = () => getLocalItem(TOKEN_KEY, '');
export const setToken = (t) => setLocalItem(TOKEN_KEY, t || '');
export const clearToken = () => removeLocalItem(TOKEN_KEY);
export { getCookie };

const api = axios.create({ timeout: 30000, withCredentials: true });

api.interceptors.request.use((cfg) => {
  const t = getToken();
  if (t) cfg.headers.Authorization = `Bearer ${t}`;
  const method = (cfg.method || 'get').toLowerCase();
  if (method !== 'get' && method !== 'head') {
    const csrf = getCookie('cp_csrf');
    if (csrf) cfg.headers['X-CP-CSRF'] = csrf;
  }
  return cfg;
});

api.interceptors.response.use(
  (r) => r,
  (err) => {
    const status = err?.response?.status;
    if (status === 401 && !err?.config?.suppressUnauthorizedEvent) {
      dispatchBrowserEvent('pool-unauthorized');
    }
    return Promise.reject(err);
  }
);

// Thin helpers returning response data directly.
export const get = (url, params, config = {}) => api.get(url, { ...config, params }).then((r) => r.data);
export const post = (url, body, config = {}) => api.post(url, body, config).then((r) => r.data);
export const put = (url, body, config = {}) => api.put(url, body, config).then((r) => r.data);
export const patch = (url, body, config = {}) => api.patch(url, body, config).then((r) => r.data);
export const del = (url, body, config = {}) => api.delete(url, { ...config, data: body }).then((r) => r.data);

// Auth helpers (session-based portal + admin).
export const me = (config = {}) => get('/auth/me', undefined, config);
export const login = (email, password) => post('/auth/login', { email, password }, { suppressUnauthorizedEvent: true });
export const registerUser = (email, password, name) =>
  post('/auth/register', { email, password, name }, { suppressUnauthorizedEvent: true });
export const logout = () => post('/auth/logout', {}, { suppressUnauthorizedEvent: true });

// ── OAuth login helpers (web-login / paste-back import) ──
export const oauthStart = (provider, groupName = '') => post('/admin/oauth/start', {
  provider,
  group_name: groupName,
});
export const oauthComplete = (sessionId, redirected, label, groupName) =>
  post('/admin/oauth/complete', {
    session_id: sessionId,
    redirected,
    label,
    group_name: groupName,
  });

export const errMsg = (e) => {
  if (e?.name === 'ApiError' && typeof e?.userMessage === 'string') return e.userMessage;
  const data = e?.response?.data;
  if (typeof data?.error === 'string') return data.error;
  if (typeof data?.error?.message === 'string') return data.error.message;
  if (typeof data?.message === 'string') return data.message;
  return e?.message || String(e);
};

export const errRequestID = (e) => {
  if (e?.name === 'ApiError' && typeof e?.requestId === 'string') return e.requestId;
  const data = e?.response?.data;
  const bodyID = typeof data?.error?.request_id === 'string' ? data.error.request_id : '';
  const headerID = typeof e?.response?.headers?.['x-request-id'] === 'string' ? e.response.headers['x-request-id'] : '';
  return bodyID || headerID;
};

export const statusCode = (e) => e?.response?.status || 0;
export const isUnauthorizedError = (e) => {
  const status = statusCode(e);
  return status === 401 || status === 403;
};

export const isAbortError = (e) => normalizeApiError(e).code === 'REQUEST_ABORTED';

// ── Batch operations with progress ──
export const batchOp = async (label, ids, fn, onProgress) => {
  const results = { success: [], failed: [] };
  for (let i = 0; i < ids.length; i++) {
    try {
      await fn(ids[i]);
      results.success.push(ids[i]);
    } catch (e) {
      results.failed.push({ id: ids[i], error: errMsg(e), request_id: errRequestID(e) });
    }
    if (onProgress) onProgress(i + 1, ids.length, results);
  }
  return results;
};

// ── Debounce utility ──
export const debounce = (fn, ms) => {
  let timer;
  return (...args) => {
    clearBrowserTimeout(timer);
    timer = setBrowserTimeout(() => fn(...args), ms);
  };
};

// ── Format helpers for display ──
export const truncate = (str, len = 30) =>
  str && str.length > len ? str.slice(0, len) + '…' : str || '';

export default api;
