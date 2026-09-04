import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { Nav } from '../src/components/pool/index.jsx';

interface NavEntry {
  itemKey: string;
  text: string;
  items?: NavEntry[];
}

const TypedNav = Nav as React.ComponentType<{
  items: NavEntry[];
  selectedKeys: string[];
  isCollapsed: boolean;
  storageScope?: string;
  onClick: (event: { itemKey: string; group?: boolean }) => void;
}>;

const items: NavEntry[] = [{
  itemKey: 'group:access',
  text: 'Access',
  items: [{ itemKey: '/providers', text: 'Providers' }],
}];

describe('pool navigation groups', () => {
  it('uses a persistent native button with correct group and current-page semantics', () => {
    window.localStorage.clear();
    const onClick = vi.fn();
    const { unmount } = render(<TypedNav items={items} selectedKeys={['/providers']} isCollapsed={false} storageScope="operator@example.test" onClick={onClick} />);
    const trigger = screen.getByRole('button', { name: 'Access' });
    const group = screen.getByRole('group', { name: 'Access' });
    expect(trigger.tagName).toBe('BUTTON');
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(trigger).toHaveAttribute('aria-controls', group.id);
    expect(trigger.closest('section')).toHaveAttribute('data-nav-group', 'group:access');
    expect(group).toHaveAttribute('data-state', 'open');
    expect(screen.getByRole('button', { name: 'Providers' })).toHaveAttribute('aria-current', 'page');
    expect(trigger).not.toHaveAttribute('aria-current');

    trigger.focus();
    fireEvent.click(trigger);
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(group).toHaveAttribute('hidden');
    expect(onClick).toHaveBeenCalledWith({ itemKey: 'group:access', group: true });

    unmount();
    const persisted = render(<TypedNav items={items} selectedKeys={['/providers']} isCollapsed={false} storageScope="operator@example.test" onClick={onClick} />);
    expect(screen.getByRole('button', { name: 'Access' })).toHaveAttribute('aria-expanded', 'false');

    persisted.rerender(<TypedNav items={items} selectedKeys={['/providers']} isCollapsed storageScope="operator@example.test" onClick={onClick} />);
    const collapsedTrigger = screen.getByRole('button', { name: 'Access' });
    expect(collapsedTrigger).toHaveAttribute('aria-expanded', 'false');
    expect(document.getElementById(collapsedTrigger.getAttribute('aria-controls') || '')).toHaveAttribute('hidden');
  });
});
