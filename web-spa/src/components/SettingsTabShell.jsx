import React from 'react';
import { Banner, Button, Spin, Tag, Typography } from './pool/index.jsx';
import { IconUndo } from './pool/icons.jsx';
import LoadErrorBanner from './LoadErrorBanner.jsx';

const initialTabSpinStyle = { width: '100%', marginTop: 80 };

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

function SavedDiffPanel({ diffs, onUndo, undoLoading = false, onClose }) {
  if (!diffs || diffs.length === 0) return null;
  const actions = [
    onUndo ? <Button key="undo" size="small" icon={<IconUndo />} loading={undoLoading} onClick={onUndo}>撤销</Button> : null,
    <Button key="close" size="small" theme="borderless" onClick={onClose}>关闭</Button>,
  ].filter(Boolean);
  return (
    <Banner
      type="success"
      title={`已保存 ${diffs.length} 项变更`}
      description={
        <div style={{ maxHeight: 120, overflow: 'auto', fontSize: 12, fontFamily: 'monospace', lineHeight: 1.6 }}>
          {diffs.map((d, i) => (
            <div key={i}>
              [{d.section}] <b>{d.key}</b>: {JSON.stringify(d.old_value)} → {JSON.stringify(d.new_value)}
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
  children,
}) {
  if (loading && !lastRefresh) return <Spin spinning style={initialTabSpinStyle} />;
  return (
    <div className={className}>
      {toolbar ? <div className="pool-toolbar">{toolbar}</div> : null}
      <LoadErrorBanner error={error} onRetry={onRetry} />
      <SavedDiffPanel diffs={diffs} onUndo={onUndo} undoLoading={undoLoading} onClose={onClearDiffs} />
      {settingsErrorTitle ? (
        <SettingsErrorBanner title={settingsErrorTitle} section={settingsErrorSection} errors={settingsErrors} />
      ) : null}
      {children}
    </div>
  );
}
