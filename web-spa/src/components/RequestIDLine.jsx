import React from 'react';
import { Button, Toast, Typography } from './pool/index.jsx';
import { IconCopy } from './pool/icons.jsx';
import { writeClipboard } from '../lib/browserClipboard.js';

export async function copyRequestID(requestID) {
  if (await writeClipboard(requestID)) {
    Toast.success('已复制请求 ID');
    return;
  }
  Toast.error('复制失败，请手动选择请求 ID');
}

export default function RequestIDLine({ requestID, compact = false }) {
  if (!requestID) return null;
  return (
    <Typography.Text
      type="tertiary"
      size="small"
      className={['pool-request-id', compact ? 'is-compact' : ''].filter(Boolean).join(' ')}
    >
      请求 ID: <span className="pool-mono">{requestID}</span>
      <Button
        size="small"
        theme="borderless"
        icon={<IconCopy />}
        onClick={() => copyRequestID(requestID)}
        aria-label="复制请求 ID"
      />
    </Typography.Text>
  );
}
