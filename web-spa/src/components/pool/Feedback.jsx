import React, { useEffect, useMemo, useState } from 'react';
import * as TooltipPrimitive from '@radix-ui/react-tooltip';
import * as AvatarPrimitive from '@radix-ui/react-avatar';

import { Button } from './Button.jsx';

const TOAST_EVENT = 'pool-ui-toast';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function normalizeContent(content) {
  if (typeof content === 'string') return { content };
  if (content && typeof content === 'object') return content;
  return { content: String(content ?? '') };
}

function pushToast(type, message) {
  if (typeof window === 'undefined') return;
  window.dispatchEvent(new CustomEvent(TOAST_EVENT, { detail: { id: `${Date.now()}:${Math.random()}`, type, ...normalizeContent(message) } }));
}

export const Toast = {
  success: (message) => pushToast('success', message),
  error: (message) => pushToast('error', message),
  warning: (message) => pushToast('warning', message),
  info: (message) => pushToast('info', message),
};

export function ToastViewport() {
  const [items, setItems] = useState([]);
  useEffect(() => {
    const onToast = (event) => {
      const item = event.detail;
      setItems((value) => [...value, item].slice(-5));
      window.setTimeout(() => {
        setItems((value) => value.filter((entry) => entry.id !== item.id));
      }, 4200);
    };
    window.addEventListener(TOAST_EVENT, onToast);
    return () => window.removeEventListener(TOAST_EVENT, onToast);
  }, []);
  return (
    <div className="pool-toast-viewport" aria-live="polite" aria-relevant="additions">
      {items.map((item) => (
        <div key={item.id} className={cx('pool-toast', `pool-toast--${item.type}`)}>
          {item.title ? <strong>{item.title}</strong> : null}
          <div className="pool-toast-text">{item.content}</div>
        </div>
      ))}
    </div>
  );
}

export function Spin({ size }) {
  return <span className={cx('pool-spinner', size === 'large' ? 'pool-spinner--large' : '')} role="status" aria-label="loading" />;
}

export function Banner({ type = 'info', title, description, children, closeIcon, className, ...props }) {
  return (
    <div className={cx('pool-banner', type ? `pool-banner--${type}` : '', className)} role={type === 'danger' ? 'alert' : 'status'} {...props}>
      <div>
        {title ? <div className="pool-text-strong">{title}</div> : null}
        {description ? <div className="pool-text-secondary">{description}</div> : null}
        {children}
      </div>
      {closeIcon}
    </div>
  );
}

export function Tag({ children, color = 'grey', size, className, ...props }) {
  return (
    <span className={cx('pool-tag', color ? `pool-tag--${color}` : '', size ? `pool-tag--${size}` : '', className)} {...props}>
      {children}
    </span>
  );
}

export function Space({ children, className, vertical = false, wrap = false, spacing, style, ...props }) {
  const domProps = props;
  return (
    <span
      className={cx(vertical ? 'pool-stack' : 'pool-inline', wrap ? 'pool-inline--wrap' : '', className)}
      style={{ ...(spacing !== undefined ? { gap: typeof spacing === 'number' ? `${spacing}px` : spacing } : {}), ...(style || {}) }}
      {...domProps}
    >
      {children}
    </span>
  );
}

export function Divider({ margin = '12px 0' }) {
  return <div role="separator" style={{ height: 1, background: 'var(--pool-border)', margin }} />;
}

export function Tooltip({ content, children }) {
  if (!content) return children;
  return (
    <TooltipPrimitive.Provider delayDuration={250}>
      <TooltipPrimitive.Root>
        <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
        <TooltipPrimitive.Portal>
          <TooltipPrimitive.Content className="pool-toast" sideOffset={6}>
            {content}
            <TooltipPrimitive.Arrow />
          </TooltipPrimitive.Content>
        </TooltipPrimitive.Portal>
      </TooltipPrimitive.Root>
    </TooltipPrimitive.Provider>
  );
}

function Text({ children, type, strong, link, size, className, onClick, ellipsis, style, ...props }) {
  const content = (
    <span
      className={cx(
        type === 'danger' ? 'pool-danger-text' : '',
        type === 'secondary' || type === 'tertiary' || type === 'quaternary' ? 'pool-text-tertiary' : '',
        strong ? 'pool-text-strong' : '',
        ellipsis ? 'pool-text-clamp' : '',
        className,
      )}
      style={style}
      onClick={onClick}
      {...props}
    >
      {children}
    </span>
  );
  if (link) {
    return (
      <button type="button" className="pool-table-sort" onClick={onClick} style={style}>
        {children}
      </button>
    );
  }
  return content;
}

function Title({ children, heading = 4, className, style, ...props }) {
  const TagName = `h${Math.min(6, Math.max(1, Number(heading) || 4))}`;
  return <TagName className={cx('pool-page-title', className)} style={style} {...props}>{children}</TagName>;
}

export const Typography = { Text, Title };

export function Avatar({ children, size, className, style, ...props }) {
  const resolvedStyle = useMemo(() => style, [style]);
  return (
    <AvatarPrimitive.Root className={cx('pool-avatar', size ? `pool-avatar--${size}` : '', className)} style={resolvedStyle} {...props}>
      <AvatarPrimitive.Fallback>{children}</AvatarPrimitive.Fallback>
    </AvatarPrimitive.Root>
  );
}

export function LocaleProvider({ children }) {
  return children;
}

export { Button };
