import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Button, Typography } from '@douyinfe/semi-ui';
import { IconClose, IconRefresh } from '@douyinfe/semi-icons';
import { assetSignatureFromHTML, currentAssetSignature } from '../lib/assetSignature.js';
import { abortController, abortSignal, createAbortController } from '../lib/browserAbort.js';
import {
  addDocumentListener,
  addWindowListener,
  clearBrowserInterval,
  clearBrowserTimeout,
  isDocumentVisible,
  setBrowserInterval,
  setBrowserTimeout,
} from '../lib/browserLifecycle.js';
import { getLocale } from '../lib/i18n.js';
import { reloadPage } from '../lib/browserNavigation.js';
import { fetchText } from '../lib/browserNetwork.js';
import { reportClientError } from './AppErrorBoundary.jsx';

const checkIntervalMs = 60000;
const checkTimeoutMs = 10000;
const reportFailureAfter = 3;
const updateEventName = 'pool-update-available';

async function fetchLatestSignature(signal) {
  const html = await fetchText('/console/', {
    cache: 'no-store',
    credentials: 'same-origin',
    headers: { 'Cache-Control': 'no-cache' },
    signal,
  });
  return assetSignatureFromHTML(html);
}

export default function AppUpdateNotice() {
  const [pendingSignature, setPendingSignature] = useState('');
  const [dismissedSignature, setDismissedSignature] = useState('');
  const [locale, setLocale] = useState(getLocale());
  const pendingSignatureRef = useRef('');
  const dismissedSignatureRef = useRef('');
  const initialSignature = useMemo(() => currentAssetSignature(), []);
  const copy = locale === 'en'
    ? { title: 'New version available', body: 'Refresh when ready.', refresh: 'Refresh', close: 'Dismiss' }
    : { title: '新版本可用', body: '刷新后载入最新控制台。', refresh: '刷新', close: '暂不' };

  useEffect(() => {
    pendingSignatureRef.current = pendingSignature;
  }, [pendingSignature]);

  useEffect(() => {
    dismissedSignatureRef.current = dismissedSignature;
  }, [dismissedSignature]);

  useEffect(() => {
    const onLocaleChange = (event) => setLocale(event.detail || getLocale());
    return addWindowListener('pool-locale-change', onLocaleChange);
  }, []);

  useEffect(() => {
    let stopped = false;
    let checking = false;
    let activeController = null;
    let timer = 0;
    let firstCheckTimer = 0;
    let consecutiveFailures = 0;
    let failureReported = false;

    const markAvailable = (event) => {
      if (stopped) return;
      const signature = event?.detail?.signature || `manual:${Date.now()}`;
      pendingSignatureRef.current = signature;
      setPendingSignature(signature);
    };
    const check = async () => {
      const visibleUpdate = pendingSignatureRef.current && pendingSignatureRef.current !== dismissedSignatureRef.current;
      if (!isDocumentVisible() || checking || visibleUpdate) return;
      checking = true;
      const controller = createAbortController();
      activeController = controller;
      const abortTimer = controller ? setBrowserTimeout(() => abortController(controller), checkTimeoutMs) : null;
      try {
        const latest = await fetchLatestSignature(abortSignal(controller));
        consecutiveFailures = 0;
        failureReported = false;
        if (!stopped && latest && initialSignature && latest !== initialSignature) {
          pendingSignatureRef.current = latest;
          setPendingSignature(latest);
        }
      } catch (error) {
        consecutiveFailures += 1;
        if (!stopped && !failureReported && consecutiveFailures >= reportFailureAfter) {
          failureReported = true;
          reportClientError(error, {
            source: 'update.poll',
            stack: `console update polling failed ${consecutiveFailures} times`,
          });
        }
      } finally {
        clearBrowserTimeout(abortTimer);
        if (activeController === controller) activeController = null;
        checking = false;
      }
    };
    const onVisible = () => {
      if (isDocumentVisible()) check();
    };

    const removeUpdateListener = addWindowListener(updateEventName, markAvailable);
    const removeVisibilityListener = addDocumentListener('visibilitychange', onVisible);
    const removeOnlineListener = addWindowListener('online', check);
    timer = setBrowserInterval(check, checkIntervalMs);
    firstCheckTimer = setBrowserTimeout(check, 5000);

    return () => {
      stopped = true;
      clearBrowserInterval(timer);
      clearBrowserTimeout(firstCheckTimer);
      abortController(activeController);
      removeUpdateListener();
      removeVisibilityListener();
      removeOnlineListener();
    };
  }, [initialSignature]);

  if (!pendingSignature || pendingSignature === dismissedSignature) return null;
  return (
    <div className="pool-update-notice" role="status" aria-live="polite">
      <div className="pool-update-copy">
        <Typography.Text strong>{copy.title}</Typography.Text>
        <Typography.Text type="tertiary" size="small">{copy.body}</Typography.Text>
      </div>
      <Button size="small" theme="solid" icon={<IconRefresh />} onClick={reloadPage}>
        {copy.refresh}
      </Button>
      <Button
        size="small"
        theme="borderless"
        icon={<IconClose />}
        aria-label={copy.close}
        onClick={() => {
          dismissedSignatureRef.current = pendingSignature;
          setDismissedSignature(pendingSignature);
        }}
      />
    </div>
  );
}
