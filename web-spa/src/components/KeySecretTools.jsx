import React from 'react';
import { Button, Space, Tag, Toast, Tooltip, Typography } from './pool/index.jsx';
import { IconClose, IconCopy } from './pool/icons.jsx';
import { writeClipboard } from '../lib/browserClipboard.js';
import { browserOrigin } from '../lib/browserNavigation.js';
import { t } from '../lib/i18n.js';

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

export async function copyText(text, label = t('common.copied', '已复制')) {
  const value = String(text || '');
  if (await writeClipboard(value)) {
    Toast.success(label);
    return true;
  }
  Toast.error(t('keys.copy_failed'));
  return false;
}

export function KeyCopyActions({ secret, compact = false }) {
  if (!secret) {
    return <Tag color="grey">{t('keys.legacy_rotate')}</Tag>;
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
        <Button size="small" icon={<IconCopy />} onClick={() => copyText(secret, t('keys.copied_key'))}>{t('keys.copy_key')}</Button>
        <Button size="small" icon={<IconCopy />} onClick={() => copyText(cmd, t('keys.copied_command'))}>{t('keys.copy_command')}</Button>
      </Space>
    </div>
  );
}

export function KeyReveal({ secret }) {
  if (!secret) return null;
  const cmd = installCommand(secret);
  return (
    <div className="pool-key-reveal">
      <p className="pool-key-reveal__warning" role="status">{t('keys.one_time_warning')}</p>
      <div className="pool-copy-line">
        <Typography.Text className="pool-mono pool-copy-code">{secret}</Typography.Text>
        <Button icon={<IconCopy />} onClick={() => copyText(secret, t('keys.copied_key'))}>{t('keys.copy_key')}</Button>
      </div>
      <div className="pool-copy-line">
        <Typography.Text className="pool-mono pool-copy-code">{cmd}</Typography.Text>
        <Button icon={<IconCopy />} onClick={() => copyText(cmd, t('keys.copied_command'))}>{t('keys.copy_command')}</Button>
      </div>
      <Typography.Text type="tertiary" size="small">
        {t('keys.install_help')}
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
        <strong>{t('keys.created_panel')}</strong>
        <Button size="small" theme="borderless" icon={<IconClose />} onClick={onClose} aria-label={t('common.close')} />
      </div>
      <KeyReveal secret={secret} />
    </div>
  );
}
