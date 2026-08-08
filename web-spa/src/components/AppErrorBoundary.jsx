import React from 'react';
import { Button, Space, Typography } from './pool/index.jsx';
import { IconHome, IconRefresh } from './pool/icons.jsx';
import { currentAssetSignature } from '../lib/assetSignature.js';
import { errRequestID } from '../api.js';
import { dispatchBrowserEvent } from '../lib/browserEvents.js';
import {
  assignLocation,
  browserPathname,
  isSameOriginURL,
  reloadPage,
} from '../lib/browserNavigation.js';
import { browserUserAgent, postJSONKeepalive } from '../lib/browserNetwork.js';
import { getSessionItem, removeSessionItem, setSessionItem } from '../lib/browserStorage.js';
import RequestIDLine from './RequestIDLine.jsx';

const reloadKey = 'pool_chunk_reload_at';
const updateEventName = 'pool-update-available';
const globalHandlersDisposerKey = '__pool_global_error_handlers_disposer__';
const chunkErrorPatterns = [
  /ChunkLoadError/i,
  /Loading chunk \d+ failed/i,
  /Failed to fetch dynamically imported module/i,
  /Importing a module script failed/i,
  /Unable to preload CSS/i,
  /vite:preloadError/i,
];
const errorReportWindowMs = 15000;
const maxErrorReportKeys = 64;
const reportedErrorKeys = new Map();

export function isChunkLoadError(error) {
  const text = `${error?.name || ''} ${error?.message || ''}`;
  return chunkErrorPatterns.some((pattern) => pattern.test(text));
}

function resourceURLFromErrorEvent(event) {
  const target = event?.target;
  if (!target || target === window) return '';
  return target.currentSrc || target.src || target.href || '';
}

