import React, { useEffect, useRef, useState } from 'react';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';

import { Button } from './Button.jsx';
import { ConfirmDialog } from './Dialog.jsx';
import { MoreHorizontal } from './icons.jsx';
import { cancelBrowserAnimationFrame, requestBrowserAnimationFrame } from '../../lib/browserLifecycle.js';

export function ActionMenu({ items = [], label = '更多操作' }) {
  const [confirmItem, setConfirmItem] = useState(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const dialogFrameRef = useRef(null);
  useEffect(() => () => cancelBrowserAnimationFrame(dialogFrameRef.current), []);
  return (
    <>
      <DropdownMenu.Root modal={false} open={menuOpen} onOpenChange={setMenuOpen}>
        <DropdownMenu.Trigger asChild>
          <Button theme="borderless" icon={<MoreHorizontal />} aria-label={label} />
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content className="pool-account-menu" align="end">
            {items.map((item) => (
              <DropdownMenu.Item
                key={item.label}
                className="pool-account-menu-item"
                disabled={item.disabled}
                onSelect={() => {
                  setMenuOpen(false);
                  cancelBrowserAnimationFrame(dialogFrameRef.current);
                  if (item.confirm) {
                    dialogFrameRef.current = requestBrowserAnimationFrame(() => {
                      dialogFrameRef.current = null;
                      setConfirmItem(item);
                    });
                    return;
                  }
                  dialogFrameRef.current = requestBrowserAnimationFrame(() => {
                    dialogFrameRef.current = null;
                    void item.onSelect?.();
                  });
                }}
              >
                {item.icon}
                <span className={item.destructive ? 'pool-danger-text' : ''}>{item.label}</span>
              </DropdownMenu.Item>
            ))}
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
      {confirmItem ? (
        <ConfirmDialog
          open
          destructive={confirmItem.destructive}
          title={confirmItem.confirm.title}
          description={confirmItem.confirm.description}
          confirmText={confirmItem.confirm.confirmText}
          cancelText={confirmItem.confirm.cancelText}
          onCancel={() => setConfirmItem(null)}
          onConfirm={() => {
            const action = confirmItem.onSelect;
            setConfirmItem(null);
            void action?.();
          }}
        />
      ) : null}
    </>
  );
}

export default ActionMenu;
