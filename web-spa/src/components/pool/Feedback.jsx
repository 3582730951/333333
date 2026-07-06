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
      }, Number(item.duration) ? Number(item.duration) * 1000 : 4200);
    };
    window.addEventListener(TOAST_EVENT, onToast);
    return () => window.removeEventListener(TOAST_EVENT, onToast);
  }, []);
  const close = (id) => setItems((value) => value.filter((entry) => entry.id !== id));
  const iconFor = (type) => {
    if (type === 'success') return '✓';
    if (type === 'error') return '!';
    if (type === 'warning') return '!';
    return 'i';
  };
  return (
    <div className="pool-toast-viewport" aria-relevant="additions">
      {items.map((item) => (
        <div
          key={item.id}
          className={cx('pool-toast', `pool-toast--${item.type}`)}
          role={item.type === 'error' ? 'alert' : 'status'}
          aria-live={item.type === 'error' ? 'assertive' : 'polite'}
        >
          <span className="pool-toast__icon" aria-hidden="true">{iconFor(item.type)}</span>
          <div className="pool-toast__body">
            {item.title ? <strong className="pool-toast__title">{item.title}</strong> : null}
            <div className="pool-toast-text">{item.content}</div>
          </div>
          <Button className="pool-toast__close" theme="borderless" aria-label="关闭通知" onClick={() => close(item.id)}>×</Button>
        </div>
      ))}
    </div>
  );
}

export function Spin({ size, className, spinning, ...props }) {
  return <span className={cx('pool-spinner', size === 'large' ? 'pool-spinner--large' : '', className)} role="status" aria-label="正在加载" {...props} />;
}

export function Banner({ type = 'info', title, description, children, closeIcon, actions, onClose, className, ...props }) {
  return (
    <div className={cx('pool-banner', type ? `pool-banner--${type}` : '', className)} role={type === 'danger' ? 'alert' : 'status'} {...props}>
      <div>
        {title ? <div className="pool-text-strong">{title}</div> : null}
        {description ? <div className="pool-text-secondary">{description}</div> : null}
        {children}
        {actions?.length ? <div className="pool-banner__actions">{actions}</div> : null}
      </div>
      {closeIcon}
      {onClose ? <Button theme="borderless" aria-label="关闭提示" onClick={onClose}>×</Button> : null}
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
