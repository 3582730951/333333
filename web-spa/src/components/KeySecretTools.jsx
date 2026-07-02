import React from 'react';
import { Button, Space, Tag, Toast, Tooltip, Typography } from './pool/index.jsx';
import { IconClose, IconCopy } from './pool/icons.jsx';
import { writeClipboard } from '../lib/browserClipboard.js';
import { browserOrigin } from '../lib/browserNavigation.js';

export function installCommand(secret) {
  const key = encodeURIComponent(secret || '');
  const url = `${browserOrigin()}/file/${key}`;
  return `curl -fsSL '${url}' | bash`;
}

export function maskSecret(secret) {
  if (!secret) return '';
  if (secret.length <= 16) return secret;
  return `${secret.slice(0, 7)}...${secret.slice(-8)}`;
}

export async function copyText(text, label = '已复制') {
  const value = String(text || '');
  if (await writeClipboard(value)) {
    Toast.success(label);
    return true;
  }
  Toast.error('复制失败，请手动选择文本');
  return false;
}

export function KeyCopyActions({ secret, compact = false }) {
  if (!secret) {
    return <Tag color="grey">旧 Key，需轮换后复制</Tag>;
  }
  const cmd = installCommand(secret);
  return (
    <div className={`pool-key-actions${compact ? ' is-compact' : ''}`}>
      {!compact && (
        <Tooltip content={secret}>
          <Typography.Text className="pool-mono pool-key-mask">{maskSecret(secret)}</Typography.Text>
        </Tooltip>
      )}
      <Space spacing={6} wrap>
        <Button size="small" icon={<IconCopy />} onClick={() => copyText(secret, '已复制 API Key')}>复制 Key</Button>
        <Button size="small" icon={<IconCopy />} onClick={() => copyText(cmd, '已复制安装命令')}>复制安装命令</Button>
      </Space>
    </div>
  );
}

export function KeyReveal({ secret }) {
  if (!secret) return null;
  const cmd = installCommand(secret);
  return (
    <div className="pool-key-reveal">
      <div className="pool-copy-line">
        <Typography.Text className="pool-mono pool-copy-code">{secret}</Typography.Text>
        <Button icon={<IconCopy />} onClick={() => copyText(secret, '已复制 API Key')}>复制 Key</Button>
      </div>
      <div className="pool-copy-line">
        <Typography.Text className="pool-mono pool-copy-code">{cmd}</Typography.Text>
        <Button icon={<IconCopy />} onClick={() => copyText(cmd, '已复制安装命令')}>复制安装命令</Button>
      </div>
      <Typography.Text type="tertiary" size="small">
        安装命令会访问当前 VPS，自动写入 Codex、Claude Code 网关配置，并接入 rtk。
      </Typography.Text>
    </div>
  );
}

export function KeyCreatedPanel({ secret, onClose }) {
  if (!secret) return null;
  return (
    <div className="pool-key-created">
      <div className="pool-key-created-head">
        <span className="pool-key-created-ok">✓</span>
        <strong>新 Key 已创建</strong>
        <Button size="small" theme="borderless" icon={<IconClose />} onClick={onClose} aria-label="关闭" />
      </div>
      <KeyReveal secret={secret} />
    </div>
  );
}
