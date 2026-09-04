import React from 'react';
import { Banner, Button, LoadingState, Tag, Typography } from './pool/index.jsx';
import { IconUndo } from './pool/icons.jsx';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import { t } from '../lib/i18n.js';

function settingsErrorEntries(errors) {
  if (!errors || typeof errors !== 'object') return [];
  return Object.entries(errors)
    .filter(([, message]) => typeof message === 'string' && message.trim())
    .map(([key, message]) => ({ key, message }));
}

function SettingsErrorBanner({ title, section, errors }) {
  const entries = settingsErrorEntries(errors || section?.settings_errors);
  if (entries.length === 0) return null;
  return (
    <Banner
      type="danger"
      closeIcon={null}
      title={title}
      description={
        <div style={{ display: 'grid', gap: 4 }}>
          {entries.map((item) => (
            <div key={item.key}>
              <Tag size="small" color="red">{item.key}</Tag>
              <Typography.Text size="small" style={{ marginLeft: 6 }}>{item.message}</Typography.Text>
            </div>
          ))}
        </div>
      }
      style={{ marginBottom: 12 }}
    />
  );
}

const SENSITIVE_DIFF_PART = /(?:^|_)(?:api_key|password|passwd|secret|token|authorization|credential|private_key|client_id|username|email|otp_url)(?:_|$)/;

function isSensitiveDiffKey(key) {
  const normalized = String(key || '')
    .replace(/([a-z0-9])([A-Z])/g, '$1_$2')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '_');
  return SENSITIVE_DIFF_PART.test(normalized);
}

function redactDiffValue(value, key, depth = 0) {
  if (isSensitiveDiffKey(key)) return value == null || value === '' ? '(未设置)' : '••••';
  if (depth >= 4) return '[…]';
  if (Array.isArray(value)) return value.map((item) => redactDiffValue(item, '', depth + 1));
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([childKey, childValue]) => (
      [childKey, redactDiffValue(childValue, childKey, depth + 1)]
    )));
  }
  return value;
}

export function formatSavedDiffValue(key, value) {
  try {
    const encoded = JSON.stringify(redactDiffValue(value, key));
    if (encoded === undefined) return 'null';
    return encoded.length > 240 ? `${encoded.slice(0, 237)}…` : encoded;
  } catch {
    return '"[unavailable]"';
  }
}

function SavedDiffPanel({ diffs, onUndo, undoLoading = false, onClose }) {
  if (!diffs || diffs.length === 0) return null;
  const actions = [
    onUndo ? <Button key="undo" size="small" icon={<IconUndo />} loading={undoLoading} onClick={onUndo}>{t('settings.undo')}</Button> : null,
    <Button key="close" size="small" theme="borderless" onClick={onClose}>{t('common.close')}</Button>,
  ].filter(Boolean);
  return (
    <Banner
      type="success"
      title={`${t('settings.saved_prefix')} ${diffs.length} ${t('settings.saved_changes_suffix')}`}
      description={
        <div style={{ maxHeight: 120, overflow: 'auto', fontSize: 'var(--pool-type-caption)', fontFamily: 'monospace', lineHeight: 1.6 }}>
          {diffs.map((d, i) => (
            <div key={i}>
              [{d.section}] <b>{d.key}</b>: {formatSavedDiffValue(d.key, d.old_value)} → {formatSavedDiffValue(d.key, d.new_value)}
            </div>
          ))}
        </div>
      }
      actions={actions}
      style={{ marginBottom: 16 }}
      onClose={onClose}
    />
  );
}

export default function SettingsTabShell({
  loading,
  lastRefresh,
  error,
  onRetry,
  toolbar = null,
  diffs = null,
  onUndo,
  undoLoading = false,
  onClearDiffs,
  settingsErrorTitle = '',
  settingsErrorSection,
  settingsErrors,
  className = '',
  toolbarClassName = '',
  children,
}) {
  if (loading && !lastRefresh) return <LoadingState title={t('settings.loading')} />;
  if (error && !lastRefresh) return <LoadErrorBanner error={error} onRetry={onRetry} title={settingsErrorTitle || undefined} />;
  return (
    <div className={className}>
      {toolbar ? <div className={['pool-toolbar', toolbarClassName].filter(Boolean).join(' ')}>{toolbar}</div> : null}
      <LoadErrorBanner error={error} onRetry={onRetry} title={error ? t('settings.refresh_stale') : undefined} />
      <SavedDiffPanel diffs={diffs} onUndo={onUndo} undoLoading={undoLoading} onClose={onClearDiffs} />
      {settingsErrorTitle ? (
        <SettingsErrorBanner title={settingsErrorTitle} section={settingsErrorSection} errors={settingsErrors} />
      ) : null}
      {children}
    </div>
  );
}
