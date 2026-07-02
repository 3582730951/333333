import React, { useMemo } from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import * as AlertDialogPrimitive from '@radix-ui/react-alert-dialog';

import { Button } from './Button.jsx';
import { X } from './icons.jsx';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function useOpenProps({ open, visible, onOpenChange, onCancel, onClose }) {
  const isOpen = open ?? visible ?? false;
  const setOpen = (next) => {
    onOpenChange?.(next);
    if (!next) {
      onCancel?.();
      onClose?.();
    }
  };
  return [isOpen, setOpen];
}

export function Modal({
  open,
  visible,
  onOpenChange,
  onCancel,
  onOk,
  confirmLoading,
  okText = '确定',
  cancelText = '取消',
  title,
  children,
  footer,
  width,
  maskClosable = true,
  className,
  ...props
}) {
  const [isOpen, setOpen] = useOpenProps({ open, visible, onOpenChange, onCancel });
  const cssVars = useMemo(() => ({ '--pool-modal-width': typeof width === 'number' ? `${width}px` : width }), [width]);
  return (
    <DialogPrimitive.Root open={isOpen} onOpenChange={setOpen}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="pool-modal-overlay" onClick={maskClosable ? undefined : (event) => event.preventDefault()} />
        <DialogPrimitive.Content className={cx('pool-modal-content', className)} style={cssVars} {...props}>
          <div className="pool-modal-header">
            <DialogPrimitive.Title className="pool-modal-title">{title}</DialogPrimitive.Title>
            <DialogPrimitive.Close asChild>
              <Button theme="borderless" icon={<X />} aria-label="关闭" />
            </DialogPrimitive.Close>
          </div>
          <div className="pool-modal-body">{children}</div>
          {footer !== null ? (
            <div className="pool-modal-footer">
              {footer || (
                <>
                  <Button onClick={() => setOpen(false)} disabled={confirmLoading}>{cancelText}</Button>
                  <Button theme="solid" loading={confirmLoading} onClick={onOk}>{okText}</Button>
                </>
              )}
            </div>
          ) : null}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

export function Drawer({ open, visible, onOpenChange, onCancel, onClose, title, children, width = 560, className, footer, ...props }) {
  const [isOpen, setOpen] = useOpenProps({ open, visible, onOpenChange, onCancel, onClose });
  const cssVars = useMemo(() => ({ '--pool-drawer-width': typeof width === 'number' ? `${width}px` : width }), [width]);
  return (
    <DialogPrimitive.Root open={isOpen} onOpenChange={setOpen}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="pool-drawer-overlay" />
        <DialogPrimitive.Content className={cx('pool-drawer-content', className)} style={cssVars} {...props}>
          <div className="pool-drawer-header">
            <DialogPrimitive.Title className="pool-drawer-title">{title}</DialogPrimitive.Title>
            <DialogPrimitive.Close asChild>
              <Button theme="borderless" icon={<X />} aria-label="关闭" />
            </DialogPrimitive.Close>
          </div>
          <div className="pool-drawer-body">{children}</div>
          {footer ? <div className="pool-drawer-footer">{footer}</div> : null}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

export function ConfirmDialog({ open, title, description, confirmText = '确认', cancelText = '取消', destructive, onConfirm, onCancel, children }) {
  const trigger = children ? <AlertDialogPrimitive.Trigger asChild>{children}</AlertDialogPrimitive.Trigger> : null;
  return (
    <AlertDialogPrimitive.Root open={open}>
      {trigger}
      <AlertDialogPrimitive.Portal>
        <AlertDialogPrimitive.Overlay className="pool-modal-overlay" />
        <AlertDialogPrimitive.Content className="pool-modal-content">
          <div className="pool-modal-header">
            <AlertDialogPrimitive.Title className="pool-modal-title">{title}</AlertDialogPrimitive.Title>
          </div>
          <div className="pool-modal-body">{description}</div>
          <div className="pool-modal-footer">
            <AlertDialogPrimitive.Cancel asChild>
              <Button onClick={onCancel}>{cancelText}</Button>
            </AlertDialogPrimitive.Cancel>
            <AlertDialogPrimitive.Action asChild>
              <Button theme={destructive ? undefined : 'solid'} type={destructive ? 'danger' : undefined} onClick={onConfirm}>{confirmText}</Button>
            </AlertDialogPrimitive.Action>
          </div>
        </AlertDialogPrimitive.Content>
      </AlertDialogPrimitive.Portal>
    </AlertDialogPrimitive.Root>
  );
}
