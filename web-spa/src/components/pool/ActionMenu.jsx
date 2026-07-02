import React, { useState } from 'react';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';

import { Button } from './Button.jsx';
import { ConfirmDialog } from './Dialog.jsx';
import { MoreHorizontal } from './icons.jsx';

export function ActionMenu({ items = [], label = '更多操作' }) {
  const [confirmItem, setConfirmItem] = useState(null);
  return (
    <>
      <DropdownMenu.Root>
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
                onSelect={(event) => {
                  event.preventDefault();
                  if (item.destructive && item.confirm) {
                    setConfirmItem(item);
                    return;
                  }
                  item.onSelect?.();
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
          onConfirm={async () => {
            await confirmItem.onSelect?.();
            setConfirmItem(null);
          }}
        />
      ) : null}
    </>
  );
}

export default ActionMenu;
