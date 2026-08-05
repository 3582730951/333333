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
  onClick: (event: { itemKey: string; group?: boolean }) => void;
}>;

const items: NavEntry[] = [{
  itemKey: 'group:access',
  text: 'Access',
  items: [{ itemKey: '/providers', text: 'Providers' }],
}];

describe('pool navigation groups', () => {
  it('uses a heading-like label when expanded and an actionable control when collapsed', () => {
    const onClick = vi.fn();
    const { rerender } = render(<TypedNav items={items} selectedKeys={[]} isCollapsed={false} onClick={onClick} />);
    expect(screen.queryByRole('button', { name: 'Access' })).not.toBeInTheDocument();
    const heading = screen.getByRole('heading', { name: 'Access', level: 2 });
    const group = screen.getByRole('group', { name: 'Access' });
    expect(heading.closest('section')).toHaveAttribute('aria-labelledby', heading.id);
    expect(group).toHaveAttribute('aria-labelledby', heading.id);
    expect(screen.getByRole('button', { name: 'Providers' })).toBeInTheDocument();

    rerender(<TypedNav items={items} selectedKeys={[]} isCollapsed onClick={onClick} />);
    fireEvent.click(screen.getByRole('button', { name: 'Access' }));
    expect(onClick).toHaveBeenCalledWith({ itemKey: 'group:access', group: true });
  });
});
