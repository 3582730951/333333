import React, { useState } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import OrderedEgressSelect from '../src/components/OrderedEgressSelect.jsx';

const OrderedSelect = OrderedEgressSelect as any;

const options = [
  { label: 'Primary proxy', value: 'primary' },
  { label: 'Standby proxy', value: 'standby' },
];

describe('ordered egress selection', () => {
  it('announces primary/standby order and supports keyboard reordering', () => {
    const onChange = vi.fn();
    render(<OrderedSelect value={['primary', 'standby']} onChange={onChange} options={options} />);

    const order = screen.getByRole('list', { name: '出口故障转移顺序' });
    expect(order).toHaveTextContent('主出口');
    expect(order).toHaveTextContent('备用 1');
    fireEvent.keyDown(screen.getByRole('button', { name: /Standby proxy，使用上下方向键调整顺序/ }), { key: 'ArrowUp' });
    expect(onChange).toHaveBeenCalledWith(['standby', 'primary']);
  });

  it('exposes combobox state and skips disabled options during keyboard selection', async () => {
    const onChange = vi.fn();
    function StatefulSelect() {
      const [value, setValue] = useState<string[]>([]);
      return (
        <OrderedSelect
          value={value}
          onChange={(next: string[]) => { setValue(next); onChange(next); }}
          options={[
            { label: 'Unavailable', value: 'disabled', disabled: true },
            { label: 'Available', value: 'available' },
          ]}
        />
      );
    }

    render(<StatefulSelect />);
    const trigger = screen.getByRole('combobox', { name: '选择出口' });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'true'));

    const search = await screen.findByRole('combobox', { name: '选择出口搜索' });
    expect(search.getAttribute('aria-activedescendant')).toMatch(/option-1$/);
    fireEvent.keyDown(search, { key: 'Enter' });
    expect(onChange).toHaveBeenCalledWith(['available']);
    expect(screen.queryByText('disabled', { selector: '.pool-multi-select__tag span' })).not.toBeInTheDocument();
  });
});
