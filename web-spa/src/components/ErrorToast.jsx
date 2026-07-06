import React from 'react';
import { Toast, Typography } from './pool/index.jsx';
import { errMsg, errRequestID } from '../api.js';
import RequestIDLine from './RequestIDLine.jsx';

const INTERNAL_ERROR_PATTERNS = [
  /admin_token/i,
  /usage_records/i,
  /fixture/i,
  /sqlite/i,
  /postgres/i,
  /sql/i,
  /stack trace/i,
];

export function safeUserErrorMessage(message, fallback = '操作未完成，请稍后重试或检查网络连接。') {
  const text = String(message || '').trim();
  if (!text) return fallback;
  if (INTERNAL_ERROR_PATTERNS.some((pattern) => pattern.test(text))) return fallback;
  return text.length > 180 ? `${text.slice(0, 177)}…` : text;
}

export function errorToastMessage(error, prefix = '') {
  const message = safeUserErrorMessage(errMsg(error));
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
