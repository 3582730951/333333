import React, { forwardRef } from 'react';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

export const Button = forwardRef(function Button({
  children,
  icon,
  loading = false,
  disabled = false,
  theme,
  type,
  size,
  block,
  className,
  htmlType = 'button',
  onClick,
  'aria-label': ariaLabel,
  title,
  ...props
}, ref) {
  const variant = theme === 'solid' || type === 'primary'
    ? 'primary'
    : type === 'danger'
      ? 'danger'
      : theme === 'borderless'
        ? 'borderless'
        : theme === 'light'
          ? 'light'
          : theme === 'outline' || type === 'tertiary'
            ? 'outline'
            : 'default';
  const iconOnly = !!icon && !children;
  return (
    <button
      type={htmlType}
      className={cx(
        'pool-button',
        `pool-button--${variant}`,
        size ? `pool-button--${size}` : '',
        block ? 'pool-button--block' : '',
        iconOnly ? 'pool-icon-button pool-button--icon-only' : '',
        className,
      )}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      aria-label={ariaLabel || (iconOnly ? title : undefined)}
      title={title}
      onClick={onClick}
      ref={ref}
      {...props}
    >
      {loading ? <span className="pool-spinner" aria-hidden="true" /> : icon ? <span className="pool-button__icon">{icon}</span> : null}
      {children ? <span className="pool-button__label">{children}</span> : null}
    </button>
  );
});

Button.displayName = 'Button';

export function IconButton({ icon, label, ...props }) {
  return <Button icon={icon} aria-label={label || props['aria-label']} title={props.title || label} {...props} />;
}

export default Button;
