import React from 'react';
import { Toast, Typography } from '@douyinfe/semi-ui';
import { errMsg, errRequestID } from '../api.js';
import RequestIDLine from './RequestIDLine.jsx';

export function errorToastMessage(error, prefix = '') {
  const message = errMsg(error);
  return prefix ? `${prefix}: ${message}` : message;
}

export function showErrorToast(error, options = {}) {
  const message = errorToastMessage(error, options.prefix);
  const requestID = errRequestID(error);
  if (!requestID) {
    Toast.error(message);
    return;
  }
  Toast.error({
    content: (
      <div className="pool-error-toast">
        <Typography.Text type="danger">{message}</Typography.Text>
        <RequestIDLine requestID={requestID} compact />
      </div>
    ),
    duration: options.duration ?? 6,
  });
}
