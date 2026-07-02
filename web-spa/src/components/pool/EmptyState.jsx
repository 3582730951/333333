import React from 'react';
import { Inbox } from './icons.jsx';

export function EmptyState({ title = '暂无数据', desc, action, icon }) {
  return (
    <div className="pool-empty">
      <div className="pool-empty-icon">{icon || <Inbox />}</div>
      <div className="pool-empty-title">{title}</div>
      {desc ? <div className="pool-text-tertiary">{desc}</div> : null}
      {action}
    </div>
  );
}

export function LoadingState({ title = '加载中' }) {
  return <div className="pool-empty"><span className="pool-spinner" /><div>{title}</div></div>;
}

export function ErrorState({ title = '加载失败', action }) {
  return <div className="pool-empty"><div className="pool-empty-title">{title}</div>{action}</div>;
}

export default EmptyState;