function isConsoleAssetURL(url) {
  if (!url) return false;
  try {
    if (!isSameOriginURL(url)) return false;
    const parsed = new URL(url, 'https://console.local');
    return /^\/console\/assets\/.+\.(?:js|css)$/.test(parsed.pathname);
  } catch {
    return /\/console\/assets\/.+\.(?:js|css)(?:[?#]|$)/.test(String(url));
  }
}

function errorFromWindowEvent(event) {
  if (event?.error) return event.error;
  const resourceURL = resourceURLFromErrorEvent(event);
  const message = resourceURL
    ? `Failed to load application asset: ${resourceURL}`
    : String(event?.message || 'Unhandled browser error event');
  const error = new Error(message);
  error.name = resourceURL ? 'ResourceLoadError' : 'WindowError';
  if (event?.filename) {
    error.stack = `${error.name}: ${message}\n    at ${event.filename}:${event.lineno || 0}:${event.colno || 0}`;
  }
  return error;
}

function canAutoReloadChunk() {
  const last = Number(getSessionItem(reloadKey, '0') || 0);
  const now = Date.now();
  if (now - last < 60000) return false;
  setSessionItem(reloadKey, String(now));
  return true;
}

function clearAutoReloadGuard() {
  removeSessionItem(reloadKey);
}

function chunkUpdateSignature(error) {
  return `chunk:${String(error?.message || error || 'vite-preload-error').slice(0, 300)}`;
}

export function notifyChunkUpdateAvailable(error) {
  dispatchBrowserEvent(updateEventName, { signature: chunkUpdateSignature(error) });
}

function pruneReportedErrorKeys(now) {
  for (const [key, ts] of reportedErrorKeys) {
    if (now - ts > errorReportWindowMs || reportedErrorKeys.size > maxErrorReportKeys) {
      reportedErrorKeys.delete(key);
    }
  }
}

export function clientErrorSignature(error, info = {}) {
  const assetSignature = String(info.assetSignature || currentAssetSignature() || '').slice(0, 600);
  const message = String(error?.message || error || 'unknown error').slice(0, 300);
  const stack = String(error?.stack || info.stack || '').slice(0, 600);
  const componentStack = String(info.componentStack || '').slice(0, 600);
  return [browserPathname(), assetSignature, message, stack, componentStack].join('|');
}

export function shouldReportClientError(error, info = {}, now = Date.now()) {
  const key = clientErrorSignature(error, info);
  pruneReportedErrorKeys(now);
  const last = reportedErrorKeys.get(key);
  if (last && now - last < errorReportWindowMs) {
    return false;
  }
  reportedErrorKeys.set(key, now);
  pruneReportedErrorKeys(now);
  return true;
}

export function reportClientError(error, info = {}) {
  const assetSignature = currentAssetSignature();
  const requestID = String(info.requestID || errRequestID(error) || '').slice(0, 200);
  const reportInfo = { ...info, assetSignature };
  if (!shouldReportClientError(error, reportInfo)) return false;
  const payload = {
    source: info.source || 'react',
    message: String(error?.message || error || 'unknown error').slice(0, 500),
    stack: String(error?.stack || info.stack || '').slice(0, 2000),
    component_stack: String(info.componentStack || '').slice(0, 2000),
    path: browserPathname(),
    asset_signature: assetSignature,
    request_id: requestID,
    resource_url: String(info.resourceURL || '').slice(0, 1000),
    user_agent: browserUserAgent(),
    occurred_at: new Date().toISOString(),
  };
  const body = JSON.stringify(payload);
  return postJSONKeepalive('/client/errors', body, (sendError) => {
    if (import.meta.env?.DEV) console.debug('client error report failed', sendError);
  });
}

function listen(target, type, handler, options) {
  target.addEventListener(type, handler, options);
  return () => target.removeEventListener(type, handler, options);
}

export function uninstallGlobalErrorHandlers() {
  if (typeof window === 'undefined') return false;
  const dispose = window[globalHandlersDisposerKey];
  if (typeof dispose !== 'function') return false;
  dispose();
  return true;
}

export function installGlobalErrorHandlers() {
  if (typeof window === 'undefined') return () => {};
  uninstallGlobalErrorHandlers();

  const disposers = [];
  const onPreloadError = (event) => {
    const error = event.payload || new Error('vite:preloadError');
    reportClientError(error, { source: 'vite.preloadError' });
    notifyChunkUpdateAvailable(error);
    if (canAutoReloadChunk()) {
      event.preventDefault();
      reloadPage();
    }
  };
  const onUnhandledRejection = (event) => {
    const reason = event.reason;
    reportClientError(reason, { source: 'unhandledrejection' });
    if (isChunkLoadError(reason)) {
      notifyChunkUpdateAvailable(reason);
      if (canAutoReloadChunk()) {
        reloadPage();
      }
    }
  };
  const onWindowError = (event) => {
    const resourceURL = resourceURLFromErrorEvent(event);
    const error = errorFromWindowEvent(event);
    reportClientError(error, { source: 'window.error', resourceURL });
    if (isChunkLoadError(error) || isConsoleAssetURL(resourceURL)) {
      notifyChunkUpdateAvailable(error);
      if (canAutoReloadChunk()) {
        reloadPage();
      }
    }
  };

  disposers.push(listen(window, 'vite:preloadError', onPreloadError));
  disposers.push(listen(window, 'unhandledrejection', onUnhandledRejection));
  disposers.push(listen(window, 'error', onWindowError, true));

  const dispose = () => {
    while (disposers.length > 0) {
      const disposeListener = disposers.pop();
      try { disposeListener(); } catch { /* ignore listener cleanup failures */ }
    }
    if (window[globalHandlersDisposerKey] === dispose) {
      delete window[globalHandlersDisposerKey];
    }
  };
  window[globalHandlersDisposerKey] = dispose;
  return dispose;
}

export default class AppErrorBoundary extends React.Component {
  constructor(props) {
    super(props);
    this.state = { error: null, chunkError: false };
  }

  componentDidUpdate(prevProps, prevState) {
    if (this.state.error && prevProps.resetKey !== this.props.resetKey) {
      this.reset();
      return;
    }
    if (this.state.error && !prevState.error && typeof this.props.onFallbackCommit === 'function') {
      this.props.onFallbackCommit();
    }
  }

  componentDidCatch(error, info) {
    const chunkError = isChunkLoadError(error);
    reportClientError(error, info);
    if (chunkError) {
      notifyChunkUpdateAvailable(error);
      if (canAutoReloadChunk()) {
        reloadPage();
        return;
      }
    }
    this.setState({ error, chunkError });
  }

  reset = () => {
    clearAutoReloadGuard();
    this.setState({ error: null, chunkError: false });
  };

  reload = () => {
    clearAutoReloadGuard();
    reloadPage();
  };

  goHome = () => {
    clearAutoReloadGuard();
    if (typeof this.props.onHome === 'function') {
      this.props.onHome();
      this.reset();
      return;
    }
    assignLocation('/console/');
  };

  render() {
    const { error, chunkError } = this.state;
    if (!error) return this.props.children;
    const isPage = this.props.variant === 'page';
    const requestID = errRequestID(error);
    const rootClass = ['pool-error-boundary', isPage ? 'is-page' : '', this.props.className || ''].filter(Boolean).join(' ');
    const panelClass = ['pool-error-panel', isPage ? 'is-page' : ''].filter(Boolean).join(' ');
    return (
      <div className={rootClass}>
        <div className={panelClass}>
          <Typography.Title heading={isPage ? 1 : 4} tabIndex={isPage ? -1 : undefined} style={{ margin: 0 }}>
            {chunkError ? '页面模块已更新' : '页面遇到错误'}
          </Typography.Title>
          <Typography.Text type="secondary">
            {chunkError
              ? '当前浏览器缓存的模块已过期，刷新后会加载最新版本。'
              : (isPage ? '错误已记录，当前页面已隔离，其他页面仍可继续使用。' : '错误已记录，可以刷新当前页面或返回控制台首页。')}
          </Typography.Text>
          <RequestIDLine requestID={requestID} />
          <Space wrap>
            {!chunkError && isPage ? <Button theme="solid" icon={<IconRefresh />} onClick={this.reset}>重试</Button> : null}
            <Button theme={chunkError || !isPage ? 'solid' : 'light'} icon={<IconRefresh />} onClick={this.reload}>刷新</Button>
            <Button icon={<IconHome />} onClick={this.goHome}>回到首页</Button>
          </Space>
        </div>
      </div>
    );
  }
}
