import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import ResponsiveDataView from '../src/components/ResponsiveDataView';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: true, isTablet: false, isDesktop: false }),
}));

const definition = {
  desktopColumns: [],
  mobileSummary: (row: { id: string; name: string }) => `打开 ${row.name}`,
  details: (row: { id: string; name: string }) => <div>详情：{row.name}</div>,
  actions: [],
};

describe('ResponsiveDataView selection', () => {
  it('uses the refreshed row for an open detail drawer and closes it when the row disappears', () => {
    const { rerender } = render(
      <ResponsiveDataView rows={[{ id: 'one', name: '初始记录' }]} definition={definition} rowKey={(row) => row.id} />,
    );

    fireEvent.click(screen.getByRole('button', { name: '打开 初始记录' }));
    expect(screen.getByRole('dialog', { name: '详情' })).toHaveTextContent('详情：初始记录');

    rerender(<ResponsiveDataView rows={[{ id: 'one', name: '刷新后的记录' }]} definition={definition} rowKey={(row) => row.id} />);
    expect(screen.getByRole('dialog', { name: '详情' })).toHaveTextContent('详情：刷新后的记录');

    rerender(<ResponsiveDataView rows={[]} definition={definition} rowKey={(row) => row.id} />);
    expect(screen.queryByRole('dialog', { name: '详情' })).not.toBeInTheDocument();
  });

  it('keeps a legitimate falsey record selected', () => {
    const falseyDefinition = {
      desktopColumns: [],
      mobileSummary: (row: number) => `打开记录 ${row}`,
      details: (row: number) => <div>详情值：{row}</div>,
      actions: [],
    };
    render(<ResponsiveDataView rows={[0, 1]} definition={falseyDefinition} rowKey={(row) => row} />);

    fireEvent.click(screen.getByRole('button', { name: '打开记录 0' }));
    expect(screen.getByRole('dialog', { name: '详情' })).toHaveTextContent('详情值：0');
  });
});
