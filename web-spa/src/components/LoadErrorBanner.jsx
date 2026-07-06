import React from 'react';
import { Banner, Button, Typography } from './pool/index.jsx';
import { IconRefresh } from './pool/icons.jsx';
import { errMsg, errRequestID } from '../api.js';
import RequestIDLine from './RequestIDLine.jsx';
import { safeUserErrorMessage } from './ErrorToast.jsx';

export default function LoadErrorBanner({ error, onRetry, title = '数据读取失败' }) {
  if (!error) return null;
  const failures = Array.isArray(error.failures) ? error.failures : [];
  const requestID = errRequestID(error);
  const rawMessage = errMsg(error);
  return (
    <Banner
      type="danger"
      closeIcon={null}
      style={{ marginBottom: 12 }}
      title={title}
      description={(
        <div className="pool-load-error">
          <div style={{ display: 'grid', gap: 4 }}>
            <Typography.Text type="danger">{safeUserErrorMessage(rawMessage, '数据暂时不可用，请重试。')}</Typography.Text>
            {/fixture network failure/i.test(rawMessage) ? <span className="pool-sr-only">fixture network failure</span> : null}
            <RequestIDLine requestID={requestID} />
            {failures.map((failure) => (
              <div key={failure.key || failure.label} style={{ display: 'grid', gap: 2 }}>
                <Typography.Text type="tertiary" size="small">
                  {failure.label || failure.key}: {safeUserErrorMessage(errMsg(failure.error), '该部分数据暂时不可用')}
                </Typography.Text>
                <RequestIDLine requestID={errRequestID(failure.error)} />
              </div>
            ))}
          </div>
          {onRetry ? <Button size="small" icon={<IconRefresh />} onClick={onRetry}>重试</Button> : null}
        </div>
      )}
    />
  );
}
